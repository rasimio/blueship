package core

import (
	"encoding/json"
	"testing"
)

func TestProjectLegacyMessageProjectsUnambiguousText(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"первая строка"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"SECRET_BYTES"}},
		{"type":"text","text":"вторая строка"}
	]`)

	got := ProjectLegacyMessage("user", raw)

	if got.Status != ProjectionProjected {
		t.Fatalf("status = %q, want %q (reason=%q)", got.Status, ProjectionProjected, got.Reason)
	}
	if got.VisibleText == nil || *got.VisibleText != "первая строка\n\nвторая строка" {
		t.Fatalf("visible text = %#v", got.VisibleText)
	}
	if got.ProjectorVersion != LegacyMessageProjectorVersion {
		t.Fatalf("projector version = %q", got.ProjectorVersion)
	}
}

func TestProjectLegacyMessageRefusesProviderExpandedText(t *testing.T) {
	raw := json.RawMessage(`[{"type":"text","text":"[reply to: old private text]\n\nмой ответ"}]`)

	got := ProjectLegacyMessage("user", raw)

	if got.Status != ProjectionUnprojectableLegacy {
		t.Fatalf("status = %q, want %q", got.Status, ProjectionUnprojectableLegacy)
	}
	if got.VisibleText != nil {
		t.Fatalf("unsafe text leaked into projection: %q", *got.VisibleText)
	}
	if got.Reason != "provider_expanded_text" {
		t.Fatalf("reason = %q", got.Reason)
	}
}

func TestProjectLegacyMessageClassifiesToolTranscriptAsNonDialogue(t *testing.T) {
	raw := json.RawMessage(`[{
		"type":"tool_result",
		"tool_use_id":"call_1",
		"content":"large private tool output"
	}]`)

	got := ProjectLegacyMessage("user", raw)

	if got.Status != ProjectionNonDialogue {
		t.Fatalf("status = %q, want %q", got.Status, ProjectionNonDialogue)
	}
	if got.VisibleText != nil {
		t.Fatal("tool transcript must not have visible text")
	}
}

func TestProjectLegacyMessageDoesNotLeakMixedToolResultText(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"adapter note"},
		{"type":"tool_result","tool_use_id":"call_1","content":"private output"}
	]`)

	got := ProjectLegacyMessage("user", raw)

	if got.Status != ProjectionNonDialogue || got.VisibleText != nil {
		t.Fatalf("projection = %#v", got)
	}
}

func TestProjectLegacyMessageRefusesMalformedContent(t *testing.T) {
	got := ProjectLegacyMessage("assistant", json.RawMessage(`{"broken":`))

	if got.Status != ProjectionUnprojectableLegacy || got.Reason != "malformed_content" {
		t.Fatalf("projection = %#v", got)
	}
}

func TestProjectLegacyMessageDoesNotCallEmptyHumanRowNonDialogue(t *testing.T) {
	got := ProjectLegacyMessage("user", json.RawMessage(`[]`))

	if got.Status != ProjectionUnprojectableLegacy || got.Reason != "empty_content" {
		t.Fatalf("projection = %#v", got)
	}
}

func TestProjectMessageForWritePrefersExactTransportText(t *testing.T) {
	transportText := "мой ответ"
	blocks := []ContentBlock{{
		Type: "text",
		Text: "[reply to: a very large parent]\n\nмой ответ",
	}, {
		Type:   "image",
		Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "SECRET_BYTES"},
	}}

	got := ProjectMessageForWrite(Message{
		Role:        "user",
		Content:     blocks,
		VisibleText: &transportText,
	}, blocks)

	if got.Status != ProjectionProjected {
		t.Fatalf("status = %q, reason=%q", got.Status, got.Reason)
	}
	if got.VisibleText == nil || *got.VisibleText != transportText {
		t.Fatalf("visible text = %#v", got.VisibleText)
	}
	if got.ProjectorVersion != CanonicalMessageProjectorVersion {
		t.Fatalf("projector version = %q", got.ProjectorVersion)
	}
}

func TestProjectMessageForWritePreservesExactVisibleWhitespace(t *testing.T) {
	visible := "  ответ с авторским отступом\n"
	blocks := []ContentBlock{{Type: "text", Text: "ответ с авторским отступом"}}

	got := ProjectMessageForWrite(Message{
		Role:        "assistant",
		Content:     blocks,
		VisibleText: &visible,
	}, blocks)

	if got.VisibleText == nil || *got.VisibleText != visible {
		t.Fatalf("visible text = %#v, want exact %q", got.VisibleText, visible)
	}
}

func TestProjectMessageForWriteSanitizesAssistantProjectionWithoutMutatingRawContent(t *testing.T) {
	const marker = "11111111-1111-1111-1111-111111111111"
	raw := "<html><body><channel|>thought\n" +
		"call:memory_search{\"query\":\"birthday\"}|>\nДа, 25-го.</body></html>\n\n" +
		"[attached: " + marker + "]"
	visible := raw
	blocks := []ContentBlock{{Type: "text", Text: raw}}

	got := ProjectMessageForWrite(Message{
		Role:        "assistant",
		Content:     blocks,
		VisibleText: &visible,
	}, blocks)

	if got.VisibleText == nil {
		t.Fatal("visible text is nil")
	}
	if *got.VisibleText != "Да, 25-го." {
		t.Fatalf("visible text = %q, want sanitized human text", *got.VisibleText)
	}
	if blocks[0].Text != raw {
		t.Fatalf("raw content mutated: got %q, want %q", blocks[0].Text, raw)
	}
	if visible != raw {
		t.Fatalf("caller-visible input mutated: got %q, want %q", visible, raw)
	}
}

func TestSanitizeLeakedAssistantTextKeepsGatewaySemantics(t *testing.T) {
	got := SanitizeLeakedAssistantText(
		"  <tool_call>{\"name\":\"memory_search\"}</tool_call>\n<br>thought\nответ</html>  ",
	)
	if got != "ответ" {
		t.Fatalf("sanitized text = %q, want %q", got, "ответ")
	}
}

func TestProjectMessageForWriteAllowsExplicitEmptyAttachmentTurn(t *testing.T) {
	empty := ""
	blocks := []ContentBlock{{
		Type:   "image",
		Source: &ImageSource{Type: "base64", MediaType: "image/png", Data: "SECRET_BYTES"},
	}}

	got := ProjectMessageForWrite(Message{
		Role:        "user",
		Content:     blocks,
		VisibleText: &empty,
	}, blocks)

	if got.Status != ProjectionProjected {
		t.Fatalf("status = %q", got.Status)
	}
	if got.VisibleText == nil || *got.VisibleText != "" {
		t.Fatalf("visible text = %#v", got.VisibleText)
	}
}

func TestProjectMessageForWriteProjectsAssistantVisibleText(t *testing.T) {
	blocks := []ContentBlock{
		{Type: "text", Text: "Сначала проверю."},
		{Type: "tool_use", ID: "call_1", Name: "chat_recall"},
	}

	got := ProjectMessageForWrite(Message{Role: "assistant", Content: blocks}, blocks)

	if got.Status != ProjectionProjected {
		t.Fatalf("status = %q, reason=%q", got.Status, got.Reason)
	}
	if got.VisibleText == nil || *got.VisibleText != "Сначала проверю." {
		t.Fatalf("visible text = %#v", got.VisibleText)
	}
	if got.ProjectorVersion != CanonicalMessageProjectorVersion {
		t.Fatalf("projector version = %q", got.ProjectorVersion)
	}
}

func TestProjectMessageForWriteClassifiesToolOnlyAssistantAsNonDialogue(t *testing.T) {
	blocks := []ContentBlock{{
		Type: "tool_use",
		ID:   "call_1",
		Name: "chat_recall",
	}}

	got := ProjectMessageForWrite(Message{Role: "assistant", Content: blocks}, blocks)

	if got.Status != ProjectionNonDialogue || got.VisibleText != nil {
		t.Fatalf("projection = %#v", got)
	}
}
