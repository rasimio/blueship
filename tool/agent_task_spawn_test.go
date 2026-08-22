package tool

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestSpawnGuardBoundsDepthAndFanOut(t *testing.T) {
	parent := bs.AgentTask{ID: uuid.New()}

	depth, err := spawnGuard(parent, 0)
	if err != nil || depth != 1 {
		t.Fatalf("a chat-created task may spawn: depth=%d err=%v", depth, err)
	}

	if _, err := spawnGuard(parent, maxLiveChildren); err == nil || !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("fan-out past the live-children cap must be refused, got %v", err)
	}

	child := bs.AgentTask{ID: uuid.New(), Config: json.RawMessage(`{"spawn_depth":1,"spawned_by":"` + parent.ID.String() + `"}`)}
	if _, err := spawnGuard(child, 0); err == nil || !strings.Contains(err.Error(), "sub-task") {
		t.Fatalf("a sub-task must not spawn further tasks, got %v", err)
	}
}

func TestCountLiveChildrenIgnoresTerminalAndForeignTasks(t *testing.T) {
	parent := uuid.New()
	other := uuid.New()
	mk := func(status string, by uuid.UUID) bs.AgentTask {
		return bs.AgentTask{Status: status, Config: json.RawMessage(`{"spawned_by":"` + by.String() + `"}`)}
	}
	tasks := []bs.AgentTask{
		mk("pending", parent),
		mk("running", parent),
		mk("paused", parent),
		mk("done", parent),
		mk("failed", parent),
		mk("running", other),
		{Status: "running"}, // chat-created, no lineage
	}
	if got := countLiveChildren(tasks, parent); got != 3 {
		t.Fatalf("live children = %d, want 3", got)
	}
}

func TestWithSpawnLineageKeepsCallerConfig(t *testing.T) {
	parent := uuid.New()
	out, err := withSpawnLineage(json.RawMessage(`{"skills":["x"],"start_at":"2026-08-22T10:00:00Z"}`), parent, 1)
	if err != nil {
		t.Fatalf("withSpawnLineage: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(out, &cfg); err != nil {
		t.Fatalf("stamped config is not JSON: %v", err)
	}
	if cfg["spawned_by"] != parent.String() || cfg["spawn_depth"] != float64(1) {
		t.Fatalf("lineage not stamped: %v", cfg)
	}
	if cfg["start_at"] != "2026-08-22T10:00:00Z" || cfg["skills"] == nil {
		t.Fatalf("caller config lost: %v", cfg)
	}
	if spawnDepthOf(out) != 1 {
		t.Fatalf("spawnDepthOf round-trip = %d, want 1", spawnDepthOf(out))
	}
	if spawnDepthOf(nil) != 0 || spawnDepthOf(json.RawMessage(`not json`)) != 0 {
		t.Fatal("missing or broken config must read as depth 0")
	}
}
