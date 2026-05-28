// Streaming support for the Anthropic Messages API.

package anthropic

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

	bs "github.com/rasimio/blueship/core"
)

// StreamComplete sends a streaming completion request to the Anthropic Messages
// API, dispatching per-event callbacks (cb.OnText for text deltas, cb.OnToolUse
// once a tool_use block's input JSON is fully assembled, cb.OnThinking for
// thinking deltas), and returns the fully assembled response once the stream
// ends (so callers dispatch tool calls exactly as they would for Complete).
// This makes *Provider satisfy bs.StreamCompletionProvider.
//
// Streaming works identically over API-key and OAuth auth — it is the same
// /v1/messages endpoint with "stream":true; only the Authorization header
// differs. It retries rate_limit/overloaded errors, but only while no text or
// tool_use has yet been delivered — once the caller has seen output a retry
// would duplicate events, so the error is surfaced instead.
func (p *Provider) StreamComplete(ctx context.Context, req bs.CompletionRequest, cb *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= len(p.backoffs); attempt++ {
		resp, emitted, err := p.streamOnce(ctx, req, cb)
		if err == nil {
			return resp, nil
		}
		lastErr = err

		if emitted {
			return nil, err // events already dispatched — a retry would duplicate
		}

		errMsg := err.Error()
		if !strings.Contains(errMsg, "rate_limit") && !strings.Contains(errMsg, "overloaded") {
			return nil, err
		}

		if attempt < len(p.backoffs) {
			p.logger.Warn("anthropic stream retryable error",
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

	return nil, fmt.Errorf("anthropic stream failed after %d retries: %w", len(p.backoffs), lastErr)
}

// streamOnce performs a single streaming attempt. The bool reports whether any
// text or tool_use reached cb — once true the request must not be retried.
func (p *Provider) streamOnce(ctx context.Context, req bs.CompletionRequest, cb *bs.StreamCallbacks) (*bs.CompletionResponse, bool, error) {
	apiReq := apiRequest{
		Model:     req.Model,
		MaxTokens: req.MaxTokens,
		System:    buildSystem(req.System, p.oauth),
		Messages:  buildMessages(req.Messages),
		Tools:     buildTools(req.Tools),
		Stream:    true,
	}

	applyThinkingAndEffort(&apiReq, req)

	body, err := json.Marshal(apiReq)
	if err != nil {
		return nil, false, fmt.Errorf("marshal request: %w", err)
	}

	p.logger.Info("anthropic API request (stream)",
		"model", req.Model,
		"messages", len(req.Messages),
		"tools", len(req.Tools),
		"system_len", len(req.System),
		"body_bytes", len(body),
	)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", messagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("anthropic-beta", "oauth-2025-04-20")

	bearer := p.apiKey
	if p.oauth {
		tok, err := p.tokenSource()
		if err != nil {
			return nil, false, fmt.Errorf("anthropic-oauth auth: %w", err)
		}
		bearer = tok
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)

	// Streaming responses have no fixed length, so use a client with no global
	// timeout and rely on ctx for cancellation. The transport is shared with
	// p.httpClient so tests can inject a mock RoundTripper.
	streamClient := &http.Client{Transport: p.httpClient.Transport}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, false, fmt.Errorf("anthropic stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, false, fmt.Errorf("anthropic stream API status %d: %s", resp.StatusCode, errBody)
	}

	return parseAnthropicStream(ctx, resp.Body, cb)
}

// parseAnthropicStream consumes an Anthropic Messages API SSE stream. It
// dispatches cb.OnText for text deltas, cb.OnThinking for thinking deltas,
// and cb.OnToolUse once a tool_use block's input JSON is fully assembled
// (at content_block_stop), then assembles the final content blocks: text and
// tool_use are returned; thinking blocks are dropped from the response
// (matching Complete's filter — they're already delivered live via
// cb.OnThinking). The bool reports whether any text or tool_use reached cb.
func parseAnthropicStream(ctx context.Context, body io.Reader, cb *bs.StreamCallbacks) (*bs.CompletionResponse, bool, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	// blockState accumulates one content block, keyed by its stream index.
	type blockState struct {
		kind     string // "text" | "tool_use" | "thinking"
		text     strings.Builder
		toolID   string
		toolName string
		toolArgs strings.Builder
	}
	blocks := map[int]*blockState{}
	var ordered []int

	var (
		stopReason string
		usage      bs.Usage
		emitted    bool
	)

	for scanner.Scan() {
		// Fast-path cancellation between events. A cancel mid-read also surfaces
		// via scanner.Err() below once the transport aborts the body.
		select {
		case <-ctx.Done():
			return nil, emitted, ctx.Err()
		default:
		}

		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue // SSE "event:" lines and blank separators — type is in data
		}
		data := strings.TrimPrefix(line, "data: ")

		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(data), &head) != nil {
			continue
		}

		switch head.Type {
		case "message_start":
			var ev struct {
				Message struct {
					Usage bs.Usage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				usage = ev.Message.Usage
			}

		case "content_block_start":
			var ev struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			switch ev.ContentBlock.Type {
			case "text", "tool_use":
				blocks[ev.Index] = &blockState{
					kind:     ev.ContentBlock.Type,
					toolID:   ev.ContentBlock.ID,
					toolName: ev.ContentBlock.Name,
				}
				ordered = append(ordered, ev.Index)
			case "thinking", "redacted_thinking":
				// Track thinking blocks so deltas can stream live via cb.OnThinking.
				// They are NOT appended to `ordered` — the final response drops
				// them (Complete does the same filter).
				blocks[ev.Index] = &blockState{kind: "thinking"}
			}

		case "content_block_delta":
			var ev struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			st := blocks[ev.Index]
			if st == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					st.text.WriteString(ev.Delta.Text)
					if cb != nil && cb.OnText != nil {
						cb.OnText(ev.Delta.Text)
					}
					emitted = true
				}
			case "input_json_delta":
				st.toolArgs.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				if ev.Delta.Thinking != "" && cb != nil && cb.OnThinking != nil {
					cb.OnThinking(ev.Delta.Thinking)
				}
			}

		case "content_block_stop":
			var ev struct {
				Index int `json:"index"`
			}
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}
			st := blocks[ev.Index]
			if st == nil || st.kind != "tool_use" {
				continue
			}
			// Tool input is now fully assembled — surface it so a UI can
			// render the call before agent loop executes it.
			raw := json.RawMessage(st.toolArgs.String())
			if !json.Valid(raw) {
				raw = json.RawMessage("{}")
			}
			if cb != nil && cb.OnToolUse != nil {
				cb.OnToolUse(st.toolID, st.toolName, raw)
				emitted = true
			}

		case "message_delta":
			var ev struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				if ev.Delta.StopReason != "" {
					stopReason = ev.Delta.StopReason
				}
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
			}

		case "error":
			var ev struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Error.Message != "" {
				return nil, emitted, fmt.Errorf("anthropic stream %s: %s", ev.Error.Type, ev.Error.Message)
			}
			return nil, emitted, fmt.Errorf("anthropic stream error")
		}
	}

	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, emitted, ctx.Err()
		}
		return nil, emitted, fmt.Errorf("anthropic stream read: %w", err)
	}

	content := make([]bs.ContentBlock, 0, len(ordered))
	for _, idx := range ordered {
		st := blocks[idx]
		switch st.kind {
		case "text":
			if st.text.Len() > 0 {
				content = append(content, bs.ContentBlock{Type: "text", Text: st.text.String()})
			}
		case "tool_use":
			raw := json.RawMessage(st.toolArgs.String())
			if !json.Valid(raw) {
				raw = json.RawMessage("{}")
			}
			content = append(content, bs.ContentBlock{
				Type:  "tool_use",
				ID:    st.toolID,
				Name:  st.toolName,
				Input: raw,
			})
		}
	}

	return &bs.CompletionResponse{
		Content:    content,
		StopReason: stopReason,
		Usage:      usage,
	}, emitted, nil
}
