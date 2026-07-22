package core

import (
	"strings"
	"testing"
)

var _ TaskNotificationJournal = (*AgentTaskStore)(nil)

func TestNormalizeTaskDeliveryRefs(t *testing.T) {
	ref := TaskDeliveryRef{InputID: "notes", ItemKey: "note:1"}
	refs, err := normalizeTaskDeliveryRefs([]TaskDeliveryRef{
		{InputID: " notes ", ItemKey: " note:1 "},
		ref,
		{InputID: "", ItemKey: "ignored"},
	})
	if err != nil {
		t.Fatalf("normalizeTaskDeliveryRefs: %v", err)
	}
	if len(refs) != 1 || refs[0] != ref {
		t.Fatalf("refs = %#v, want [%#v]", refs, ref)
	}

	_, err = normalizeTaskDeliveryRefs([]TaskDeliveryRef{{
		InputID: "notes",
		ItemKey: strings.Repeat("x", TaskDeliveryItemKeyMaxBytes+1),
	}})
	if err == nil || !strings.Contains(err.Error(), "exceeds 512 bytes") {
		t.Fatalf("oversized item key error = %v", err)
	}

	_, err = normalizeTaskDeliveryRefs([]TaskDeliveryRef{{
		InputID: strings.Repeat("i", taskDeliveryInputIDMaxBytes+1),
		ItemKey: "note:1",
	}})
	if err == nil || !strings.Contains(err.Error(), "input_id exceeds 64 bytes") {
		t.Fatalf("oversized input id error = %v", err)
	}
}

func TestTaskNotificationOccurrenceKeyIsCanonicalAndUnambiguous(t *testing.T) {
	first := []TaskDeliveryRef{
		{InputID: "notes", ItemKey: "note:1"},
		{InputID: "calendar", ItemKey: "event:2"},
	}
	second := []TaskDeliveryRef{
		{InputID: "calendar", ItemKey: "event:2"},
		{InputID: "notes", ItemKey: "note:1"},
	}
	if got, want := taskNotificationOccurrenceKey(first), taskNotificationOccurrenceKey(second); got != want {
		t.Fatalf("occurrence key depends on ref order: %q != %q", got, want)
	}
	if got, partial := taskNotificationOccurrenceKey(first), taskNotificationOccurrenceKey(first[:1]); got == partial {
		t.Fatal("partially overlapping ref sets must not look like an exact idempotent occurrence")
	}

	// Length-prefixing must distinguish values that a delimiter-based hash
	// could accidentally collapse into the same byte stream.
	left := []TaskDeliveryRef{{InputID: "a", ItemKey: "bc"}}
	right := []TaskDeliveryRef{{InputID: "ab", ItemKey: "c"}}
	if taskNotificationOccurrenceKey(left) == taskNotificationOccurrenceKey(right) {
		t.Fatal("occurrence key is ambiguous across field boundaries")
	}
	if got := taskNotificationOccurrenceKey(first); !strings.HasPrefix(got, "refs:v1:") || len(got) > TaskDeliveryItemKeyMaxBytes {
		t.Fatalf("occurrence key = %q, want bounded versioned key", got)
	}
}
