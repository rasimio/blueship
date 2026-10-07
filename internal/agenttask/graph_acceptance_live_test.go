package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/anthropic"
	"github.com/rasimio/blueship/runtime/handler"
)

// A read-only credential snapshot: replay never refreshes or writes tokens.
type acceptanceReplayToken struct {
	mu    sync.Mutex
	value string
}

func (s *acceptanceReplayToken) AccessToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.value == "" {
		return "", fmt.Errorf("replay credential unavailable")
	}
	return s.value, nil
}
func (s *acceptanceReplayToken) Invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = ""
}

// Replays qualitative acceptance at the original review's clock instant.
// No database, browser, external actions, or delivery. This deliberately does
// not claim to replay the separate full-document source-grounding audit.
func TestLiveGraphAcceptanceReplay(t *testing.T) {
	if os.Getenv("BLUESHIP_LIVE_ACCEPTANCE_REPLAY") != "1" {
		t.Skip("explicit live replay opt-in required")
	}
	inputPath, promptPath, outputPath := os.Getenv("BLUESHIP_REPLAY_INPUT"), os.Getenv("BLUESHIP_REPLAY_PROMPT"), os.Getenv("BLUESHIP_REPLAY_OUTPUT")
	token := os.Getenv("BLUESHIP_LIVE_OAUTH_ACCESS_TOKEN")
	if inputPath == "" || promptPath == "" || outputPath == "" || token == "" {
		t.Fatal("explicit input, prompt, output and access token required")
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Task  core.AgentTask `json:"task"`
		Steps []struct {
			Status     string `json:"status"`
			Checkpoint struct {
				Traces []json.RawMessage `json:"traces"`
			} `json:"checkpoint"`
		} `json:"steps"`
		Calls []struct {
			StartedAt    time.Time `json:"started_at"`
			Model        string    `json:"model"`
			Effort       string    `json:"effort"`
			ThinkingMode string    `json:"thinking_mode"`
			MaxTokens    int       `json:"max_output_tokens"`
		} `json:"calls"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var traces []json.RawMessage
	for _, step := range fixture.Steps {
		if step.Status == "done" {
			traces = append(traces, step.Checkpoint.Traces...)
		}
	}
	rawTraces, err := json.Marshal(traces)
	if err != nil {
		t.Fatal(err)
	}
	// Explicit caller-selected original review time avoids guessing a role from
	// model parameters, which can coincide with other phases.
	reviewedAt, err := time.Parse(time.RFC3339Nano, os.Getenv("BLUESHIP_REPLAY_REVIEWED_AT"))
	if err != nil {
		t.Fatal(err)
	}
	var request core.CompletionRequest
	found := false
	for _, call := range fixture.Calls {
		if call.StartedAt.Equal(reviewedAt) {
			request.Model, request.Effort, request.ThinkingMode, request.MaxTokens = call.Model, call.Effort, call.ThinkingMode, call.MaxTokens
			found = true
		}
	}
	if !found {
		t.Fatal("review time must identify an actual recorded provider call")
	}
	if fixture.Task.Result == nil {
		t.Fatal("saved result is missing")
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(graphAcceptanceInput(fixture.Task, *fixture.Task.Result, rawTraces, extractURLs(*fixture.Task.Result), extractFetchedURLsFromTrace(rawTraces))), &input); err != nil {
		t.Fatal(err)
	}
	input["reviewed_at"] = reviewedAt.UTC().Format(time.RFC3339Nano)
	replayInput, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	request.System, request.Temperature = string(prompt), 0.01
	if effort := os.Getenv("BLUESHIP_REPLAY_EFFORT"); effort != "" {
		request.Effort = effort
	}
	if os.Getenv("BLUESHIP_REPLAY_CURRENT_BUDGET") == "1" {
		request.MaxTokens = graphAcceptanceMaxTokens
	}
	request.Messages = []core.Message{{Role: "user", Content: core.NormalizeContent(string(replayInput))}}
	provider := anthropic.NewOAuthProvider(&acceptanceReplayToken{value: token}, 90*time.Second, nil, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	started := time.Now()
	router := core.NewLLMRouter(provider, map[string]core.CompletionProvider{"anthropic-oauth": provider}, nil)
	response, callErr := router.Complete(ctx, request)
	var verdict AcceptanceVerdict
	var parseErr error
	complete := callErr == nil && response != nil && response.StopReason != "max_tokens"
	if complete {
		parseErr = json.Unmarshal([]byte(contentToText(response.Content)), &verdict)
	}
	// Preserve incomplete responses as evidence too. Previously max_tokens
	// discarded the very response needed to diagnose a failed review replay.
	record := map[string]any{"input_report": inputPath, "replayed_reviewed_at": reviewedAt, "elapsed_ms": time.Since(started).Milliseconds(), "response": response, "provider_failed": callErr != nil, "review_complete": complete && parseErr == nil, "request": request}
	if complete && parseErr == nil {
		record["verdict"] = verdict
	}
	if parseErr != nil {
		record["parse_error"] = parseErr.Error()
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(encoded)
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if callErr != nil {
		t.Fatal("review provider failed; response preserved")
	}
	if !complete {
		t.Fatal("incomplete review response; evidence preserved")
	}
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	t.Logf("met=%v reason=%s output=%s", verdict.Met, verdict.Reason, outputPath)
	if os.Getenv("BLUESHIP_REPLAY_EXPECT_REJECT") == "1" && verdict.Met {
		t.Fatal("invalid historical report accepted")
	}
	if os.Getenv("BLUESHIP_REPLAY_REPAIR") != "1" || verdict.Met {
		return
	}
	var saved struct {
		Steps []core.TaskStep `json:"steps"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	cfg := &core.Config{}
	cfg.Models.Primary.Provider = "anthropic-oauth"
	cfg.Models.Primary.Name = strings.TrimPrefix(request.Model, "anthropic-oauth:")
	cfg.Models.Primary.Effort = "medium"
	deps := core.AgentDeps{Config: cfg, LLM: router, Prompts: core.NewFilePromptStore(filepath.Dir(promptPath))}
	repairCtx, repairCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer repairCancel()
	repairStarted := time.Now()
	candidate, err := handler.NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(repairCtx, fixture.Task, saved.Steps, deps, *fixture.Task.Result, verdict.Reason)
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), token, "[credential]"))
	}
	repairMS := time.Since(repairStarted).Milliseconds()
	repairRecord := map[string]any{"input_report": inputPath, "initial_verdict": verdict, "repair_candidate": candidate, "repair_ms": repairMS, "grounding_replayed": false}
	saveRepair := func() {
		t.Helper()
		body, err := json.MarshalIndent(repairRecord, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outputPath+".repair.json", body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Preserve the candidate before another network call, including failed or
	// truncated reviews. A failed experiment must still explain its outcome.
	saveRepair()
	input["result"], input["reviewed_at"] = candidate, time.Now().UTC().Format(time.RFC3339Nano)
	repairedInput, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	request.Messages = []core.Message{{Role: "user", Content: core.NormalizeContent(string(repairedInput))}}
	reviewStarted := time.Now()
	response, err = router.Complete(repairCtx, request)
	repairRecord["review_ms"] = time.Since(reviewStarted).Milliseconds()
	repairRecord["review_response"] = response
	if err != nil {
		repairRecord["review_error"] = strings.ReplaceAll(err.Error(), token, "[credential]")
	}
	saveRepair()
	if err != nil {
		t.Fatal(strings.ReplaceAll(err.Error(), token, "[credential]"))
	}
	if response == nil || response.StopReason == "max_tokens" {
		t.Fatal("incomplete repaired review")
	}
	var repairedVerdict AcceptanceVerdict
	if err := json.Unmarshal([]byte(contentToText(response.Content)), &repairedVerdict); err != nil {
		t.Fatal(err)
	}
	repairRecord["repair_acceptance"] = repairedVerdict
	saveRepair()
	t.Logf("repair_ms=%d met=%v reason=%s", repairMS, repairedVerdict.Met, repairedVerdict.Reason)
	if !repairedVerdict.Met {
		t.Fatal("repaired report still fails qualitative acceptance")
	}
}
