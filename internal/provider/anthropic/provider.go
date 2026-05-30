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

	bs "github.com/rasimio/blueship/internal/core"
)

const messagesURL = "https://api.anthropic.com/v1/messages"

// claudeCodeIdentity is the system block Anthropic requires on OAuth-authed
// (subscription) requests — without it the API rejects inference calls from
// the Claude Code OAuth client. Sent as a separate first system block so it
// stays out of the cached user-prompt breakpoint.
const claudeCodeIdentity = "You are Claude Code, Anthropic's official CLI for Claude."

// TokenSource returns a fresh bearer token. Used by the OAuth code path so
// the request builder doesn't need to know about refresh logic.
type TokenSource func() (string, error)

// Provider implements bs.CompletionProvider using the Anthropic Messages API.
// Auth is either a static API key (x-api-key-style bearer) or an OAuth token
// supplied by tokenSource — the latter requires the Claude Code identity
// system block and the oauth-2025-04-20 beta header.
type Provider struct {
	apiKey      string
	tokenSource TokenSource
	oauth       bool
	httpClient  *http.Client
	logger      *slog.Logger
	backoffs    []time.Duration
}

// NewProvider creates a new Anthropic CompletionProvider using a static API key.
func NewProvider(apiKey string, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) *Provider {
	return &Provider{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
		logger:     logger,
		backoffs:   backoffs,
	}
}

// NewOAuthProvider creates a Provider that authenticates via the Anthropic
// OAuth subscription flow (Claude Code). The bearer token is fetched fresh
// for each request via tokenSource (which handles refresh internally).
func NewOAuthProvider(tokenSource TokenSource, timeout time.Duration, backoffs []time.Duration, logger *slog.Logger) *Provider {
	return &Provider{
		tokenSource: tokenSource,
		oauth:       true,
		httpClient:  &http.Client{Timeout: timeout},
		logger:      logger,
		backoffs:    backoffs,
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
//
//	Type "enabled"  → manual budget (BudgetTokens required). Legacy; deprecated
//	                  on Claude 4.6+ and rejected on Opus 4.7/4.8 by the public
//	                  API (the OAuth/Claude-Code surface is more lenient).
//	Type "adaptive" → model decides depth (Claude 4.6+); BudgetTokens omitted,
//	                  guided by output_config.effort.
//
// Display "summarized" returns thinking-block text (default "omitted" on
// Opus 4.7/4.8); we ask for summarized so OnThinking still surfaces reasoning.
type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

// outputConfig carries Anthropic's effort control (output_config.effort:
// low|medium|high|xhigh|max). Omitted entirely when Effort is empty.
type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

type systemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

// apiTool wraps ToolDefinition with optional cache_control for prompt caching.
type apiTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
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
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	Stream       bool            `json:"stream,omitempty"`
	System       []systemBlock   `json:"system,omitempty"`
	Messages     []apiMessage    `json:"messages"`
	Tools        []apiTool       `json:"tools,omitempty"`
	Temperature  float64         `json:"temperature,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
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

// applyThinkingAndEffort sets thinking + output_config.effort on apiReq from the
// request. Centralised so Complete and StreamComplete stay in lockstep.
//
//	ThinkingMode "adaptive" → thinking:{type:adaptive} (Claude 4.6+); the model
//	  decides depth, guided by Effort; budget_tokens omitted.
//	ThinkingMode "off"      → no thinking block (Effort may still apply).
//	ThinkingMode ""         → legacy: manual thinking when ThinkingBudget > 0.
//
// Temperature is forced to 0 (→ omitted) whenever thinking is active, since the
// API requires the default temperature with extended thinking.
func applyThinkingAndEffort(apiReq *apiRequest, req bs.CompletionRequest) {
	if req.Effort != "" {
		apiReq.OutputConfig = &outputConfig{Effort: req.Effort}
	}
	switch req.ThinkingMode {
	case "adaptive":
		apiReq.Thinking = &thinkingConfig{Type: "adaptive", Display: "summarized"}
		apiReq.Temperature = 0
	case "off":
		// effort-only: no thinking block
	default:
		if req.ThinkingBudget > 0 {
			apiReq.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: req.ThinkingBudget}
			apiReq.MaxTokens += req.ThinkingBudget
			apiReq.Temperature = 0
		}
	}
}

// thinkingActive reports whether the request asked for any thinking — used to
// decide whether to strip thinking blocks from the assembled response.
func thinkingActive(req bs.CompletionRequest) bool {
	return req.ThinkingMode == "adaptive" || (req.ThinkingMode == "" && req.ThinkingBudget > 0)
}

func (p *Provider) sendOnce(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	apiMsgs := buildMessages(req.Messages)
	if p.oauth {
		if trimmed, n := trimTrailingAssistant(apiMsgs); n > 0 {
			p.logger.Warn("anthropic-oauth: dropped trailing assistant message(s) — OAuth surface forbids prefill, conversation must end with a user turn",
				"dropped", n)
			apiMsgs = trimmed
		}
	}

	apiReq := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    buildSystem(req.System, p.oauth),
		Messages:  apiMsgs,
		Tools:     buildTools(req.Tools),
	}

	applyThinkingAndEffort(&apiReq, req)

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
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	bearer := p.apiKey
	if p.oauth {
		tok, err := p.tokenSource()
		if err != nil {
			return nil, fmt.Errorf("anthropic-oauth auth: %w", err)
		}
		bearer = tok
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)

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
	if thinkingActive(req) {
		filtered := make([]bs.ContentBlock, 0, len(content))
		for _, b := range content {
			if b.Type != "thinking" {
				filtered = append(filtered, b)
			}
		}
		content = filtered
	}

	if apiResp.Usage.CacheReadTokens > 0 || apiResp.Usage.CacheCreationTokens > 0 {
		p.logger.Info("prompt cache",
			"cache_read", apiResp.Usage.CacheReadTokens,
			"cache_write", apiResp.Usage.CacheCreationTokens,
		)
	}

	return &bs.CompletionResponse{
		Content:    content,
		StopReason: apiResp.StopReason,
		Usage:      apiResp.Usage,
	}, nil
}

// buildSystem converts a system prompt string to the Anthropic array format with cache_control.
// The minimum cacheable size is 1024 tokens (~3072 chars); we only add cache_control if met.
// When oauth is true, prepends the Claude Code identity block — required by
// Anthropic on subscription-authed requests.
func buildSystem(system string, oauth bool) []systemBlock {
	var blocks []systemBlock
	if oauth {
		blocks = append(blocks, systemBlock{Type: "text", Text: claudeCodeIdentity})
	}
	if system != "" {
		block := systemBlock{Type: "text", Text: system}
		if len([]rune(system))/3 >= 1024 {
			block.CacheControl = &cacheControl{Type: "ephemeral"}
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// buildTools converts ToolDefinitions to apiTools with cache_control on the last entry.
func buildTools(tools []bs.ToolDefinition) []apiTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]apiTool, len(tools))
	for i, t := range tools {
		out[i] = apiTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	// Mark the last tool — Anthropic caches everything up to and including the breakpoint.
	out[len(out)-1].CacheControl = &cacheControl{Type: "ephemeral"}
	return out
}

// trimTrailingAssistant drops trailing assistant-role messages so the wire
// array ends with a user turn. The Anthropic OAuth (Claude Code subscription)
// surface rejects a messages array ending with an assistant message — 400
// "This model does not support assistant message prefill. The conversation
// must end with a user message" — whereas the API-key surface tolerates it as
// a prefill. A trailing assistant turn is always a degenerate state here:
// nothing in blueship intentionally prefills, so it only arises when an
// upstream step appended an empty tool_results user turn that buildMessages
// then drops as empty content, or when a crashed iteration left an orphaned
// assistant turn in a shared session. Dropping it lets the call proceed
// instead of hard-failing the whole agent iteration. Returns the trimmed
// slice and the number of messages dropped (for logging). Caller gates this
// on p.oauth so the API-key path keeps its (unused but valid) prefill ability.
func trimTrailingAssistant(msgs []apiMessage) ([]apiMessage, int) {
	dropped := 0
	for len(msgs) > 0 && msgs[len(msgs)-1].Role == "assistant" {
		msgs = msgs[:len(msgs)-1]
		dropped++
	}
	return msgs, dropped
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
