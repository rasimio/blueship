package agent

import (
	"testing"
	"time"
)

// The default cap sits inside image generation's healthy latency range —
// production runs 54-85s on an ordinary day — so falling back to it cancels
// real generations whenever the backend slows at all. One afternoon produced
// three 90s cancellations, each of an image already paid for. This pins the
// tool to a cap outside its own tail.
func TestImageGenerationOutlivesTheDefaultToolTimeout(t *testing.T) {
	got := resolveToolExecutionTimeout(0, "image_generate")
	if got <= defaultToolExecutionTimeout {
		t.Fatalf("image_generate resolves to %v, within the default %v that was cancelling healthy generations", got, defaultToolExecutionTimeout)
	}
	// Above 85s observed tail with real headroom, but still bounded: a hung
	// stream must not hold a chat turn open indefinitely.
	if got < 3*time.Minute || got > 10*time.Minute {
		t.Fatalf("image_generate timeout %v is outside the sane band", got)
	}
}

// An explicit per-turn override still beats every per-tool default; the
// special case must not grow priority over configuration.
func TestExplicitOverrideBeatsPerToolTimeouts(t *testing.T) {
	if got := resolveToolExecutionTimeout(7*time.Second, "image_generate"); got != 7*time.Second {
		t.Fatalf("override ignored: %v", got)
	}
}
