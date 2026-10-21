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
	// Revisable reports that the run can take this draft back: instead of
	// replacement text the validator may return a *ResponseRevision, and the
	// model answers again, with its tools, after reading the feedback. False
	// for a draft with pending tool calls, on a closing turn without tools,
	// once the run has used its revision, and outside an interactive run.
	Revisable bool `json:"revisable,omitempty"`
}

// ResponseValidator returns safe visible text, or an error to stop publication.
// When set, the agent buffers text until validation, before persistence and
// streaming. Domain-specific interpretation belongs to the host.
type ResponseValidator func(context.Context, ResponseValidationRequest) (string, error)

// ResponseRevision is a validator's answer for a Revisable draft that the
// model should redo rather than have rewritten: typically a draft that
// claims an action no tool performed. The draft is neither persisted nor
// shown; the model reads Feedback as the next message and answers again.
type ResponseRevision struct {
	Feedback string
}

func (r *ResponseRevision) Error() string { return "response revision requested" }

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
