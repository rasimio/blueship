package core

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestDefinitelyNotSentMarker(t *testing.T) {
	sentinel := errors.New("sender unavailable")
	marked := DefinitelyNotSent(sentinel)
	if !IsDefinitelyNotSent(marked) || !errors.Is(marked, sentinel) {
		t.Fatalf("marked error lost type or cause: %v", marked)
	}
	if wrapped := fmt.Errorf("notify: %w", marked); !IsDefinitelyNotSent(wrapped) {
		t.Fatalf("marker did not survive wrapping: %v", wrapped)
	}
	if got := DefinitelyNotSent(marked); got != marked {
		t.Fatal("marking an already-marked error should be idempotent")
	}
	if DefinitelyNotSent(nil) != nil || IsDefinitelyNotSent(nil) {
		t.Fatal("nil must remain an unmarked nil error")
	}

	permanent := PermanentlyNotSent(sentinel)
	if !IsPermanentlyNotSent(permanent) || !IsDefinitelyNotSent(permanent) || !errors.Is(permanent, sentinel) {
		t.Fatalf("permanent marker lost type hierarchy or cause: %v", permanent)
	}
	if got := PermanentlyNotSent(permanent); got != permanent {
		t.Fatal("marking an already-permanent error should be idempotent")
	}
	if got := DefinitelyNotSent(permanent); got != permanent {
		t.Fatal("permanent error must already satisfy DefinitelyNotSent")
	}
	if PermanentlyNotSent(nil) != nil || IsPermanentlyNotSent(nil) {
		t.Fatal("nil must remain an unmarked nil permanent error")
	}
}

func TestDefinitelyNotSentAfter(t *testing.T) {
	sentinel := errors.New("rate limited")
	delay := 37 * time.Second
	marked := DefinitelyNotSentAfter(sentinel, delay)
	if !IsDefinitelyNotSent(marked) || IsPermanentlyNotSent(marked) || !errors.Is(marked, sentinel) {
		t.Fatalf("delayed marker lost type or cause: %v", marked)
	}
	if got, ok := NotificationRetryDelay(fmt.Errorf("notify: %w", marked)); !ok || got != delay {
		t.Fatalf("retry delay = %v, %v; want %v, true", got, ok, delay)
	}

	updated := DefinitelyNotSentAfter(marked, 2*delay)
	if got, ok := NotificationRetryDelay(updated); !ok || got != 2*delay {
		t.Fatalf("updated retry delay = %v, %v; want %v, true", got, ok, 2*delay)
	}
	permanent := PermanentlyNotSent(marked)
	if got, ok := NotificationRetryDelay(permanent); ok || got != 0 {
		t.Fatalf("permanent retry delay = %v, %v; want 0, false", got, ok)
	}
	if got := DefinitelyNotSentAfter(permanent, delay); got != permanent {
		t.Fatal("delayed marker must not weaken an existing permanent rejection")
	}
	if DefinitelyNotSentAfter(nil, delay) != nil {
		t.Fatal("nil must remain nil")
	}
}
