package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
)

const defaultBaseURL = "https://api.openai.com/v1"

// CompletionProvider implements bs.CompletionProvider using OpenAI-compatible Chat Completions.
type CompletionProvider struct {
	apiKey      string
	baseURL     string
	httpClient  *http.Client
	extraParams map[string]any // additional JSON fields merged into every request
}

// NewCompletionProvider creates a new OpenAI completion provider.
func NewCompletionProvider(apiKey string, timeout time.Duration) *CompletionProvider {
	return &CompletionProvider{
		apiKey:     apiKey,
		baseURL:    defaultBaseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

// NewCompatibleProvider creates a provider for any OpenAI-compatible API (MLX, vLLM, Ollama, etc.).
// extraParams are merged into every request JSON (e.g. {"chat_template_kwargs": {"enable_thinking": false}}).
func NewCompatibleProvider(baseURL string, apiKey string, timeout time.Duration, extraParams map[string]any) *CompletionProvider {
	return &CompletionProvider{
		apiKey:      apiKey,
		baseURL:     baseURL,
		httpClient:  &http.Client{Timeout: timeout},
		extraParams: extraParams,
	}
}

type chatCompletionRequest struct {
	Model       string             `json:"model"`
	Messages    []chatMessage      `json:"messages"`
	Tools       []toolDefinition   `json:"tools,omitempty"`
	MaxTokens   int                `json:"max_tokens,omitempty"`
	Temperature float64            `json:"temperature,omitempty"`
	ToolChoice  string             `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    any             `json:"content,omitempty"`
	ToolCalls  []toolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type toolDefinition struct {
	Type     string         `json:"type"`
	Function functionSchema `json:"function"`
}

type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Idx      int          `json:"index"`
	Function functionCall `json:"function"`
}

func (tc toolCall) Index() int { return tc.Idx }

type functionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionResponse struct {
	Choices []struct {
		FinishReason string       `json:"finish_reason"`
		Message      chatMessage  `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *apiError `json:"error,omitempty"`
}

// Complete sends a completion request to an OpenAI-compatible endpoint.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	messages := buildMessages(req.System, req.Messages)
	tools := buildTools(req.Tools)

	payload := chatCompletionRequest{
		Model:     req.Model,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: req.MaxTokens,
	}
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Merge extra params (e.g. chat_template_kwargs) into request JSON.
	if len(p.extraParams) > 0 {
		var m map[string]any
		json.Unmarshal(body, &m)
		for k, v := range p.extraParams {
			m[k] = v
		}
		body, _ = json.Marshal(m)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai request: %w", err)
	}
	defer resp.Body.Close()

	var result chatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("openai API returned %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if len(result.Choices) == 0 {
		return nil, fmt.Errorf("openai returned empty choices")
	}

	choice := result.Choices[0]
	contentBlocks := toContentBlocks(choice.Message)

	return &bs.CompletionResponse{
		Content: contentBlocks,
		StopReason: mapStopReason(choice.FinishReason),
		Usage: bs.Usage{
			InputTokens:  result.Usage.PromptTokens,
			OutputTokens: result.Usage.CompletionTokens,
		},
	}, nil
}

// streamChatCompletionRequest adds the stream field.
type streamChatCompletionRequest struct {
	chatCompletionRequest
	Stream bool `json:"stream"`
}

// streamChunkDelta is the delta object inside a streaming chunk.
type streamChunkDelta struct {
	Content   string     `json:"content,omitempty"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
}

// streamChunk is one SSE chunk from the streaming endpoint.
type streamChunk struct {
	Choices []struct {
		Delta        streamChunkDelta `json:"delta"`
		FinishReason *string          `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

// StreamComplete sends a streaming completion request. Calls onText for each text chunk.
func (p *CompletionProvider) StreamComplete(ctx context.Context, req bs.CompletionRequest, onText func(string)) (*bs.CompletionResponse, error) {
	messages := buildMessages(req.System, req.Messages)
	tools := buildTools(req.Tools)

	payload := streamChatCompletionRequest{
		chatCompletionRequest: chatCompletionRequest{
			Model:     req.Model,
			Messages:  messages,
			Tools:     tools,
			MaxTokens: req.MaxTokens,
		},
		Stream: true,
	}
	if len(tools) > 0 {
		payload.ToolChoice = "auto"
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	if len(p.extraParams) > 0 {
		var m map[string]any
		json.Unmarshal(body, &m)
		for k, v := range p.extraParams {
			m[k] = v
		}
		body, _ = json.Marshal(m)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	// No global timeout for streaming.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp chatCompletionResponse
		json.NewDecoder(resp.Body).Decode(&errResp)
		msg := fmt.Sprintf("openai stream API returned %d", resp.StatusCode)
		if errResp.Error != nil {
			msg += ": " + errResp.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}

	// Parse SSE stream.
	var (
		textBuf   strings.Builder
		toolCalls []toolCall
		usage     bs.Usage
		stopReason string
	)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				textBuf.WriteString(delta.Content)
				if onText != nil {
					onText(delta.Content)
				}
			}

			// Accumulate tool calls from deltas.
			for _, tc := range delta.ToolCalls {
				for len(toolCalls) <= tc.Index() {
					toolCalls = append(toolCalls, toolCall{})
				}
				idx := tc.Index()
				if tc.ID != "" {
					toolCalls[idx].ID = tc.ID
				}
				if tc.Type != "" {
					toolCalls[idx].Type = tc.Type
				}
				if tc.Function.Name != "" {
					toolCalls[idx].Function.Name = tc.Function.Name
				}
				toolCalls[idx].Function.Arguments += tc.Function.Arguments
			}

			if fr := chunk.Choices[0].FinishReason; fr != nil {
				stopReason = *fr
			}
		}

		if chunk.Usage != nil {
			usage = bs.Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
		}
	}

	// Build content blocks.
	var blocks []bs.ContentBlock
	if textBuf.Len() > 0 {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: textBuf.String()})
	}
	for _, tc := range toolCalls {
		rawArgs := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(rawArgs) {
			rawArgs = json.RawMessage("{}")
		}
		blocks = append(blocks, bs.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: rawArgs,
		})
	}

	return &bs.CompletionResponse{
		Content:    blocks,
		StopReason: mapStopReason(stopReason),
		Usage:      usage,
	}, nil
}

func buildMessages(system string, messages []bs.Message) []chatMessage {
	var out []chatMessage
	if strings.TrimSpace(system) != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}

	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		switch msg.Role {
		case "user":
			userMsg, toolMsgs := buildUserMessages(blocks)
			if userMsg != nil {
				out = append(out, *userMsg)
			}
			out = append(out, toolMsgs...)
		case "assistant":
			out = append(out, buildAssistantMessage(blocks))
		default:
			if len(blocks) > 0 {
				out = append(out, chatMessage{Role: msg.Role, Content: extractText(blocks)})
			}
		}
	}
	return out
}

func buildUserMessages(blocks []bs.ContentBlock) (*chatMessage, []chatMessage) {
	var toolMsgs []chatMessage
	var parts []map[string]any
	var text strings.Builder

	for _, b := range blocks {
		switch b.Type {
		case "tool_result":
			toolMsgs = append(toolMsgs, chatMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    stringifyContent(b.Content),
			})
		case "image":
			if b.Source != nil && b.Source.Type == "base64" && b.Source.MediaType != "" {
				parts = append(parts, map[string]any{
					"type": "image_url",
					"image_url": map[string]any{
						"url": "data:" + b.Source.MediaType + ";base64," + b.Source.Data,
					},
				})
			}
		case "text":
			text.WriteString(b.Text)
		}
	}

	if text.Len() == 0 && len(parts) == 0 {
		return nil, toolMsgs
	}

	if text.Len() > 0 {
		parts = append([]map[string]any{{"type": "text", "text": text.String()}}, parts...)
	}

	if len(parts) == 1 && parts[0]["type"] == "text" {
		return &chatMessage{Role: "user", Content: parts[0]["text"]}, toolMsgs
	}

	return &chatMessage{Role: "user", Content: parts}, toolMsgs
}

func buildAssistantMessage(blocks []bs.ContentBlock) chatMessage {
	var calls []toolCall
	var text strings.Builder

	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			calls = append(calls, toolCall{
				ID:   b.ID,
				Type: "function",
				Function: functionCall{
					Name:      b.Name,
					Arguments: args,
				},
			})
		case "text":
			text.WriteString(b.Text)
		}
	}

	msg := chatMessage{Role: "assistant"}
	if text.Len() > 0 {
		msg.Content = text.String()
	}
	if len(calls) > 0 {
		msg.ToolCalls = calls
	}
	return msg
}

func buildTools(tools []bs.ToolDefinition) []toolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolDefinition, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolDefinition{
			Type: "function",
			Function: functionSchema{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return out
}

func toContentBlocks(msg chatMessage) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	if s, ok := msg.Content.(string); ok && s != "" {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: s})
	}
	for _, call := range msg.ToolCalls {
		rawArgs := json.RawMessage(call.Function.Arguments)
		if !json.Valid(rawArgs) {
			rawArgs, _ = json.Marshal(call.Function.Arguments)
		}
		blocks = append(blocks, bs.ContentBlock{
			Type:  "tool_use",
			ID:    call.ID,
			Name:  call.Function.Name,
			Input: rawArgs,
		})
	}
	return blocks
}

func mapStopReason(reason string) string {
	switch reason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func extractText(blocks []bs.ContentBlock) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

func stringifyContent(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		data, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(data)
	}
}
