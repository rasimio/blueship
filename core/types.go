package core

import (
	"context"
	"encoding/json"
)

// Message represents a message in LLM conversation format (role + content).
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []ContentBlock — normalized to []ContentBlock on storage
}

// ContentBlock is an element of the content array in LLM API messages.
type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use
	Name      string          `json:"name,omitempty"`        // tool_use
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result
	Content   any             `json:"content,omitempty"`     // tool_result (string|[]ContentBlock)
	IsError   bool            `json:"is_error,omitempty"`    // tool_result
	Source    *ImageSource    `json:"source,omitempty"`      // image
}

// ImageSource holds base64-encoded image data for vision API.
type ImageSource struct {
	Type      string `json:"type"`       // "base64"
	MediaType string `json:"media_type"` // "image/jpeg"
	Data      string `json:"data"`       // base64-encoded
}

// ToolDefinition describes a tool available to an LLM.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Usage contains token usage information from an LLM API response.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ToolHandler processes a tool call and returns a result or error.
type ToolHandler func(ctx context.Context, input json.RawMessage) (any, error)

// NormalizeContent converts content to the canonical []ContentBlock format.
// string → []{type:"text", text:s}; []ContentBlock → as-is; already JSON array → decode.
func NormalizeContent(content any) []ContentBlock {
	if content == nil {
		return []ContentBlock{}
	}

	switch v := content.(type) {
	case string:
		return []ContentBlock{{Type: "text", Text: v}}

	case []ContentBlock:
		return v

	case []any:
		data, err := json.Marshal(v)
		if err != nil {
			return []ContentBlock{{Type: "text", Text: "marshal error"}}
		}
		var blocks []ContentBlock
		if err := json.Unmarshal(data, &blocks); err != nil {
			return []ContentBlock{{Type: "text", Text: string(data)}}
		}
		return blocks

	default:
		data, err := json.Marshal(v)
		if err != nil {
			return []ContentBlock{{Type: "text", Text: "unknown content type"}}
		}

		var blocks []ContentBlock
		if err := json.Unmarshal(data, &blocks); err == nil && len(blocks) > 0 {
			return blocks
		}

		var s string
		if err := json.Unmarshal(data, &s); err == nil {
			return []ContentBlock{{Type: "text", Text: s}}
		}

		return []ContentBlock{{Type: "text", Text: string(data)}}
	}
}

// EstimateTokens estimates token count from content blocks.
// Uses ~3 chars per token as a compromise for mixed Latin/Cyrillic text.
func EstimateTokens(blocks []ContentBlock) int {
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			total += len([]rune(b.Text)) / 3
		case "tool_use":
			total += len([]rune(b.Name))/3 + len(b.Input)/3
		case "image":
			total += 1600
		case "tool_result":
			if s, ok := b.Content.(string); ok {
				total += len([]rune(s)) / 3
			} else {
				data, _ := json.Marshal(b.Content)
				total += len(data) / 3
			}
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}
