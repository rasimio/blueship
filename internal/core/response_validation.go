package core

import (
	"context"
	"encoding/json"
)

// ToolExecutionResult carries actual execution evidence, never a truncated
// debug preview or a model's proposed call.
type ToolExecutionResult struct {
	Name    string          `json:"name"`
	Input   json.RawMessage `json:"input"`
	Output  string          `json:"output"`
	IsError bool            `json:"is_error"`
}

type ResponseValidationRequest struct {
	SessionID       string                `json:"session_id,omitempty"`
	CurrentDatetime string                `json:"current_datetime"`
	Timezone        string                `json:"timezone,omitempty"`
	Text            string                `json:"text"`
	UserText        string                `json:"user_text"`
	Tools           []ToolExecutionResult `json:"tools"`
	PendingTools    []string              `json:"pending_tools,omitempty"`
}

// ResponseValidator returns safe visible text, or an error to stop publication.
// When set, the agent buffers text until validation, before persistence and
// streaming. Domain-specific interpretation belongs to the host.
type ResponseValidator func(context.Context, ResponseValidationRequest) (string, error)

type responseValidationContextKey struct{}

// WithResponseValidationContext makes the completed calls of this invocation
// available to outgoing tools. Each snapshot precedes its tool's execution.
func WithResponseValidationContext(ctx context.Context, req ResponseValidationRequest) context.Context {
	req.Tools = append([]ToolExecutionResult(nil), req.Tools...)
	req.PendingTools = nil
	return context.WithValue(ctx, responseValidationContextKey{}, req)
}

func ResponseValidationContext(ctx context.Context) (ResponseValidationRequest, bool) {
	req, ok := ctx.Value(responseValidationContextKey{}).(ResponseValidationRequest)
	return req, ok
}
