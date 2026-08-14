package core

import (
	"encoding/json"
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

// TestJSONBObjectNeverYieldsNull guards the driver-level trap behind the
// scheduler outage of 2026-08-14: a nil json.RawMessage reaches lib/pq as SQL
// NULL and an empty one as an invalid jsonb literal, and the first of those
// poisons the column for every later `SELECT *` reader.
func TestJSONBObjectNeverYieldsNull(t *testing.T) {
	for _, empty := range []json.RawMessage{nil, {}, json.RawMessage("   ")} {
		if got := jsonbObject(empty); string(got) != "{}" {
			t.Fatalf("jsonbObject(%q) = %q, want {}", empty, got)
		}
	}
	// A real checkpoint passes through untouched — including the JSON null
	// literal, which is a value the column can hold and read back.
	for _, kept := range []json.RawMessage{
		json.RawMessage(`{"phase":"iteration_2"}`),
		json.RawMessage(`null`),
	} {
		if got := jsonbObject(kept); string(got) != string(kept) {
			t.Fatalf("jsonbObject(%q) = %q, want it unchanged", kept, got)
		}
	}
}
