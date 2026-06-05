package openaicodex

import (
	"encoding/json"
	"strings"
	"testing"

	bs "github.com/rasimio/blueship/internal/core"
)

// TestBuildUserInputImage locks the fix: an attached image block must
// serialize into a Responses API input_image data URL alongside the
// user's text, in order. The original bug was buildUserInput having no
// "image" case, so the bytes vanished before the model saw them.
func TestBuildUserInputImage(t *testing.T) {
	blocks := []bs.ContentBlock{
		{Type: "text", Text: "разбери этот скрин"},
		{Type: "image", Source: &bs.ImageSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}},
	}

	items := buildUserInput(blocks)
	if len(items) != 1 {
		t.Fatalf("want 1 input item, got %d", len(items))
	}
	msg, ok := items[0].(inputMessage)
	if !ok {
		t.Fatalf("item 0 is %T, want inputMessage", items[0])
	}
	if len(msg.Content) != 2 {
		t.Fatalf("want 2 content parts (text+image), got %d", len(msg.Content))
	}

	txt, ok := msg.Content[0].(inputTextContent)
	if !ok || txt.Type != "input_text" || txt.Text != "разбери этот скрин" {
		t.Fatalf("content[0] not the text item: %#v", msg.Content[0])
	}
	img, ok := msg.Content[1].(inputImageContent)
	if !ok {
		t.Fatalf("content[1] is %T, want inputImageContent", msg.Content[1])
	}
	if img.Type != "input_image" {
		t.Fatalf("image type = %q, want input_image", img.Type)
	}
	if want := "data:image/png;base64,AAAA"; img.ImageURL != want {
		t.Fatalf("image_url = %q, want %q", img.ImageURL, want)
	}

	// The bytes must survive marshaling — that's the whole point.
	raw, _ := json.Marshal(items)
	if !strings.Contains(string(raw), `"type":"input_image"`) ||
		!strings.Contains(string(raw), "data:image/png;base64,AAAA") {
		t.Fatalf("marshaled input missing image item: %s", raw)
	}
}

// TestBuildUserInputImageDefaultMime falls back to image/jpeg when the
// source omits a media type, mirroring the Anthropic vision path.
func TestBuildUserInputImageDefaultMime(t *testing.T) {
	items := buildUserInput([]bs.ContentBlock{
		{Type: "image", Source: &bs.ImageSource{Data: "QkJCQg=="}},
	})
	msg := items[0].(inputMessage)
	img := msg.Content[0].(inputImageContent)
	if !strings.HasPrefix(img.ImageURL, "data:image/jpeg;base64,") {
		t.Fatalf("default mime not applied: %q", img.ImageURL)
	}
}

// TestBuildUserInputSkipsEmptyImage drops nil/empty sources rather than
// emitting a malformed input_image the backend would reject.
func TestBuildUserInputSkipsEmptyImage(t *testing.T) {
	items := buildUserInput([]bs.ContentBlock{
		{Type: "text", Text: "hi"},
		{Type: "image", Source: nil},
		{Type: "image", Source: &bs.ImageSource{MediaType: "image/png", Data: ""}},
	})
	if len(items) != 1 {
		t.Fatalf("want 1 item, got %d", len(items))
	}
	if msg := items[0].(inputMessage); len(msg.Content) != 1 {
		t.Fatalf("empty image sources should be skipped; content = %d", len(msg.Content))
	}
}

// TestBuildUserInputImageThenToolResult preserves wire order: pending
// user content (text+image) flushes as one message before the following
// function_call_output top-level item.
func TestBuildUserInputImageThenToolResult(t *testing.T) {
	items := buildUserInput([]bs.ContentBlock{
		{Type: "text", Text: "look"},
		{Type: "image", Source: &bs.ImageSource{MediaType: "image/png", Data: "AAAA"}},
		{Type: "tool_result", ToolUseID: "call_1", Content: "ok"},
	})
	if len(items) != 2 {
		t.Fatalf("want 2 items (message, function_call_output), got %d", len(items))
	}
	if _, ok := items[0].(inputMessage); !ok {
		t.Fatalf("item 0 is %T, want inputMessage", items[0])
	}
	out, ok := items[1].(inputFunctionCallOutput)
	if !ok || out.CallID != "call_1" {
		t.Fatalf("item 1 not the function_call_output: %#v", items[1])
	}
}
