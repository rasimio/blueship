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
