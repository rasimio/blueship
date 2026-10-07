package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	bs "github.com/rasimio/blueship/internal/core"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type readBatchProvider struct{ calls int }

func (p *readBatchProvider) Complete(context.Context, bs.CompletionRequest) (*bs.CompletionResponse, error) {
	p.calls++
	if p.calls > 1 {
		return &bs.CompletionResponse{StopReason: "end_turn", Content: []bs.ContentBlock{{Type: "text", Text: "Complete"}}}, nil
	}
	var blocks []bs.ContentBlock
	for i := 0; i < 5; i++ {
		blocks = append(blocks, bs.ContentBlock{Type: "tool_use", ID: fmt.Sprint(i), Name: "fetch", Input: json.RawMessage(fmt.Sprintf(`{"index":%d}`, i))})
	}
	return &bs.CompletionResponse{StopReason: "tool_use", Content: blocks}, nil
}

func TestTrackedReadBatchParallelismCheckpointAndOrder(t *testing.T) {
	registry := bs.NewToolRegistry()
	entered := make(chan struct{}, 5)
	release := make(chan struct{})
	var active, peak atomic.Int32
	registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(ctx context.Context, input json.RawMessage) (any, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		entered <- struct{}{}
		select {
		case <-release:
			return string(input), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	if err := registry.MarkReadOnly("fetch"); err != nil {
		t.Fatal(err)
	}
	loop := NewLoop(&readBatchProvider{}, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		r *RunResult
		e error
	}
	done := make(chan outcome, 1)
	checkpoints := 0
	go func() {
		r, e := loop.RunTracked(ctx, RunConfig{SessionID: "batch", Model: "test", MaxTurns: 3, MaxTokens: 100, ParallelReadTools: 3, StrictTools: true, ToolOverride: []string{"fetch"}, OnCheckpoint: func(_ context.Context, cp RunCheckpoint) error {
			if cp.Phase == "tool_completed" {
				checkpoints++
			}
			return nil
		}}, "Read independent sources")
		done <- outcome{r, e}
	}()
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("reads ran serially")
		}
	}
	close(release)
	result := <-done
	if result.e != nil || result.r == nil || len(result.r.ToolTraces) != 5 || peak.Load() != 3 || active.Load() != 0 || checkpoints != 5 {
		t.Fatalf("invalid batch: %+v peak=%d active=%d checkpoints=%d", result, peak.Load(), active.Load(), checkpoints)
	}
	for i, trace := range result.r.ToolTraces {
		if trace.BlockID != fmt.Sprint(i) || trace.Receipt == nil {
			t.Fatal("lost ordered receipt", trace)
		}
		if trace.StartedAt == nil || trace.StartedAt.IsZero() || trace.StartedAt.After(time.Now()) || trace.DurationMs < 0 || trace.TimedOut {
			t.Fatal("missing or invalid attempt timing", trace)
		}
		raw, err := json.Marshal(trace)
		var saved ToolTrace
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &saved); err != nil || saved.StartedAt == nil || !saved.StartedAt.Equal(*trace.StartedAt) || saved.DurationMs != trace.DurationMs {
			t.Fatal("timing lost from durable trace", saved, err)
		}
	}
}

func TestReadBatchCheckpointFailurePreventsDispatch(t *testing.T) {
	registry := bs.NewToolRegistry()
	var calls atomic.Int32
	registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(context.Context, json.RawMessage) (any, error) { calls.Add(1); return "unexpected", nil })
	_ = registry.MarkReadOnly("fetch")
	loop := NewLoop(&readBatchProvider{}, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	failure := errors.New("checkpoint unavailable")
	_, err := loop.RunTracked(context.Background(), RunConfig{SessionID: "batch", Model: "test", MaxTurns: 3, ParallelReadTools: 3, OnCheckpoint: func(_ context.Context, cp RunCheckpoint) error {
		if cp.Phase == "read_batch_started" {
			return failure
		}
		return nil
	}}, "read")
	if !errors.Is(err, failure) || calls.Load() != 0 {
		t.Fatal(err, calls.Load())
	}
	// Unknown/effectful tools or denied tools force the sequential policy path.
	blocks := []bs.ContentBlock{{Type: "tool_use", Name: "fetch"}, {Type: "tool_use", Name: "write"}}
	if loop.canBatchReads(RunConfig{ParallelReadTools: 3}, blocks, nil) {
		t.Fatal("unknown mutation batched")
	}
	blocks[1].Name = "fetch"
	if loop.canBatchReads(RunConfig{ParallelReadTools: 3, StrictTools: true}, blocks, nil) {
		t.Fatal("denied read batched")
	}
}

func TestReadBatchCancellationDrainsWorkersAndSkipsQueuedReads(t *testing.T) {
	registry := bs.NewToolRegistry()
	entered := make(chan struct{}, 5)
	var calls, active atomic.Int32
	registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(ctx context.Context, _ json.RawMessage) (any, error) {
		calls.Add(1)
		active.Add(1)
		defer active.Add(-1)
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	_ = registry.MarkReadOnly("fetch")
	loop := NewLoop(nil, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	blocks := make([]bs.ContentBlock, 5)
	for i := range blocks {
		blocks[i] = bs.ContentBlock{Type: "tool_use", Name: "fetch", Input: json.RawMessage(`{}`)}
	}
	results, stop := loop.startReadBatch(ctx, RunConfig{ParallelReadTools: 3}, blocks)
	defer stop()
	for i := 0; i < 3; i++ {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("batch did not start")
		}
	}
	stop()
	if calls.Load() != 3 || active.Load() != 0 {
		t.Fatal("queued reads ran or workers leaked", calls.Load(), active.Load())
	}
	for _, ch := range results {
		select {
		case r := <-ch:
			if !r.isError {
				t.Fatal("canceled read succeeded")
			}
		default:
			t.Fatal("result channel stranded")
		}
	}
}

func TestReadBatchTimedOutHandlerRetainsCapacity(t *testing.T) {
	registry := bs.NewToolRegistry()
	entered := make(chan int, 5)
	exited := make(chan struct{}, 5)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	var calls atomic.Int32
	registry.Register("fetch", "fetch", json.RawMessage(`{}`), func(_ context.Context, input json.RawMessage) (any, error) {
		var index int
		_ = json.Unmarshal(input, &index)
		calls.Add(1)
		entered <- index
		<-release // Deliberately ignores cancellation to exercise the hard boundary.
		exited <- struct{}{}
		return "late", nil
	})
	_ = registry.MarkReadOnly("fetch")
	loop := NewLoop(nil, &fakeMessageStore{}, registry, nil, &bs.Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	blocks := make([]bs.ContentBlock, 5)
	for i := range blocks {
		blocks[i] = bs.ContentBlock{Type: "tool_use", Name: "fetch", Input: json.RawMessage(fmt.Sprint(i))}
	}
	results, stop := loop.startReadBatch(ctx, RunConfig{ParallelReadTools: 3, ToolTimeout: 30 * time.Millisecond}, blocks)
	for i := 0; i < 3; i++ {
		select {
		case index := <-entered:
			select {
			case result := <-results[index]:
				if !result.timedOut {
					t.Fatal("expected timeout")
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("uncooperative handler stranded cancellation")
	}
	if calls.Load() != 3 {
		t.Fatal("timed-out handlers released capacity prematurely", calls.Load())
	}
	releaseAll()
	for i := 0; i < 3; i++ {
		select {
		case <-exited:
		case <-ctx.Done():
			t.Fatal("handler did not exit after release")
		}
	}
	stop()
}
