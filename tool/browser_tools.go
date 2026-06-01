package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/webaccess/browser"
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
		"Open a URL and return the extracted text + title. HTML pages are rendered via headless Chrome (full JS execution); PDF files (by .pdf extension or magic bytes) are decoded in pure Go and returned with `--- Page N ---` markers so you can cite by page. Use after browser_search to read a specific source, quote facts from the real page, or load an arxiv/long PDF for detailed analysis.",
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
			persistBrowserFetchOutput(ctx, deps, input, res)
			return res, nil
		},
	)

	return nil
}

// persistBrowserFetchOutput adapts a browser.FetchResult into the
// generic ToolOutputRecord shape and writes it via persistToolOutput.
// Per-tool typed extras (requested_url for abstract→PDF rewrite
// auditing, page_count for PDFs, etc.) ride in the Metadata jsonb so
// the generic store doesn't grow typed columns per tool.
func persistBrowserFetchOutput(ctx context.Context, deps *bs.Deps, rawInput json.RawMessage, res *browser.FetchResult) {
	if res == nil {
		return
	}
	requested := res.RequestedURL
	if requested == "" {
		requested = res.URL
	}
	meta, err := json.Marshal(map[string]any{
		"requested_url": requested,
		"final_url":     res.URL,
		"title":         res.Title,
		"page_count":    res.PageCount,
	})
	if err != nil {
		meta = json.RawMessage(`{}`)
	}
	persistToolOutput(ctx, deps, bs.ToolOutputRecord{
		ToolName:     ToolBrowserFetch,
		ToolInput:    rawInput,
		Output:       res.Text,
		OutputFormat: res.SourceKind, // "html" or "pdf"
		Metadata:     meta,
	})
}

// persistToolOutput writes a row into agent_task_tool_outputs when a
// tool runs inside an agent_task iteration. Chat-mode invocations (no
// task id in ctx) skip persistence — the store is sized for background
// agent runs, not every assistant turn. Errors are logged, not
// returned: a transient DB hiccup must not fail an otherwise-good tool
// call the model already paid for.
//
// Exposed at package scope so other tool registrations (a peer repo-read tool,
// db_query, file_read, etc.) can plug into the same audit store
// without each rewriting the ctx → store boilerplate.
func persistToolOutput(ctx context.Context, deps *bs.Deps, rec bs.ToolOutputRecord) {
	if deps == nil {
		return
	}
	taskID, ok := bs.TaskIDFromContext(ctx)
	if !ok || taskID == uuid.Nil {
		return // chat-mode call, nothing to attribute
	}
	rec.TaskID = taskID
	rec.Iteration, _ = bs.IterationFromContext(ctx)

	db, err := deps.DB("ship")
	if err != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("tool output persist skipped, ship DB unavailable",
				"tool", rec.ToolName, "task_id", taskID, "error", err)
		}
		return
	}
	store := bs.NewAgentTaskStore(db)
	if err := store.RecordToolOutput(ctx, rec); err != nil && deps.Logger != nil {
		deps.Logger.Warn("tool output persist failed",
			"tool", rec.ToolName, "task_id", taskID,
			"iteration", rec.Iteration, "error", err)
	}
}
