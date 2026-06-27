package openaicodex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	bs "github.com/rasimio/blueship/internal/core"
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

	return parseSSEStream(resp.Body, cb, toolNameSet(req.Tools))
}

func parseSSEStream(body interface{ Read([]byte) (int, error) }, cb *bs.StreamCallbacks, knownTools map[string]bool) (*bs.CompletionResponse, error) {
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

	flushText := func() {
		if currentText.Len() == 0 {
			return
		}
		raw := currentText.String()
		currentText.Reset()

		clean, xmlCalls := extractXMLToolProtocol(raw, knownTools)
		if clean != "" {
			allBlocks = append(allBlocks, bs.ContentBlock{Type: "text", Text: clean})
			if cb != nil && cb.OnText != nil {
				cb.OnText(clean)
			}
		}
		for _, call := range xmlCalls {
			allBlocks = append(allBlocks, call)
			if cb != nil && cb.OnToolUse != nil {
				cb.OnToolUse(call.ID, call.Name, call.Input)
			}
		}
	}

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
				// Flush accumulated text after sanitizing Codex XML-style
				// tool protocol. The ChatGPT Codex backend sometimes emits
				// <tool_use>/<tool_result> as output_text instead of native
				// function_call events; those must never be streamed or
				// persisted as user-visible assistant text.
				flushText()
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
	flushText()

	stopReason := mapStatus(status, allBlocks)

	return &bs.CompletionResponse{
		Content:    allBlocks,
		StopReason: stopReason,
		Usage:      usage,
	}, nil
}

var (
	xmlToolUseRe    = regexp.MustCompile(`(?s)<tool_use\b[^>]*>.*?</tool_use>`)
	xmlToolResultRe = regexp.MustCompile(`(?s)<tool_result\b[^>]*>.*?</tool_result>`)
	xmlToolNameRe   = regexp.MustCompile(`(?s)\bname\s*=\s*["']([^"']+)["']`)
	xmlArgumentsRe  = regexp.MustCompile(`(?s)<arguments>\s*(.*?)\s*</arguments>`)
)

func toolNameSet(tools []bs.ToolDefinition) map[string]bool {
	if len(tools) == 0 {
		return nil
	}
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		if t.Name != "" {
			out[t.Name] = true
		}
	}
	return out
}

func extractXMLToolProtocol(text string, knownTools map[string]bool) (string, []bs.ContentBlock) {
	matches := xmlToolUseRe.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return strings.TrimSpace(stripXMLToolResults(text)), nil
	}

	var calls []bs.ContentBlock
	firstCallStart := -1
	for _, m := range matches {
		block := text[m[0]:m[1]]
		name := xmlToolName(block)
		if !isKnownTool(name, knownTools) {
			continue
		}
		if firstCallStart == -1 || m[0] < firstCallStart {
			firstCallStart = m[0]
		}
		calls = append(calls, bs.ContentBlock{
			Type:  "tool_use",
			ID:    "call_xml_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			Name:  name,
			Input: xmlToolArguments(block),
		})
	}
	if len(calls) == 0 {
		clean := stripXMLToolResults(text)
		clean = stripXMLToolUses(clean)
		return strings.TrimSpace(clean), nil
	}

	// Preserve only natural language before the first XML tool call. Anything
	// after a tool call belongs to the leaked protocol/fake continuation and
	// would be accumulated into the final user-visible reply.
	clean := text[:firstCallStart]
	clean = stripXMLToolResults(clean)
	clean = stripXMLToolUses(clean)
	return strings.TrimSpace(clean), calls
}

func stripXMLToolUses(text string) string {
	return xmlToolUseRe.ReplaceAllString(text, "")
}

func stripXMLToolResults(text string) string {
	return xmlToolResultRe.ReplaceAllString(text, "")
}

func xmlToolName(block string) string {
	m := xmlToolNameRe.FindStringSubmatch(block)
	if len(m) != 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func xmlToolArguments(block string) json.RawMessage {
	args := "{}"
	if m := xmlArgumentsRe.FindStringSubmatch(block); len(m) == 2 {
		args = strings.TrimSpace(m[1])
	} else if start := strings.Index(block, ">"); start >= 0 {
		end := strings.LastIndex(block, "</tool_use>")
		if end > start {
			args = strings.TrimSpace(block[start+1 : end])
		}
	}
	raw := json.RawMessage(args)
	if !json.Valid(raw) {
		raw = json.RawMessage("{}")
	}
	return raw
}

func isKnownTool(name string, knownTools map[string]bool) bool {
	return len(knownTools) > 0 && knownTools[name]
}

func readLimited(r interface{ Read([]byte) (int, error) }, max int) ([]byte, error) {
	buf := make([]byte, max)
	n, _ := r.Read(buf)
	return buf[:n], nil
}
