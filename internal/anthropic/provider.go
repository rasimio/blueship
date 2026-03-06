package anthropic

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

const messagesURL = "https://api.anthropic.com/v1/messages"

// Provider implements bs.CompletionProvider using the Anthropic Messages API.
type Provider struct {
	apiKey     string
	httpClient *http.Client
	logger     *slog.Logger
	backoffs   []time.Duration
}

// NewProvider creates a new Anthropic CompletionProvider.
func NewProvider(apiKey string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) *Provider {
	return &Provider{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
		backoffs:   backoffs,
	}
}

// Complete sends a completion request to the Anthropic Messages API.
// Includes built-in retry on rate_limit and overloaded errors.
func (p *Provider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= len(p.backoffs); attempt++ {
		resp, err := p.sendOnce(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		errMsg := err.Error()
		if !strings.Contains(errMsg, "rate_limit") && !strings.Contains(errMsg, "overloaded") {
			return nil, err
		}

		if attempt < len(p.backoffs) {
			p.logger.Warn("anthropic API retryable error",
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

	return nil, fmt.Errorf("anthropic API failed after %d retries: %w", len(p.backoffs), lastErr)
}

// thinkingConfig controls extended thinking in the API request.
type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type apiMessage struct {
	Role    string            `json:"role"`
	Content []apiContentBlock `json:"content"`
}

type apiContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Source    *bs.ImageSource `json:"source,omitempty"`
}

// apiRequest is the wire format for Anthropic Messages API.
type apiRequest struct {
	Model       string              `json:"model"`
	MaxTokens   int                 `json:"max_tokens"`
	System      string              `json:"system,omitempty"`
	Messages    []apiMessage        `json:"messages"`
	Tools       []bs.ToolDefinition `json:"tools,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
	Thinking    *thinkingConfig     `json:"thinking,omitempty"`
}

// apiResponse is the wire format for Anthropic Messages API response.
type apiResponse struct {
	ID         string            `json:"id"`
	Role       string            `json:"role"`
	Content    []bs.ContentBlock `json:"content"`
	Model      string            `json:"model"`
	StopReason string            `json:"stop_reason"`
	Usage      bs.Usage          `json:"usage"`
}

func (p *Provider) sendOnce(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	apiReq := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  buildMessages(req.Messages),
		Tools:     req.Tools,
	}

	if req.ThinkingBudget > 0 {
		apiReq.Thinking = &thinkingConfig{
			Type:         "enabled",
			BudgetTokens: req.ThinkingBudget,
		}
		apiReq.MaxTokens += req.ThinkingBudget
		apiReq.Temperature = 0 // temperature must be unset (0) with extended thinking
	}

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	p.logger.Info("anthropic API request",
		"model", req.Model,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"system_len", len(req.System),
		"body_bytes", len(body),
	)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", messagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("anthropic API status %d: %s", resp.StatusCode, respBody)
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Filter out thinking blocks — invisible to user
	content := apiResp.Content
	if req.ThinkingBudget > 0 {
		filtered := make([]bs.ContentBlock, 0, len(content))
		for _, b := range content {
			if b.Type != "thinking" {
				filtered = append(filtered, b)
			}
		}
		content = filtered
	}

	return &bs.CompletionResponse{
		Content:    content,
		StopReason: apiResp.StopReason,
		Usage:      apiResp.Usage,
	}, nil
}

func buildMessages(messages []bs.Message) []apiMessage {
	out := make([]apiMessage, 0, len(messages))
	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		content := make([]apiContentBlock, 0, len(blocks))
		for _, block := range blocks {
			if wire, ok := toAPIContentBlock(block); ok {
				content = append(content, wire)
			}
		}
		if len(content) == 0 {
			continue
		}
		out = append(out, apiMessage{Role: msg.Role, Content: content})
	}
	return out
}

func toAPIContentBlock(block bs.ContentBlock) (apiContentBlock, bool) {
	switch block.Type {
	case "text":
		if block.Text == "" {
			return apiContentBlock{}, false
		}
		return apiContentBlock{Type: "text", Text: block.Text}, true
	case "image":
		if block.Source == nil {
			return apiContentBlock{}, false
		}
		return apiContentBlock{Type: "image", Source: block.Source}, true
	case "tool_use":
		return apiContentBlock{
			Type:  "tool_use",
			ID:    block.ID,
			Name:  block.Name,
			Input: normalizeToolInput(block.Input),
		}, true
	case "tool_result":
		return apiContentBlock{
			Type:      "tool_result",
			ToolUseID: block.ToolUseID,
			Content:   normalizeToolResultContent(block.Content),
			IsError:   block.IsError,
		}, true
	default:
		return apiContentBlock{}, false
	}
}

func normalizeToolInput(input json.RawMessage) json.RawMessage {
	if len(input) == 0 {
		return json.RawMessage([]byte("{}"))
	}
	return input
}

func normalizeToolResultContent(content any) any {
	if content == nil {
		return ""
	}
	if blocks, ok := content.([]bs.ContentBlock); ok {
		wire := make([]apiContentBlock, 0, len(blocks))
		for _, b := range blocks {
			if converted, ok := toAPIContentBlock(b); ok {
				wire = append(wire, converted)
			}
		}
		if len(wire) == 0 {
			return ""
		}
		return wire
	}
	if s, ok := content.(string); ok {
		return s
	}
	if raw, ok := content.(json.RawMessage); ok {
		return string(raw)
	}
	if b, ok := content.([]byte); ok {
		return string(b)
	}
	data, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprintf("%v", content)
	}
	return string(data)
}
