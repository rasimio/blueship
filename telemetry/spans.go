package telemetry

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Typed span helpers keep attribute keys consistent across the codebase.
// They live here (vs. inline at call sites) so a single rename in this
// file fans out everywhere — also makes it trivial to grep "what spans
// does the framework emit?".
//
// All helpers return the child context plus the span; callers must
// `defer span.End()`. RecordError is a no-op when err == nil so call
// sites can `defer telemetry.RecordError(span, err)` after assigning to
// a named return.

// StartTaskSpan opens the root span for one execution of an agent_task
// (one scheduler tick that actually fires a handler). Children — tool
// calls, LLM calls, sub-iterations — attach via the returned context.
func StartTaskSpan(ctx context.Context, taskID, handler, strategy, dispatch string, iteration int) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, "agent_task.run",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("agent_task.id", taskID),
			attribute.String("agent_task.handler", handler),
			attribute.String("agent_task.strategy", strategy),
			attribute.String("agent_task.dispatch", dispatch),
			attribute.Int("agent_task.iteration", iteration),
		),
	)
}

// StartIterationSpan opens a child span for one LLM-loop iteration
// inside a Background-style handler.
func StartIterationSpan(ctx context.Context, instructionKey string, iteration, maxIterations int) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, "agent_task.iteration",
		trace.WithAttributes(
			attribute.String("agent_task.instruction_key", instructionKey),
			attribute.Int("agent_task.iteration", iteration),
			attribute.Int("agent_task.max_iterations", maxIterations),
		),
	)
}

// StartToolSpan opens a span around a single tool invocation. The span
// name is "tool.<name>" so traces group by tool when sorted.
func StartToolSpan(ctx context.Context, name string, inputSize int) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, "tool."+name,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("tool.name", name),
			attribute.Int("tool.input_size_bytes", inputSize),
		),
	)
}

// StartLLMSpan opens a span around a single LLM completion call. Token
// counts and stop reason are attached after the call returns via
// AnnotateLLMResult.
func StartLLMSpan(ctx context.Context, provider, model string) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, "llm.complete",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("llm.provider", provider),
			attribute.String("llm.model", model),
		),
	)
}

// AnnotateToolResult adds output size + error flag to the tool span just
// before End. Pulled out as a helper so call sites stay a one-liner
// regardless of which fields exist.
func AnnotateToolResult(span trace.Span, outputSize int, isError bool) {
	span.SetAttributes(
		attribute.Int("tool.output_size_bytes", outputSize),
		attribute.Bool("tool.is_error", isError),
	)
	if isError {
		span.SetStatus(codes.Error, "tool returned error")
	}
}

// AnnotateLLMResult attaches token counts and stop reason after the LLM
// call returns. Zero values are safe — they just won't be filterable.
func AnnotateLLMResult(span trace.Span, inputTokens, outputTokens int, stopReason string) {
	if inputTokens > 0 {
		span.SetAttributes(attribute.Int("llm.tokens.input", inputTokens))
	}
	if outputTokens > 0 {
		span.SetAttributes(attribute.Int("llm.tokens.output", outputTokens))
	}
	if stopReason != "" {
		span.SetAttributes(attribute.String("llm.stop_reason", stopReason))
	}
}

// RecordError marks a span as failed and records the error. nil-safe so
// call sites can use it inside a deferred wrapper that captures the
// named return regardless of branch.
func RecordError(span trace.Span, err error) {
	if err == nil || span == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
