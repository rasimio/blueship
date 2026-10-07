package agenttask

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestGroundingWindowIncludesActuallyReadLatePassage(t *testing.T) {
	prefix := strings.Repeat("Вступление. ", 10000)
	text := prefix + "NOWAIT fails immediately; SKIP LOCKED skips locked rows." + strings.Repeat("Ending. ", 3000)
	window := groundingReadWindow(text, 4000, utf8.RuneCountInString(prefix))
	if !strings.Contains(window, "NOWAIT fails immediately") || !strings.Contains(window, "excerpt begins") || !utf8.ValidString(window) || len(window) > 4200 {
		t.Fatal("relevant evidence omitted or unbounded", len(window))
	}
	if got := groundingReadWindow("short", 4000, 0); got != "short" {
		t.Fatal(got)
	}
	message, _ := buildGroundingUserMessage("NOWAIT: https://example.com/docs", []ToolOutput{{Output: text, Metadata: map[string]any{"requested_url": "https://example.com/docs", "read_offset_chars": float64(utf8.RuneCountInString(prefix))}}})
	if !strings.Contains(message, "NOWAIT fails immediately; SKIP LOCKED skips locked rows.") {
		t.Fatal("auditor lost saved read window")
	}
}
