package gemini

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/core"
)

const generateContentURL = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// CompletionProvider implements bs.CompletionProvider using Gemini generateContent.
type CompletionProvider struct {
	apiKey      string
	httpClient  *http.Client
	logger      *slog.Logger
	generateURL string
}

// NewCompletionProvider creates a new Gemini completion provider.
func NewCompletionProvider(apiKey string, timeout time.Duration) *CompletionProvider {
	return &CompletionProvider{
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: timeout},
		logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
		generateURL: generateContentURL,
	}
}

type generateRequest struct {
	SystemInstruction *content      `json:"systemInstruction,omitempty"`
	Contents          []content     `json:"contents"`
	Tools             []toolWrapper `json:"tools,omitempty"`
	GenerationConfig  *genConfig    `json:"generationConfig,omitempty"`
}

type genConfig struct {
	MaxOutputTokens int             `json:"maxOutputTokens,omitempty"`
	ThinkingConfig  *thinkingConfig `json:"thinkingConfig,omitempty"`
}

type thinkingConfig struct {
	ThinkingBudget int `json:"thinkingBudget"`
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
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
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
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount,omitempty"`
		CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
		TotalTokenCount      int `json:"totalTokenCount,omitempty"`
	} `json:"usageMetadata,omitempty"`
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

	generation := &genConfig{MaxOutputTokens: req.MaxTokens}
	if req.ThinkingBudget >= 0 {
		generation.ThinkingConfig = &thinkingConfig{ThinkingBudget: req.ThinkingBudget}
	}

	payload := generateRequest{
		SystemInstruction: sys,
		Contents:          contents,
		Tools:             buildTools(req.Tools),
		GenerationConfig:  generation,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := fmt.Sprintf(p.generateURL, req.Model, p.apiKey)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
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

	stopReason := mapStopReason(cand.FinishReason)
	if hasToolUseBlock(blocks) {
		stopReason = "tool_use"
	}

	return &bs.CompletionResponse{
		Content:    blocks,
		StopReason: stopReason,
		Usage:      toUsage(result.UsageMetadata),
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
				out = append(out, content{Role: "user", Parts: []part{tp}})
			}
		case "assistant":
			modelParts := buildModelParts(blocks)
			if len(modelParts) > 0 {
				out = append(out, content{Role: "model", Parts: modelParts})
			}
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
				parts = append(parts, part{InlineData: &inlineData{MimeType: b.Source.MediaType, Data: b.Source.Data}})
			}
		case "tool_result":
			toolParts = append(toolParts, part{FunctionResponse: &functionResponse{Name: b.Name, Response: normalizeFunctionResponse(b.Content)}})
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
				parts = append(parts, part{Text: b.Text, ThoughtSignature: b.ThoughtSignature})
			}
		case "tool_use":
			args := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &args)
			}
			parts = append(parts, part{FunctionCall: &functionCall{Name: b.Name, Args: args}, ThoughtSignature: b.ThoughtSignature})
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
		funcs = append(funcs, functionDecl{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	return []toolWrapper{{FunctionDeclarations: funcs}}
}

func toContentBlocks(c content) []bs.ContentBlock {
	var blocks []bs.ContentBlock
	for i, p := range c.Parts {
		if p.Text != "" {
			blocks = append(blocks, bs.ContentBlock{Type: "text", Text: p.Text, ThoughtSignature: p.ThoughtSignature})
		}
		if p.FunctionCall != nil {
			rawArgs, _ := json.Marshal(p.FunctionCall.Args)
			blocks = append(blocks, bs.ContentBlock{
				Type:             "tool_use",
				ID:               createToolUseID(p.FunctionCall.Name, rawArgs, p.ThoughtSignature, i),
				Name:             p.FunctionCall.Name,
				Input:            rawArgs,
				ThoughtSignature: p.ThoughtSignature,
			})
		}
	}
	return blocks
}

func createToolUseID(name string, rawArgs []byte, thoughtSignature string, index int) string {
	seed := fmt.Sprintf("%s|%s|%s|%d", name, rawArgs, thoughtSignature, index)
	sum := sha1.Sum([]byte(seed))
	return "gemini_" + hex.EncodeToString(sum[:8])
}

func hasToolUseBlock(blocks []bs.ContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "tool_use" {
			return true
		}
	}
	return false
}

func toUsage(meta *struct {
	PromptTokenCount     int `json:"promptTokenCount,omitempty"`
	CandidatesTokenCount int `json:"candidatesTokenCount,omitempty"`
	TotalTokenCount      int `json:"totalTokenCount,omitempty"`
}) bs.Usage {
	if meta == nil {
		return bs.Usage{}
	}
	input := meta.PromptTokenCount
	output := meta.CandidatesTokenCount
	if input == 0 && meta.TotalTokenCount > output {
		input = meta.TotalTokenCount - output
	}
	if output == 0 && meta.TotalTokenCount > input {
		output = meta.TotalTokenCount - input
	}
	return bs.Usage{InputTokens: input, OutputTokens: output}
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
