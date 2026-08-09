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
	// The day marker carries the clock too: a day whose first message lands at
	// 23:50 otherwise reads as if it had started in the morning.
	want := []string{
		"[date: Saturday 2026-07-18 16:47]",
		"[date: Sunday 2026-07-19 20:13]",
		"[date: Monday 2026-07-20 11:50]",
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
	if out[len(out)-2].Content != "[date: Monday 2026-07-20 11:50]" {
		t.Fatalf("expected Monday marker before current message, got %v", out[len(out)-2].Content)
	}
}

// Inside one calendar day the window used to be an unbroken run of messages,
// so the model could not tell two minutes from three hours. That is how a reply
// placed a training session and the report about it at the same moment: the
// training was discussed at 18:20, the report arrived at 21:29, and nothing in
// the prompt distinguished those.
func TestAnnotateDialogDaysMarksGapsWithinOneDay(t *testing.T) {
	loc := time.FixedZone("MSK", 3*3600)
	day := func(h, m int) time.Time { return time.Date(2026, 8, 8, h, m, 0, 0, loc) }
	now := day(21, 30)

	msgs := []bs.Message{
		{Role: "user", Content: "утро", CreatedAt: day(9, 5)},
		{Role: "assistant", Content: "ответ", CreatedAt: day(9, 6)},
		// Same conversation, seconds apart — must stay quiet.
		{Role: "user", Content: "ещё", CreatedAt: day(9, 7)},
		// Nine hours later: the hole the model has to see.
		{Role: "user", Content: "тренировка", CreatedAt: day(18, 20)},
		{Role: "assistant", Content: "ответ", CreatedAt: day(18, 21)},
		// Three hours later again.
		{Role: "user", Content: "отчёт", CreatedAt: day(21, 29)},
	}

	out := annotateDialogDays(msgs, now)

	var markers []string
	for _, m := range out {
		s, ok := m.Content.(string)
		if !ok {
			continue
		}
		if strings.HasPrefix(s, "[date: ") || strings.HasPrefix(s, "[time: ") {
			markers = append(markers, s)
		}
	}
	want := []string{
		"[date: Saturday 2026-08-08 09:05]",
		"[time: 18:20]",
		"[time: 21:29]",
	}
	if len(markers) != len(want) {
		t.Fatalf("markers = %v, want %v", markers, want)
	}
	for i := range want {
		if markers[i] != want[i] {
			t.Errorf("marker[%d] = %q, want %q", i, markers[i], want[i])
		}
	}

	// The gap marker has to sit immediately before the message it dates, or it
	// times the wrong turn.
	for i, m := range out {
		if s, ok := m.Content.(string); ok && s == "[time: 18:20]" {
			if i+1 >= len(out) || out[i+1].Content != "тренировка" {
				t.Errorf("the 18:20 marker does not precede the message it belongs to")
			}
		}
	}
}

// Cache stability. The dialogue prefix has to be byte-identical between turns,
// so the markers must be a function of the messages and not of the clock —
// which is what rules out stamping messages with a relative "N minutes ago".
func TestAnnotateDialogDaysMarkersDoNotMoveWithTheClock(t *testing.T) {
	loc := time.FixedZone("MSK", 3*3600)
	msgs := []bs.Message{
		{Role: "user", Content: "a", CreatedAt: time.Date(2026, 8, 8, 9, 5, 0, 0, loc)},
		{Role: "user", Content: "b", CreatedAt: time.Date(2026, 8, 8, 18, 20, 0, 0, loc)},
	}

	render := func(now time.Time) string {
		var b strings.Builder
		for _, m := range annotateDialogDays(msgs, now) {
			if s, ok := m.Content.(string); ok {
				b.WriteString(s)
				b.WriteString("|")
			}
		}
		return b.String()
	}

	first := render(time.Date(2026, 8, 8, 18, 25, 0, 0, loc))
	later := render(time.Date(2026, 8, 9, 3, 40, 0, 0, loc))
	if first != later {
		t.Errorf("markers changed as the clock moved, which invalidates the cached prefix on every turn:\n at 18:25: %s\n next day: %s", first, later)
	}
}

// A conversation nobody paused in should carry exactly one marker: the day.
func TestAnnotateDialogDaysStaysQuietInContinuousTalk(t *testing.T) {
	loc := time.FixedZone("MSK", 3*3600)
	var msgs []bs.Message
	at := time.Date(2026, 8, 8, 12, 0, 0, 0, loc)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, bs.Message{Role: "user", Content: "x", CreatedAt: at})
		at = at.Add(90 * time.Second)
	}

	out := annotateDialogDays(msgs, at)
	if len(out) != len(msgs)+1 {
		var markers []string
		for _, m := range out {
			if s, ok := m.Content.(string); ok && strings.HasPrefix(s, "[") {
				markers = append(markers, s)
			}
		}
		t.Errorf("continuous talk got %d markers, want 1 (the day): %v", len(out)-len(msgs), markers)
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
