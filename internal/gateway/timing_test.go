package gateway

import (
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestTurnTimerRecordsReportAndSummary(t *testing.T) {
	timer := newTurnTimer()
	timer.Add(bs.TimingSpan{Name: "llm.stream_complete", DurationMs: 123, Detail: "role=cortex"})
	timer.RecordSince("tool.execute", time.Now().Add(-10*time.Millisecond), "tool=memory_search")

	report := timer.Report()
	if report.TotalMs < 0 {
		t.Fatalf("total_ms = %d, want non-negative", report.TotalMs)
	}
	if len(report.Spans) != 2 {
		t.Fatalf("spans = %d, want 2: %#v", len(report.Spans), report.Spans)
	}
	summary := timer.Summary()
	if !strings.Contains(summary, "llm.stream_complete(role=cortex)=123ms") {
		t.Fatalf("summary missing llm span: %s", summary)
	}
	if !strings.Contains(summary, "tool.execute(tool=memory_search)=") {
		t.Fatalf("summary missing tool span: %s", summary)
	}
}
