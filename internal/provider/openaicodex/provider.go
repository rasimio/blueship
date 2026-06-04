package openaicodex

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

const responsesURL = "https://chatgpt.com/backend-api/codex/responses"

// CompletionProvider implements bs.CompletionProvider using OpenAI Responses API
// via the ChatGPT backend (OAuth subscription).
type CompletionProvider struct {
	tokens     *TokenStore
	httpClient *http.Client
	logger     *slog.Logger
	backoffs   []time.Duration
}

// NewCompletionProvider creates a provider using OAuth tokens.
func NewCompletionProvider(tokens *TokenStore, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) *CompletionProvider {
	return &CompletionProvider{
		tokens:     tokens,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
		backoffs:   backoffs,
	}
}

// Complete sends a completion request. Codex requires streaming, so this
// delegates to StreamComplete with nil callbacks.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	return p.StreamComplete(ctx, req, nil)
}

// --- Request types ---

type responsesRequest struct {
	Model        string           `json:"model"`
	Instructions string           `json:"instructions"`
	Input        []any            `json:"input"`
	Stream       bool             `json:"stream"`
	Store        bool             `json:"store"`
	Reasoning    *reasoningConfig `json:"reasoning,omitempty"`
	Tools        []responseTool   `json:"tools,omitempty"`
}

// reasoningConfig maps to the Codex Responses API reasoning object. Only
// effort is wired today; summary/verbosity are left at backend defaults.
type reasoningConfig struct {
	Effort string `json:"effort"`
}

type responseTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Input item types for the Responses API.

type inputTextContent struct {
	Type string `json:"type"` // "input_text"
	Text string `json:"text"`
}

type inputMessage struct {
	Role    string             `json:"role"`
	Content []inputTextContent `json:"content"`
}

type outputTextContent struct {
	Type        string `json:"type"` // "output_text"
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type inputAssistantMessage struct {
	Type    string              `json:"type"` // "message"
	Role    string              `json:"role"` // "assistant"
	ID      string              `json:"id"`
	Content []outputTextContent `json:"content"`
	Status  string              `json:"status"` // "completed"
}

type inputFunctionCall struct {
	Type      string `json:"type"` // "function_call"
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type inputFunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// --- Response types ---

type outputItem struct {
	Type      string              `json:"type"` // "message", "function_call", "reasoning"
	ID        string              `json:"id"`
	Role      string              `json:"role,omitempty"`
	Content   []outputTextContent `json:"content,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
	Name      string              `json:"name,omitempty"`
	Arguments string              `json:"arguments,omitempty"`
	Status    string              `json:"status,omitempty"`
}

// --- Conversion ---

func buildRequest(req bs.CompletionRequest) responsesRequest {
	var input []any
	var contextBlocks []string

	for _, msg := range req.Messages {
		blocks := bs.NormalizeContent(msg.Content)
		switch msg.Role {
		case "user":
			// Extract [context]...[/context] blocks into instructions.
			var filtered []bs.ContentBlock
			for _, b := range blocks {
				if b.Type == "text" && strings.HasPrefix(b.Text, "[context]\n") {
					contextBlocks = append(contextBlocks, b.Text)
				} else {
					filtered = append(filtered, b)
				}
			}
			items := buildUserInput(filtered)
			input = append(input, items...)
		case "assistant":
			items := buildAssistantInput(blocks)
			input = append(input, items...)
		}
	}

	// Append extracted context to instructions so the model treats it
	// as authoritative system-level information, not user text.
	instructions := req.System
	if len(contextBlocks) > 0 {
		instructions += "\n\n" + strings.Join(contextBlocks, "\n\n")
	}

	return responsesRequest{
		Model:        req.Model,
		Instructions: instructions,
		Input:        input,
		Stream:       true,
		Store:        false,
		Reasoning:    codexReasoning(req.Effort),
		Tools:        buildTools(req.Tools),
	}
}

// codexReasoning maps the generic CompletionRequest.Effort onto the Codex
// Responses API reasoning.effort field. gpt-5.5 on the Codex backend accepts
// exactly: none|low|medium|high|xhigh (verified live — it rejects gpt-5's
// "minimal" with a 400). "none" is the instant, no-reasoning tier; "xhigh" is
// the deepest. The Anthropic-flavoured "minimal"/"max" aliases collapse to the
// nearest supported tier so a shared model_config.effort works across
// providers. An empty or unrecognised effort returns nil so the request omits
// the field entirely and the backend applies its default — preserving the
// pre-existing behaviour for callers that don't set Effort.
func codexReasoning(effort string) *reasoningConfig {
	switch e := strings.ToLower(strings.TrimSpace(effort)); e {
	case "none", "low", "medium", "high", "xhigh":
		return &reasoningConfig{Effort: e}
	case "minimal":
		return &reasoningConfig{Effort: "none"}
	case "max":
		return &reasoningConfig{Effort: "xhigh"}
	default:
		return nil
	}
}

func buildUserInput(blocks []bs.ContentBlock) []any {
	var items []any
	var textParts []string

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_result":
			if len(textParts) > 0 {
				items = append(items, inputMessage{
					Role:    "user",
					Content: []inputTextContent{{Type: "input_text", Text: strings.Join(textParts, "\n")}},
				})
				textParts = nil
			}
			output := stringifyContent(b.Content)
			items = append(items, inputFunctionCallOutput{
				Type:   "function_call_output",
				CallID: b.ToolUseID,
				Output: output,
			})
		}
	}

	if len(textParts) > 0 {
		items = append(items, inputMessage{
			Role:    "user",
			Content: []inputTextContent{{Type: "input_text", Text: strings.Join(textParts, "\n")}},
		})
	}
	return items
}

func buildAssistantInput(blocks []bs.ContentBlock) []any {
	var items []any
	var textParts []string

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_use":
			if len(textParts) > 0 {
				items = append(items, inputAssistantMessage{
					Type: "message",
					Role: "assistant",
					ID:   "msg_text",
					Content: []outputTextContent{{
						Type:        "output_text",
						Text:        strings.Join(textParts, "\n"),
						Annotations: []any{},
					}},
					Status: "completed",
				})
				textParts = nil
			}
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			items = append(items, inputFunctionCall{
				Type:      "function_call",
				ID:        "fc_" + b.ID,
				CallID:    b.ID,
				Name:      b.Name,
				Arguments: args,
			})
		}
	}

	if len(textParts) > 0 {
		items = append(items, inputAssistantMessage{
			Type: "message",
			Role: "assistant",
			ID:   "msg_text",
			Content: []outputTextContent{{
				Type:        "output_text",
				Text:        strings.Join(textParts, "\n"),
				Annotations: []any{},
			}},
			Status: "completed",
		})
	}
	return items
}

func buildTools(tools []bs.ToolDefinition) []responseTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]responseTool, len(tools))
	for i, t := range tools {
		out[i] = responseTool{
			Type:        "function",
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		}
	}
	return out
}

func mapStatus(status string, content []bs.ContentBlock) string {
	for _, b := range content {
		if b.Type == "tool_use" {
			return "tool_use"
		}
	}
	switch status {
	case "completed":
		return "end_turn"
	case "incomplete":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func stringifyContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		data, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(data)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
