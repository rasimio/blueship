package openaicodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	bs "github.com/rasimio/blueship/core"
)

// StreamComplete sends a streaming request via the OpenAI Responses API SSE
// endpoint. Dispatches per-event callbacks: cb.OnText for each output_text
// delta as it arrives, and cb.OnToolUse once a function_call's arguments are
// fully assembled (on response.output_item.done for the function_call — the
// Responses API streams argument fragments via response.function_call_arguments.delta).
// cb may be nil; each field is independently nil-checked.
func (p *CompletionProvider) StreamComplete(ctx context.Context, req bs.CompletionRequest, cb *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	token, err := p.tokens.AccessToken()
	if err != nil {
		return nil, fmt.Errorf("openai-codex auth: %w", err)
	}

	payload := buildRequest(req)

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	p.logger.Info("openai-codex API request (stream)",
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

	// No global timeout for streaming.
	streamClient := &http.Client{}
	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-codex stream request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errBody []byte
		errBody, _ = readLimited(resp.Body, 2048)
		return nil, fmt.Errorf("openai-codex stream API returned %d: %s", resp.StatusCode, truncate(string(errBody), 500))
	}

	return parseSSEStream(resp.Body, cb)
}

func parseSSEStream(body interface{ Read([]byte) (int, error) }, cb *bs.StreamCallbacks) (*bs.CompletionResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	var (
		allBlocks []bs.ContentBlock
		usage     bs.Usage
		status    string

		// Accumulators for streaming items.
		currentText     strings.Builder
		currentToolArgs strings.Builder
	)

	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		if data == "[DONE]" {
			break
		}

		switch currentEvent {
		case "response.output_text.delta":
			var ev struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Delta != "" {
				currentText.WriteString(ev.Delta)
				if cb != nil && cb.OnText != nil {
					cb.OnText(ev.Delta)
				}
			}

		case "response.function_call_arguments.delta":
			var ev struct {
				Delta string `json:"delta"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				currentToolArgs.WriteString(ev.Delta)
			}

		case "response.output_item.added":
			var ev struct {
				Item outputItem `json:"item"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Item.Type == "function_call" {
				currentToolArgs.Reset()
			}

		case "response.output_item.done":
			var ev struct {
				Item outputItem `json:"item"`
			}
			if json.Unmarshal([]byte(data), &ev) != nil {
				continue
			}

			switch ev.Item.Type {
			case "message":
				// Flush accumulated text.
				if currentText.Len() > 0 {
					allBlocks = append(allBlocks, bs.ContentBlock{Type: "text", Text: currentText.String()})
					currentText.Reset()
				}
			case "function_call":
				args := ev.Item.Arguments
				if args == "" {
					args = currentToolArgs.String()
				}
				rawArgs := json.RawMessage(args)
				if !json.Valid(rawArgs) {
					rawArgs = json.RawMessage("{}")
				}
				allBlocks = append(allBlocks, bs.ContentBlock{
					Type:  "tool_use",
					ID:    ev.Item.CallID,
					Name:  ev.Item.Name,
					Input: rawArgs,
				})
				// Tool input is now fully assembled — surface live so a UI
				// can render the call before agent loop dispatches it.
				if cb != nil && cb.OnToolUse != nil {
					cb.OnToolUse(ev.Item.CallID, ev.Item.Name, rawArgs)
				}
				// tool finalized
				currentToolArgs.Reset()
			}

		case "response.completed":
			var ev struct {
				Response struct {
					Status string `json:"status"`
					Usage  struct {
						InputTokens  int `json:"input_tokens"`
						OutputTokens int `json:"output_tokens"`
					} `json:"usage"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil {
				status = ev.Response.Status
				usage = bs.Usage{
					InputTokens:  ev.Response.Usage.InputTokens,
					OutputTokens: ev.Response.Usage.OutputTokens,
				}
			}

		case "response.failed":
			var ev struct {
				Response struct {
					Error *struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"response"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Response.Error != nil {
				return nil, fmt.Errorf("openai-codex stream failed: %s", ev.Response.Error.Message)
			}
			return nil, fmt.Errorf("openai-codex stream failed")

		case "error":
			var ev struct {
				Message string `json:"message"`
			}
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Message != "" {
				return nil, fmt.Errorf("openai-codex stream error: %s", ev.Message)
			}
		}

		currentEvent = ""
	}

	// Flush any remaining accumulated text.
	if currentText.Len() > 0 {
		allBlocks = append(allBlocks, bs.ContentBlock{Type: "text", Text: currentText.String()})
	}

	stopReason := mapStatus(status, allBlocks)

	return &bs.CompletionResponse{
		Content:    allBlocks,
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

func readLimited(r interface{ Read([]byte) (int, error) }, max int) ([]byte, error) {
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return buf[:n], nil
}
