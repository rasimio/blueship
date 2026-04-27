// Package ollama implements a CompletionProvider using Ollama's native
// /api/chat endpoint. The OpenAI-compatible /v1/chat/completions endpoint
// has bugs around the Gemma reasoning field, so we speak the native
// protocol directly. Responses are NDJSON when streaming (one JSON object
// per line, terminated by {"done": true}).
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
)

// dumpRequestBodyIfEnabled writes the outgoing JSON to the path in
// OLLAMA_DUMP_REQ_PATH, when set. Diagnostic only — leave unset in
// normal operation.
func dumpRequestBodyIfEnabled(body []byte) {
	path := os.Getenv("OLLAMA_DUMP_REQ_PATH")
	if path == "" {
		return
	}
	_ = os.WriteFile(path, body, 0o644)
}

const defaultBaseURL = "http://localhost:11434"

// CompletionProvider talks to Ollama via /api/chat.
type CompletionProvider struct {
	baseURL    string
	httpClient *http.Client
}

// NewCompletionProvider creates a provider targeting the given Ollama base URL.
// Pass empty baseURL to use http://localhost:11434.
func NewCompletionProvider(baseURL string, timeout time.Duration) *CompletionProvider {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &CompletionProvider{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
}

type toolSchema struct {
	Type     string         `json:"type"`
	Function functionSchema `json:"function"`
}

type functionSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type ollamaMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
	Name      string           `json:"name,omitempty"` // tool name for role=tool
}

type ollamaToolCall struct {
	Function ollamaToolCallFn `json:"function"`
}

type ollamaToolCallFn struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"` // Ollama emits args as a JSON object, not a string
}

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Tools    []toolSchema    `json:"tools,omitempty"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"`
	Options  map[string]any  `json:"options,omitempty"`
}

type chatResponse struct {
	Model     string        `json:"model"`
	Message   ollamaMessage `json:"message"`
	Done      bool          `json:"done"`
	DoneReason string       `json:"done_reason,omitempty"`

	PromptEvalCount int `json:"prompt_eval_count,omitempty"`
	EvalCount       int `json:"eval_count,omitempty"`

	Error string `json:"error,omitempty"`
}

// Complete performs a non-streaming chat completion.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	payload := p.buildRequest(req, false)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	dumpRequestBodyIfEnabled(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp chatResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		msg := fmt.Sprintf("ollama API returned %d", resp.StatusCode)
		if errResp.Error != "" {
			msg += ": " + errResp.Error
		}
		return nil, fmt.Errorf("%s", msg)
	}

	var result chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("ollama error: %s", result.Error)
	}

	return &bs.CompletionResponse{
		Content:    toContentBlocks(result.Message),
		StopReason: mapStopReason(result.DoneReason, len(result.Message.ToolCalls) > 0),
		Usage: bs.Usage{
			InputTokens:  result.PromptEvalCount,
			OutputTokens: result.EvalCount,
		},
	}, nil
}

// StreamComplete performs a streaming chat completion. Calls onText for each
// content delta as it arrives. Ollama streams NDJSON (one JSON object per line).
func (p *CompletionProvider) StreamComplete(ctx context.Context, req bs.CompletionRequest, onText func(string)) (*bs.CompletionResponse, error) {
	payload := p.buildRequest(req, true)
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	dumpRequestBodyIfEnabled(body)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// No global timeout for streaming.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("ollama stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errResp chatResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		msg := fmt.Sprintf("ollama stream API returned %d", resp.StatusCode)
		if errResp.Error != "" {
			msg += ": " + errResp.Error
		}
		return nil, fmt.Errorf("%s", msg)
	}

	var (
		textBuf    strings.Builder
		toolCalls  []ollamaToolCall
		usage      bs.Usage
		doneReason string
	)

	scanner := bufio.NewScanner(resp.Body)
	// Ollama may send large deltas when `think` content arrives, so give the
	// scanner plenty of room per line.
	scanner.Buffer(make([]byte, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var chunk chatResponse
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		if chunk.Error != "" {
			return nil, fmt.Errorf("ollama stream error: %s", chunk.Error)
		}
		if chunk.Message.Content != "" {
			textBuf.WriteString(chunk.Message.Content)
			if onText != nil {
				onText(chunk.Message.Content)
			}
		}
		// Tool calls in Ollama streaming arrive fully-formed in the final
		// message chunk (they are not streamed incrementally), so append as-is.
		if len(chunk.Message.ToolCalls) > 0 {
			toolCalls = append(toolCalls, chunk.Message.ToolCalls...)
		}
		if chunk.Done {
			doneReason = chunk.DoneReason
			usage = bs.Usage{
				InputTokens:  chunk.PromptEvalCount,
				OutputTokens: chunk.EvalCount,
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}

	var blocks []bs.ContentBlock
	if textBuf.Len() > 0 {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: textBuf.String()})
	}
	for _, tc := range toolCalls {
		argsJSON, err := json.Marshal(tc.Function.Arguments)
		if err != nil || !json.Valid(argsJSON) {
			argsJSON = json.RawMessage("{}")
		}
		blocks = append(blocks, bs.ContentBlock{
			Type:  "tool_use",
			ID:    generateToolUseID(tc.Function.Name),
			Name:  tc.Function.Name,
			Input: argsJSON,
		})
	}

	return &bs.CompletionResponse{
		Content:    blocks,
		StopReason: mapStopReason(doneReason, len(toolCalls) > 0),
		Usage:      usage,
	}, nil
}

// buildRequest constructs the Ollama /api/chat payload from the generic
// CompletionRequest. Temperature, max_tokens, etc. go under "options".
// "think" is always false — Gemma's thinking channel is a separate
// content field that we don't want to surface into assistant messages.
func (p *CompletionProvider) buildRequest(req bs.CompletionRequest, stream bool) chatRequest {
	options := map[string]any{}
	if req.MaxTokens > 0 {
		options["num_predict"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		options["temperature"] = req.Temperature
	}

	return chatRequest{
		Model:    req.Model,
		Messages: buildMessages(req.System, req.Messages),
		Tools:    buildTools(req.Tools),
		Stream:   stream,
		Think:    false,
		Options:  options,
	}
}

func buildMessages(system string, messages []bs.Message) []ollamaMessage {
	var out []ollamaMessage
	if strings.TrimSpace(system) != "" {
		out = append(out, ollamaMessage{Role: "system", Content: system})
	}
	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		switch msg.Role {
		case "user":
			// Split tool_result blocks into their own role=tool messages;
			// remaining text/image blocks collapse into one user message.
			var text strings.Builder
			var toolMsgs []ollamaMessage
			for _, b := range blocks {
				switch b.Type {
				case "tool_result":
					toolMsgs = append(toolMsgs, ollamaMessage{
						Role:    "tool",
						Content: stringifyContent(b.Content),
					})
				case "text":
					text.WriteString(b.Text)
				case "image":
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString("[image attached]")
				}
			}
			if text.Len() > 0 {
				out = append(out, ollamaMessage{Role: "user", Content: text.String()})
			}
			out = append(out, toolMsgs...)
		case "assistant":
			msg := ollamaMessage{Role: "assistant"}
			var text strings.Builder
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text.WriteString(b.Text)
				case "tool_use":
					var args map[string]any
					if len(b.Input) > 0 {
						_ = json.Unmarshal(b.Input, &args)
					}
					if args == nil {
						args = map[string]any{}
					}
					msg.ToolCalls = append(msg.ToolCalls, ollamaToolCall{
						Function: ollamaToolCallFn{
							Name:      b.Name,
							Arguments: args,
						},
					})
				}
			}
			msg.Content = text.String()
			out = append(out, msg)
		default:
			out = append(out, ollamaMessage{Role: msg.Role, Content: extractText(blocks)})
		}
	}
	return out
}

func buildTools(tools []bs.ToolDefinition) []toolSchema {
	if len(tools) == 0 {
		return nil
	}
	out := make([]toolSchema, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolSchema{
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

func toContentBlocks(msg ollamaMessage) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	if msg.Content != "" {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		argsJSON, err := json.Marshal(tc.Function.Arguments)
		if err != nil || !json.Valid(argsJSON) {
			argsJSON = json.RawMessage("{}")
		}
		blocks = append(blocks, bs.ContentBlock{
			Type:  "tool_use",
			ID:    generateToolUseID(tc.Function.Name),
			Name:  tc.Function.Name,
			Input: argsJSON,
		})
	}
	return blocks
}

func mapStopReason(doneReason string, hasTools bool) string {
	if hasTools {
		return "tool_use"
	}
	switch doneReason {
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

// generateToolUseID returns a stable-looking ID for an Ollama tool call.
// Ollama doesn't emit IDs, but downstream code needs one to match tool_results
// back to the assistant's tool_use block.
func generateToolUseID(name string) string {
	return fmt.Sprintf("ollama_%s_%d", name, time.Now().UnixNano())
}
