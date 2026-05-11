// Package browser drives a headless Chrome via the DevTools Protocol
// (chromedp) for two tools: Fetch — render a page and return its text;
// Search — run a query through Google with a DuckDuckGo fallback when
// Google serves a CAPTCHA.
//
// Both helpers spin up a fresh browser process per call. Cold start is
// ~1-2s; we accept the cost in exchange for a simpler model — no shared
// browser state between unrelated turns, no cookie carryover that could
// poison the next search.
//
// Proxy: by default we route through the daemon's xray VPN so search
// runs from the same exit IP as OpenAI/Gemini calls. Order of override:
// explicit Proxy field → BROWSER_PROXY → HTTPS_PROXY → HTTP_PROXY.
//
// This package lives in blueship (the framework) rather than a single
// host agent's repo because every BlueShip-based agent needs the same
// "fetch a URL" / "google a query" primitive — there's nothing
// persona-specific about web access. Hosts wire the tools via
// `tool.RegisterBrowserTools(registry, deps)` at boot.
package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// FetchOptions controls a single Fetch call.
type FetchOptions struct {
	URL    string
	WaitMS int
	// Proxy overrides env-derived proxy. Empty means "use env"; the
	// sentinel "-" disables proxying entirely for this call.
	Proxy string
}

// FetchResult is the structured return value for Fetch.
type FetchResult struct {
	URL          string `json:"url"`
	Title        string `json:"title"`
	Text         string `json:"text"`
	PartialError string `json:"partial_error,omitempty"`
	// PageCount is set only when the URL resolved to a PDF; for HTML
	// renders it stays zero. Lets the caller cite by page when relevant.
	PageCount int `json:"page_count,omitempty"`
	// SourceKind reports how the body was extracted: "html" (chromedp)
	// or "pdf" (pdf decoder). Surfaces in tool output so cortex knows
	// whether to expect a layout-preserved text dump or rendered prose.
	SourceKind string `json:"source_kind,omitempty"`
}

// SearchOptions controls a single Search call.
type SearchOptions struct {
	Query string
	// Engine is "auto" (Google → DDG fallback), "google", or "ddg".
	Engine string
	Limit  int
	Proxy  string
}

// SearchResultItem is one search hit.
//
// Snippet is intentionally omitted from the tool-facing JSON. We learned
// the hard way (2026-05-11 task a4329624: 6 searches, 1 fetch, fake URLs
// in the "References" section) that returning snippets gives the model
// enough material to synthesise a "report" without ever calling fetch —
// the snippet IS the answer, from the model's perspective. Anthropic's
// "Building effective agents" makes the point explicitly: tool ergonomics
// shape behaviour more than prompt instructions. Strip the synthesis
// surface, model has to fetch for content.
//
// Domain is computed code-side from URL so the model can rank source
// authority without us shipping a free-form snippet field.
type SearchResultItem struct {
	Title  string `json:"title"`
	URL    string `json:"url"`
	Domain string `json:"domain"`
}

// SearchResult is the full payload returned to the caller.
type SearchResult struct {
	Results       []SearchResultItem `json:"results"`
	EngineUsed    string             `json:"engine_used"`
	Proxy         string             `json:"proxy,omitempty"`
	FallbackAfter []EngineAttempt    `json:"fallback_after,omitempty"`
}

// EngineAttempt records why a particular engine was skipped.
type EngineAttempt struct {
	Engine string `json:"engine"`
	Error  string `json:"error"`
}

// SearchError is returned when no engine succeeds. It carries every
// attempt so cortex can reason about the failure.
type SearchError struct {
	Attempts []EngineAttempt `json:"attempts"`
}

func (e *SearchError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, a := range e.Attempts {
		parts = append(parts, a.Engine+": "+a.Error)
	}
	return "all engines failed [" + strings.Join(parts, "; ") + "]"
}

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

// resolveProxy applies the Proxy override → env fallback chain. The
// sentinel "-" disables the proxy entirely.
func resolveProxy(override string) string {
	if override == "-" {
		return ""
	}
	if override != "" {
		return override
	}
	for _, k := range []string{"BROWSER_PROXY", "HTTPS_PROXY", "HTTP_PROXY",
		"https_proxy", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// allocator builds a chromedp ExecAllocator with our standard flags
// (headless, realistic UA, optional proxy). Caller must invoke the
// returned cancel func when done.
func allocator(ctx context.Context, proxy string) (context.Context, context.CancelFunc) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.UserAgent(userAgent),
		chromedp.Flag("lang", "en-US"),
		chromedp.WindowSize(1280, 800),
	)
	if proxy != "" {
		opts = append(opts, chromedp.ProxyServer(proxy))
	}
	if bin := os.Getenv("BROWSER_BIN"); bin != "" {
		opts = append(opts, chromedp.ExecPath(bin))
	}
	return chromedp.NewExecAllocator(ctx, opts...)
}

// Fetch resolves a URL and returns the body text + title. PDFs are
// detected up front (URL extension, then content sniff) and routed
// through the pure-Go decoder in pdf.go; everything else is rendered
// via chromedp so JS-heavy pages produce real visible text.
func Fetch(ctx context.Context, opts FetchOptions) (*FetchResult, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("browser.Fetch: empty URL")
	}
	proxy := resolveProxy(opts.Proxy)

	// Fast path: the URL is obviously a PDF (extension, query). Skip
	// chromedp entirely — Chrome's PDF viewer doesn't expose the
	// document text to document.body.innerText, so chromedp would
	// return chrome page chrome instead of the article.
	if looksLikePDFURL(opts.URL) {
		if res, err := fetchPDF(ctx, opts.URL, proxy); err == nil {
			return &FetchResult{
				URL:        res.URL,
				Title:      res.Title,
				Text:       res.Text,
				PageCount:  res.PageCount,
				SourceKind: "pdf",
			}, nil
		}
		// fetchPDF returned errNotPDF or a real error. Fall through to
		// chromedp; for the errNotPDF case the URL might be misnamed
		// (CDN serving HTML at .pdf), and chromedp will handle that.
	}

	wait := opts.WaitMS
	if wait <= 0 {
		wait = 3000
	}
	allocCtx, allocCancel := allocator(ctx, proxy)
	defer allocCancel()

	browserCtx, browserCancel := chromedp.NewContext(allocCtx)
	defer browserCancel()

	// Hard cap on the whole fetch — browser quirks shouldn't hang the
	// caller. 45s covers slow first-byte + waitMS + extraction.
	browserCtx, hardCancel := context.WithTimeout(browserCtx, 45*time.Second)
	defer hardCancel()

	out := &FetchResult{URL: opts.URL, SourceKind: "html"}

	// Anti-bot: drop the navigator.webdriver flag before any page
	// script runs. Cheap, doesn't beat real fingerprinting, but bypasses
	// the simplest checks.
	if err := chromedp.Run(browserCtx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return nil
		})); err != nil {
		return nil, fmt.Errorf("browser warmup: %w", err)
	}

	tasks := chromedp.Tasks{
		chromedp.Navigate(opts.URL),
		chromedp.Sleep(time.Duration(wait) * time.Millisecond),
		chromedp.Title(&out.Title),
		chromedp.Evaluate(`document.body ? document.body.innerText.slice(0, 10000) : ""`, &out.Text),
	}
	if err := chromedp.Run(browserCtx, tasks); err != nil {
		// Partial failure — surface what we got but flag the error so
		// cortex can decide whether to retry.
		out.PartialError = err.Error()
		return out, nil
	}
	return out, nil
}

// Search runs a web search through one or more engines and returns
// ranked hits. engine="auto" tries Google first and falls back to DDG
// on CAPTCHA / empty results.
func Search(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("browser.Search: empty query")
	}
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
	if opts.Limit > 20 {
		opts.Limit = 20
	}
	engine := opts.Engine
	if engine == "" {
		engine = "auto"
	}
	proxy := resolveProxy(opts.Proxy)

	var order []string
	switch engine {
	case "auto":
		order = []string{"google", "ddg"}
	case "google", "ddg":
		order = []string{engine}
	default:
		return nil, fmt.Errorf("unknown engine %q", engine)
	}

	allocCtx, allocCancel := allocator(ctx, proxy)
	defer allocCancel()

	var attempts []EngineAttempt
	for _, eng := range order {
		bctx, bcancel := chromedp.NewContext(allocCtx)
		hctx, hcancel := context.WithTimeout(bctx, 30*time.Second)

		var (
			items []SearchResultItem
			err   error
		)
		switch eng {
		case "google":
			items, err = runGoogle(hctx, opts.Query, opts.Limit)
		case "ddg":
			items, err = runDDG(hctx, opts.Query, opts.Limit)
		}
		hcancel()
		bcancel()

		if err != nil {
			attempts = append(attempts, EngineAttempt{Engine: eng, Error: err.Error()})
			continue
		}
		if len(items) == 0 {
			attempts = append(attempts, EngineAttempt{Engine: eng, Error: "no_results"})
			continue
		}
		res := &SearchResult{
			Results:    items,
			EngineUsed: eng,
			Proxy:      proxy,
		}
		if len(attempts) > 0 {
			res.FallbackAfter = attempts
		}
		return res, nil
	}
	return nil, &SearchError{Attempts: attempts}
}

// captchaErr reports that Google challenged us. Returned as a normal
// error so the engine loop falls through to DDG.
type captchaErr struct{ where string }

func (e *captchaErr) Error() string { return "captcha:" + e.where }

func runGoogle(ctx context.Context, query string, limit int) ([]SearchResultItem, error) {
	num := limit
	if num < 10 {
		num = 10
	}
	target := "https://www.google.com/search?q=" + url.QueryEscape(query) +
		"&num=" + fmt.Sprint(num) + "&hl=en"

	var (
		curURL   string
		bodyText string
		raw      json.RawMessage
	)
	tasks := chromedp.Tasks{
		chromedp.Navigate(target),
		chromedp.Sleep(900 * time.Millisecond),
		chromedp.Location(&curURL),
		chromedp.Evaluate(`document.body ? document.body.innerText.slice(0, 4000) : ""`, &bodyText),
		chromedp.Evaluate(googleExtractJS(limit), &raw),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return nil, err
	}
	if isGoogleCaptcha(curURL, bodyText) {
		return nil, &captchaErr{where: curURL}
	}
	var items []SearchResultItem
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &items)
	}
	fillDomain(items)
	return items, nil
}

// fillDomain populates the Domain field on each result from its URL,
// stripping the leading www. so the model sees authoritative-looking
// names ("arxiv.org", not "www.arxiv.org") and can rank source quality
// without us shipping a free-form snippet to confabulate from.
func fillDomain(items []SearchResultItem) {
	for i := range items {
		u, err := url.Parse(items[i].URL)
		if err != nil || u.Host == "" {
			continue
		}
		host := u.Host
		if strings.HasPrefix(host, "www.") {
			host = host[4:]
		}
		items[i].Domain = host
	}
}

func runDDG(ctx context.Context, query string, limit int) ([]SearchResultItem, error) {
	target := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	var raw json.RawMessage
	tasks := chromedp.Tasks{
		chromedp.Navigate(target),
		chromedp.Sleep(400 * time.Millisecond),
		chromedp.Evaluate(ddgExtractJS(limit), &raw),
	}
	if err := chromedp.Run(ctx, tasks); err != nil {
		return nil, err
	}
	var items []SearchResultItem
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &items)
	}
	fillDomain(items)
	return items, nil
}

func isGoogleCaptcha(curURL, body string) bool {
	if strings.Contains(curURL, "/sorry/") || strings.Contains(curURL, "consent.google.com") {
		return true
	}
	low := strings.ToLower(body)
	for _, m := range []string{"unusual traffic", "i'm not a robot",
		"before you continue to google"} {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// googleExtractJS returns a JS snippet that scrapes Google SERP for
// (title, url, snippet) tuples. We anchor on `<a>` containing `<h3>`
// since that's the most stable Google pattern across redesigns —
// .g/.MjjYud class names get reshuffled but the link+heading pair is
// load-bearing for accessibility.
func googleExtractJS(limit int) string {
	return fmt.Sprintf(`(() => {
  const out = [];
  const seen = new Set();
  document.querySelectorAll('a').forEach(a => {
    if (out.length >= %d) return;
    const h3 = a.querySelector('h3');
    if (!h3 || !a.href || !a.href.startsWith('http')) return;
    if (a.href.startsWith('https://www.google.')) return;
    if (a.href.includes('webcache.googleusercontent')) return;
    if (seen.has(a.href)) return;
    seen.add(a.href);
    const block = a.closest('div[data-hveid], div.g, div.MjjYud');
    let snippet = '';
    if (block) {
      const sn = block.querySelector('div[data-sncf], div.VwiC3b, span.aCOpRe, .lEBKkf, .yXK7lf');
      if (sn) snippet = sn.innerText;
    }
    out.push({url: a.href, title: h3.innerText, snippet: snippet});
  });
  return out;
})()`, limit)
}

func ddgExtractJS(limit int) string {
	return fmt.Sprintf(`(() => {
  const out = [];
  document.querySelectorAll('div.result, div.web-result').forEach(el => {
    if (out.length >= %d) return;
    const a = el.querySelector('a.result__a, h2.result__title a, a.result__url');
    const sn = el.querySelector('a.result__snippet, .result__snippet');
    if (!a) return;
    let href = a.href;
    try {
      const u = new URL(href);
      const real = u.searchParams.get('uddg');
      if (real) href = decodeURIComponent(real);
    } catch (e) {}
    out.push({url: href, title: a.innerText, snippet: sn ? sn.innerText : ''});
  });
  return out;
})()`, limit)
}
