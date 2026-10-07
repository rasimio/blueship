package agenttask

import (
	"strings"
	"testing"
)

func TestGroundingIncludesEveryMultiQueryReadWindow(t *testing.T) {
	body := strings.Repeat("я", 6000) + "FIRST_ATTRIBUTE" + strings.Repeat("я", 20000) + "SECOND_ATTRIBUTE" + strings.Repeat("я", 6000)
	doc := ToolOutput{Output: body, Metadata: map[string]any{"read_offset_chars": float64(6000), "read_offsets_chars": []any{float64(6000), float64(26015), float64(6000), "bad", float64(-1)}}}
	got := groundingDocumentWindow(doc, 4000)
	if !strings.Contains(got, "FIRST_ATTRIBUTE") || !strings.Contains(got, "SECOND_ATTRIBUTE") || strings.Count(got, "FIRST_ATTRIBUTE") != 1 {
		t.Fatal("multi-query evidence lost or duplicated", got)
	}
	if len(got) > 4300 {
		t.Fatal("window budget exceeded apart from delimiters", len(got))
	}
	doc.Metadata["read_offsets_chars"] = []any{"bad", float64(-1)}
	if got := groundingDocumentWindow(doc, 4000); !strings.Contains(got, "FIRST_ATTRIBUTE") {
		t.Fatal("legacy offset fallback lost", got)
	}
}
