package agent

import (
	"fmt"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// Everything dynamic about a turn — memory traces, reflex guidance, felt time,
// the clock — rides in the turn context, and providers cache by prefix. So the
// one property that has to hold is that the turn context cannot change anything
// ahead of itself: swap it for something completely different and every earlier
// message must come out byte-identical.
//
// This is checkable and was not checked. Move the anchor to the head — a
// one-token edit, and the intuitive place to put context someone wants the model
// to read first — and the daemon still works, tests still pass, and every turn
// silently re-uploads the entire dialogue at full price. Production measured
// 15.6% cache hit rate when the clock led the system prompt, against 92-95%
// within an interaction once it moved to the tail.
func TestTurnContextCannotInvalidateAnythingAheadOfIt(t *testing.T) {
	base := []bs.Message{
		{Role: "user", Content: "первое"},
		{Role: "assistant", Content: "ответ"},
		{Role: "user", Content: "второе"},
		{Role: "assistant", Content: "ещё ответ"},
		{Role: "user", Content: "последнее"},
	}

	render := func(turnContext string) []bs.Message {
		msgs := make([]bs.Message, len(base))
		copy(msgs, base)
		return withTurnContext(msgs, turnContextAnchor(msgs), turnContext)
	}

	a := render("[memory]\nтрасса A\n[/memory]")
	b := render("[memory]\nсовершенно другое содержимое, и длиннее\n[/memory]\n[felt_time]\nNow: …\n[/felt_time]")

	if len(a) != len(b) {
		t.Fatalf("the two renders differ in length (%d vs %d), so the array shape depends on the turn context", len(a), len(b))
	}

	// Every message before the last must be identical. The last one is where the
	// context is allowed to land.
	for i := 0; i < len(a)-1; i++ {
		if fmt.Sprint(a[i].Content) != fmt.Sprint(b[i].Content) {
			t.Errorf("message %d changed with the turn context, which invalidates the cached prefix from there on:\n A: %v\n B: %v",
				i, a[i].Content, b[i].Content)
		}
		if a[i].Role != b[i].Role {
			t.Errorf("message %d changed role with the turn context", i)
		}
	}

	// And the context has to actually be present, or this test would pass on a
	// function that does nothing.
	last := fmt.Sprint(a[len(a)-1].Content)
	if !contains(last, "трасса A") {
		t.Errorf("the turn context is not in the final message: %q", last)
	}
}

// The anchor points at the last message so the context lands at the tail. Any
// earlier index puts dynamic bytes in front of dialogue that would otherwise be
// cached, and an index inside a tool exchange could wedge between a call and its
// result.
func TestTurnContextAnchorIsTheLastMessage(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		msgs := make([]bs.Message, n)
		for i := range msgs {
			msgs[i] = bs.Message{Role: "user", Content: fmt.Sprintf("m%d", i)}
		}
		if got := turnContextAnchor(msgs); got != n-1 {
			t.Errorf("turnContextAnchor(%d messages) = %d, want %d (the last): anything earlier puts per-turn bytes ahead of cacheable dialogue", n, got, n-1)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
