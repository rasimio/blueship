package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

const defaultBaseURL = "https://api.openai.com/v1"

// CompletionProvider implements bs.CompletionProvider using OpenAI-compatible Chat Completions.
type CompletionProvider struct {
	apiKey      string
	baseURL     string
	httpClient  *http.Client
	extraParams map[string]any // additional JSON fields merged into every request
	dropImages  bool           // strip image content blocks before sending (text-only endpoints like MLX)
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
//
// Two extraParams keys are interpreted at construction time and stripped from the
// merged request payload:
//   - "drop_images": true  → image content blocks are replaced by a text marker
//     (for endpoints that only accept text content)
func NewCompatibleProvider(baseURL string, apiKey string, timeout time.Duration, extraParams map[string]any) *CompletionProvider {
	p := &CompletionProvider{
		apiKey:     apiKey,
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
	}
	if extraParams != nil {
		if v, ok := extraParams["drop_images"]; ok {
			if b, ok := v.(bool); ok {
				p.dropImages = b
			}
			delete(extraParams, "drop_images")
		}
		if len(extraParams) > 0 {
			p.extraParams = extraParams
		}
	}
	return p
}

type chatCompletionRequest struct {
	Model           string           `json:"model"`
	Messages        []chatMessage    `json:"messages"`
	Tools           []toolDefinition `json:"tools,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	Temperature     float64          `json:"temperature,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	ToolChoice      string           `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
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
		FinishReason string      `json:"finish_reason"`
		Message      chatMessage `json:"message"`
	} `json:"choices"`
	Usage usageReport `json:"usage"`
	Error *apiError   `json:"error,omitempty"`
}

// usageReport covers both spellings of prompt-cache accounting seen on
// OpenAI-compatible backends: the standard nested
// prompt_tokens_details.cached_tokens, and DeepSeek's flat
// prompt_cache_hit_tokens. Backends without caching report neither, read as
// zero, and behave exactly as before.
type usageReport struct {
	PromptTokens         int `json:"prompt_tokens"`
	CompletionTokens     int `json:"completion_tokens"`
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// usage normalizes to the accounting the rest of the stack assumes, which is
// Anthropic's: input tokens and cache reads are disjoint. OpenAI-compatible
// backends instead fold cached tokens into prompt_tokens, so they are
// subtracted back out here — otherwise every cache hit would be double-counted
// in any cost or hit-rate math over llm_usage.
func (u usageReport) usage() bs.Usage {
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.PromptCacheHitTokens
	}
	input := u.PromptTokens - cached
	if input < 0 {
		input, cached = u.PromptTokens, 0
	}
	return bs.Usage{
		InputTokens:     input,
		OutputTokens:    u.CompletionTokens,
		CacheReadTokens: cached,
	}
}

// Complete sends a completion request to an OpenAI-compatible endpoint.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	messages := buildMessages(req.System, req.Messages, p.dropImages)
	tools := buildTools(req.Tools)

	payload := chatCompletionRequest{
		Model:           req.Model,
		Messages:        messages,
		Tools:           tools,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.Effort,
		Temperature:     req.Temperature,
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
		Content:    contentBlocks,
		StopReason: stopReasonForBlocks(choice.FinishReason, contentBlocks),
		Usage:      result.Usage.usage(),
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
	Usage *usageReport `json:"usage,omitempty"`
	Error *apiError    `json:"error,omitempty"`
}

// StreamComplete sends a streaming completion request. Dispatches per-event
// callbacks: cb.OnText for each text delta as it arrives, and cb.OnToolUse
// once each tool_call's argument JSON is fully assembled (end-of-stream — the
// OpenAI Chat Completions SSE protocol streams function_call arguments in
// fragments, so the JSON is only structurally complete after the stream
// finishes). cb may be nil; each field is independently nil-checked.
func (p *CompletionProvider) StreamComplete(ctx context.Context, req bs.CompletionRequest, cb *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	messages := buildMessages(req.System, req.Messages, p.dropImages)
	tools := buildTools(req.Tools)

	payload := streamChatCompletionRequest{
		chatCompletionRequest: chatCompletionRequest{
			Model:           req.Model,
			Messages:        messages,
			Tools:           tools,
			MaxTokens:       req.MaxTokens,
			ReasoningEffort: req.Effort,
			Temperature:     req.Temperature,
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
		textBuf    strings.Builder
		toolCalls  []toolCall
		usage      bs.Usage
		stopReason string
		finished   bool
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
			finished = true
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return nil, fmt.Errorf("decode stream: %w", err)
		}
		if chunk.Error != nil {
			return nil, fmt.Errorf("openai stream: %s", chunk.Error.Message)
		}

		if len(chunk.Choices) > 0 {
			delta := chunk.Choices[0].Delta

			if delta.Content != "" {
				textBuf.WriteString(delta.Content)
				if cb != nil && cb.OnText != nil {
					cb.OnText(delta.Content)
				}
			}

			// Accumulate tool calls from deltas. OpenAI streams arguments as
			// JSON fragments; OnToolUse is fired once the stream ends and each
			// call's argument JSON is fully assembled (see end of function).
			for _, tc := range delta.ToolCalls {
				if tc.Index() < 0 || tc.Index() >= 128 {
					return nil, fmt.Errorf("invalid tool call index: %d", tc.Index())
				}
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
				if *fr != "" {
					finished = true
				}
			}
		}

		if chunk.Usage != nil {
			usage = chunk.Usage.usage()
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stream: %w", err)
	}
	if !finished {
		return nil, fmt.Errorf("incomplete completion stream: %w", io.ErrUnexpectedEOF)
	}
	// Build content blocks only after a complete stream. Partial arguments must
	// never be replaced with {} and dispatched as a real tool invocation.
	var blocks []bs.ContentBlock
	if textBuf.Len() > 0 {
		blocks = append(blocks, bs.ContentBlock{Type: "text", Text: textBuf.String()})
	}
	for _, tc := range toolCalls {
		rawArgs := json.RawMessage(tc.Function.Arguments)
		if !json.Valid(rawArgs) {
			return nil, fmt.Errorf("incomplete tool arguments for %s", tc.Function.Name)
		}
		blocks = append(blocks, bs.ContentBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: rawArgs,
		})
	}

	// Surface fully-assembled tool calls — OpenAI's SSE delivers arguments
	// piecewise, so this is the earliest the JSON is structurally complete.
	if cb != nil && cb.OnToolUse != nil {
		for _, block := range blocks {
			if block.Type == "tool_use" {
				cb.OnToolUse(block.ID, block.Name, block.Input)
			}
		}
	}

	return &bs.CompletionResponse{
		Content:    blocks,
		StopReason: stopReasonForBlocks(stopReason, blocks),
		Usage:      usage,
	}, nil
}

func buildMessages(system string, messages []bs.Message, dropImages bool) []chatMessage {
	var out []chatMessage
	if strings.TrimSpace(system) != "" {
		out = append(out, chatMessage{Role: "system", Content: system})
	}

	answered := answeredToolUseIDs(messages)
	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		if msg.Role == "assistant" {
			blocks = dropUnansweredToolUses(blocks, answered)
			if len(blocks) == 0 {
				continue
			}
		}
		switch msg.Role {
		case "user":
			// Tool results must come first: the API rejects the request unless
			// every tool_call_id in the preceding assistant message is answered
			// immediately after it. Text riding the same turn — a turn context
			// block, a final-answer directive — becomes a user message behind
			// them rather than wedging between the call and its results.
			userMsg, toolMsgs := buildUserMessages(blocks, dropImages)
			out = append(out, toolMsgs...)
			if userMsg != nil {
				out = append(out, *userMsg)
			}
		case "assistant":
			out = append(out, buildAssistantMessage(blocks))
		default:
			if len(blocks) > 0 {
				out = append(out, chatMessage{Role: msg.Role, Content: extractText(blocks)})
			}
		}
	}
	return pruneOrphanToolCalls(out)
}

// pruneOrphanToolCalls drops every tool_call whose reply is not sitting where
// the backend looks for it, and every tool reply whose call is not there. The
// API checks position, not presence: a call must be answered by the tool
// message directly behind its own, one per call, in order — anything else fails
// the whole request with "insufficient tool messages following tool_calls
// message", losing the turn.
//
// The dialog arrives that way for ordinary reasons, not corruption. A message
// that lands while a tool is still running separates the call from its result.
// A history window trimmed to a token budget opens or closes mid-round, leaving
// a call whose reply was cut or a reply whose call was. And a backend that
// streams a call without an id gives both halves an empty tool_call_id, so the
// pair can never be matched at all.
//
// Dropping loses the record that a tool ran, which is the cheaper loss: the
// turn still happens, and the results usually survive as text elsewhere in the
// answer.
func pruneOrphanToolCalls(msgs []chatMessage) []chatMessage {
	keepReply := make([]bool, len(msgs))
	keptCalls := make([][]toolCall, len(msgs))

	for i, msg := range msgs {
		if msg.Role == "tool" || len(msg.ToolCalls) == 0 {
			continue
		}
		for k, call := range msg.ToolCalls {
			reply := i + 1 + k
			if call.ID == "" ||
				reply >= len(msgs) ||
				msgs[reply].Role != "tool" ||
				msgs[reply].ToolCallID != call.ID {
				// Once the run breaks, every later call is out of position
				// too — their replies could only have followed this one.
				break
			}
			keptCalls[i] = append(keptCalls[i], call)
			keepReply[reply] = true
		}
	}

	out := make([]chatMessage, 0, len(msgs))
	for i, msg := range msgs {
		if msg.Role == "tool" {
			if keepReply[i] {
				out = append(out, msg)
			}
			continue
		}
		if len(msg.ToolCalls) > 0 {
			msg.ToolCalls = keptCalls[i]
			// An assistant turn that was nothing but a dropped call has
			// nothing left to say.
			if len(msg.ToolCalls) == 0 && isBlankContent(msg.Content) {
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

func isBlankContent(content any) bool {
	switch v := content.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []map[string]any:
		return len(v) == 0
	default:
		return false
	}
}

// answeredToolUseIDs collects every tool_use id the history answers. The wire
// format requires each one to be followed by its result; a turn that ended
// before its tools ran — a truncated call, a crash, a barge-in — leaves a call
// that never will be, and the API then rejects the whole conversation on every
// later turn rather than just the turn that broke. Reconciling here keeps the
// invariant a property of the request instead of a hope about the history.
func answeredToolUseIDs(messages []bs.Message) map[string]bool {
	answered := map[string]bool{}
	for _, msg := range messages {
		for _, b := range bs.NormalizeContent(msg.Content) {
			if b.Type == "tool_result" && b.ToolUseID != "" {
				answered[b.ToolUseID] = true
			}
		}
	}
	return answered
}

func dropUnansweredToolUses(blocks []bs.ContentBlock, answered map[string]bool) []bs.ContentBlock {
	kept := make([]bs.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "tool_use" && !answered[b.ID] {
			continue
		}
		kept = append(kept, b)
	}
	return kept
}

func buildUserMessages(blocks []bs.ContentBlock, dropImages bool) (*chatMessage, []chatMessage) {
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
			if dropImages {
				if text.Len() > 0 {
					text.WriteString("\n")
				}
				text.WriteString("[image attached]")
				continue
			}
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

// stopReasonForBlocks reconciles the reported finish reason with what the
// model actually emitted. Only "tool_calls" is specified to accompany tool
// calls, but OpenAI-compatible backends also return them under "stop" and
// truncate one under "length". A caller that trusts the reason then ends the
// turn with the calls already persisted and no results, and the API refuses
// that history from then on: "an assistant message with 'tool_calls' must be
// followed by tool messages responding to each 'tool_call_id'".
func stopReasonForBlocks(reason string, blocks []bs.ContentBlock) string {
	mapped := mapStopReason(reason)
	if mapped == "tool_use" {
		return mapped
	}
	for _, b := range blocks {
		if b.Type == "tool_use" {
			return "tool_use"
		}
	}
	return mapped
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
