package openaicodex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
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

// Complete sends a completion request using the OpenAI Responses API.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= len(p.backoffs); attempt++ {
		resp, err := p.sendOnce(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err
		errMsg := err.Error()
		retryable := strings.Contains(errMsg, "503") ||
			strings.Contains(errMsg, "429") ||
			strings.Contains(errMsg, "deadline exceeded")
		if !retryable {
			return nil, err
		}

		if attempt < len(p.backoffs) {
			p.logger.Warn("openai-codex retryable error",
				"error", err,
				"attempt", attempt+1,
				"backoff", p.backoffs[attempt],
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(p.backoffs[attempt]):
			}
		}
	}
	return nil, fmt.Errorf("openai-codex failed after %d retries: %w", len(p.backoffs), lastErr)
}

func (p *CompletionProvider) sendOnce(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	token, err := p.tokens.AccessToken()
	if err != nil {
		return nil, fmt.Errorf("openai-codex auth: %w", err)
	}

	payload := buildRequest(req, false)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	p.logger.Info("openai-codex API request",
		"model", req.Model,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"system_len", len(req.System),
		"body_bytes", len(body),
	)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", responsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-codex request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openai-codex API returned %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var apiResp responsesResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	content := parseOutput(apiResp.Output)
	stopReason := mapStatus(apiResp.Status, content)

	return &bs.CompletionResponse{
		Content:    content,
		StopReason: stopReason,
		Usage: bs.Usage{
			InputTokens:  apiResp.Usage.InputTokens,
			OutputTokens: apiResp.Usage.OutputTokens,
		},
	}, nil
}

// --- Request types ---

type responsesRequest struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions"`
	Input           []any          `json:"input"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
	Tools           []responseTool `json:"tools,omitempty"`
	MaxOutputTokens *int           `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
}

type responseTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Input item types for the Responses API.
type inputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inputAssistantMessage struct {
	Type    string              `json:"type"`   // "message"
	Role    string              `json:"role"`   // "assistant"
	ID      string              `json:"id"`
	Content []outputTextContent `json:"content"`
	Status  string              `json:"status"` // "completed"
}

type outputTextContent struct {
	Type string `json:"type"` // "output_text"
	Text string `json:"text"`
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

type responsesResponse struct {
	ID     string       `json:"id"`
	Status string       `json:"status"` // "completed", "incomplete", "failed"
	Output []outputItem `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

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

func buildRequest(req bs.CompletionRequest, stream bool) responsesRequest {
	var input []any

	for _, msg := range req.Messages {
		blocks := bs.NormalizeContent(msg.Content)
		switch msg.Role {
		case "user":
			items := buildUserInput(blocks)
			input = append(input, items...)
		case "assistant":
			items := buildAssistantInput(blocks)
			input = append(input, items...)
		}
	}

	r := responsesRequest{
		Model:        req.Model,
		Instructions: req.System,
		Input:        input,
		Stream:       stream,
		Tools:        buildTools(req.Tools),
	}
	if req.MaxTokens > 0 {
		r.MaxOutputTokens = &req.MaxTokens
	}
	if req.Temperature > 0 {
		t := req.Temperature
		r.Temperature = &t
	}
	return r
}

func buildUserInput(blocks []bs.ContentBlock) []any {
	var items []any
	var textParts []string

	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "tool_result":
			// Flush text before tool result.
			if len(textParts) > 0 {
				items = append(items, inputMessage{Role: "user", Content: strings.Join(textParts, "\n")})
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
		items = append(items, inputMessage{Role: "user", Content: strings.Join(textParts, "\n")})
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
			// Flush text before tool call.
			if len(textParts) > 0 {
				items = append(items, inputAssistantMessage{
					Type:    "message",
					Role:    "assistant",
					ID:      "msg_text",
					Content: []outputTextContent{{Type: "output_text", Text: strings.Join(textParts, "\n")}},
					Status:  "completed",
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
			Type:    "message",
			Role:    "assistant",
			ID:      "msg_text",
			Content: []outputTextContent{{Type: "output_text", Text: strings.Join(textParts, "\n")}},
			Status:  "completed",
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

func parseOutput(output []outputItem) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	for _, item := range output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					blocks = append(blocks, bs.ContentBlock{Type: "text", Text: c.Text})
				}
			}
		case "function_call":
			rawArgs := json.RawMessage(item.Arguments)
			if !json.Valid(rawArgs) {
				rawArgs, _ = json.Marshal(item.Arguments)
			}
			blocks = append(blocks, bs.ContentBlock{
				Type:  "tool_use",
				ID:    item.CallID,
				Name:  item.Name,
				Input: rawArgs,
			})
		}
	}
	return blocks
}

func mapStatus(status string, content []bs.ContentBlock) string {
	hasToolUse := false
	for _, b := range content {
		if b.Type == "tool_use" {
			hasToolUse = true
			break
		}
	}
	if hasToolUse {
		return "tool_use"
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
