package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// SlogHandler is the BlueShip-flavoured slog.Handler. It wraps any inner
// handler (text/json/file…) and adds two responsibilities:
//
//  1. Trace correlation. If the context carries an active OTel span, the
//     record is enriched with trace_id / span_id attributes. Downstream
//     log aggregators can then jump from a log line to the full trace.
//
//  2. Alert routing. If alerter != nil and the record meets minAlert,
//     the same record (level + message + attrs) is forwarded to the
//     Telegram sink. The forward runs first; the inner handler still
//     receives the record so file/stderr keep the audit trail.
type SlogHandler struct {
	next     slog.Handler
	alerter  *Alerter
	minAlert slog.Level
}

// NewSlogHandler builds the chain. Pass alerter=nil to disable Telegram
// routing; the handler still does trace correlation. minAlert defaults
// to Error if zero.
func NewSlogHandler(next slog.Handler, alerter *Alerter, minAlert slog.Level) *SlogHandler {
	if minAlert == 0 {
		minAlert = slog.LevelError
	}
	return &SlogHandler{next: next, alerter: alerter, minAlert: minAlert}
}

// ParseLevel maps a config string ("warn"/"error"/...) to slog.Level.
// Falls back to Error on unknown input — strict default beats silent
// over-paging.
func ParseLevel(s string) slog.Level {
	switch s {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "":
		return slog.LevelError
	}
	return slog.LevelError
}

func (h *SlogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		sc := span.SpanContext()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}

	if h.alerter != nil && r.Level >= h.minAlert {
		var attrs []slog.Attr
		r.Attrs(func(a slog.Attr) bool {
			attrs = append(attrs, a)
			return true
		})
		h.alerter.Send(ctx, r.Level, r.Message, attrs...)
	}

	return h.next.Handle(ctx, r)
}

func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{next: h.next.WithAttrs(attrs), alerter: h.alerter, minAlert: h.minAlert}
}

func (h *SlogHandler) WithGroup(name string) slog.Handler {
	return &SlogHandler{next: h.next.WithGroup(name), alerter: h.alerter, minAlert: h.minAlert}
}
