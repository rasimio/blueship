package telemetry

import (
	"context"
	"fmt"
	"io"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation library name attached to every span
// emitted by the framework. Hosts that want their own scope should call
// otel.Tracer("their-name") directly after the global TracerProvider is
// installed.
const TracerName = "blueship"

// Shutdown is returned by NewTracer; hosts call it during graceful
// shutdown to flush queued spans. nil-safe.
type Shutdown func(context.Context) error

// NewTracer installs a global OTel TracerProvider and returns a Tracer
// scoped to the framework. When Tracing.Enabled is false the function
// returns a no-op tracer plus a no-op Shutdown — instrumentation calls
// at the call sites stay free of conditional checks.
func NewTracer(cfg Config) (trace.Tracer, Shutdown, error) {
	if !cfg.Tracing.Enabled || cfg.Tracing.Exporter == "" || cfg.Tracing.Exporter == "none" {
		return noop.NewTracerProvider().Tracer(TracerName), noopShutdown, nil
	}

	exp, closeExp, err := newExporter(cfg.Tracing)
	if err != nil {
		return nil, nil, fmt.Errorf("telemetry: build exporter: %w", err)
	}

	// resource.Default() pins one OTel schema URL, semconv pins another;
	// merging them refuses with "conflicting Schema URL". Skip the merge —
	// we only need service.name for routing/filtering, and the SDK-default
	// attrs (runtime, host) still come through via the SDK regardless.
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	)

	sampleRate := cfg.Tracing.SampleRate
	if sampleRate <= 0 {
		sampleRate = 1.0
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(sampleRate))),
	)
	otel.SetTracerProvider(tp)

	shutdown := func(ctx context.Context) error {
		err := tp.Shutdown(ctx)
		if closeExp != nil {
			_ = closeExp()
		}
		return err
	}
	return tp.Tracer(TracerName), shutdown, nil
}

// newExporter resolves the configured exporter. "stdout" writes pretty
// JSON spans to stderr; "file" appends compact JSON spans to FilePath
// (so log shippers / Tempo's file receiver can pick them up later).
func newExporter(cfg Tracing) (sdktrace.SpanExporter, func() error, error) {
	switch cfg.Exporter {
	case "stdout":
		exp, err := stdouttrace.New(stdouttrace.WithPrettyPrint())
		return exp, nil, err
	case "file":
		if cfg.FilePath == "" {
			return nil, nil, fmt.Errorf("telemetry: file exporter requires file_path")
		}
		f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("open trace file: %w", err)
		}
		exp, err := stdouttrace.New(stdouttrace.WithWriter(f))
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		return exp, func() error { return closeIfWriter(f) }, nil
	default:
		return nil, nil, fmt.Errorf("unknown trace exporter %q", cfg.Exporter)
	}
}

func noopShutdown(context.Context) error { return nil }

// closeIfWriter avoids importing io just for one call site.
func closeIfWriter(w io.Closer) error { return w.Close() }
