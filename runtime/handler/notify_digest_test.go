package handler

import (
	"strings"
	"testing"

	"github.com/rasimio/blueship/runtime/agent"
)

func TestNotifyDigestExtractsTLDR(t *testing.T) {
	report := `### 1. TL;DR

Сообщение с погодой отправлено. 32°C, дождь, гроза во второй половине дня.

### 2. Body

Огромная простыня с шестью секциями и references...`
	got := notifyDigest(report)
	if !strings.Contains(got, "32°C") || strings.Contains(got, "простыня") {
		t.Fatalf("digest must keep TL;DR essence and drop the body, got: %q", got)
	}
	if !strings.Contains(got, "артефактах") {
		t.Fatalf("digest must point at the artefacts, got: %q", got)
	}
}

func TestNotifyDigestShortTextUntouched(t *testing.T) {
	short := "Погода: 32°C, дождь. Источник: accuweather.com"
	if got := notifyDigest(short); got != short {
		t.Fatalf("short notify must pass through, got: %q", got)
	}
}

func TestIterationSentMessage(t *testing.T) {
	ok := []agent.ToolTrace{{Name: "browser_fetch"}, {Name: "message_send", Error: false}}
	failed := []agent.ToolTrace{{Name: "message_send", Error: true}}
	if !iterationSentMessage(ok) {
		t.Fatal("successful send must suppress notify")
	}
	if iterationSentMessage(failed) {
		t.Fatal("failed send must NOT suppress notify")
	}
}
