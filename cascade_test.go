package blueship

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type cascadeFake struct {
	call   func(context.Context, CompletionRequest) (*CompletionResponse, error)
	stream func(context.Context, CompletionRequest, *StreamCallbacks) (*CompletionResponse, error)
}

func (f cascadeFake) Complete(c context.Context, r CompletionRequest) (*CompletionResponse, error) {
	return f.call(c, r)
}
func (f cascadeFake) StreamComplete(c context.Context, r CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error) {
	if f.stream != nil {
		return f.stream(c, r, cb)
	}
	return f.call(c, r)
}
func answer() *CompletionResponse {
	return &CompletionResponse{Content: []ContentBlock{{Type: "text", Text: "ok"}}}
}
func TestCascadeFailoverCooldownAndRecovery(t *testing.T) {
	clock := time.Now()
	cloudCalls := 0
	localCalls := 0
	cloud := cascadeFake{call: func(context.Context, CompletionRequest) (*CompletionResponse, error) {
		cloudCalls++
		if cloudCalls == 1 {
			return nil, errors.New("502")
		}
		return answer(), nil
	}}
	local := cascadeFake{call: func(_ context.Context, r CompletionRequest) (*CompletionResponse, error) {
		localCalls++
		if r.Model != "local" || r.Effort != "low" || r.MaxTokens != 128 {
			t.Fatalf("route overrides: %+v", r)
		}
		return answer(), nil
	}}
	c, err := NewCascadeProvider([]CascadeRoute{{Name: "cloud", Model: "cloud", Provider: cloud, Cooldown: time.Minute}, {Name: "local", Model: "local", Provider: local, Effort: "low", MaxTokens: 128}})
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return clock }
	req := CompletionRequest{Model: "requested", Effort: "max", MaxTokens: 256}
	for i := 0; i < 2; i++ {
		if _, err = c.Complete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if cloudCalls != 1 || localCalls != 2 {
		t.Fatalf("cooldown bypassed: cloud=%d local=%d", cloudCalls, localCalls)
	}
	if req.Model != "requested" || req.MaxTokens != 256 {
		t.Fatal("request mutated")
	}
	clock = clock.Add(2 * time.Minute)
	if _, err = c.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if c.Status().LastSuccessfulRoute != "cloud" || cloudCalls != 2 {
		t.Fatal("did not recover primary")
	}
}
func TestCascadeTimeoutAndCallerCancellation(t *testing.T) {
	var local atomic.Int32
	stuck := cascadeFake{call: func(c context.Context, _ CompletionRequest) (*CompletionResponse, error) {
		<-c.Done()
		return nil, c.Err()
	}}
	fallback := cascadeFake{call: func(context.Context, CompletionRequest) (*CompletionResponse, error) {
		local.Add(1)
		return answer(), nil
	}}
	c, _ := NewCascadeProvider([]CascadeRoute{{Name: "cloud", Model: "a", Provider: stuck, Timeout: 10 * time.Millisecond}, {Name: "local", Model: "b", Provider: fallback}})
	if _, err := c.Complete(context.Background(), CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if local.Load() != 1 {
		t.Fatal("attempt timeout did not fall back")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Complete(ctx, CompletionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if local.Load() != 1 {
		t.Fatal("caller cancellation retried")
	}
}
func TestCascadeDoesNotReplayAfterStreamCallbacks(t *testing.T) {
	for _, kind := range []string{"text", "tool", "thinking"} {
		t.Run(kind, func(t *testing.T) {
			local := 0
			primary := cascadeFake{stream: func(_ context.Context, _ CompletionRequest, cb *StreamCallbacks) (*CompletionResponse, error) {
				switch kind {
				case "text":
					cb.OnText("partial")
				case "tool":
					cb.OnToolUse("id", "execute", nil)
				case "thinking":
					cb.OnThinking("partial")
				}
				return nil, errors.New("connection lost")
			}}
			fallback := cascadeFake{call: func(context.Context, CompletionRequest) (*CompletionResponse, error) { local++; return answer(), nil }}
			c, _ := NewCascadeProvider([]CascadeRoute{{Name: "primary", Model: "a", Provider: primary}, {Name: "fallback", Model: "b", Provider: fallback}})
			_, err := c.StreamComplete(context.Background(), CompletionRequest{}, &StreamCallbacks{OnText: func(string) {}, OnThinking: func(string) {}, OnToolUse: func(string, string, json.RawMessage) {}})
			if err == nil || local != 0 {
				t.Fatal("partial stream replayed")
			}
		})
	}
}

func TestCascadeSelectionOverridesOnlyPreferredRoute(t *testing.T) {
	var seen []CompletionRequest
	var attempts []CascadeAttempt
	c, _ := NewCascadeProvider([]CascadeRoute{
		{Name: "cloud", Model: "default", Effort: "high", Provider: cascadeFake{call: func(_ context.Context, r CompletionRequest) (*CompletionResponse, error) {
			seen = append(seen, r)
			return nil, errors.New("offline")
		}}},
		{Name: "local", Model: "qwen", Provider: cascadeFake{call: func(_ context.Context, r CompletionRequest) (*CompletionResponse, error) {
			seen = append(seen, r)
			return answer(), nil
		}}},
	})
	effort := "max"
	ctx := WithCascadeSelection(context.Background(), CascadeSelection{Route: "cloud", Model: "chosen", Effort: &effort})
	ctx = WithCascadeObserver(ctx, func(a CascadeAttempt) { attempts = append(attempts, a) })
	if _, err := c.Complete(ctx, CompletionRequest{Model: "old", Effort: "xhigh"}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].Model != "chosen" || seen[0].Effort != "max" || seen[1].Model != "qwen" || seen[1].Effort != "" {
		t.Fatalf("route settings leaked: %+v", seen)
	}
	if len(attempts) != 4 || attempts[1].Phase != "failed" || attempts[3].Phase != "succeeded" {
		t.Fatal(attempts)
	}
	seen = nil
	if _, err := c.Complete(WithCascadeSelection(context.Background(), CascadeSelection{Route: "local", Only: true}), CompletionRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].Model != "qwen" {
		t.Fatal("offline selection called cloud")
	}
	if _, err := c.Complete(WithCascadeSelection(context.Background(), CascadeSelection{Route: "unknown"}), CompletionRequest{}); err == nil {
		t.Fatal("unknown route accepted")
	}
}
