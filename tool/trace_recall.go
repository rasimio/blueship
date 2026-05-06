package tool

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// ToolTraceRecall is the canonical tool name. See builtin.go for the
// rationale on per-package constants vs free-form strings.
const ToolTraceRecall = "trace_recall"

// trace_recall lets an agent look at its own recent execution. The OTel
// file exporter has been writing every span to disk for a while; until
// now nothing on the agent side could read it back, so the agent had no
// way to notice "I just made 14 tool calls in 30 minutes and 3 of them
// failed". This tool aggregates spans by name/role and returns a
// compact summary suitable for an LLM to reason over.
//
// Self-recursion guard: spans emitted by this tool itself
// (`tool.trace_recall`) are excluded from the result so the agent
// doesn't see its own act of looking at traces as part of the work it's
// trying to understand.

// rawSpan mirrors the stdouttrace JSON shape closely enough to extract
// what we need. Anything we don't read is left as json.RawMessage to
// keep parse cost down.
type rawSpan struct {
	Name       string         `json:"Name"`
	StartTime  time.Time      `json:"StartTime"`
	EndTime    time.Time      `json:"EndTime"`
	Attributes []rawAttribute `json:"Attributes"`
	Status     rawStatus      `json:"Status"`
}

type rawAttribute struct {
	Key   string   `json:"Key"`
	Value rawValue `json:"Value"`
}

type rawValue struct {
	Type  string          `json:"Type"`
	Value json.RawMessage `json:"Value"`
}

type rawStatus struct {
	Code        string `json:"Code"`
	Description string `json:"Description"`
}

// stringAttr returns a string attribute by key, or "" if absent.
func (s rawSpan) stringAttr(key string) string {
	for _, a := range s.Attributes {
		if a.Key != key || a.Value.Type != "STRING" {
			continue
		}
		var v string
		if json.Unmarshal(a.Value.Value, &v) == nil {
			return v
		}
	}
	return ""
}

// intAttr returns an INT64 attribute by key, or 0 if absent.
func (s rawSpan) intAttr(key string) int64 {
	for _, a := range s.Attributes {
		if a.Key != key || a.Value.Type != "INT64" {
			continue
		}
		var v int64
		if json.Unmarshal(a.Value.Value, &v) == nil {
			return v
		}
	}
	return 0
}

// boolAttr returns a BOOL attribute by key.
func (s rawSpan) boolAttr(key string) bool {
	for _, a := range s.Attributes {
		if a.Key != key || a.Value.Type != "BOOL" {
			continue
		}
		var v bool
		if json.Unmarshal(a.Value.Value, &v) == nil {
			return v
		}
	}
	return false
}

func (s rawSpan) durationMS() int64 {
	return s.EndTime.Sub(s.StartTime).Milliseconds()
}

// readSpans loads every span whose EndTime falls inside [since, now].
// We read the whole file (jsonl) and filter — at file sizes typical for
// a single agent (single-digit MB) this is faster and simpler than
// tail-from-end. If files grow past ~50MB this becomes a real concern;
// at that point, switch to a reverse-line scanner.
func readSpans(path string, since time.Time) ([]rawSpan, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open trace file: %w", err)
	}
	defer f.Close()

	var out []rawSpan
	scanner := bufio.NewScanner(f)
	// Some spans (e.g. agent_task.run with full output_size_bytes payload)
	// can exceed the 64 KiB default scan buffer. Bump it.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var sp rawSpan
		if err := json.Unmarshal(line, &sp); err != nil {
			continue // malformed line — skip, don't fail the whole call
		}
		if sp.EndTime.Before(since) {
			continue
		}
		out = append(out, sp)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read trace file: %w", err)
	}
	return out, nil
}

// toolStat / llmStat are aggregation buckets returned to the LLM.
// Field names use snake_case so the JSON looks natural in the prompt.
type toolStat struct {
	Name   string `json:"name"`
	Count  int    `json:"count"`
	AvgMS  int64  `json:"avg_ms"`
	MaxMS  int64  `json:"max_ms"`
	Errors int    `json:"errors"`
}

type llmStat struct {
	Model        string `json:"model"`
	Count        int    `json:"count"`
	InputTokens  int64  `json:"total_input_tokens"`
	OutputTokens int64  `json:"total_output_tokens"`
	AvgMS        int64  `json:"avg_ms"`
}

type slowSpan struct {
	Name       string `json:"name"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

type errorSpan struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	StartedAt   string `json:"started_at"`
}

type traceSummary struct {
	WindowMinutes int                  `json:"window_minutes"`
	TotalSpans    int                  `json:"total_spans"`
	ToolCalls     []toolStat           `json:"tool_calls,omitempty"`
	LLMCalls      []llmStat            `json:"llm_calls,omitempty"`
	AgentTasks    map[string]int       `json:"agent_tasks_by_handler,omitempty"`
	Errors        []errorSpan          `json:"errors,omitempty"`
	SlowSpans     []slowSpan           `json:"slow_spans,omitempty"`
	Note          string               `json:"note,omitempty"`
}

func summarize(spans []rawSpan, window int) traceSummary {
	out := traceSummary{
		WindowMinutes: window,
		TotalSpans:    len(spans),
	}

	type toolAgg struct {
		count     int
		totalMS   int64
		maxMS     int64
		errors    int
	}
	type llmAgg struct {
		count        int
		totalMS      int64
		inputTokens  int64
		outputTokens int64
	}

	tools := map[string]*toolAgg{}
	llms := map[string]*llmAgg{}
	handlers := map[string]int{}

	for _, sp := range spans {
		dur := sp.durationMS()
		isErr := sp.Status.Code == "Error" || sp.boolAttr("tool.is_error")

		switch {
		case len(sp.Name) > 5 && sp.Name[:5] == "tool.":
			toolName := sp.Name[5:]
			if toolName == "trace_recall" {
				continue // self-recursion guard
			}
			a := tools[toolName]
			if a == nil {
				a = &toolAgg{}
				tools[toolName] = a
			}
			a.count++
			a.totalMS += dur
			if dur > a.maxMS {
				a.maxMS = dur
			}
			if isErr {
				a.errors++
			}

		case sp.Name == "llm.complete":
			model := sp.stringAttr("llm.model")
			if model == "" {
				model = "unknown"
			}
			a := llms[model]
			if a == nil {
				a = &llmAgg{}
				llms[model] = a
			}
			a.count++
			a.totalMS += dur
			a.inputTokens += sp.intAttr("llm.tokens.input")
			a.outputTokens += sp.intAttr("llm.tokens.output")

		case sp.Name == "agent_task.run":
			handler := sp.stringAttr("agent_task.handler")
			if handler != "" {
				handlers[handler]++
			}
		}

		if isErr {
			out.Errors = append(out.Errors, errorSpan{
				Name:        sp.Name,
				Description: sp.Status.Description,
				StartedAt:   sp.StartTime.Format(time.RFC3339),
			})
		}
	}

	for name, a := range tools {
		avg := int64(0)
		if a.count > 0 {
			avg = a.totalMS / int64(a.count)
		}
		out.ToolCalls = append(out.ToolCalls, toolStat{
			Name: name, Count: a.count, AvgMS: avg, MaxMS: a.maxMS, Errors: a.errors,
		})
	}
	sort.Slice(out.ToolCalls, func(i, j int) bool {
		return out.ToolCalls[i].Count > out.ToolCalls[j].Count
	})

	for model, a := range llms {
		avg := int64(0)
		if a.count > 0 {
			avg = a.totalMS / int64(a.count)
		}
		out.LLMCalls = append(out.LLMCalls, llmStat{
			Model: model, Count: a.count,
			InputTokens: a.inputTokens, OutputTokens: a.outputTokens, AvgMS: avg,
		})
	}
	sort.Slice(out.LLMCalls, func(i, j int) bool {
		return out.LLMCalls[i].Count > out.LLMCalls[j].Count
	})

	if len(handlers) > 0 {
		out.AgentTasks = handlers
	}

	// Top 5 slowest spans regardless of category. Useful for catching
	// "why was that one call 12 seconds long" cases.
	sortedByDur := make([]rawSpan, len(spans))
	copy(sortedByDur, spans)
	sort.Slice(sortedByDur, func(i, j int) bool {
		return sortedByDur[i].durationMS() > sortedByDur[j].durationMS()
	})
	maxSlow := min(5, len(sortedByDur))
	for i := range maxSlow {
		sp := sortedByDur[i]
		detail := ""
		if sp.Name == "llm.complete" {
			detail = "model=" + sp.stringAttr("llm.model")
		} else if len(sp.Name) > 5 && sp.Name[:5] == "tool." {
			detail = "is_error=" + boolStr(sp.boolAttr("tool.is_error"))
		}
		out.SlowSpans = append(out.SlowSpans, slowSpan{
			Name: sp.Name, DurationMS: sp.durationMS(), Detail: detail,
		})
	}

	if out.TotalSpans == 0 {
		out.Note = "no spans in window — either the window is too short or telemetry was disabled"
	}

	return out
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// RegisterTraceRecall installs the trace_recall tool. Caller passes the
// path to the OTel file exporter's output; if empty, the tool is not
// registered (no other source for self-observation exists).
func RegisterTraceRecall(r *bs.ToolRegistry, tracePath string) {
	if tracePath == "" {
		return
	}

	r.Register(ToolTraceRecall,
		"Покажи свою собственную работу за последние N минут — какие тулы ты вызывала, к каким моделям обращалась, что упало, что было медленным. Это твой единственный канал самонаблюдения. Используй когда хочешь понять 'что я делала только что', найти узкие места ('почему долго думала?') или заметить ошибки которые сама не помнишь. Возвращает агрегаты: tool_calls (по имени), llm_calls (по модели), agent_tasks_by_handler, errors, slow_spans (top-5 по длительности).",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"window_minutes":{"type":"integer","default":30,"minimum":1,"maximum":1440,"description":"How far back to look (minutes). Default 30."}
			}
		}`),
		func(ctx context.Context, input json.RawMessage) (any, error) {
			var p struct {
				WindowMinutes int `json:"window_minutes"`
			}
			_ = json.Unmarshal(input, &p)
			if p.WindowMinutes <= 0 {
				p.WindowMinutes = 30
			}
			if p.WindowMinutes > 1440 {
				p.WindowMinutes = 1440
			}

			since := time.Now().Add(-time.Duration(p.WindowMinutes) * time.Minute)
			spans, err := readSpans(tracePath, since)
			if err != nil {
				return nil, err
			}
			return summarize(spans, p.WindowMinutes), nil
		},
	)
}
