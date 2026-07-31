package gateway

import (
	"context"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/runtime/agent"
)

// stubModelStore serves a fixed role→ModelRef map, so these cases exercise the
// routing decision without a database.
type stubModelStore struct {
	refs map[string]bs.ModelRef
}

func (s *stubModelStore) Load(context.Context) error    { return nil }
func (s *stubModelStore) Refresh(context.Context) error { return nil }
func (s *stubModelStore) Get(role string) bs.ModelRef   { return s.refs[role] }
func (s *stubModelStore) ForRouter(role string) string {
	ref, ok := s.refs[role]
	if !ok || ref.Name == "" {
		return ""
	}
	if ref.Provider == "" {
		return ref.Name
	}
	return ref.Provider + ":" + ref.Name
}
func (s *stubModelStore) Update(context.Context, string, string, string) error { return nil }
func (s *stubModelStore) Roles() []string {
	roles := make([]string, 0, len(s.refs))
	for role := range s.refs {
		roles = append(roles, role)
	}
	return roles
}

func visionGateway(refs map[string]bs.ModelRef) *Gateway {
	return &Gateway{deps: &bs.Deps{
		ModelStore: &stubModelStore{refs: refs},
		Config:     &bs.Config{},
	}}
}

// A text-only cortex cannot answer a turn carrying an image, so such a turn
// must land on the vision model instead.
func TestApplyVisionModelRoutesImageTurns(t *testing.T) {
	g := visionGateway(map[string]bs.ModelRef{
		"cortex": {Provider: "deepseek", Name: "deepseek-v4-flash"},
		"vision": {Provider: "anthropic-oauth", Name: "claude-opus-5", MaxTokens: 32000, ContextWindow: 262144, Temperature: 0.7},
	})

	cfg := agent.RunConfig{Model: "deepseek:deepseek-v4-flash", MaxTokens: 8192}
	content := []bs.ContentBlock{
		{Type: "text", Text: "что на фото?"},
		{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}},
	}

	if !g.applyVisionModel(&cfg, content) {
		t.Fatal("image turn should route to the vision model")
	}
	if cfg.Model != "anthropic-oauth:claude-opus-5" {
		t.Fatalf("model = %q, want the vision model", cfg.Model)
	}
	if cfg.MaxTokens != 32000 || cfg.ContextWindow != 262144 {
		t.Fatalf("generation limits should come from the vision row, got max=%d ctx=%d", cfg.MaxTokens, cfg.ContextWindow)
	}
}

// Reasoning controls are not portable between models: inheriting them across
// tiers has already broken chat once with a 400. Every one of them must come
// from the row that owns the model actually being called.
func TestApplyVisionModelNeverInheritsReasoningControls(t *testing.T) {
	g := visionGateway(map[string]bs.ModelRef{
		"vision": {Provider: "anthropic-oauth", Name: "claude-opus-5", Effort: "high", ThinkingMode: "adaptive"},
	})

	cfg := agent.RunConfig{
		Model:        "deepseek:deepseek-v4-flash",
		Effort:       "xhigh",
		ThinkingMode: "off",
		Temperature:  0.9,
	}
	content := []bs.ContentBlock{{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}}}

	if !g.applyVisionModel(&cfg, content) {
		t.Fatal("image turn should route to the vision model")
	}
	if cfg.Effort != "high" {
		t.Errorf("effort = %q, want the vision row's own value", cfg.Effort)
	}
	if cfg.ThinkingMode != "adaptive" {
		t.Errorf("thinking_mode = %q, want the vision row's own value", cfg.ThinkingMode)
	}
	if cfg.Temperature != 0 {
		t.Errorf("temperature = %v, want the vision row's own value, not the previous tier's", cfg.Temperature)
	}
}

// Text turns are the common case and must not be diverted — the vision model
// is typically the slower, costlier one.
func TestApplyVisionModelLeavesTextTurnsAlone(t *testing.T) {
	g := visionGateway(map[string]bs.ModelRef{
		"vision": {Provider: "anthropic-oauth", Name: "claude-opus-5"},
	})

	cfg := agent.RunConfig{Model: "deepseek:deepseek-v4-flash", Effort: "medium"}
	before := cfg

	for name, content := range map[string]any{
		"plain string": "просто текст",
		"text blocks":  []bs.ContentBlock{{Type: "text", Text: "просто текст"}},
		"tool result":  []bs.ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: "ok"}},
	} {
		cfg = before
		if g.applyVisionModel(&cfg, content) {
			t.Fatalf("%s: text-only turn should not be rerouted", name)
		}
		if !sameRunConfig(cfg, before) {
			t.Fatalf("%s: config mutated on a text turn: %#v", name, cfg)
		}
	}
}

// The role is optional. A deployment that never configures it keeps running
// every turn on its normal model rather than losing images to an empty model
// name.
func TestApplyVisionModelIsInertWithoutTheRole(t *testing.T) {
	g := visionGateway(map[string]bs.ModelRef{
		"cortex": {Provider: "anthropic-oauth", Name: "claude-opus-5"},
	})

	cfg := agent.RunConfig{Model: "anthropic-oauth:claude-opus-5", Effort: "xhigh"}
	before := cfg
	content := []bs.ContentBlock{{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}}}

	if g.applyVisionModel(&cfg, content) {
		t.Fatal("no vision row configured: the turn must be left alone")
	}
	if !sameRunConfig(cfg, before) {
		t.Fatalf("config mutated without a vision row: %#v", cfg)
	}
}

// sameRunConfig compares the fields vision routing is allowed to touch.
// RunConfig holds slices, so it is not directly comparable.
func sameRunConfig(a, b agent.RunConfig) bool {
	return a.Model == b.Model &&
		a.MaxTokens == b.MaxTokens &&
		a.ContextWindow == b.ContextWindow &&
		a.Temperature == b.Temperature &&
		a.ThinkingBudget == b.ThinkingBudget &&
		a.ThinkingMode == b.ThinkingMode &&
		a.Effort == b.Effort &&
		a.MessageBudget == b.MessageBudget &&
		a.MessageBudgetSource == b.MessageBudgetSource
}
