package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rasimio/blueship/internal/core"
)

func TestGraphStepHonorsConfiguredResearchTurns(t *testing.T) {
	for _, limit := range []int{2, 12} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			generation, reviews, reads := 0, 0, 0
			wantReads := min(limit, 9)
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				if req.System == "review instructions" {
					reviews++
					return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"checked"}`}}}, nil
				}
				generation++
				if reads < wantReads {
					if len(req.Tools) == 0 {
						t.Fatal("research forced to summarize before configured limit", generation)
					}
					return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: fmt.Sprint(generation), Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
				}
				if limit == 2 && len(req.Tools) != 0 {
					t.Fatal("explicit small tool budget ignored")
				}
				return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Confirmed findings"}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute", "background-graph-review": "review instructions"})
			deps.Config.Gateway.MaxTurns = limit
			deps.Config.Models.Primary.Name = "test-model"
			deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { reads++; return "source", nil })
			_ = deps.Registry.MarkReadOnly("fetch")
			result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New()}, core.TaskStep{ID: "research", Kind: "read", Tools: []string{"fetch"}}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
			if err != nil || !accepted || result.Output != "Confirmed findings" || reads != wantReads || generation != wantReads+1 || reviews != 1 {
				t.Fatal(result, accepted, err, reads, generation, reviews)
			}
		})
	}
}

func TestGraphStepReviewerRetryDoesNotReplayTools(t *testing.T) {
	for _, kind := range []string{"read", "action"} {
		t.Run(kind, func(t *testing.T) {
			generation, review, tools := 0, 0, 0
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				if strings.Contains(req.System, "review instructions") {
					var input struct {
						CurrentDatetime string            `json:"current_datetime"`
						Other           map[string]string `json:"assigned_to_other_steps"`
					}
					if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &input); err != nil {
						t.Fatal(err)
					}
					if input.Other["other"] != "Find storage" || input.Other["source"] != "" {
						t.Fatal("review scope missing", input.Other)
					}
					instant, err := time.Parse(time.RFC3339, input.CurrentDatetime)
					if err != nil || time.Since(instant) > 5*time.Second {
						t.Fatalf("review lacks current clock: %q", input.CurrentDatetime)
					}
					if req.MaxTokens < 4096 {
						t.Fatal("review budget cannot cover reasoning and verdict", req.MaxTokens)
					}
					review++
					if review == 1 {
						return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "thinking"}, {Type: "text", Text: `{"accepted":true,"reason":"cut off"}`}}}, nil
					}
					return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"source verified","accepted_final":false}`}}}, nil
				}
				generation++
				if generation == 1 {
					return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "fetch1", Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
				}
				return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Confirmed source data"}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
			deps.Config.Models.Primary.Name = "test-model"
			deps.TaskStepGoals = map[string]string{"source": "Fetch source", "other": "Find storage"}
			deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { tools++; return "source data", nil })
			if kind == "read" {
				if err := deps.Registry.MarkReadOnly("fetch"); err != nil {
					t.Fatal(err)
				}
			}
			var saved json.RawMessage
			persist := func(_ context.Context, cp json.RawMessage) error {
				saved = append(json.RawMessage(nil), cp...)
				return nil
			}
			task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "Find source"}
			step := core.TaskStep{ID: "source", Goal: "Fetch source", Acceptance: "Confirmed source", Kind: kind, Tools: []string{"fetch"}, Attempts: 1}
			handler := NewBackground(time.UTC, nil, nil, nil)
			result, accepted, err := handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if !errors.Is(err, ErrGraphVerificationUnavailable) || accepted || result.Output != "Confirmed source data" || tools != 1 {
				t.Fatal(result, accepted, err, tools)
			}
			var cp graphStepCheckpoint
			if err := json.Unmarshal(saved, &cp); err != nil || !cp.CandidateReady || cp.SessionID == "" || len(cp.Receipts) != 1 {
				t.Fatal("candidate not recoverable", cp, err)
			}
			step.Checkpoint = saved
			step.Attempts = 2
			result, accepted, err = handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if err != nil || !accepted || result.Output != "Confirmed source data" || tools != 1 || generation != 2 || review != 2 {
				t.Fatal(result, accepted, err, tools, generation, review)
			}
		})
	}
}

func TestGraphStepRejectsChangedToolEffectsBeforeModelCall(t *testing.T) {
	provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
		t.Fatal("model called before tool validation")
		return nil, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{})
	deps.Config.Models.Primary.Name = "test-model"
	deps.Registry.Register("write", "write", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { t.Fatal("mutation dispatched"); return nil, nil })
	_, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{}, core.TaskStep{Kind: "read", Tools: []string{"write"}}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err == nil || accepted {
		t.Fatal("effectful read accepted")
	}
}

func TestGraphStepStopsAfterUncertainExternalEffect(t *testing.T) {
	for _, kind := range []string{"action", "finalize"} {
		t.Run(kind, func(t *testing.T) {
			modelCalls, actions := 0, 0
			provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
				modelCalls++
				return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{
					{Type: "tool_use", ID: "write1", Name: "write", Input: json.RawMessage(`{"target":"report"}`)},
					{Type: "tool_use", ID: "write2", Name: "write", Input: json.RawMessage(`{"target":"report"}`)},
				}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions"})
			deps.Config.Models.Primary.Name = "test-model"
			deps.Registry.Register("write", "write", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
				actions++ // Simulate a committed external effect with a lost response.
				return nil, errors.New("connection reset after sending request")
			})
			var saved json.RawMessage
			persist := func(_ context.Context, cp json.RawMessage) error {
				saved = append(json.RawMessage(nil), cp...)
				return nil
			}
			task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "Save report"}
			step := core.TaskStep{ID: "save", Goal: "Save report", Acceptance: "Receipt", Kind: kind, Tools: []string{"write"}, Attempts: 1}
			handler := NewBackground(time.UTC, nil, nil, nil)
			_, accepted, err := handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if !errors.Is(err, ErrGraphActionUncertain) || accepted || actions != 1 || modelCalls != 1 {
				t.Fatal("continued after unknown effect", accepted, err, actions, modelCalls)
			}
			var cp graphStepCheckpoint
			if err := json.Unmarshal(saved, &cp); err != nil {
				t.Fatal(err)
			}
			if cp.Phase != "reconciliation" || cp.PendingTool == nil || cp.PendingTool.ID != "write1" || len(cp.Receipts) != 1 || !cp.Receipts[0].IsError {
				t.Fatal("unknown operation lost", cp)
			}
			step.Checkpoint = saved
			step.Attempts = 2
			_, accepted, err = handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if !errors.Is(err, ErrGraphActionUncertain) || accepted || actions != 1 || modelCalls != 1 {
				t.Fatal("recovery replayed operation", accepted, err, actions, modelCalls)
			}
		})
	}
}

type repairSessionStore struct {
	*recordingMessageStore
	created int
}

func (s *repairSessionStore) CreateSessionWithSource(context.Context, string, string, string, string) (string, error) {
	s.created++
	s.appended = nil
	return "fresh-repair-session", nil
}

func TestGraphRepairUsesFreshDialogAndRetainsEarlierReceipts(t *testing.T) {
	generation, review, tools := 0, 0, 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if req.System == "review instructions" {
			review++
			var in struct {
				Receipts []core.ToolExecutionResult `json:"receipts"`
			}
			if err := json.Unmarshal([]byte(core.ExtractText(core.NormalizeContent(req.Messages[0].Content))), &in); err != nil {
				t.Fatal(err)
			}
			if len(in.Receipts) != 2 || in.Receipts[0].Output != "Verified GPU source" || in.Receipts[1].Output != `"Verified PSU source"` {
				t.Fatal("repair lost or duplicated evidence", in.Receipts)
			}
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"both sources read"}`}}}, nil
		}
		generation++
		if generation == 1 {
			input := core.ExtractText(core.NormalizeContent(req.Messages[0].Content))
			if !strings.Contains(input, "GPU already found") || !strings.Contains(input, "Find only the missing PSU") || strings.Contains(input, "STALE_DIALOG") {
				t.Fatal("repair did not use clean candidate and feedback", input)
			}
			return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "new-psu", Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "GPU and PSU verified"}}}, nil
	}}
	deps, base := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	store := &repairSessionStore{recordingMessageStore: base}
	deps.Store = store
	base.appended = []core.Message{{Role: "user", Content: "STALE_DIALOG"}}
	deps.Config.Models.Primary.Name = "test-model"
	deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { tools++; return "Verified PSU source", nil })
	_ = deps.Registry.MarkReadOnly("fetch")
	prior := graphStepCheckpoint{SessionID: "old-session", Phase: "rejected", CandidateReady: true, Output: "GPU already found", ReviewReason: "Find only the missing PSU", Receipts: []core.ToolExecutionResult{{Name: "fetch", Output: "Verified GPU source"}}}
	raw, _ := json.Marshal(prior)
	var saved json.RawMessage
	result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New()}, core.TaskStep{ID: "components", Kind: "read", Tools: []string{"fetch"}, Attempts: 2, Checkpoint: raw}, nil, deps, func(_ context.Context, cp json.RawMessage) error {
		saved = append(json.RawMessage(nil), cp...)
		return nil
	})
	if err != nil || !accepted || result.Output != "GPU and PSU verified" || generation != 2 || review != 1 || tools != 1 || store.created != 1 {
		t.Fatal(result, accepted, err, generation, review, tools, store.created)
	}
	var checkpoint graphStepCheckpoint
	if err = json.Unmarshal(saved, &checkpoint); err != nil || len(checkpoint.Receipts) != 2 || checkpoint.SessionID != "fresh-repair-session" {
		t.Fatal("repair evidence not durably retained", checkpoint, err)
	}
}

func TestGraphReviewRequiresCanonicalVerdict(t *testing.T) {
	for _, raw := range []string{`{"accepted_final":true}`, `{"accepted":"true"}`, `{"accepted":true} {"accepted":true}`} {
		provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: raw}}}, nil
		}}
		deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-review": "review"})
		deps.Config.Models.Primary.Name = "test-model"
		cp, _ := json.Marshal(graphStepCheckpoint{SessionID: "existing", CandidateReady: true, Output: "Candidate"})
		_, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{}, core.TaskStep{Kind: "read", Checkpoint: cp}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
		if accepted || !errors.Is(err, ErrGraphVerificationUnavailable) {
			t.Fatal("noncanonical verdict accepted", raw, accepted, err)
		}
	}
}

func TestGraphFinalizerHasRoomForCompleteReport(t *testing.T) {
	generation := 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if strings.Contains(req.System, "review instructions") {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"complete"}`}}}, nil
		}
		generation++
		if req.MaxTokens < 16384 {
			return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: "Incomplete table"}}}, nil
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Complete report with all components and totals"}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "Complete report"}
	step := core.TaskStep{ID: "report", Kind: "finalize", Goal: "Combine verified findings", Attempts: 1}
	result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), task, step, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || generation != 1 || result.Output != "Complete report with all components and totals" {
		t.Fatal(result, accepted, err, generation)
	}
}

func TestGraphReadRepairAfterResearchDeadlineCannotFetch(t *testing.T) {
	generation := 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if req.System == "review instructions" {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"height matches saved source"}`}}}, nil
		}
		if len(req.Tools) != 0 {
			t.Fatal("late repair exposed research tools")
		}
		generation++
		if generation == 1 {
			return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "late", Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Cooler height is 159 mm, below the 163 mm case limit."}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { t.Fatal("late tool executed"); return nil, nil })
	_ = deps.Registry.MarkReadOnly("fetch")
	cp, _ := json.Marshal(graphStepCheckpoint{Phase: "rejected", CandidateReady: true, Output: "Height unknown", ReviewReason: "Saved source says 159 mm", Receipts: []core.ToolExecutionResult{{Name: "fetch", Output: "Height:159 mm"}}})
	deadline := time.Now().Add(time.Minute)
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), CreatedAt: time.Now().Add(-8 * time.Minute), Deadline: &deadline}
	out, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), task, core.TaskStep{ID: "cooler", Kind: "read", Tools: []string{"fetch"}, Checkpoint: cp, Attempts: 2}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || generation != 2 || !strings.Contains(out.Output, "159 mm") {
		t.Fatal(out, accepted, err, generation)
	}
}

func TestGraphReadToolDeadlineCancelsFetchButAllowsSynthesis(t *testing.T) {
	calls, fetches := 0, 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if req.System == "review instructions" {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"saved facts"}`}}}, nil
		}
		calls++
		if calls == 1 {
			return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "fetch", Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
		}
		if len(req.Tools) != 0 {
			t.Error("research tools remain after deadline")
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Saved evidence with the unavailable page identified"}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	ended := make(chan struct{})
	deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(ctx context.Context, _ json.RawMessage) (any, error) {
		fetches++
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	})
	_ = deps.Registry.MarkReadOnly("fetch")
	deadline := time.Now().Add(3*time.Minute + 200*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), CreatedAt: time.Now().Add(-8 * time.Minute), Deadline: &deadline}
	out, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(ctx, task, core.TaskStep{ID: "source", Kind: "read", Tools: []string{"fetch"}, Attempts: 1}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	select {
	case <-ended:
	case <-ctx.Done():
		t.Fatal("tool did not stop")
	}
	if err != nil || !accepted || calls != 2 || fetches != 1 || ctx.Err() != nil || out.Output == "" {
		t.Fatal(out, accepted, err, calls, fetches, ctx.Err())
	}
}

func TestGraphPureReportDefersOnlyToMandatoryTaskAcceptance(t *testing.T) {
	for _, criteriaPresent := range []bool{false, true} {
		t.Run(fmt.Sprint(criteriaPresent), func(t *testing.T) {
			reviews := 0
			provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
				if req.System == "review instructions" {
					reviews++
					return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"verified"}`}}}, nil
				}
				return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Report candidate"}}}, nil
			}}
			deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
			deps.Config.Models.Primary.Name = "test-model"
			deps.FinalAcceptanceOwned = true
			task := core.AgentTask{ID: uuid.New(), UserID: uuid.New()}
			if criteriaPresent {
				criteria := "Complete original task"
				task.AcceptanceCriteria = &criteria
			}
			var saved json.RawMessage
			step := core.TaskStep{ID: "report", Kind: "finalize", Attempts: 1}
			h := NewBackground(time.UTC, nil, nil, nil)
			out, accepted, err := h.ExecuteGraphStep(context.Background(), task, step, nil, deps, func(_ context.Context, cp json.RawMessage) error {
				saved = append(json.RawMessage(nil), cp...)
				return nil
			})
			if err != nil || !accepted || out.Output != "Report candidate" {
				t.Fatal(out, accepted, err)
			}
			if criteriaPresent && reviews != 0 || !criteriaPresent && reviews != 1 {
				t.Fatal("review ownership incorrect", reviews)
			}
			step.Checkpoint = saved
			_, accepted, err = h.ExecuteGraphStep(context.Background(), task, step, nil, deps, func(context.Context, json.RawMessage) error { return nil })
			if err != nil || !accepted || criteriaPresent && reviews != 0 || !criteriaPresent && reviews != 1 {
				t.Fatal("resumed candidate repeated generation/review", accepted, err, reviews)
			}
		})
	}
}

func TestGraphReadRepairRetainsCandidateDuringInterruptedTool(t *testing.T) {
	provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
		return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "text", Text: "INTERNAL repair narration"}, {Type: "tool_use", ID: "rate", Name: "fetch", Input: json.RawMessage(`{}`)}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	deps.Registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { return "rate", nil })
	_ = deps.Registry.MarkReadOnly("fetch")
	raw, _ := json.Marshal(graphStepCheckpoint{Phase: "rejected", CandidateReady: true, Output: "Six priced items", ReviewReason: "Missing exchange rate"})
	var saved graphStepCheckpoint
	_, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New()}, core.TaskStep{ID: "items", Kind: "read", Tools: []string{"fetch"}, Attempts: 2, Checkpoint: raw}, nil, deps, func(_ context.Context, raw json.RawMessage) error {
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		if saved.Phase == "tool_started" {
			return context.DeadlineExceeded
		}
		return nil
	})
	if err == nil || accepted || saved.CandidateReady || saved.LastCandidate != "Six priced items" || saved.LastCandidateReview != "Missing exchange rate" {
		t.Fatal("last complete candidate lost", saved, accepted, err)
	}
}

func TestGraphLateRepairUsesOnlyRegisteredEvidenceReader(t *testing.T) {
	calls, reads := 0, 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if req.System == "review instructions" {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"saved source confirms height"}`}}}, nil
		}
		calls++
		if calls == 1 {
			if len(req.Tools) != 1 || req.Tools[0].Name != "fetch" {
				t.Fatal("wrong restricted tools", req.Tools)
			}
			return &core.CompletionResponse{StopReason: "tool_use", Content: []core.ContentBlock{{Type: "tool_use", ID: "saved", Name: "fetch", Input: json.RawMessage(`{}`)}, {Type: "tool_use", ID: "denied", Name: "search", Input: json.RawMessage(`{}`)}}}, nil
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Saved height 165 mm"}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	for _, name := range []string{"fetch", "search"} {
		deps.Registry.Register(name, name, json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) {
			t.Fatal("external handler executed after deadline")
			return nil, nil
		})
	}
	_ = deps.Registry.MarkReadOnly("fetch", "search")
	if err := deps.Registry.RegisterEvidenceReader("fetch", func(ctx context.Context, _ json.RawMessage) (any, error) {
		if ctx.Err() != nil {
			t.Fatal("saved read inherited expired network deadline")
		}
		reads++
		return "height 165 mm", nil
	}); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ := json.Marshal(graphStepCheckpoint{Phase: "rejected", CandidateReady: true, Output: "Height unknown", ReviewReason: "Read saved case specification"})
	deadline := time.Now().Add(time.Minute)
	result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New(), CreatedAt: time.Now().Add(-8 * time.Minute), Deadline: &deadline}, core.TaskStep{ID: "case", Kind: "read", Tools: []string{"fetch", "search"}, Checkpoint: checkpoint, Attempts: 2}, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || reads != 1 || !strings.Contains(result.Output, "165") {
		t.Fatal(result, accepted, err, reads)
	}
}

func TestGraphReadStepHasRoomForCoordinationTable(t *testing.T) {
	generation := 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if strings.Contains(req.System, "review instructions") {
			return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":true,"reason":"complete"}`}}}, nil
		}
		generation++
		if req.MaxTokens < 16384 {
			return &core.CompletionResponse{StopReason: "max_tokens", Content: []core.ContentBlock{{Type: "text", Text: "| ChatGPT | 800M | ... | Grok | 117 млн MAU"}}}, nil
		}
		return &core.CompletionResponse{StopReason: "end_turn", Content: []core.ContentBlock{{Type: "text", Text: "Complete table for every player"}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "Market"}
	step := core.TaskStep{ID: "consolidate", Kind: "read", Goal: "Merge branch results into one table", Attempts: 1, MaxAttempts: 3}
	result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), task, step, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || generation != 1 || result.Output != "Complete table for every player" {
		t.Fatal(result, accepted, err, generation)
	}
}

func TestGraphLastRejectedReadHandsOnCandidateWithGaps(t *testing.T) {
	reviews := 0
	provider := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		if !strings.Contains(req.System, "review instructions") {
			t.Fatal("saved candidate was regenerated")
		}
		reviews++
		return &core.CompletionResponse{Content: []core.ContentBlock{{Type: "text", Text: `{"accepted":false,"reason":"Dynamics July→September missing"}`}}}, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute instructions", "background-graph-review": "review instructions"})
	deps.Config.Models.Primary.Name = "test-model"
	task := core.AgentTask{ID: uuid.New(), UserID: uuid.New(), Title: "Market"}
	for _, tc := range []struct {
		kind     string
		attempt  int
		accepted bool
	}{{"read", 1, false}, {"read", 2, false}, {"read", 3, true}, {"action", 1, true}} {
		t.Run(fmt.Sprintf("%s_%d", tc.kind, tc.attempt), func(t *testing.T) {
			cp, _ := json.Marshal(graphStepCheckpoint{SessionID: "existing", CandidateReady: true, Phase: "verification", Output: "Table for 10 players",
				Receipts: []core.ToolExecutionResult{{Output: "created"}}})
			var saved json.RawMessage
			persist := func(_ context.Context, raw json.RawMessage) error {
				saved = append(json.RawMessage(nil), raw...)
				return nil
			}
			step := core.TaskStep{ID: "consolidate", Kind: tc.kind, Attempts: tc.attempt, MaxAttempts: 3, Checkpoint: cp}
			handler := NewBackground(time.UTC, nil, nil, nil)
			result, accepted, err := handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if err != nil || accepted != tc.accepted {
				t.Fatal(result, accepted, err)
			}
			marked := strings.Contains(result.Output, "[unverified_step_result]") && strings.Contains(result.Output, "Dynamics July→September missing")
			if marked != tc.accepted || !strings.HasPrefix(result.Output, "Table for 10 players") {
				t.Fatalf("candidate/gap marking wrong: %q", result.Output)
			}
			if !tc.accepted {
				return
			}
			// A resumed claim reuses the saved decision instead of reviewing again.
			before := reviews
			step.Checkpoint = saved
			again, accepted, err := handler.ExecuteGraphStep(context.Background(), task, step, nil, deps, persist)
			if err != nil || !accepted || again.Output != result.Output || reviews != before {
				t.Fatal("handed-on candidate not durable", again, accepted, err, reviews-before)
			}
		})
	}
}

func TestGraphActionWithoutReceiptHandsOnInsteadOfStopping(t *testing.T) {
	provider := &capturingProvider{respond: func(core.CompletionRequest) (*core.CompletionResponse, error) {
		t.Fatal("unperformed action was reviewed or regenerated")
		return nil, nil
	}}
	deps, _ := backgroundTestDeps(provider, map[string]string{"background-graph-step": "execute", "background-graph-review": "review"})
	deps.Config.Models.Primary.Name = "test-model"
	cp, _ := json.Marshal(graphStepCheckpoint{SessionID: "existing", CandidateReady: true, Phase: "verification", Output: "Saved nothing",
		Receipts: []core.ToolExecutionResult{{Name: "memory_update", IsError: true, Output: "denied"}}})
	step := core.TaskStep{ID: "memory_save", Kind: "action", Attempts: 1, MaxAttempts: 3, Checkpoint: cp}
	result, accepted, err := NewBackground(time.UTC, nil, nil, nil).ExecuteGraphStep(context.Background(), core.AgentTask{ID: uuid.New(), UserID: uuid.New()}, step, nil, deps, func(context.Context, json.RawMessage) error { return nil })
	if err != nil || !accepted || !strings.Contains(result.Output, "[unverified_step_result]") || !strings.Contains(result.Output, "not performed") {
		t.Fatal(result, accepted, err)
	}
}
