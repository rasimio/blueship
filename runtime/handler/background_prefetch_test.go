package handler

import (
	"strings"
	"testing"
)

func TestToolResultIsEmpty(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty array", raw: `[]`, want: true},
		{name: "empty wrapper notes", raw: `{"notes":[]}`, want: true},
		{name: "empty wrapper data", raw: `{"data":[]}`, want: true},
		{name: "nonempty array", raw: `[{"id":"n1"}]`, want: false},
		{name: "nonempty wrapper", raw: `{"notes":[{"id":"n1"}]}`, want: false},
		{name: "plain error text", raw: `something`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolResultIsEmpty(tt.raw); got != tt.want {
				t.Fatalf("toolResultIsEmpty(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestHourInRange(t *testing.T) {
	if !hourInRange(1, 0, 8) {
		t.Fatal("1 should be inside 0..8")
	}
	if hourInRange(9, 0, 8) {
		t.Fatal("9 should be outside 0..8")
	}
	if !hourInRange(23, 22, 6) || !hourInRange(2, 22, 6) {
		t.Fatal("wrapped 22..6 range should include 23 and 2")
	}
	if hourInRange(12, 22, 6) {
		t.Fatal("wrapped 22..6 range should exclude 12")
	}
}

func TestFormatBackgroundCycleHeaderFramesAutonomy(t *testing.T) {
	got := formatBackgroundCycleHeader("Periodic heartbeat check", "deadline scan")
	if strings.Contains(got, "[TASK:") || strings.Contains(got, "Mission:") {
		t.Fatalf("header still uses task-assignment framing:\n%s", got)
	}
	for _, want := range []string{
		"[Autonomous background cycle]",
		"Standing intention: Periodic heartbeat check",
		"Context: deadline scan",
		"your own background process",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("header missing %q:\n%s", want, got)
		}
	}
}

func TestStripBackgroundStatusTails(t *testing.T) {
	in := "Расим, время подкатывается к письму про Fable/Codex.\n\nГотово, напомнила."
	want := "Расим, время подкатывается к письму про Fable/Codex."
	if got := stripBackgroundStatusTails(in); got != want {
		t.Fatalf("stripBackgroundStatusTails() = %q, want %q", got, want)
	}

	onlyStatus := "Готово, напомнила."
	if got := stripBackgroundStatusTails(onlyStatus); got != onlyStatus {
		t.Fatalf("single-line status should be preserved, got %q", got)
	}
}
