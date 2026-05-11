package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

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
//
// deps is captured by the browser_fetch closure so we can write a row
// into agent_task_fetched_docs every time a fetch happens during an
// agent_task iteration. Persisting the full body lets the grounding
// evaluator (Gate C) audit claims against real page text rather than
// the 500-char truncated tool trace.
func RegisterBrowserTools(r *bs.ToolRegistry, deps *bs.Deps) error {
	r.Register(ToolBrowserSearch,
		"Web search via headless Chrome (Google with automatic DuckDuckGo fallback on CAPTCHA). Returns {results:[{title,url,domain,tier}], engine_used} — URLs, titles, domain, and quality tier ONLY, no snippets, no descriptions. This is deliberate: search results are a navigation index, not a source of facts. You CANNOT cite anything based on search results alone. The next required step after search is browser_fetch on the most promising URLs; cite facts only from the rendered/extracted text browser_fetch returns. `tier` is a 1-5 source-quality signal: 1=peer-reviewed/official (arxiv, NeurIPS, nature), 2=official lab blog (anthropic.com, deepmind.com), 3=docs/Q&A (github docs, stackoverflow), 4=neutral default, 5=low-trust SEO/content-farm — prefer tier 1-2 and cross-check tier 4-5 against a tier 1-2 source before citing. Runs through VPN (xray), no external API dependency.",
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
			persistFetchedDoc(ctx, deps, res)
			return res, nil
		},
	)

	return nil
}

// persistFetchedDoc writes a row into agent_task_fetched_docs when a
// browser_fetch runs inside an agent_task iteration. Chat-mode fetches
// (no task id in ctx) skip persistence — the doc table is sized for
// background research, not every assistant turn. Errors are logged,
// not returned: a transient DB hiccup must not fail an otherwise-good
// fetch the model already paid for.
func persistFetchedDoc(ctx context.Context, deps *bs.Deps, res *browser.FetchResult) {
	if deps == nil || res == nil {
		return
	}
	taskID, ok := bs.TaskIDFromContext(ctx)
	if !ok || taskID == uuid.Nil {
		return // chat-mode call, nothing to attribute
	}
	iteration, _ := bs.IterationFromContext(ctx)

	db, err := deps.DB("ship")
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("browser_fetch: persist skipped, ship DB unavailable",
				"task_id", taskID, "error", err)
		}
		return
	}
	requested := res.RequestedURL
	if requested == "" {
		requested = res.URL
	}
	store := bs.NewAgentTaskStore(db)
	err = store.RecordFetchedDoc(ctx, bs.FetchedDocRecord{
		TaskID:       taskID,
		Iteration:    iteration,
		RequestedURL: requested,
		FinalURL:     res.URL,
		Title:        res.Title,
		Text:         res.Text,
		SourceKind:   res.SourceKind,
		PageCount:    res.PageCount,
	})
	if err != nil && deps.Logger != nil {
		deps.Logger.Warn("browser_fetch: persist failed",
			"task_id", taskID, "iteration", iteration,
			"url", res.URL, "error", err)
	}
}
