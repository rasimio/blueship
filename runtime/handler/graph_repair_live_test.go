package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/anthropic"
)

type repairReplayToken struct{ value string }

func (s *repairReplayToken) AccessToken() (string, error) {
	if s.value == "" {
		return "", fmt.Errorf("replay credential unavailable")
	}
	return s.value, nil
}
func (s *repairReplayToken) Invalidate() { s.value = "" }

// Opt-in replay of a saved draft through the actual repair handler. No tools,
// database, delivery, token refresh or claim of full task acceptance.
func TestLiveGraphRepairReplay(t *testing.T) {
	if os.Getenv("BLUESHIP_LIVE_REPAIR_REPLAY") != "1" {
		t.Skip("explicit live replay required")
	}
	input, output := os.Getenv("BLUESHIP_REPLAY_INPUT"), os.Getenv("BLUESHIP_REPLAY_OUTPUT")
	token, promptPath := os.Getenv("BLUESHIP_LIVE_OAUTH_ACCESS_TOKEN"), os.Getenv("BLUESHIP_REPLAY_PROMPT")
	if input == "" || output == "" || input == output || token == "" || promptPath == "" {
		t.Fatal("distinct paths, prompt and credential snapshot required")
	}
	data, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Task  core.AgentTask  `json:"task"`
		Steps []core.TaskStep `json:"steps"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	feedback := os.Getenv("BLUESHIP_REPLAY_FEEDBACK")
	if feedback == "" || (fixture.Task.Result == nil || *fixture.Task.Result == "") {
		t.Fatal("saved draft and explicit feedback required")
	}
	provider := anthropic.NewOAuthProvider(&repairReplayToken{value: token}, 90*time.Second, nil, discardLogger())
	router := core.NewLLMRouter(provider, map[string]core.CompletionProvider{"anthropic-oauth": provider}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var response *core.CompletionResponse
	meter := &capturingProvider{respond: func(req core.CompletionRequest) (*core.CompletionResponse, error) {
		var callErr error
		response, callErr = router.Complete(ctx, req)
		return response, callErr
	}}
	deps, _ := backgroundTestDeps(meter, map[string]string{"background-graph-repair": string(prompt)})
	deps.Config.Models.Primary = core.ModelRef{Provider: "anthropic-oauth", Name: "claude-opus-5", Effort: "medium", ThinkingMode: "adaptive"}
	if effort := os.Getenv("BLUESHIP_REPLAY_EFFORT"); effort != "" {
		deps.Config.Models.Primary.Effort = effort
	}
	started := time.Now()
	result, repairErr := NewBackground(time.UTC, nil, nil, nil).RepairGraphResult(ctx, fixture.Task, fixture.Steps, deps, *fixture.Task.Result, feedback)
	record := map[string]any{"source_fixture": input, "elapsed_ms": time.Since(started).Milliseconds(), "response": response, "result": result, "feedback": feedback, "repair_failed": repairErr != nil}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
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
	if repairErr != nil {
		t.Fatal(repairErr)
	}
	if strings.TrimSpace(result) == "" || result == *fixture.Task.Result {
		t.Fatal("repair produced no changed candidate")
	}
	t.Logf("repair candidate saved; elapsed=%s; final gates not run", time.Since(started).Round(time.Millisecond))
}
