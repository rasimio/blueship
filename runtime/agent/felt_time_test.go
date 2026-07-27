package agent

import (
	"strings"
	"testing"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestAnnotateDialogDaysInsertsBoundaryMarkers(t *testing.T) {
	loc := time.FixedZone("CET", 2*3600)
	now := time.Date(2026, 7, 20, 11, 50, 0, 0, loc) // Monday
	msgs := []bs.Message{
		{Role: "user", Content: "суббота вопрос", CreatedAt: time.Date(2026, 7, 18, 16, 47, 0, 0, loc)},
		{Role: "assistant", Content: "суббота ответ", CreatedAt: time.Date(2026, 7, 18, 16, 48, 0, 0, loc)},
		{Role: "user", Content: "воскресенье вопрос", CreatedAt: time.Date(2026, 7, 19, 20, 13, 0, 0, loc)},
		{Role: "user", Content: "понедельник вопрос", CreatedAt: now},
	}

	out := annotateDialogDays(msgs, now)

	var markers []string
	for _, m := range out {
		if s, ok := m.Content.(string); ok && strings.HasPrefix(s, "[date: ") {
			markers = append(markers, s)
		}
	}
	want := []string{
		"[date: Saturday 2026-07-18]",
		"[date: Sunday 2026-07-19]",
		"[date: Monday 2026-07-20]",
	}
	if len(markers) != len(want) {
		t.Fatalf("markers = %v, want %v", markers, want)
	}
	for i := range want {
		if markers[i] != want[i] {
			t.Fatalf("marker[%d] = %q, want %q", i, markers[i], want[i])
		}
	}
	if len(out) != len(msgs)+3 {
		t.Fatalf("len(out) = %d, want %d", len(out), len(msgs)+3)
	}
	// The Monday marker must sit directly before the Monday message.
	if out[len(out)-2].Content != "[date: Monday 2026-07-20]" {
		t.Fatalf("expected Monday marker before current message, got %v", out[len(out)-2].Content)
	}
}

func TestAnnotateDialogDaysOffWithoutTurnNow(t *testing.T) {
	msgs := []bs.Message{{Role: "user", Content: "hi", CreatedAt: time.Now()}}
	out := annotateDialogDays(msgs, time.Time{})
	if len(out) != 1 {
		t.Fatalf("expected untouched messages, got %d", len(out))
	}
}

func TestFeltTimeContextStaleGap(t *testing.T) {
	loc := time.FixedZone("CET", 2*3600)
	now := time.Date(2026, 7, 20, 11, 50, 0, 0, loc)
	msgs := []bs.Message{
		{Role: "assistant", Content: "старое", CreatedAt: time.Date(2026, 7, 18, 20, 14, 0, 0, loc)},
		{Role: "user", Content: "текущее", CreatedAt: now},
	}

	got := feltTimeContext(msgs, now, true)
	for _, needle := range []string{"[felt_time]", "Now: Monday 2026-07-20 11:50", "Saturday 2026-07-18 20:14", "40h ago"} {
		if !strings.Contains(got, needle) {
			t.Fatalf("felt_time missing %q in %q", needle, got)
		}
	}
}

func TestFeltTimeContextQuietWhenRecentSameDay(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, loc)
	msgs := []bs.Message{
		{Role: "assistant", Content: "недавнее", CreatedAt: now.Add(-30 * time.Minute)},
		{Role: "user", Content: "текущее", CreatedAt: now},
	}
	if got := feltTimeContext(msgs, now, true); got != "" {
		t.Fatalf("expected empty felt_time for recent same-day gap, got %q", got)
	}
	if got := feltTimeContext(msgs[1:], now, true); got != "" {
		t.Fatalf("expected empty felt_time with no prior message, got %q", got)
	}
}

func TestFeltTimeContextPromptOnlyUsesLatestStoredMessage(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, loc)
	msgs := []bs.Message{
		{Role: "user", Content: "old question", CreatedAt: now.Add(-73 * time.Hour)},
		{Role: "assistant", Content: "last reply", CreatedAt: now.Add(-72 * time.Hour)},
	}

	got := feltTimeContext(msgs, now, false)
	if !strings.Contains(got, "Tuesday 2026-07-21 12:00 — 72h ago") {
		t.Fatalf("prompt-only felt_time should use latest stored assistant, got %q", got)
	}
}
