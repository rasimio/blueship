package core

import (
	"strings"
	"testing"
)

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
}
