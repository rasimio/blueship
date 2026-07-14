package gateway

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTelegramPreviewTextCapsUnicodeAndKeepsToolStatus(t *testing.T) {
	status := ">> browser_fetch..."
	preview := telegramPreviewText(strings.Repeat("я", 5000), status)
	if n := utf8.RuneCountInString(preview); n != telegramPreviewMaxRunes {
		t.Fatalf("preview runes = %d, want %d", n, telegramPreviewMaxRunes)
	}
	if !strings.HasSuffix(preview, "`"+status+"`") {
		t.Fatalf("preview lost tool status: %q", preview[len(preview)-40:])
	}
}

func TestTelegramPreviewTextWithToolOnly(t *testing.T) {
	if got := telegramPreviewText("", ">> notes..."); got != "`>> notes...`" {
		t.Fatalf("tool-only preview = %q", got)
	}
}
