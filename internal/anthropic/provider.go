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

// apiRequest is the wire format for Anthropic Messages API.
type apiRequest struct {
	Model       string              `json:"model"`
	MaxTokens   int                 `json:"max_tokens"`
	System      string              `json:"system,omitempty"`
	Messages    []bs.Message        `json:"messages"`
	Tools       []bs.ToolDefinition `json:"tools,omitempty"`
	Temperature float64             `json:"temperature,omitempty"`
}

// apiResponse is the wire format for Anthropic Messages API response.
type apiResponse struct {
	ID         string           `json:"id"`
	Role       string           `json:"role"`
	Content    []bs.ContentBlock `json:"content"`
	Model      string           `json:"model"`
	StopReason string           `json:"stop_reason"`
	Usage      bs.Usage         `json:"usage"`
}

func (p *Provider) sendOnce(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	apiReq := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  req.Messages,
		Tools:     req.Tools,
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

	return &bs.CompletionResponse{
		Content:    apiResp.Content,
		StopReason: apiResp.StopReason,
		Usage:      apiResp.Usage,
	}, nil
}
