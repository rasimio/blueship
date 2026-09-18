package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func validateResponse(ctx context.Context, cfg RunConfig, userMessage any, content []bs.ContentBlock, receipts []bs.ToolExecutionResult) ([]bs.ContentBlock, error) {
	if cfg.ResponseValidator == nil || strings.TrimSpace(bs.ExtractText(content)) == "" {
		return content, nil
	}
	request := responseValidationRequest(cfg, userMessage, receipts)
	request.Text = bs.ExtractText(content)
	for _, block := range content {
		if block.Type == "tool_use" {
			request.PendingTools = append(request.PendingTools, block.Name)
		}
	}
	safe, err := cfg.ResponseValidator(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("validate response: %w", err)
	}
	if safe == request.Text {
		return content, nil
	}
	if strings.TrimSpace(safe) == "" && len(request.PendingTools) == 0 {
		return nil, fmt.Errorf("validate response: empty terminal replacement")
	}
	out := make([]bs.ContentBlock, 0, len(content))
	placed := false
	for _, block := range content {
		if block.Type == "text" {
			if !placed && safe != "" {
				out = append(out, bs.ContentBlock{Type: "text", Text: safe})
			}
			placed = true
			continue
		}
		out = append(out, block)
	}
	return out, nil
}

func responseValidationRequest(cfg RunConfig, userMessage any, receipts []bs.ToolExecutionResult) bs.ResponseValidationRequest {
	var userText string
	switch value := userMessage.(type) {
	case string:
		userText = value
	case []bs.ContentBlock:
		userText = bs.ExtractText(value)
	}
	if cfg.VisibleUserText != nil {
		userText = *cfg.VisibleUserText
	}
	now := cfg.TurnNow
	if now.IsZero() {
		now = time.Now().UTC()
	}
	request := bs.ResponseValidationRequest{UserText: userText, Tools: receipts,
		SessionID: cfg.SessionID, CurrentDatetime: now.Format(time.RFC3339)}
	if !cfg.TurnNow.IsZero() {
		request.Timezone = cfg.TurnNow.Location().String()
	}
	return request
}
