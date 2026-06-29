package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

type turnTimer struct {
	started time.Time
	mu      sync.Mutex
	spans   []bs.TimingSpan
}

func newTurnTimer() *turnTimer {
	return &turnTimer{started: time.Now()}
}

func (t *turnTimer) Add(span bs.TimingSpan) {
	if t == nil || span.Name == "" {
		return
	}
	t.mu.Lock()
	t.spans = append(t.spans, span)
	t.mu.Unlock()
}

func (t *turnTimer) RecordSince(name string, started time.Time, detail string) {
	if t == nil || started.IsZero() {
		return
	}
	t.Add(bs.TimingSpan{
		Name:       name,
		DurationMs: timingDurationMs(time.Since(started)),
		Detail:     detail,
	})
}

func (t *turnTimer) Report() bs.TimingReport {
	if t == nil {
		return bs.TimingReport{}
	}
	t.mu.Lock()
	spans := append([]bs.TimingSpan(nil), t.spans...)
	t.mu.Unlock()
	return bs.TimingReport{
		TotalMs: timingDurationMs(time.Since(t.started)),
		Spans:   spans,
	}
}

func (t *turnTimer) Summary() string {
	report := t.Report()
	parts := make([]string, 0, len(report.Spans)+1)
	parts = append(parts, fmt.Sprintf("total=%dms", report.TotalMs))
	for _, span := range report.Spans {
		label := span.Name
		if span.Detail != "" {
			label += "(" + span.Detail + ")"
		}
		parts = append(parts, fmt.Sprintf("%s=%dms", label, span.DurationMs))
	}
	return strings.Join(parts, " ")
}

func timingDurationMs(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Millisecond)
}

func (g *Gateway) finishTurnTiming(ctx context.Context, sink bs.ResponseSink, us *UserState, timings *turnTimer) {
	if timings == nil {
		return
	}
	report := timings.Report()
	if ts, ok := sink.(bs.TimingSink); ok {
		_ = ts.SendTiming(ctx, report)
	}
	attrs := []any{
		"chat_id", us.ChatID,
		"total_ms", report.TotalMs,
		"spans", timings.Summary(),
	}
	if len(report.Spans) == 0 {
		attrs = append(attrs, slog.String("note", "no spans recorded"))
	}
	g.logger.Info("turn timing summary", attrs...)
}
