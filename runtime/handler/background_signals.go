package handler

import (
	"context"
	"regexp"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

// recentSearchResultURLs pulls URLs out of browser_search outputs in
// the last N iterations of a task. Returns deduplicated URLs in the
// order they appeared (most recent first), capped at the limit so the
// injected prompt block doesn't bloat. Used by the fetch-rhythm
// dictation in Background.Run — the agent gets a concrete list of
// "open one of THESE" instead of an abstract "fetch something".
func recentSearchResultURLs(ctx context.Context, deps core.AgentDeps, taskID uuid.UUID, limit int) []string {
	if deps.DB == nil {
		return nil
	}
	db, err := deps.DB("ship")
	if err != nil {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		WITH recent AS (
		  SELECT tool_calls FROM blueship.agent_task_iterations
		  WHERE task_id = $1 ORDER BY iteration DESC LIMIT 3
		)
		SELECT tc->>'output' FROM recent, jsonb_array_elements(tool_calls) AS tc
		WHERE tc->>'name' = 'browser_search'`, taskID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	urlRe := reBrowserSearchURL
	seen := map[string]struct{}{}
	out := []string{}
	for rows.Next() {
		var output string
		if err := rows.Scan(&output); err != nil {
			continue
		}
		for _, m := range urlRe.FindAllStringSubmatch(output, -1) {
			u := m[1]
			if _, ok := seen[u]; ok {
				continue
			}
			seen[u] = struct{}{}
			out = append(out, u)
			if len(out) >= limit {
				return out
			}
		}
	}
	return out
}

// reBrowserSearchURL pulls "url":"…" pairs out of a browser_search
// tool result. The result JSON looks like
//
//	{"results":[{"title":"…","url":"https://…","snippet":"…"}, …]}
//
// and a regex over the serialised form is cheaper than parsing the
// whole nested JSON when we just want the URLs.
var reBrowserSearchURL = regexp.MustCompile(`"url"\s*:\s*"(https?://[^"]+)"`)

// recentBrowserToolUsage counts browser_search vs browser_fetch calls
// across the last `lastN` iterations of a task by reading the
// agent_task_iterations audit log. Returns (searches, fetches). Used
// by the fetch-rhythm enforcement to inject a hard warning into the
// next iteration's user message when the model is searching without
// reading. Both zero on DB errors — caller treats absence of data as
// "no enforcement needed", which is the safe fallback.
func recentBrowserToolUsage(ctx context.Context, deps core.AgentDeps, taskID uuid.UUID, lastN int) (searches, fetches int) {
	if deps.DB == nil {
		return 0, 0
	}
	db, err := deps.DB("ship")
	if err != nil {
		return 0, 0
	}
	rows, err := db.QueryContext(ctx, `
		WITH recent AS (
		  SELECT tool_calls FROM blueship.agent_task_iterations
		  WHERE task_id = $1 ORDER BY iteration DESC LIMIT $2
		)
		SELECT tc->>'name' FROM recent, jsonb_array_elements(tool_calls) AS tc`,
		taskID, lastN)
	if err != nil {
		return 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		switch name {
		case "browser_search":
			searches++
		case "browser_fetch":
			fetches++
		}
	}
	return searches, fetches
}

// Background implements core.AgentHandler for recurring scheduled tasks
// (heartbeat, inner-thought, session-summary, etc.). Long-running goals
// have their own lifecycle primitive (core.Goal) and their own executor
// (StructuredGoalExecutor). This handler no longer knows anything about
// goals.
//
// Uses a shared session across iterations for non-recurring tasks, so the
// LLM sees full conversation history. Recurring tasks (schedule != nil)
// get a fresh session per tick to bound history growth.
//
// Auto-pause: if the LLM calls a tool flagged async/peer-callable (via its
// registration metadata — see core.ToolRegistry), the handler pauses
// until an external callback wakes the task. The list of pause-triggering
// tool names is agent-configurable; it is NOT hardcoded here.
