package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/rasimio/blueship/internal/core"
)

// capturingProvider records every completion request and replies with a
// scripted response (or a bare end_turn when the script is exhausted).
type capturingProvider struct {
	requests  []core.CompletionRequest
	responses []*core.CompletionResponse
	respond   func(core.CompletionRequest) (*core.CompletionResponse, error)
}

type contextWindowModelStore struct {
	refs map[string]core.ModelRef
}

func (s contextWindowModelStore) Load(context.Context) error { return nil }

func (s contextWindowModelStore) Get(role string) core.ModelRef {
	return s.refs[role]
}

func (s contextWindowModelStore) ForRouter(role string) string {
	return s.refs[role].ForRouter()
}

func (s contextWindowModelStore) Update(context.Context, string, string, string) error {
	return nil
}

func (s contextWindowModelStore) Roles() []string { return nil }

func (s contextWindowModelStore) Refresh(context.Context) error { return nil }

func (p *capturingProvider) Complete(_ context.Context, req core.CompletionRequest) (*core.CompletionResponse, error) {
	p.requests = append(p.requests, req)
	if p.respond != nil {
		return p.respond(req)
	}
	if len(p.responses) == 0 {
		return &core.CompletionResponse{
			StopReason: "end_turn",
			Content:    []core.ContentBlock{{Type: "text", Text: "[DONE] handled"}},
		}, nil
	}
	resp := p.responses[0]
	p.responses = p.responses[1:]
	return resp, nil
}

// recordingMessageStore is a minimal in-memory MessageStore: appends are
// recorded and served back as the dialog window; everything else is a no-op.
type recordingMessageStore struct {
	appended []core.Message
	archived []string
}

func (s *recordingMessageStore) Append(_ context.Context, _ string, msg core.Message) error {
	s.appended = append(s.appended, msg)
	return nil
}

func (s *recordingMessageStore) AppendWithTokens(_ context.Context, _ string, msg core.Message, _ int) error {
	s.appended = append(s.appended, msg)
	return nil
}

func (s *recordingMessageStore) MessagesForAPI(context.Context, string, int) ([]core.Message, error) {
	return nil, nil
}

func (s *recordingMessageStore) DialogMessagesForAPI(context.Context, string, int, bool) ([]core.Message, error) {
	return append([]core.Message(nil), s.appended...), nil
}

func (s *recordingMessageStore) RecentToolObservations(context.Context, string, time.Time, int) ([]core.ToolObservation, error) {
	return nil, nil
}

func (s *recordingMessageStore) AllMessagesForAPI(context.Context, string) ([]core.Message, error) {
	return nil, nil
}

func (s *recordingMessageStore) CompactSession(context.Context, string, string, int) error {
	return nil
}

func (s *recordingMessageStore) CreateSession(context.Context, string, string) (string, error) {
	return "sess-test", nil
}

func (s *recordingMessageStore) CreateSessionWithSource(context.Context, string, string, string, string) (string, error) {
	return "sess-test", nil
}

func (s *recordingMessageStore) ArchiveSession(_ context.Context, sessionID string) error {
	s.archived = append(s.archived, sessionID)
	return nil
}

func (s *recordingMessageStore) LatestAssistantMessageID(context.Context, string) (string, error) {
	return "", nil
}

func (s *recordingMessageStore) RecordLastInputTokens(context.Context, string, int) error {
	return nil
}

func (s *recordingMessageStore) RecordLLMUsage(context.Context, core.LLMUsageRecord) error {
	return nil
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// backgroundTestDeps assembles AgentDeps for a full handler Run with the
// capturing provider and recording store. CompactThreshold is raised so the
// loop's compactor never fires mid-test and eats a scripted response.
func backgroundTestDeps(provider *capturingProvider, prompts map[string]string) (core.AgentDeps, *recordingMessageStore) {
	cfg := &core.Config{}
	cfg.Gateway.MaxTurns = 2
	cfg.Limits.CompactThreshold = 1 << 20
	store := &recordingMessageStore{}
	return core.AgentDeps{
		LLM:      provider,
		Registry: core.NewToolRegistry(),
		Store:    store,
		Prompts:  core.NewMapPromptStore(prompts),
		Logger:   discardLogger(),
		Config:   cfg,
	}, store
}

func TestNewTaskCompactorLoadsCompactPrompt(t *testing.T) {
	messages := []core.Message{
		{Role: "user", Content: "please dig through the whole backlog and summarize it"},
		{Role: "assistant", Content: "here is a very long walkthrough of everything that happened"},
		{Role: "user", Content: "tail"},
	}

	run := func(prompts map[string]string) string {
		provider := &capturingProvider{responses: []*core.CompletionResponse{{
			StopReason: "end_turn",
			Content:    []core.ContentBlock{{Type: "text", Text: "summary"}},
		}}}
		cfg := &core.Config{}
		cfg.Limits.CompactThreshold = 1
		cfg.Limits.CompactKeep = 1
		deps := core.AgentDeps{
			LLM:     provider,
			Config:  cfg,
			Logger:  discardLogger(),
			Prompts: core.NewMapPromptStore(prompts),
		}
		c := newTaskCompactor(context.Background(), deps)
		if c == nil {
			t.Fatal("expected a compactor, got nil")
		}
		summary, _, err := c.Compact(context.Background(), messages)
		if err != nil {
			t.Fatalf("Compact: %v", err)
		}
		if summary != "summary" {
			t.Fatalf("summary = %q, want %q", summary, "summary")
		}
		if len(provider.requests) != 1 {
			t.Fatalf("expected 1 summarize call, got %d", len(provider.requests))
		}
		return provider.requests[0].System
	}

	if system := run(map[string]string{"compact": "COMPACT-INSTRUCTION"}); system != "COMPACT-INSTRUCTION" {
		t.Fatalf("summarize system prompt = %q, want the compact prompt", system)
	}
	// Missing prompt file degrades to the old instruction-less behavior.
	if system := run(map[string]string{}); system != "" {
		t.Fatalf("summarize system prompt = %q, want empty when no compact prompt is shipped", system)
	}
}

func TestBackgroundRunIncludePersonaLeadsPromptKeys(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{"focus-frame": "FOCUS-FRAME-PROMPT"})
	deps.Config.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "SOUL-PERSONA-TEXT", nil
	}
	deps.Config.Gateway.ResolvePlatformPrompts = func(context.Context, string) (string, string, error) {
		return "PLATFORM-PREAMBLE", "PLATFORM-AGENTS", nil
	}

	task := core.AgentTask{
		ID:            uuid.New(),
		SoulID:        uuid.New(),
		UserID:        uuid.New(),
		Title:         "personality tick",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"write the daily note in your own voice","system_prompt_keys":["focus-frame"],"include_persona":true,"skip_reflex":true}`),
		MaxIterations: 3,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("no LLM call recorded")
	}
	system := provider.requests[0].System
	if !strings.HasPrefix(system, "[current_datetime:") {
		t.Fatalf("system prompt must start with [current_datetime: …], got %q", system[:min(len(system), 60)])
	}
	personaIdx := strings.Index(system, "SOUL-PERSONA-TEXT")
	frameIdx := strings.Index(system, "FOCUS-FRAME-PROMPT")
	if personaIdx < 0 || frameIdx < 0 {
		t.Fatalf("system prompt missing persona (%d) or prompt-key content (%d):\n%s", personaIdx, frameIdx, system)
	}
	if personaIdx > frameIdx {
		t.Fatalf("persona must precede the prompt-key contents (persona at %d, keys at %d)", personaIdx, frameIdx)
	}
	if strings.Contains(system, "PLATFORM-PREAMBLE") || strings.Contains(system, "PLATFORM-AGENTS") {
		t.Fatalf("platform preamble/agents must stay excluded with include_persona:\n%s", system)
	}
}

func TestBackgroundRunUsesRoleContextWindow(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, nil)
	deps.ModelStore = contextWindowModelStore{refs: map[string]core.ModelRef{
		"background": {
			Provider:      "ollama",
			Name:          "gemma4:26b",
			ContextWindow: 32768,
		},
	}}

	task := core.AgentTask{
		ID:            uuid.New(),
		SoulID:        uuid.New(),
		UserID:        uuid.New(),
		Title:         "context-window check",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"finish the check","skip_reflex":true}`),
		MaxIterations: 3,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.requests) == 0 {
		t.Fatal("no LLM call recorded")
	}
	if got := provider.requests[0].ContextWindow; got != 32768 {
		t.Fatalf("completion context window = %d, want role context window 32768", got)
	}
}

func TestBackgroundRunPromptKeysWithoutIncludePersonaStayBare(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{"focus-frame": "FOCUS-FRAME-PROMPT"})
	deps.Config.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "SOUL-PERSONA-TEXT", nil
	}

	task := core.AgentTask{
		ID:            uuid.New(),
		SoulID:        uuid.New(),
		UserID:        uuid.New(),
		Title:         "research tick",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"inline instruction","system_prompt_keys":["focus-frame"],"skip_reflex":true}`),
		MaxIterations: 3,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	system := provider.requests[0].System
	if !strings.Contains(system, "FOCUS-FRAME-PROMPT") {
		t.Fatalf("prompt-key content missing:\n%s", system)
	}
	if strings.Contains(system, "SOUL-PERSONA-TEXT") {
		t.Fatalf("persona must not be injected without include_persona:\n%s", system)
	}
}

func TestBackgroundRunIncludePersonaDegradesWhenPersonaUnavailable(t *testing.T) {
	provider := &capturingProvider{}
	deps, _ := backgroundTestDeps(provider, map[string]string{"focus-frame": "FOCUS-FRAME-PROMPT"})
	deps.Config.Gateway.ResolveSoulPersona = func(context.Context, uuid.UUID) (string, error) {
		return "", fmt.Errorf("persona store down")
	}

	task := core.AgentTask{
		ID:            uuid.New(),
		SoulID:        uuid.New(),
		UserID:        uuid.New(),
		Title:         "personality tick",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"inline instruction","system_prompt_keys":["focus-frame"],"include_persona":true,"skip_reflex":true}`),
		MaxIterations: 3,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run must degrade silently on persona resolve failure, got: %v", err)
	}
	system := provider.requests[0].System
	if !strings.Contains(system, "FOCUS-FRAME-PROMPT") {
		t.Fatalf("prompt-key content missing after degrade:\n%s", system)
	}
	if strings.Contains(system, "SOUL-PERSONA-TEXT") {
		t.Fatalf("no persona text expected on resolve failure:\n%s", system)
	}
}

func TestBackgroundRunRendersAcceptanceFeedback(t *testing.T) {
	provider := &capturingProvider{}
	deps, store := backgroundTestDeps(provider, map[string]string{})

	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Title:         "research task",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"inline instruction","skip_reflex":true}`),
		Progress:      json.RawMessage(`{"acceptance_feedback":"Report cites zero URLs; criteria demands at least five distinct sources."}`),
		Iteration:     1,
		MaxIterations: 6,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.appended) == 0 {
		t.Fatal("no user message appended")
	}
	userMsg, _ := store.appended[0].Content.(string)
	for _, want := range []string{
		"[acceptance feedback]",
		"The previous iteration was rejected by the acceptance gate. Address this before finishing:",
		"Report cites zero URLs; criteria demands at least five distinct sources.",
		"[/acceptance feedback]",
	} {
		if !strings.Contains(userMsg, want) {
			t.Fatalf("user message missing %q:\n%s", want, userMsg)
		}
	}
}

func TestBackgroundRunTruncatesLongAcceptanceFeedback(t *testing.T) {
	provider := &capturingProvider{}
	deps, store := backgroundTestDeps(provider, map[string]string{})

	long := strings.Repeat("x", 700)
	progress, _ := json.Marshal(map[string]string{"acceptance_feedback": long})
	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Title:         "research task",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"inline instruction","skip_reflex":true}`),
		Progress:      progress,
		Iteration:     1,
		MaxIterations: 6,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	userMsg, _ := store.appended[0].Content.(string)
	if !strings.Contains(userMsg, strings.Repeat("x", 600)+"...") {
		t.Fatalf("feedback must be truncated to 600 chars with an ellipsis:\n%s", userMsg)
	}
	if strings.Contains(userMsg, strings.Repeat("x", 601)) {
		t.Fatalf("feedback exceeded the 600-char cap:\n%s", userMsg)
	}
}

func TestBackgroundRunNoAcceptanceFeedbackBlockWhenAbsent(t *testing.T) {
	provider := &capturingProvider{}
	deps, store := backgroundTestDeps(provider, map[string]string{})

	task := core.AgentTask{
		ID:            uuid.New(),
		UserID:        uuid.New(),
		Title:         "research task",
		Strategy:      core.StrategyDirect,
		Config:        json.RawMessage(`{"prompt":"inline instruction","skip_reflex":true}`),
		Progress:      json.RawMessage(`{"phase":"iteration_1","summary":"still going"}`),
		Iteration:     1,
		MaxIterations: 6,
	}

	if _, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	userMsg, _ := store.appended[0].Content.(string)
	if strings.Contains(userMsg, "[acceptance feedback]") {
		t.Fatalf("no acceptance feedback block expected without the progress key:\n%s", userMsg)
	}
}

func TestBackgroundRunReservesRepairIterationAndPreservesDoneProgress(t *testing.T) {
	provider := &capturingProvider{responses: []*core.CompletionResponse{{
		StopReason: "end_turn",
		Content:    []core.ContentBlock{{Type: "text", Text: "draft report without an explicit done marker"}},
	}}}
	deps, store := backgroundTestDeps(provider, map[string]string{
		"background-task":      "BASE",
		"background-step":      "STEP-PHASE",
		"background-synthesis": "SYNTHESIS-PHASE",
	})
	deps.Config.Gateway.ResolveSkillCatalog = func(context.Context) ([]core.SkillMeta, error) {
		return []core.SkillMeta{{Slug: "analyst", Title: "Analyst"}}, nil
	}
	deps.Config.Gateway.ResolveSkills = func(context.Context, []string) ([]string, error) {
		return []string{"ANALYST-ROLE"}, nil
	}

	criteria := "The final report includes time and question count for every test."
	task := core.AgentTask{
		ID:                 uuid.New(),
		UserID:             uuid.New(),
		Title:              "research task",
		Strategy:           core.StrategyDirect,
		Config:             json.RawMessage(`{"skip_reflex":true}`),
		Progress:           json.RawMessage(`{"session_id":"sess-existing","phase":"iteration_4","summary":"prior findings","plan":{"plan_rev":1,"current_step_id":"step_005","steps":[{"id":"step_005","goal":"finish evidence","skills":["analyst"],"status":"pending"}]}}`),
		Iteration:          4, // current run is 5/6: synthesis, leaving 6/6 for repair
		MaxIterations:      6,
		AcceptanceCriteria: &criteria,
	}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Done {
		t.Fatal("penultimate acceptance-gated synthesis must submit even without [DONE]")
	}
	if len(store.appended) == 0 {
		t.Fatal("no user message appended")
	}
	userMsg, _ := store.appended[0].Content.(string)
	if !strings.Contains(userMsg, "SYNTHESIS-PHASE") || strings.Contains(userMsg, "STEP-PHASE") {
		t.Fatalf("iteration 5/6 must synthesize and reserve 6/6 for repair:\n%s", userMsg)
	}
	if len(store.archived) != 0 {
		t.Fatalf("session archived before acceptance verdict: %v", store.archived)
	}

	var progress bgProgress
	if err := json.Unmarshal(result.Progress, &progress); err != nil {
		t.Fatalf("result progress is not valid JSON: %v\n%s", err, result.Progress)
	}
	if progress.SessionID != "sess-existing" {
		t.Fatalf("session_id = %q, want existing session", progress.SessionID)
	}
	if progress.Plan == nil || progress.Plan.CurrentStepID != "step_005" || len(progress.Plan.Steps) != 1 {
		t.Fatalf("role plan was lost or mutated by synthesis: %#v", progress.Plan)
	}
}

func TestBackgroundRunDoesNotReserveRepairOutsideDefaultPipeline(t *testing.T) {
	criteria := "Return a complete result."
	tests := []struct {
		name          string
		config        json.RawMessage
		iteration     int
		maxIterations int
		prompts       map[string]string
	}{
		{
			name:          "custom prompt owns its phases",
			config:        json.RawMessage(`{"prompt":"custom-task","skip_reflex":true}`),
			iteration:     4,
			maxIterations: 6,
			prompts:       map[string]string{"custom-task": "CUSTOM-TASK"},
		},
		{
			name:          "two iteration default flow has no spare repair slot",
			config:        json.RawMessage(`{"skip_reflex":true}`),
			iteration:     0,
			maxIterations: 2,
			prompts:       map[string]string{"background-task": "BASE", "background-planning": "PLANNING"},
		},
		{
			name:          "three iteration default flow has no spare repair slot",
			config:        json.RawMessage(`{"skip_reflex":true}`),
			iteration:     1,
			maxIterations: 3,
			prompts:       map[string]string{"background-task": "BASE", "background-execution": "EXECUTION"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := &capturingProvider{responses: []*core.CompletionResponse{{
				StopReason: "end_turn",
				Content:    []core.ContentBlock{{Type: "text", Text: "work is still in progress"}},
			}}}
			deps, _ := backgroundTestDeps(provider, tt.prompts)
			task := core.AgentTask{
				ID:                 uuid.New(),
				UserID:             uuid.New(),
				Title:              "bounded task",
				Strategy:           core.StrategyDirect,
				Config:             tt.config,
				Iteration:          tt.iteration,
				MaxIterations:      tt.maxIterations,
				AcceptanceCriteria: &criteria,
			}

			result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Done {
				t.Fatal("flow without a supported repair slot was force-completed on N-1")
			}
		})
	}
}

func TestBackgroundRunLeavesHardFinalSessionForScheduler(t *testing.T) {
	provider := &capturingProvider{responses: []*core.CompletionResponse{{
		StopReason: "end_turn",
		Content:    []core.ContentBlock{{Type: "text", Text: "[DONE] repaired report"}},
	}}}
	deps, store := backgroundTestDeps(provider, map[string]string{
		"background-task":      "BASE",
		"background-synthesis": "SYNTHESIS",
	})
	criteria := "Return a complete result."
	task := core.AgentTask{
		ID:                 uuid.New(),
		UserID:             uuid.New(),
		Title:              "research task",
		Strategy:           core.StrategyDirect,
		Config:             json.RawMessage(`{"skip_reflex":true}`),
		Progress:           json.RawMessage(`{"session_id":"sess-existing","acceptance_feedback":"repair this"}`),
		Iteration:          5,
		MaxIterations:      6,
		AcceptanceCriteria: &criteria,
	}

	result, err := NewBackground(time.UTC, nil, nil, nil).Run(context.Background(), task, deps)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Done {
		t.Fatal("hard-final iteration must submit to the scheduler")
	}
	if len(store.archived) != 0 {
		t.Fatalf("handler archived before terminal DB transition: %v", store.archived)
	}
}
