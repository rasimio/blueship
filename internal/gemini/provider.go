package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
)

const generateContentURL = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// CompletionProvider implements bs.CompletionProvider using Gemini generateContent.
type CompletionProvider struct {
	apiKey     string
	httpClient *http.Client
}

// NewCompletionProvider creates a new Gemini completion provider.
func NewCompletionProvider(apiKey string, timeout time.Duration) *CompletionProvider {
	return &CompletionProvider{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: timeout},
	}
}

type generateRequest struct {
	SystemInstruction *content       `json:"systemInstruction,omitempty"`
	Contents          []content      `json:"contents"`
	Tools             []toolWrapper  `json:"tools,omitempty"`
	GenerationConfig  *genConfig     `json:"generationConfig,omitempty"`
}

type genConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type toolWrapper struct {
	FunctionDeclarations []functionDecl `json:"functionDeclarations"`
}

type functionDecl struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type content struct {
	Role  string `json:"role"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	InlineData       *inlineData       `json:"inlineData,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type inlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

type functionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

type functionResponse struct {
	Name     string `json:"name"`
	Response any    `json:"response"`
}

type generateResponse struct {
	Candidates []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Complete sends a completion request to Gemini.
func (p *CompletionProvider) Complete(ctx context.Context, req bs.CompletionRequest) (*bs.CompletionResponse, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("gemini not configured: missing API key")
	}

	contents := buildContents(req.Messages)
	var sys *content
	if strings.TrimSpace(req.System) != "" {
		sys = &content{Role: "system", Parts: []part{{Text: req.System}}}
	}

	tools := buildTools(req.Tools)
	payload := generateRequest{
		SystemInstruction: sys,
		Contents:          contents,
		Tools:             tools,
		GenerationConfig:  &genConfig{MaxOutputTokens: req.MaxTokens},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(generateContentURL, req.Model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini request: %w", err)
	}
	defer resp.Body.Close()

	var result generateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("gemini API returned %d", resp.StatusCode)
		if result.Error != nil {
			msg += ": " + result.Error.Message
		}
		return nil, fmt.Errorf("%s", msg)
	}
	if len(result.Candidates) == 0 {
		return nil, fmt.Errorf("gemini returned empty candidates")
	}

	cand := result.Candidates[0]
	if len(cand.Content.Parts) == 0 {
		return nil, fmt.Errorf("gemini empty content: finishReason=%s", cand.FinishReason)
	}
	blocks := toContentBlocks(cand.Content)
	if len(blocks) == 0 {
		return nil, fmt.Errorf("gemini empty blocks: finishReason=%s", cand.FinishReason)
	}

	return &bs.CompletionResponse{
		Content: blocks,
		StopReason: mapStopReason(cand.FinishReason),
		Usage: bs.Usage{},
	}, nil
}

func buildContents(messages []bs.Message) []content {
	var out []content
	for _, msg := range messages {
		blocks := bs.NormalizeContent(msg.Content)
		switch msg.Role {
		case "user":
			userParts, toolParts := buildUserParts(blocks)
			if len(userParts) > 0 {
				out = append(out, content{Role: "user", Parts: userParts})
			}
			for _, tp := range toolParts {
				out = append(out, content{Role: "tool", Parts: []part{tp}})
			}
		case "assistant":
			out = append(out, content{Role: "model", Parts: buildModelParts(blocks)})
		}
	}
	return out
}

func buildUserParts(blocks []bs.ContentBlock) ([]part, []part) {
	var parts []part
	var toolParts []part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, part{Text: b.Text})
			}
		case "image":
			if b.Source != nil && b.Source.Type == "base64" && b.Source.MediaType != "" {
				parts = append(parts, part{
					InlineData: &inlineData{
						MimeType: b.Source.MediaType,
						Data:     b.Source.Data,
					},
				})
			}
		case "tool_result":
			toolParts = append(toolParts, part{
				FunctionResponse: &functionResponse{
					Name:     b.Name,
					Response: normalizeFunctionResponse(b.Content),
				},
			})
		}
	}
	return parts, toolParts
}

func normalizeFunctionResponse(content any) map[string]any {
	if content == nil {
		return map[string]any{"result": ""}
	}
	if m, ok := content.(map[string]any); ok {
		return m
	}
	if raw, ok := content.(json.RawMessage); ok {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			return wrapResult(v)
		}
	}
	if b, ok := content.([]byte); ok {
		var v any
		if json.Unmarshal(b, &v) == nil {
			return wrapResult(v)
		}
	}
	if s, ok := content.(string); ok {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			return wrapResult(v)
		}
		return map[string]any{"result": s}
	}
	return wrapResult(content)
}

func wrapResult(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": v}
}

func buildModelParts(blocks []bs.ContentBlock) []part {
	var parts []part
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, part{Text: b.Text})
			}
		case "tool_use":
			args := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &args)
			}
			parts = append(parts, part{
				FunctionCall: &functionCall{
					Name: b.Name,
					Args: args,
				},
			})
		}
	}
	return parts
}

func buildTools(tools []bs.ToolDefinition) []toolWrapper {
	if len(tools) == 0 {
		return nil
	}
	funcs := make([]functionDecl, 0, len(tools))
	for _, t := range tools {
		funcs = append(funcs, functionDecl{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  t.InputSchema,
		})
	}
	return []toolWrapper{{FunctionDeclarations: funcs}}
}

func toContentBlocks(c content) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	for _, p := range c.Parts {
		if p.Text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: p.Text})
		}
		if p.FunctionCall != nil {
			rawArgs, _ := json.Marshal(p.FunctionCall.Args)
			blocks = append(blocks, bs.ContentBlock{
				Type:  "tool_use",
				Name:  p.FunctionCall.Name,
				Input: rawArgs,
			})
		}
	}
	return blocks
}

func mapStopReason(reason string) string {
	switch reason {
	case "MAX_TOKENS":
		return "max_tokens"
	case "TOOL_CALLS":
		return "tool_use"
	default:
		return "end_turn"
	}
}
