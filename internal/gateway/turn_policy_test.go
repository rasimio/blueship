package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

func TestNormalizeTurnPolicyFailsClosedWhenRequiredToolUnavailable(t *testing.T) {
	policy := normalizeTurnPolicy(bs.TurnPolicy{
		RequestedMode: bs.TurnPolicyOn,
		EffectiveMode: bs.TurnPolicyOn,
		Tools:         []string{"chat_recall", "note_create"},
		Strict:        true,
		Guidance:      "tool-specific",
		PreActions:    []bs.ToolAction{{Tool: "chat_recall"}},
	}, []string{"memory_search", "note_create"})

	if policy.EffectiveMode != bs.TurnPolicyOff || policy.Strict ||
		policy.Tools != nil || policy.Guidance != "" || len(policy.PreActions) != 0 {
		t.Fatalf("policy did not fail closed: %+v", policy)
	}
	if !strings.Contains(policy.UnavailableReason, "chat_recall") ||
		!containsTestString(policy.DeniedTools, "chat_recall") {
		t.Fatalf("missing availability reason/denial: %+v", policy)
	}
	if containsTestString(policy.DeniedTools, "note_create") {
		t.Fatalf("fail-off denied ordinary explicit action tool: %+v", policy)
	}
}

func TestResolveTurnPolicyExpandsLateMCPPeerToConcreteTools(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	registry := bs.NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) { return "ok", nil })
	source := &recordingMCPSource{tools: []bs.MCPTool{{
		Name:        "mcp__notion__add_page",
		Description: "add a Notion page",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Handler: func(context.Context, json.RawMessage) (any, error) {
			return "created", nil
		},
	}}}
	var resolverRequest bs.TurnPolicyRequest
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				MCPSource: source,
				TurnPolicyResolver: func(_ context.Context, req bs.TurnPolicyRequest) bs.TurnPolicy {
					resolverRequest = req
					return bs.TurnPolicy{
						RequestedMode: bs.TurnPolicyOn,
						EffectiveMode: bs.TurnPolicyOn,
						Tools:         []string{"chat_recall", "peer:mcp"},
						PreActions:    []bs.ToolAction{{Tool: "chat_recall"}},
					}
				},
			},
			RoleTools: bs.NewRoleToolStore(map[string][]string{
				"cortex": {"chat_recall", "peer:mcp"},
			}),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	us := &UserState{UserID: userID, SoulID: soulID, Registry: registry}
	policyRegistry := g.cortexTurnRegistry(context.Background(), us, false)

	policy, _ := g.resolveTurnPolicy(context.Background(), us, policyRegistry,
		"ты это говорила? и добавь в Notion", false)

	if source.loaded != soulID {
		t.Fatalf("MCP source loaded for %s, want %s", source.loaded, soulID)
	}
	for _, name := range []string{"chat_recall", "peer:mcp", "mcp__notion__add_page"} {
		if !containsTestString(resolverRequest.AvailableTools, name) {
			t.Fatalf("resolver available tools %#v missing %q", resolverRequest.AvailableTools, name)
		}
	}
	if containsTestString(policy.Tools, "peer:mcp") ||
		!containsTestString(policy.Tools, "mcp__notion__add_page") {
		t.Fatalf("policy tools were not concretized: %#v", policy.Tools)
	}
}

func TestResolveTurnPolicyFailsClosedOnSoulPolicyError(t *testing.T) {
	userID, soulID := uuid.New(), uuid.New()
	registry := bs.NewToolRegistry()
	for _, name := range []string{"chat_recall", "note_create"} {
		registry.Register(name, name, json.RawMessage(`{"type":"object"}`),
			func(context.Context, json.RawMessage) (any, error) { return "ok", nil })
	}
	g := &Gateway{
		deps: &bs.Deps{
			Config: &bs.Config{
				Gateway: bs.GatewayConfig{
					ResolveSoulToolPolicy: func(context.Context, uuid.UUID) (map[string]bool, []string, error) {
						return nil, nil, errors.New("policy database unavailable")
					},
				},
				TurnPolicyResolver: func(context.Context, bs.TurnPolicyRequest) bs.TurnPolicy {
					return bs.TurnPolicy{
						EffectiveMode: bs.TurnPolicyOn,
						Tools:         []string{"chat_recall"},
						Strict:        true,
						PreActions:    []bs.ToolAction{{Tool: "chat_recall"}},
					}
				},
			},
			RoleTools: bs.NewRoleToolStore(map[string][]string{
				"cortex": {"chat_recall", "note_create"},
			}),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	policy, allowed := g.resolveTurnPolicy(
		context.Background(),
		&UserState{UserID: userID, SoulID: soulID, Registry: registry},
		registry,
		"ты это говорила?",
		false,
	)

	if policy.EffectiveMode != bs.TurnPolicyOff || policy.Strict ||
		len(policy.PreActions) != 0 || len(policy.ObserverActions) != 0 {
		t.Fatalf("policy did not fail closed: %+v", policy)
	}
	if allowed == nil || len(allowed) != 0 {
		t.Fatalf("allowed snapshot = %#v, want explicit empty", allowed)
	}
	for _, name := range []string{"chat_recall", "note_create"} {
		if !containsTestString(policy.DeniedTools, name) {
			t.Fatalf("denied tools %#v missing %q", policy.DeniedTools, name)
		}
	}
	if policy.UnavailableReason != "soul_tool_policy_unavailable" {
		t.Fatalf("unavailable reason = %q", policy.UnavailableReason)
	}
}

func TestApplyTurnPolicyIsAtomicAcrossOffAndActive(t *testing.T) {
	ordinary := agent.RunConfig{
		ToolOverride:     []string{"notes"},
		ToolboxExpansion: []string{"notes", "browser"},
	}
	applyTurnPolicy(&ordinary, bs.TurnPolicy{
		EffectiveMode: bs.TurnPolicyOff,
		DeniedTools:   []string{"chat_recall"},
	})
	if ordinary.StrictTools || ordinary.TurnPolicyActive ||
		len(ordinary.ToolOverride) != 1 || ordinary.ToolOverride[0] != "notes" {
		t.Fatalf("off mode changed ordinary selection: %+v", ordinary)
	}
	if !containsTestString(ordinary.DeniedTools, "chat_recall") {
		t.Fatalf("off denial missing: %+v", ordinary)
	}

	active := ordinary
	applyTurnPolicy(&active, bs.TurnPolicy{
		EffectiveMode: bs.TurnPolicyCanary,
		Tools:         []string{"chat_recall"},
		Strict:        true,
		Source:        "test",
	})
	if !active.StrictTools || !active.TurnPolicyActive ||
		len(active.ToolOverride) != 1 || active.ToolOverride[0] != "chat_recall" ||
		active.ToolboxExpansion != nil {
		t.Fatalf("active mode not applied atomically: %+v", active)
	}
}

func TestApplyTurnPolicyRequireCortexDoesNotActivateTools(t *testing.T) {
	cfg := agent.RunConfig{
		ToolOverride:     []string{"notes"},
		ToolboxExpansion: []string{"notes", "browser"},
	}
	applyTurnPolicy(&cfg, bs.TurnPolicy{
		EffectiveMode: bs.TurnPolicyOff,
		RequireCortex: true,
		DeniedTools:   []string{"chat_recall"},
	})

	if !cfg.TurnPolicyActive {
		t.Fatal("RequireCortex did not bypass the optional fast tier")
	}
	if cfg.StrictTools {
		t.Fatal("inactive policy enabled strict tools")
	}
	if len(cfg.ToolOverride) != 1 || cfg.ToolOverride[0] != "notes" ||
		len(cfg.ToolboxExpansion) != 2 {
		t.Fatalf("inactive policy changed ordinary tool selection: %+v", cfg)
	}
	if !containsTestString(cfg.DeniedTools, "chat_recall") {
		t.Fatalf("inactive policy denial missing: %+v", cfg)
	}
}

func TestInactivePolicyCannotRunObserverActions(t *testing.T) {
	called := make(chan struct{}, 1)
	registry := bs.NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			called <- struct{}{}
			return "ok", nil
		})
	g := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	us := &UserState{Registry: registry}

	policy := normalizeTurnPolicy(bs.TurnPolicy{
		EffectiveMode:   bs.TurnPolicyOff,
		ObserverActions: []bs.ToolAction{{Tool: "chat_recall"}},
	}, []string{"chat_recall"})
	if len(policy.ObserverActions) != 0 {
		t.Fatalf("off policy retained observer actions: %+v", policy)
	}
	g.startTurnPolicyObservers(context.Background(), us, policy)
	select {
	case <-called:
		t.Fatal("off policy executed a shadow observer")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestApplyTurnToolPolicyEnforcesDenialOnEphemeralTurn(t *testing.T) {
	selectorCalled := false
	g := &Gateway{
		deps: &bs.Deps{Config: &bs.Config{
			ToolSelector: func(context.Context, bs.ToolSelectionRequest) bs.ToolSelection {
				selectorCalled = true
				return bs.ToolSelection{Tools: []string{"chat_recall"}}
			},
		}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	cfg := agent.RunConfig{Ephemeral: true}
	g.applyTurnToolPolicy(context.Background(), bs.NewToolRegistry(), &cfg, "private ask", nil, true,
		bs.TurnPolicy{
			EffectiveMode: bs.TurnPolicyOff,
			DeniedTools:   []string{"chat_recall"},
		})
	if selectorCalled {
		t.Fatal("ephemeral turn invoked ordinary ToolSelector")
	}
	if !containsTestString(cfg.DeniedTools, "chat_recall") {
		t.Fatalf("ephemeral denial missing: %+v", cfg)
	}
}

func containsTestString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestTurnPolicyShadowObserverIsNonBlocking(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	registry := bs.NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			close(started)
			<-release
			return map[string]any{"status": "found"}, nil
		})
	g := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	us := &UserState{Registry: registry}
	policy := bs.TurnPolicy{
		EffectiveMode:   bs.TurnPolicyShadow,
		Source:          "test",
		ObserverActions: []bs.ToolAction{{Tool: "chat_recall", Input: json.RawMessage(`{"query":"x"}`)}},
	}

	before := time.Now()
	g.startTurnPolicyObservers(context.Background(), us, policy)
	if elapsed := time.Since(before); elapsed > 50*time.Millisecond {
		t.Fatalf("shadow observer blocked caller for %v", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shadow observer did not start")
	}
	close(release)
}

func TestTurnPolicyPreActionMarksToolErrorsAsNonEvidence(t *testing.T) {
	registry := bs.NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			return nil, errors.New("provider unavailable")
		})
	g := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	us := &UserState{Registry: registry}
	guidance, traces := g.runTurnPolicyPreActions(context.Background(), us, bs.TurnPolicy{
		EffectiveMode: bs.TurnPolicyOn,
		Guidance:      "dynamic contract",
		PreActions:    []bs.ToolAction{{Tool: "chat_recall", Input: json.RawMessage(`{"query":"x"}`)}},
	}, newTurnTimer())
	if len(traces) != 1 || !traces[0].Error {
		t.Fatalf("traces = %#v", traces)
	}
	for _, want := range []string{
		"[chat_recall error]",
		"The required evidence check failed",
		"This is not found evidence",
	} {
		if !strings.Contains(guidance, want) {
			t.Fatalf("guidance missing %q:\n%s", want, guidance)
		}
	}
}

func TestTurnPolicyPreActionHonoursParentDeadlineWhenHandlerIgnoresCancellation(t *testing.T) {
	release := make(chan struct{})
	registry := bs.NewToolRegistry()
	registry.Register("chat_recall", "recall", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			<-release
			return map[string]any{"status": "found"}, nil
		})
	g := &Gateway{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	us := &UserState{Registry: registry}
	parent, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	guidance, traces := g.runTurnPolicyPreActions(parent, us, bs.TurnPolicy{
		EffectiveMode: bs.TurnPolicyOn,
		PreActions:    []bs.ToolAction{{Tool: "chat_recall"}},
	}, newTurnTimer())
	elapsed := time.Since(started)
	close(release)

	if elapsed > 250*time.Millisecond {
		t.Fatalf("non-cooperative handler blocked for %v", elapsed)
	}
	if len(traces) != 1 || !traces[0].Error ||
		!strings.Contains(traces[0].Output, "deadline exceeded") {
		t.Fatalf("timeout traces = %#v", traces)
	}
	if !strings.Contains(guidance, "This is not found evidence") {
		t.Fatalf("timeout guidance was treated as evidence:\n%s", guidance)
	}
}

func TestInteractionTierSuppressesToolBearingRulesForSelfHistory(t *testing.T) {
	userID := uuid.New()
	called := false
	registry := bs.NewToolRegistry()
	registry.Register("note_create", "note", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			called = true
			return "created", nil
		})
	g := &Gateway{
		deps:   &bs.Deps{Config: &bs.Config{Gateway: bs.GatewayConfig{InteractionTier: true}}},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	us := &UserState{
		UserID:   userID,
		ChatID:   "telegram:1",
		Registry: registry,
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(context.Context, string, string, string) *bs.ReflexContext {
				return &bs.ReflexContext{FormattedTraces: "memory", Strategy: "warm"}
			},
			RuleEngine: func(context.Context, bs.RuleContext) []bs.ActiveRule {
				return []bs.ActiveRule{{
					ID:         "tool-rule",
					Action:     "create a note",
					Tools:      []string{"note_create"},
					PreActions: []bs.ToolAction{{Tool: "note_create", Input: json.RawMessage(`{}`)}},
				}}
			},
		},
	}

	result := g.runReflexPipeline(context.Background(), us, "ты это обещала?", "",
		newTurnTimer(), bs.TurnPolicy{SuppressToolDirectives: true})
	if called || len(result.CortexTools) != 0 || strings.Contains(result.ReflexGuidance, "create a note") {
		t.Fatalf("tool-bearing rule leaked into self-history turn: %+v guidance=%q", result, result.ReflexGuidance)
	}
	if len(result.SuppressedRules) != 1 ||
		result.SuppressedRules[0].SuppressedReason != "turn_policy_tool_directives_suppressed" {
		t.Fatalf("suppressed rules = %#v", result.SuppressedRules)
	}
}

func TestLegacyReflexSuppressesSemanticToolDirectiveForActivePolicy(t *testing.T) {
	userID := uuid.New()
	registry := bs.NewToolRegistry()
	registry.Register("note_create", "note", json.RawMessage(`{"type":"object"}`),
		func(context.Context, json.RawMessage) (any, error) {
			return "created", nil
		})
	g := &Gateway{
		deps: &bs.Deps{
			Config:     &bs.Config{},
			ModelStore: staticModelStore{},
		},
		provider: reflexProviderFunc(func(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error) {
			return &bs.CompletionResponse{Content: []bs.ContentBlock{{
				Type: "text",
				Text: `{"matched_rules":["semantic-tool","semantic-dead-tool","semantic-rapport"],"intent":"free_reflection","confidence":0.95,"pre_actions":[],"tools":[]}`,
			}}}, nil
		}),
		logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		tz:                 time.UTC,
		reflexPlanTemplate: "%s\n%s\n%s\n%s",
	}
	us := &UserState{
		UserID:   userID,
		ChatID:   "telegram:1",
		Registry: registry,
		Deps: &bs.Deps{
			UserID: userID,
			ReflexPreparer: func(context.Context, string, string, string) *bs.ReflexContext {
				return &bs.ReflexContext{
					FormattedTraces: "memory",
					Strategy:        "warm",
					CandidateRules: []bs.CandidateRule{
						{ID: "semantic-tool", Trigger: "when uncertain", Action: "Call note_create immediately"},
						{ID: "semantic-dead-tool", Trigger: "when asked to save", Action: "Выполнить memory_save перед подтверждением"},
						{ID: "semantic-rapport", Trigger: "when personal", Action: "Answer warmly and directly"},
					},
				}
			},
		},
	}

	result := g.runReflexPipeline(context.Background(), us, "ты это обещала?", "",
		newTurnTimer(), bs.TurnPolicy{
			EffectiveMode:          bs.TurnPolicyOn,
			SuppressToolDirectives: true,
		})

	if strings.Contains(result.ReflexGuidance, "Call note_create") {
		t.Fatalf("semantic tool directive leaked into guidance:\n%s", result.ReflexGuidance)
	}
	if !strings.Contains(result.ReflexGuidance, "Answer warmly and directly") {
		t.Fatalf("non-tool semantic rule was over-suppressed:\n%s", result.ReflexGuidance)
	}
	if len(result.SuppressedRules) != 2 ||
		result.SuppressedRules[0].ID != "semantic-tool" ||
		result.SuppressedRules[1].ID != "semantic-dead-tool" ||
		result.SuppressedRules[0].SuppressedReason != "turn_policy_tool_directives_suppressed" ||
		result.SuppressedRules[1].SuppressedReason != "turn_policy_tool_directives_suppressed" {
		t.Fatalf("semantic suppressed rules = %#v", result.SuppressedRules)
	}
}
