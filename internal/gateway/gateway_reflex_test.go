package gateway

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

type reflexProviderFunc func(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error)

func (f reflexProviderFunc) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	return f(ctx, req)
}

type staticModelStore struct{}

func (staticModelStore) Load(context.Context) error                           { return nil }
func (staticModelStore) Get(string) bs.ModelRef                               { return bs.ModelRef{Provider: "test", Name: "reflex"} }
func (staticModelStore) ForRouter(string) string                              { return "test:reflex" }
func (staticModelStore) Update(context.Context, string, string, string) error { return nil }
func (staticModelStore) Roles() []string                                      { return []string{"reflex"} }
func (staticModelStore) Refresh(context.Context) error                        { return nil }

func TestInteractionTierPreparesCortexContextWithoutReflexLLM(t *testing.T) {
	userID := uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				Gateway: bs.GatewayConfig{InteractionTier: true},
			},
		},
		logger: logger,
	}

	preparerCalled := false
	us := &UserState{
		UserID: userID,
		ChatID: "telegram:1",
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(ctx context.Context, userID, message, priorContext string) *bs.ReflexContext {
				preparerCalled = true
				return &bs.ReflexContext{
					FormattedTraces: "[memory] important trace",
					MemoriesCount:   1,
					Strategy:        "warm",
				}
			},
			RuleEngine: func(ctx context.Context, rc bs.RuleContext) []bs.ActiveRule {
				return []bs.ActiveRule{{
					ID:      "rule-1",
					Trigger: "when done",
					Action:  "close note",
					Tools:   []string{"note_close"},
				}}
			},
		},
	}

	timings := newTurnTimer()
	result := g.runReflexPipeline(context.Background(), us, "готово", "prior", timings)

	if !preparerCalled {
		t.Fatalf("interaction-tier preflight should still call ReflexPreparer for Cortex context")
	}
	if result.InjectedCtx != "[memory] important trace" {
		t.Fatalf("Cortex context missing AME traces: %#v", result.InjectedCtx)
	}
	if result.MemoriesCount != 1 || result.Strategy != "warm" || us.LastStrategy != "warm" {
		t.Fatalf("context metadata not propagated: result=%+v last_strategy=%q", result, us.LastStrategy)
	}
	if !strings.Contains(result.ReflexGuidance, "close note") {
		t.Fatalf("rule guidance missing:\n%s", result.ReflexGuidance)
	}
	if len(result.CortexTools) != 1 || result.CortexTools[0] != "note_close" {
		t.Fatalf("forced Cortex tools not propagated: %#v", result.CortexTools)
	}
	for _, span := range timings.Report().Spans {
		if span.Name == "reflex_llm" {
			t.Fatalf("interaction-tier context prep must not call the old reflex planner LLM")
		}
	}
}

func TestInteractionTierRunsHostReflexPreActions(t *testing.T) {
	userID := uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				Gateway: bs.GatewayConfig{InteractionTier: true},
				ReflexPreActionSelector: func(ctx context.Context, req bs.ReflexPreActionRequest) []bs.ToolAction {
					return []bs.ToolAction{{Tool: "memory_associate", Input: json.RawMessage(`{"message":"` + req.Message + `"}`)}}
				},
			},
		},
		logger: logger,
	}

	reg := bs.NewToolRegistry()
	reg.Register("memory_associate", "associate", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		return map[string]any{"results": []string{}}, nil
	})
	us := &UserState{
		UserID:   userID,
		ChatID:   "telegram:1",
		Registry: reg,
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(ctx context.Context, userID, message, priorContext string) *bs.ReflexContext {
				return &bs.ReflexContext{FormattedTraces: "[retrieval]\nno_match", Strategy: "neutral"}
			},
		},
	}

	result := g.runReflexPipeline(context.Background(), us, "Как ты думаешь, я хороший разработчик?", "prior", newTurnTimer())

	if len(result.PreTraces) != 1 || result.PreTraces[0].Name != "memory_associate" {
		t.Fatalf("host reflex pre-action not executed: %#v", result.PreTraces)
	}
	if !strings.Contains(result.ReflexGuidance, "[memory_associate result]") {
		t.Fatalf("pre-action result missing from guidance:\n%s", result.ReflexGuidance)
	}
	if !strings.Contains(result.ReflexGuidance, "[memory grounding]") {
		t.Fatalf("no-match grounding guidance missing:\n%s", result.ReflexGuidance)
	}
	if !strings.Contains(result.ReflexGuidance, "I do not have enough saved evidence to judge that") {
		t.Fatalf("unsupported reassurance guard missing:\n%s", result.ReflexGuidance)
	}
}

func TestLegacyReflexHostPreActionsOverridePlannerPreActions(t *testing.T) {
	userID := uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				ReflexPreActionSelector: func(ctx context.Context, req bs.ReflexPreActionRequest) []bs.ToolAction {
					return []bs.ToolAction{{Tool: "memory_associate", Input: json.RawMessage(`{"message":"x"}`)}}
				},
			},
			ModelStore: staticModelStore{},
		},
		provider: reflexProviderFunc(func(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error) {
			return &bs.CompletionResponse{Content: []bs.ContentBlock{{
				Type: "text",
				Text: `{"intent":"free_reflection","confidence":0.95,"pre_actions":[{"tool":"browser_search","input":{"query":"x"}}],"tools":[]}`,
			}}}, nil
		}),
		logger:             logger,
		tz:                 time.UTC,
		reflexPlanTemplate: "%s\n%s\n%s\n%s",
	}

	browserSearchCalled := false
	reg := bs.NewToolRegistry()
	reg.Register("memory_associate", "associate", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		return map[string]any{"results": []string{}}, nil
	})
	reg.Register("browser_search", "search", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		browserSearchCalled = true
		return map[string]any{"results": []string{}}, nil
	})
	us := &UserState{
		UserID:   userID,
		ChatID:   "telegram:1",
		Registry: reg,
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(ctx context.Context, userID, message, priorContext string) *bs.ReflexContext {
				return &bs.ReflexContext{FormattedTraces: "[retrieval]\nno_match", Strategy: "neutral"}
			},
		},
	}

	result := g.runReflexPipeline(context.Background(), us, "Как ты думаешь, я хороший разработчик?", "prior", newTurnTimer())

	if browserSearchCalled {
		t.Fatal("planner browser_search should not run when host pre-actions are present")
	}
	if len(result.PreTraces) != 1 || result.PreTraces[0].Name != "memory_associate" {
		t.Fatalf("pre-traces = %#v, want only memory_associate; guidance=%q", result.PreTraces, result.ReflexGuidance)
	}
}

func TestAppendResearchGuidanceRequiresSourceMention(t *testing.T) {
	var guidance strings.Builder
	var research strings.Builder
	research.WriteString("[research]\n[browser_search result]\n{\"results\":[]}\n")

	appendResearchGuidance(&guidance, &research)
	got := guidance.String()
	for _, want := range []string{
		"[research usage]",
		"Search results are navigation only",
		"name the source/domain in the reply",
		"official publisher/company/source",
		"mixed research + action requests",
		"Do not add feature commentary",
		"[/research]",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("guidance missing %q:\n%s", want, got)
		}
	}
}

func TestRunReflexPreActionsFetchFirstSearchResultWhenRequested(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := &Gateway{logger: logger}
	reg := bs.NewToolRegistry()
	reg.Register("browser_search", "search", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		return map[string]any{"results": []map[string]any{
			{"url": "https://low.example/post", "tier": 5},
			{"url": "https://www.anthropic.com/news/claude-opus-4-7", "tier": 2},
		}}, nil
	})
	fetchedURL := ""
	reg.Register("browser_fetch", "fetch", json.RawMessage(`{"type":"object"}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(input, &p); err != nil {
			t.Fatal(err)
		}
		fetchedURL = p.URL
		return map[string]any{
			"url":   p.URL,
			"title": "Claude Opus 4.7",
			"text":  "Claude Opus 4.7 was released on April 16, 2026.",
		}, nil
	})
	us := &UserState{Registry: reg}
	var traces []agent.ToolTrace
	var research strings.Builder

	g.runReflexPreActions(context.Background(), us, newTurnTimer(), []bs.ToolAction{{
		Tool:  "browser_search",
		Input: json.RawMessage(`{"query":"Claude Opus 4.7 release date","fetch_first":true}`),
	}}, &traces, &research)

	if fetchedURL != "https://www.anthropic.com/news/claude-opus-4-7" {
		t.Fatalf("fetched URL = %q", fetchedURL)
	}
	if len(traces) != 2 || traces[0].Name != "browser_search" || traces[1].Name != "browser_fetch" {
		t.Fatalf("traces = %#v, want search then fetch", traces)
	}
	got := research.String()
	if !strings.Contains(got, "[browser_search result]") || !strings.Contains(got, "[browser_fetch result]") {
		t.Fatalf("research block missing search/fetch results:\n%s", got)
	}
}

func TestInteractionTierAmbiguousDeleteForcesDisambiguation(t *testing.T) {
	userID := uuid.New()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	preActionCalled := false
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				Gateway: bs.GatewayConfig{InteractionTier: true},
				ReflexPreActionSelector: func(ctx context.Context, req bs.ReflexPreActionRequest) []bs.ToolAction {
					preActionCalled = true
					return []bs.ToolAction{{Tool: "memory_associate", Input: json.RawMessage(`{"message":"x"}`)}}
				},
			},
		},
		logger: logger,
	}
	us := &UserState{
		UserID:   userID,
		ChatID:   "telegram:1",
		Registry: bs.NewToolRegistry(),
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(ctx context.Context, userID, message, priorContext string) *bs.ReflexContext {
				return &bs.ReflexContext{FormattedTraces: "[retrieval]\nno_match"}
			},
		},
	}

	result := g.runReflexPipeline(context.Background(), us, "Удали последнюю.", "prior", newTurnTimer())

	if preActionCalled {
		t.Fatalf("ambiguous delete should not run reflex pre-actions")
	}
	if !strings.Contains(result.ReflexGuidance, "[DISAMBIGUATION REQUIRED]") {
		t.Fatalf("disambiguation guidance missing:\n%s", result.ReflexGuidance)
	}
	if len(us.PendingDisambiguation) == 0 {
		t.Fatalf("pending disambiguation options not stored")
	}
	if len(result.PreTraces) != 0 {
		t.Fatalf("unexpected pre-traces: %#v", result.PreTraces)
	}
}
