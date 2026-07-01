package session

import (
	"encoding/json"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

func TestMessagesForAPIIncludesOversizedLatestToolResultTurn(t *testing.T) {
	toolResult := storedMessage(t, "user", []bs.ContentBlock{{
		Type:      "tool_result",
		ToolUseID: "call_1",
		Content:   strings.Repeat("training log\n", 2000),
	}}, 10000)
	toolUse := storedMessage(t, "assistant", []bs.ContentBlock{{
		Type:  "tool_use",
		ID:    "call_1",
		Name:  "memory_search",
		Input: json.RawMessage(`{"query":"training logs"}`),
	}}, 20)
	userRequest := storedMessage(t, "user", []bs.ContentBlock{{
		Type: "text",
		Text: "покажи мне логи всех моих тренировок",
	}}, 30)

	apiMessages := messagesForAPIFromRows([]Message{toolResult, toolUse, userRequest}, 6000)

	if len(apiMessages) != 3 {
		t.Fatalf("expected complete tool turn, got %d messages: %#v", len(apiMessages), apiMessages)
	}
	if apiMessages[0].Role != "user" || apiMessages[1].Role != "assistant" || apiMessages[2].Role != "user" {
		t.Fatalf("unexpected roles: %#v", []string{apiMessages[0].Role, apiMessages[1].Role, apiMessages[2].Role})
	}
	if !hasAPIBlock(apiMessages[1], "tool_use", "call_1") {
		t.Fatalf("assistant tool_use was not preserved: %#v", apiMessages[1])
	}
	if !hasAPIBlock(apiMessages[2], "tool_result", "call_1") {
		t.Fatalf("user tool_result was not preserved: %#v", apiMessages[2])
	}
}

func TestMessagesForAPICompletesPartialLatestToolResultTurn(t *testing.T) {
	toolResult := storedMessage(t, "user", []bs.ContentBlock{{
		Type:      "tool_result",
		ToolUseID: "call_2",
		Content:   strings.Repeat("result\n", 1200),
	}}, 5000)
	toolUse := storedMessage(t, "assistant", []bs.ContentBlock{{
		Type:  "tool_use",
		ID:    "call_2",
		Name:  "memory_search",
		Input: json.RawMessage(`{"query":"career"}`),
	}}, 20)
	userRequest := storedMessage(t, "user", []bs.ContentBlock{{
		Type: "text",
		Text: strings.Repeat("important user request ", 120),
	}}, 2000)

	apiMessages := messagesForAPIFromRows([]Message{toolResult, toolUse, userRequest}, 5050)

	if len(apiMessages) != 3 {
		t.Fatalf("expected prior user request to be forced into the tool turn, got %d messages", len(apiMessages))
	}
	if got := bs.NormalizeContent(apiMessages[0].Content)[0].Text; !strings.Contains(got, "important user request") {
		t.Fatalf("missing original user request: %#v", apiMessages[0])
	}
}

func TestMessagesForAPINeverDropsOversizedPlainLatestMessage(t *testing.T) {
	userMessage := storedMessage(t, "user", []bs.ContentBlock{{
		Type: "text",
		Text: strings.Repeat("plain latest message ", 1200),
	}}, 10000)

	apiMessages := messagesForAPIFromRows([]Message{userMessage}, 6000)

	if len(apiMessages) != 1 {
		t.Fatalf("expected latest message to survive over-budget selection, got %d", len(apiMessages))
	}
	if apiMessages[0].Role != "user" {
		t.Fatalf("unexpected role: %s", apiMessages[0].Role)
	}
}

func TestDialogMessagesForAPISkipsToolTranscript(t *testing.T) {
	msgs := []Message{
		storedMessage(t, "user", []bs.ContentBlock{{
			Type:      "tool_result",
			ToolUseID: "call_1",
			Content:   strings.Repeat("large internal result\n", 1000),
		}}, 12000),
		storedMessage(t, "assistant", []bs.ContentBlock{{
			Type:  "tool_use",
			ID:    "call_1",
			Name:  "memory_search",
			Input: json.RawMessage(`{"query":"profile"}`),
		}}, 300),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "visible latest"}}, 100),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "visible older answer"}, {
			Type:  "tool_use",
			ID:    "call_older",
			Name:  "web_search",
			Input: json.RawMessage(`{"query":"ignored"}`),
		}}, 100),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "visible older question"}}, 100),
	}

	apiMessages := dialogMessagesForAPIFromRows(msgs, 6000)
	if len(apiMessages) != 3 {
		t.Fatalf("want only three visible dialog messages, got %d: %#v", len(apiMessages), apiMessages)
	}
	if apiMessages[0].Role != "user" || apiMessages[0].Content != "visible older question" {
		t.Fatalf("dialog should start at older visible user turn, got %#v", apiMessages[0])
	}
	blocks := bs.NormalizeContent(apiMessages[1].Content)
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "visible older answer" {
		t.Fatalf("assistant mixed tool_use should keep only visible text, got %#v", blocks)
	}
	if apiMessages[2].Content != "visible latest" {
		t.Fatalf("latest visible user missing, got %#v", apiMessages[2])
	}
}

func TestDialogMessagesForAPIDropsPartialLeadingAssistantTurn(t *testing.T) {
	msgs := []Message{
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "latest question"}}, 100),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "answer without its question"}}, 100),
	}

	apiMessages := dialogMessagesForAPIFromRows(msgs, 6000)
	if len(apiMessages) != 1 || apiMessages[0].Role != "user" {
		t.Fatalf("want leading assistant dropped, got %#v", apiMessages)
	}
}

func TestDialogMessagesForAPICompactsOlderTranslationHistory(t *testing.T) {
	longHebrew := strings.Repeat("שלום עולם ", 240)
	msgs := []Message{
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "переведи ответ на иврит: שלום"}}, 10),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "recent assistant 1"}}, 10),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "recent user 1"}}, 10),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "recent assistant 2"}}, 10),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "recent user 2"}}, 10),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "recent assistant 3"}}, 10),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "recent user 3"}}, 10),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: "recent assistant 4"}}, 10),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: longHebrew}}, 1000),
		storedMessage(t, "assistant", []bs.ContentBlock{{Type: "text", Text: longHebrew}}, 1000),
		storedMessage(t, "user", []bs.ContentBlock{{Type: "text", Text: "old user plain context"}}, 10),
	}

	apiMessages := dialogMessagesForAPIFromRows(msgs, 50000)
	rendered := renderAPIMessagesText(apiMessages)

	if !strings.Contains(rendered, "переведи ответ на иврит: שלום") {
		t.Fatalf("latest translation request should stay full, got %q", rendered)
	}
	if !strings.Contains(rendered, "[history compacted: older user turn in translation workflow") {
		t.Fatalf("older user turn should be compacted, got %q", rendered)
	}
	if !strings.Contains(rendered, "[history compacted: older assistant turn in translation workflow") {
		t.Fatalf("older assistant turn should be compacted, got %q", rendered)
	}
	if strings.Contains(rendered, strings.Repeat("שלום עולם ", 40)) {
		t.Fatalf("older Hebrew payload should not survive as full text")
	}
	if !strings.Contains(rendered, "recent assistant 4") {
		t.Fatalf("recent turns should stay full, got %q", rendered)
	}
}

func storedMessage(t *testing.T, role string, blocks []bs.ContentBlock, tokens int) Message {
	t.Helper()

	data, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return Message{
		Role:          role,
		Content:       data,
		TokenEstimate: tokens,
	}
}

func renderAPIMessagesText(msgs []bs.Message) string {
	var parts []string
	for _, msg := range msgs {
		for _, block := range bs.NormalizeContent(msg.Content) {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func hasAPIBlock(msg bs.Message, blockType string, id string) bool {
	for _, block := range bs.NormalizeContent(msg.Content) {
		if block.Type != blockType {
			continue
		}
		if block.ID == id || block.ToolUseID == id {
			return true
		}
	}
	return false
}
