package gateway

import (
	"context"
	"testing"
	"time"
)

func TestDrainGuardWaitsForTurnsInFlight(t *testing.T) {
	var d drainGuard
	end := d.begin()
	if d.inFlight() != 1 {
		t.Fatalf("in flight = %d, want 1", d.inFlight())
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		end()
	}()
	if left := d.wait(2 * time.Second); left != 0 {
		t.Fatalf("drain gave up with %d turns running", left)
	}
	end() // idempotent: a turn that ends twice is still one turn
	if d.inFlight() != 0 {
		t.Fatalf("in flight after double end = %d", d.inFlight())
	}
}

func TestDrainGuardGivesUpAtTheDeadline(t *testing.T) {
	var d drainGuard
	end := d.begin()
	defer end()
	start := time.Now()
	left := d.wait(40 * time.Millisecond)
	if left != 1 {
		t.Fatalf("expected one abandoned turn, got %d", left)
	}
	if time.Since(start) < 40*time.Millisecond {
		t.Fatal("returned before the deadline")
	}
}

func TestDrainGuardZeroTimeoutDoesNotWait(t *testing.T) {
	var d drainGuard
	end := d.begin()
	defer end()
	start := time.Now()
	if left := d.wait(0); left != 1 {
		t.Fatalf("left = %d, want 1", left)
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("a zero timeout must return immediately")
	}
}

func TestTurnContextSurvivesRootCancelOnlyWhenDraining(t *testing.T) {
	root, cancel := context.WithCancel(context.WithValue(context.Background(), struct{}{}, "kept"))
	off := turnContext(root, 0)
	on := turnContext(root, time.Second)
	cancel()
	if off.Err() == nil {
		t.Fatal("with drain off the turn must die with the root")
	}
	if on.Err() != nil {
		t.Fatal("with drain on the turn must outlive the root")
	}
	if on.Value(struct{}{}) != "kept" {
		t.Fatal("values must survive the detachment")
	}
}
