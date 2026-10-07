package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

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
		"Web search via headless Chrome (Google with automatic DuckDuckGo fallback on CAPTCHA). Returns {results:[{title,url,domain,tier}], engine_used} — URLs, titles, domain, and quality tier ONLY, no snippets, no descriptions. This is deliberate: search results are a navigation index, not a source of facts. You CANNOT cite anything based on search results alone. The next required step after search is browser_fetch on the most promising URLs; cite facts only from the rendered/extracted text browser_fetch returns. `tier` is a 1-5 source-quality signal: 1=peer-reviewed/official (arxiv, NeurIPS, nature), 2=official lab blog (anthropic.com, deepmind.com), 3=docs/Q&A (github docs, stackoverflow), 4=neutral default, 5=low-trust SEO/content-farm — prefer tier 1-2 and cross-check tier 4-5 against a tier 1-2 source before citing. Uses the local browser directly; no external search API dependency.",
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
				"wait_ms":{"type":"integer","default":3000,"description":"HTML render wait after navigation, ms (ignored for PDFs)"},
				"links_query":{"type":"string","description":"Filter observed page links by case-insensitive URL or label text before paging, without a new search or download"},
				"links_offset":{"type":"integer","minimum":0,"description":"Read observed page links starting at this index; use next_links_offset for more links from the cached source"},
				"offset_chars":{"type":"integer","minimum":0,"description":"Read from this character offset; use next_offset_chars to continue a long source"},
				"limit_chars":{"type":"integer","minimum":1,"maximum":20000,"description":"Maximum characters returned; default 4000 for background tasks, 10000 in chat"},
				"query":{"type":"string","description":"Find this exact case-insensitive phrase at or after offset_chars and return its surrounding passage"},
				"queries":{"type":"array","maxItems":6,"items":{"type":"string","maxLength":160},"description":"Find up to six distinct exact phrases in the same source in one call; query_excerpts share limit_chars. Mutually exclusive with query. Use for multiple required attributes before opening another source."}
			},
			"required":["url"]
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				LinksQuery  string   `json:"links_query"`
				LinksOffset int      `json:"links_offset"`
				URL         string   `json:"url"`
				WaitMS      int      `json:"wait_ms"`
				Offset      int      `json:"offset_chars"`
				Limit       int      `json:"limit_chars"`
				Query       string   `json:"query"`
				Queries     []string `json:"queries"`
			}
			if err := json.Unmarshal(input, &p); err != nil {
				return nil, err
			}
			if err := validateBrowserQueries(p.Query, p.Queries); err != nil {
				return nil, err
			}
			if len(p.Queries) > 0 && p.Limit > 0 && p.Limit < len(p.Queries) {
				return nil, fmt.Errorf("limit_chars must allow at least one character per query")
			}
			// Keep multi-source research turns small; the complete document
			// remains saved and query/offset can retrieve any later passage.
			if taskID, ok := bs.TaskIDFromContext(ctx); ok && taskID != uuid.Nil && p.Limit <= 0 {
				p.Limit = 4000
			}
			release, err := backgroundFetchGate.acquire(ctx, p.URL)
			if err != nil {
				return nil, err
			}
			defer release()
			if res := replayTaskFetch(ctx, deps, p.URL); res != nil {
				excerpt := excerptBrowserFetchQueries(res, p.Offset, p.Limit, p.Query, p.Queries)
				excerptBrowserLinks(&excerpt, filterBrowserLinks(res.Links, p.LinksQuery), p.LinksOffset)
				persistBrowserFetchWindow(ctx, deps, input, res, excerpt.Offset, browserQueryReadOffsets(excerpt)...)
				return excerpt, nil
			}
			res, err := browser.Fetch(ctx, browser.FetchOptions{
				URL:    p.URL,
				WaitMS: p.WaitMS,
			})
			if err != nil {
				return nil, fmt.Errorf("browser_fetch: %w", err)
			}
			observedAt := time.Now().UTC()
			res.ObservedAt = &observedAt
			excerpt := excerptBrowserFetchQueries(res, p.Offset, p.Limit, p.Query, p.Queries)
			excerptBrowserLinks(&excerpt, filterBrowserLinks(res.Links, p.LinksQuery), p.LinksOffset)
			persistBrowserFetchWindow(ctx, deps, input, res, excerpt.Offset, browserQueryReadOffsets(excerpt)...)
			return excerpt, nil
		},
	)

	if err := r.MarkReadOnly(ToolBrowserSearch, ToolBrowserFetch); err != nil {
		return err
	}
	return r.RegisterEvidenceReader(ToolBrowserFetch, func(ctx context.Context, input json.RawMessage) (any, error) {
		var p struct {
			URL         string   `json:"url"`
			Offset      int      `json:"offset_chars"`
			Limit       int      `json:"limit_chars"`
			Query       string   `json:"query"`
			Queries     []string `json:"queries"`
			LinksQuery  string   `json:"links_query"`
			LinksOffset int      `json:"links_offset"`
		}
		if err := json.Unmarshal(input, &p); err != nil {
			return nil, err
		}
		if err := validateBrowserQueries(p.Query, p.Queries); err != nil {
			return nil, err
		}
		if len(p.Queries) > 0 && p.Limit > 0 && p.Limit < len(p.Queries) {
			return nil, fmt.Errorf("limit_chars must allow at least one character per query")
		}
		res := replayTaskFetch(ctx, deps, p.URL)
		if res == nil {
			return nil, fmt.Errorf("saved source unavailable; external research is closed")
		}
		if p.Limit <= 0 {
			p.Limit = 4000
		}
		excerpt := excerptBrowserFetchQueries(res, p.Offset, p.Limit, p.Query, p.Queries)
		excerptBrowserLinks(&excerpt, filterBrowserLinks(res.Links, p.LinksQuery), p.LinksOffset)
		persistBrowserFetchWindow(ctx, deps, input, res, excerpt.Offset, browserQueryReadOffsets(excerpt)...)
		return excerpt, nil
	})
}

// taskFetchCacheTTL bounds how old a stored body may be and still answer a
// repeat request. Long enough to cover a full research run (the one that
// motivated this ran over an hour and had not finished), short enough that
// a task spanning days re-reads its sources instead of citing yesterday's
// copy of a page that has since changed.
const taskFetchCacheTTL = 6 * time.Hour

// replayTaskFetch returns the body this task already downloaded for url, or
// nil to let the real fetch run. Cache lookups are best-effort: any failure
// falls through to the network, because a slow fetch beats a wrong answer
// and beats an error.
//
// Chat-mode calls have no task in context and are never cached — the cache
// is scoped to one task's own reading, where "I read this page ten minutes
// ago in service of this exact goal" is a safe thing to assume.
func replayTaskFetch(ctx context.Context, deps *bs.Deps, url string) *browser.FetchResult {
	if deps == nil || strings.TrimSpace(url) == "" {
		return nil
	}
	taskID, ok := bs.TaskIDFromContext(ctx)
	if !ok || taskID == uuid.Nil {
		return nil
	}
	db, err := deps.DB("ship")
	if err != nil {
		return nil
	}
	store := bs.NewAgentTaskStore(db)

	// Gate C asked for these to be read again; a replay would answer a
	// question nobody asked.
	if forced, err := store.TaskRequiresRefetch(ctx, taskID, url); err != nil || forced {
		return nil
	}
	cached, err := store.LookupTaskFetch(ctx, taskID, url, taskFetchCacheTTL)
	if err != nil || cached == nil || cached.Output == "" {
		return nil
	}
	metaStr := func(key string) string {
		s, _ := cached.Metadata[key].(string)
		return s
	}
	// Keep partial reads in the audit, but allow the next fetch to recover.
	if metaStr("partial_error") != "" {
		return nil
	}
	pageCount := 0
	if n, okNum := cached.Metadata["page_count"].(float64); okNum {
		pageCount = int(n)
	}
	requested := metaStr("requested_url")
	if requested == "" {
		requested = url
	}
	finalURL := metaStr("final_url")
	if finalURL == "" {
		finalURL = url
	}
	if deps.Logger != nil {
		deps.Logger.Info("browser_fetch served from task cache",
			"task_id", taskID, "url", url,
			"fetched_at_iteration", cached.Iteration,
			"age_sec", int(time.Since(cached.FetchedAt).Seconds()),
			"chars", len(cached.Output))
	}
	var links []browser.PageLink
	if raw, err := json.Marshal(cached.Metadata["links"]); err == nil {
		_ = json.Unmarshal(raw, &links)
	}
	// Older source rows have only the original persistence timestamp. Never
	// replace this with the time of a cache hit: that would invent freshness.
	observedAt := cached.FetchedAt.UTC()
	if recorded, err := time.Parse(time.RFC3339Nano, metaStr("observed_at")); err == nil {
		observedAt = recorded.UTC()
	}
	return &browser.FetchResult{
		ObservedAt:        &observedAt,
		Links:             links,
		LinksLimitReached: cached.Metadata["links_limit_reached"] == true,
		SourceTruncated:   cached.Metadata["source_truncated"] == true,
		RequestedURL:      requested,
		URL:               finalURL,
		Title:             metaStr("title"),
		Text:              cached.Output,
		PageCount:         pageCount,
		SourceKind:        cached.OutputFormat,
		FromCache:         true,
	}
}

// persistBrowserFetchOutput adapts a browser.FetchResult into the
// generic ToolOutputRecord shape and writes it via persistToolOutput.
// Per-tool typed extras (requested_url for abstract→PDF rewrite
// auditing, page_count for PDFs, etc.) ride in the Metadata jsonb so
// the generic store doesn't grow typed columns per tool.
func persistBrowserFetchOutput(ctx context.Context, deps *bs.Deps, rawInput json.RawMessage, res *browser.FetchResult) {
	persistBrowserFetchWindow(ctx, deps, rawInput, res, 0)
}

func persistBrowserFetchWindow(ctx context.Context, deps *bs.Deps, rawInput json.RawMessage, res *browser.FetchResult, offset int, readOffsets ...int) {
	if res == nil {
		return
	}
	requested := res.RequestedURL
	if requested == "" {
		requested = res.URL
	}
	meta, err := json.Marshal(map[string]any{
		"observed_at":         res.ObservedAt,
		"read_offset_chars":   offset,
		"read_offsets_chars":  readOffsets,
		"partial_error":       res.PartialError,
		"links":               res.Links,
		"links_limit_reached": res.LinksLimitReached,
		"source_truncated":    res.SourceTruncated,
		"requested_url":       requested,
		"final_url":           res.URL,
		"title":               res.Title,
		"page_count":          res.PageCount,
		// Marks a row the cache produced. Kept out of the cache's own
		// source query so a replay can never renew a document's age.
		"from_cache": res.FromCache,
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
	// A fetched document is already an observed result. Preserve it even if
	// the task deadline fired between reading the source and writing its audit.
	// WithoutCancel retains task/owner context; the write still has a hard cap.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := store.RecordToolOutput(persistCtx, rec); err != nil && deps.Logger != nil {
		deps.Logger.Warn("tool output persist failed",
			"tool", rec.ToolName, "task_id", taskID,
			"iteration", rec.Iteration, "error", err)
	}
}
