package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	bs "github.com/rasimio/blueship/core"
	"github.com/rasimio/blueship/internal/browser"
)

// browser_search and browser_fetch are framework-level tools, not
// host-specific. Every BlueShip-based agent has the same need to
// resolve URLs and run web queries, and there's nothing persona-shaped
// about HTTP / chromedp / PDF decoding. They live in this package so a
// host wiring blueship.New() can opt in via RegisterBrowserTools(...)
// without re-implementing the chromedp glue per agent.
const (
	ToolBrowserSearch = "browser_search"
	ToolBrowserFetch  = "browser_fetch"
)

// RegisterBrowserTools adds browser_search and browser_fetch to the
// registry. Hosts call this from their fx wiring after blueship.New()
// has built the core deps. Tools run in-process via chromedp for HTML
// renders; PDF URLs (and PDFs found at HTML-named URLs) are decoded
// pure-Go via ledongthuc/pdf so there's no system dependency on
// poppler / pdftotext on the host machine.
func RegisterBrowserTools(r *bs.ToolRegistry, _ *bs.Deps) error {
	r.Register(ToolBrowserSearch,
		"Web search via headless Chrome (Google with automatic DuckDuckGo fallback on CAPTCHA). Returns {results:[{title,url,domain}], engine_used} — URLs and titles ONLY, no snippets, no descriptions. This is deliberate: search results are a navigation index, not a source of facts. You CANNOT cite anything based on search results alone. The next required step after search is browser_fetch on the most promising URLs; cite facts only from the rendered/extracted text browser_fetch returns. Use the domain field to assess source authority (arxiv.org, official docs, peer-reviewed venues rank higher than blog/SEO content). Runs through VPN (xray), no external API dependency.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"Search query"},
				"engine":{"type":"string","enum":["auto","google","ddg"],"default":"auto","description":"auto = google → ddg fallback on CAPTCHA"},
				"limit":{"type":"integer","default":8,"description":"Max results (1-20)"}
			},
			"required":["query"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				Query  string `json:"query"`
				Engine string `json:"engine"`
				Limit  int    `json:"limit"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			res, err := browser.Search(ctx, browser.SearchOptions{
				Query:  p.Query,
				Engine: p.Engine,
				Limit:  p.Limit,
			})
			if err != nil {
				// Surface multi-engine failures with structured detail
				// so the caller can decide whether to rephrase.
				var se *browser.SearchError
				if errors.As(err, &se) {
					return map[string]any{
						"error":          se.Error(),
						"fallback_after": se.Attempts,
					}, nil
				}
				return nil, fmt.Errorf("browser_search: %w", err)
			}
			return res, nil
		},
	)

	r.Register(ToolBrowserFetch,
		"Открывает URL и возвращает извлечённый текст + заголовок. HTML-страницы рендерятся через headless Chrome (полный JS execution), PDF-файлы (по расширению .pdf или по magic-байтам) декодируются pure-Go и возвращаются с разметкой `--- Page N ---` так что можно цитировать по страницам. Идёт через тот же прокси что browser_search (xray VPN). Используй после browser_search чтобы прочитать конкретный источник, цитировать факты с реальной страницы, или загрузить arxiv/гос-PDF для подробного разбора.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"url":{"type":"string","description":"Absolute URL to fetch (HTML or PDF)"},
				"wait_ms":{"type":"integer","default":3000,"description":"HTML render wait after navigation, ms (ignored for PDFs)"}
			},
			"required":["url"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				URL    string `json:"url"`
				WaitMS int    `json:"wait_ms"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			res, err := browser.Fetch(ctx, browser.FetchOptions{
				URL:    p.URL,
				WaitMS: p.WaitMS,
			})
			if err != nil {
				return nil, fmt.Errorf("browser_fetch: %w", err)
			}
			return res, nil
		},
	)

	return nil
}
