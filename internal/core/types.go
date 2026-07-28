package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// Message represents a message in LLM conversation format (role + content).
//
// The extra fields below are relational metadata persisted on the
// chat_messages row but invisible to the LLM (providers serialise
// Role + Content only). They live on this struct rather than on a
// separate "session message" wrapper so the gateway can hand a
// single value to both the agent loop (LLM facing) and the session
// store (DB facing) without copy-conversion.
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []ContentBlock — normalized to []ContentBlock on storage
	// VisibleText is the exact transport-visible text for this persisted
	// message, captured before reply parents, attachment bytes, or extracted
	// document content are expanded for the provider. A non-nil empty string
	// is meaningful for attachment-only dialogue. It is persistence metadata
	// and is never serialized to a model provider.
	VisibleText *string `json:"-"`
	// ReplyToMessageID, when non-empty, marks this row as a reply to
	// the named chat_messages.id. The session store writes the
	// column on append; the cabinet's history endpoint joins the
	// parent row by it to render a relational reply-quote chip.
	// Empty for non-reply turns.
	ReplyToMessageID string `json:"-"`
	// TGMessageID is the Telegram-side message id of an inbound user message or
	// a confirmed outbound notification. Lets the gateway resolve a future
	// `msg.ReplyToMessage.MessageID` into our chat_messages.id when
	// the same chat replies to it. 0 = not from Telegram or unknown.
	TGMessageID int64 `json:"-"`
	// CreatedAt is the persistence timestamp of a stored message, populated
	// when the session store renders dialog history for the API. Never
	// serialized to providers — prompt-assembly layers use it to annotate
	// day boundaries and elapsed time. Zero for synthetic messages.
	CreatedAt time.Time `json:"-"`
}

// ContentBlock is an element of the content array in LLM API messages.
type ContentBlock struct {
	Type             string          `json:"type"`
	Text             string          `json:"text,omitempty"`
	ID               string          `json:"id,omitempty"`                // tool_use
	Name             string          `json:"name,omitempty"`              // tool_use / tool_result
	Input            json.RawMessage `json:"input,omitempty"`             // tool_use
	ThoughtSignature string          `json:"thought_signature,omitempty"` // Gemini tool_use replay
	ToolUseID        string          `json:"tool_use_id,omitempty"`       // tool_result
	Content          any             `json:"content,omitempty"`           // tool_result (string|[]ContentBlock)
	IsError          bool            `json:"is_error,omitempty"`          // tool_result
	Source           *ImageSource    `json:"source,omitempty"`            // image
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
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadTokens     int `json:"cache_read_input_tokens,omitempty"`
}

// ToolHandler processes a tool call and returns a result or error.
type ToolHandler func(ctx context.Context, input json.RawMessage) (any, error)

// ExtractText returns concatenated text from response content blocks.
func ExtractText(content []ContentBlock) string {
	var parts []string
	for _, block := range content {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

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

// EstimateTextTokens estimates prompt tokens from text conservatively.
//
// The old rune/3 heuristic badly undercounted long Hebrew / non-ASCII payloads:
// in production a ~165 KB Hebrew-heavy history estimated at ~31K tokens landed
// near ~80K actual Anthropic input tokens. For non-ASCII-heavy text, byte size is
// a better guardrail than rune count.
func EstimateTextTokens(text string) int {
	if text == "" {
		return 0
	}

	runes := 0
	nonASCII := 0
	hasHebrew := false
	for _, r := range text {
		runes++
		if r > 127 {
			nonASCII++
		}
		if r >= 0x0590 && r <= 0x05FF {
			hasHebrew = true
		}
	}

	estimate := ceilDiv(runes, 3)
	if hasHebrew || nonASCII*3 >= runes {
		estimate = maxInt(estimate, ceilDiv(len(text), 2))
		estimate = maxInt(estimate, ceilDiv(runes*5, 6)) // ~= runes / 1.2
	}
	if estimate <= 0 {
		return 1
	}
	return estimate
}

func ceilDiv(n, d int) int {
	if d <= 0 {
		return 0
	}
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

func maxInt(values ...int) int {
	max := 0
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	return max
}

// EstimateTokens estimates token count from content blocks.
func EstimateTokens(blocks []ContentBlock) int {
	total := 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			total += EstimateTextTokens(b.Text)
		case "tool_use":
			total += EstimateTextTokens(b.Name) + EstimateTextTokens(string(b.Input)) + EstimateTextTokens(b.ThoughtSignature)
		case "image":
			total += 1600
		case "tool_result":
			if s, ok := b.Content.(string); ok {
				total += EstimateTextTokens(s)
			} else {
				data, _ := json.Marshal(b.Content)
				total += EstimateTextTokens(string(data))
			}
		}
	}
	if total == 0 {
		total = 1
	}
	return total
}
