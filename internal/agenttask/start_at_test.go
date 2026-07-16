package agenttask

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	core "github.com/rasimio/blueship/internal/core"
)

func taskWithConfig(t *testing.T, cfg string) core.AgentTask {
	t.Helper()
	var task core.AgentTask
	if cfg != "" {
		task.Config = json.RawMessage(cfg)
	}
	return task
}

// A start_at in the future closes the gate; the moment passing opens it.
func TestStartAtGate(t *testing.T) {
	s := &Scheduler{logger: slog.New(slog.DiscardHandler)}
	now := time.Date(2026, 7, 15, 21, 0, 0, 0, time.UTC)

	future := taskWithConfig(t, `{"start_at":"2026-07-15T21:14:00Z"}`)
	if s.startAtGateOpen(future, now) {
		t.Fatal("gate must be closed before start_at")
	}
	if !s.startAtGateOpen(future, now.Add(15*time.Minute)) {
		t.Fatal("gate must open once start_at passes")
	}

	// Absent / empty / malformed → gate open (typo must not strand a task).
	for _, cfg := range []string{"", `{}`, `{"start_at":""}`, `{"start_at":"когда-нибудь"}`} {
		if !s.startAtGateOpen(taskWithConfig(t, cfg), now) {
			t.Fatalf("gate must be open for config %q", cfg)
		}
	}
}
