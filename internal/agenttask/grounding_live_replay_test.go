package agenttask

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
	"github.com/rasimio/blueship/internal/provider/anthropic"
)

// Replays the exact captured auditor input, without fetching sources again or
// running research, actions, delivery, or a database. An accepted audit alone
// is not proof of full task acceptance.
func TestLiveGroundingReplay(t *testing.T) {
	if os.Getenv("BLUESHIP_LIVE_GROUNDING_REPLAY") != "1" {
		t.Skip("explicit live audit replay required")
	}
	inputPath, outputPath := os.Getenv("BLUESHIP_REPLAY_INPUT"), os.Getenv("BLUESHIP_REPLAY_OUTPUT")
	token := os.Getenv("BLUESHIP_LIVE_OAUTH_ACCESS_TOKEN")
	if inputPath == "" || outputPath == "" || token == "" || inputPath == outputPath {
		t.Fatal("distinct input/output and credential snapshot required")
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Calls []struct {
			System       string         `json:"audit_system"`
			Messages     []core.Message `json:"audit_messages"`
			Model        string         `json:"model"`
			Effort       string         `json:"effort"`
			ThinkingMode string         `json:"thinking_mode"`
			MaxTokens    int            `json:"max_output_tokens"`
		} `json:"calls"`
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	var req core.CompletionRequest
	for _, c := range fixture.Calls {
		if c.System != "" && len(c.Messages) > 0 {
			req = core.CompletionRequest{System: c.System, Messages: c.Messages, Model: c.Model, Effort: c.Effort, ThinkingMode: c.ThinkingMode, MaxTokens: c.MaxTokens, Temperature: 0.2}
			break
		}
	}
	if req.System == "" {
		t.Fatal("report has no captured audit input; cannot replay exactly")
	}
	if os.Getenv("BLUESHIP_REPLAY_CURRENT_POLICY") == "1" {
		req.System = groundingPrompt(core.AgentTask{ExecutorVersion: 2})
	}
	if effort := os.Getenv("BLUESHIP_REPLAY_EFFORT"); effort != "" {
		req.Effort = effort
	}
	provider := anthropic.NewOAuthProvider(&acceptanceReplayToken{value: token}, 90*time.Second, nil, slog.New(slog.DiscardHandler))
	router := core.NewLLMRouter(provider, map[string]core.CompletionProvider{"anthropic-oauth": provider}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if os.Getenv("BLUESHIP_REPLAY_PARTITIONS") == "1" {
		replayGroundingParts(t, ctx, router, req, inputPath, outputPath)
		return
	}
	started := time.Now()
	response, callErr := router.Complete(ctx, req)
	record := map[string]any{"source_fixture": inputPath, "audit_system": req.System, "elapsed_ms": time.Since(started).Milliseconds(), "effort": req.Effort, "max_output_tokens": req.MaxTokens, "response": response, "provider_failed": callErr != nil}
	// Preserve incomplete/failed evidence too; don't discard it with t.Fatal.
	if response != nil && response.StopReason != "max_tokens" && callErr == nil {
		verdict, parseErr := parseGroundingResponse(contentToText(response.Content))
		if parseErr == nil {
			record["verdict"] = strictGraphGrounding(scoreGroundingVerdict(verdict))
		} else {
			record["parse_error"] = fmt.Sprint(parseErr)
		}
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(outputPath, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if callErr != nil {
		t.Fatal("audit provider failed; failure flag and response saved")
	}
	if response == nil || response.StopReason == "max_tokens" {
		t.Fatal("incomplete audit; response saved")
	}
	if _, ok := record["verdict"]; !ok {
		t.Fatal("unparseable audit; response saved")
	}
	t.Logf("audit replay saved to %s; elapsed=%s", outputPath, time.Since(started).Round(time.Millisecond))
}

// Reuse runtime partition scheduling and aggregation on the exact captured
// source text. Only the scope marker is replaced; no source is reconstructed.
func replayGroundingParts(t *testing.T, ctx context.Context, llm core.CompletionProvider, req core.CompletionRequest, inputPath, outputPath string) {
	t.Helper()
	report, base, err := groundingReplayPartitionBase(req)
	if err != nil {
		t.Fatal(err)
	}
	parts := partitionGroundingReport(report)
	records := make([]map[string]any, len(parts))
	started := time.Now()
	results := runGroundingParts(ctx, parts, func(i int, target string) GroundingVerdict {
		request := base
		request.Messages = append([]core.Message(nil), base.Messages...)
		last := len(request.Messages) - 1
		user := core.ExtractText(core.NormalizeContent(request.Messages[last].Content))
		request.Messages[last].Content = core.NormalizeContent(user + "\n\n[audit_target]\n" + target)
		begin := time.Now()
		response, err := llm.Complete(ctx, request)
		records[i] = map[string]any{"target": target, "response": response, "elapsed_ms": time.Since(begin).Milliseconds(), "provider_failed": err != nil}
		if err != nil || response == nil || response.StopReason == "max_tokens" {
			return GroundingVerdict{Unavailable: true, Reason: "replay partition incomplete"}
		}
		verdict, err := parseGroundingResponse(contentToText(response.Content))
		if err != nil {
			return GroundingVerdict{Unavailable: true, Reason: "replay partition unparseable"}
		}
		return strictGraphGrounding(scoreGroundingVerdict(verdict))
	})
	combined := combineGroundingParts(results)
	encoded, err := json.MarshalIndent(map[string]any{"source_fixture": inputPath, "audit_system": req.System, "elapsed_ms": time.Since(started).Milliseconds(), "parts": records, "part_verdicts": results, "verdict": combined}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write(encoded)
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if combined.Unavailable {
		t.Fatal("partition audit unavailable; evidence saved")
	}
	t.Logf("partition audit met=%v elapsed=%s; evidence saved", combined.Met, time.Since(started).Round(time.Millisecond))
}

// Captures made before source-first requests contain a single report/source
// message; newer captures have an unchanged corpus followed by report/target.
// Remove an existing target only when it exactly matches a runtime partition.
func groundingReplayPartitionBase(req core.CompletionRequest) (string, core.CompletionRequest, error) {
	const marker = "\n\n[audit_target]\n"
	if len(req.Messages) != 1 && len(req.Messages) != 2 {
		return "", req, fmt.Errorf("partition replay requires one or two captured user messages")
	}
	for _, message := range req.Messages {
		if message.Role != "user" {
			return "", req, fmt.Errorf("partition replay requires captured user messages")
		}
	}
	last := len(req.Messages) - 1
	user := core.ExtractText(core.NormalizeContent(req.Messages[last].Content))
	if !strings.HasPrefix(user, "[report]\n") {
		return "", req, fmt.Errorf("missing captured report boundary")
	}
	report := strings.TrimPrefix(user, "[report]\n")
	if last == 0 {
		var ok bool
		report, _, ok = strings.Cut(report, "\n\n[fetched_documents]\n")
		if !ok {
			return "", req, fmt.Errorf("missing captured source boundary")
		}
		for _, part := range partitionGroundingReport(report) {
			if strings.HasSuffix(user, marker+part) {
				user = strings.TrimSuffix(user, marker+part)
				break
			}
		}
	} else {
		sources := core.ExtractText(core.NormalizeContent(req.Messages[0].Content))
		if !strings.HasPrefix(sources, "[fetched_documents]\n") {
			return "", req, fmt.Errorf("missing captured source boundary")
		}
		if index := strings.LastIndex(report, marker); index >= 0 {
			candidate, target := report[:index], report[index+len(marker):]
			for _, part := range partitionGroundingReport(candidate) {
				if target == part {
					report = candidate
					user = "[report]\n" + report
					break
				}
			}
		}
	}
	req.Messages = append([]core.Message(nil), req.Messages...)
	req.Messages[last].Content = core.NormalizeContent(user)
	return report, req, nil
}
