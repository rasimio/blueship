// Package telemetry provides observability primitives for BlueShip-based
// agents: distributed tracing (OTel), structured-log enrichment, and an
// error-only Telegram alert sink.
//
// Wiring is split into two layers:
//   - The framework (this package) builds a slog.Handler chain and an
//     OpenTelemetry tracer; it is host-agnostic.
//   - The host (arlene, future agents) loads Config from yaml/env and
//     hands the constructed Tracer + slog.Logger back into deps.
//
// Tracing is opt-in via Config.Tracing.Enabled. The Telegram alerter is
// opt-in via Config.Alerts.Telegram.Enabled. Both default to off so
// adding the framework hook costs nothing for hosts that don't configure
// observability yet.
package telemetry

import "time"

// Config groups the framework-level observability knobs. Hosts populate
// it from their own config struct (yaml/env) and pass it into NewTracer
// and NewAlerter.
type Config struct {
	ServiceName string  // resource.service.name on every emitted span
	Tracing     Tracing
	Alerts      Alerts
}

// Tracing controls span emission.
type Tracing struct {
	Enabled    bool
	Exporter   string  // "stdout" | "file" | "none"
	FilePath   string  // when Exporter == "file"
	SampleRate float64 // 0.0 — drop all, 1.0 — keep all
}

// Alerts groups error-routing sinks. Today only Telegram exists; new
// channels (Slack, PagerDuty…) plug in here without touching the slog
// handler chain.
type Alerts struct {
	Telegram TelegramAlertConfig
}

// TelegramAlertConfig configures the error-only Telegram sink.
//
// Throttle dedupes identical (level, message) pairs within the window —
// a tight loop screaming the same error doesn't flood the channel.
// MinLevel is the lowest level that triggers a Telegram send; everything
// quieter still flows through the structured-log chain.
type TelegramAlertConfig struct {
	Enabled  bool
	Token    string
	ChatID   string
	MinLevel string // "warn" | "error" — defaults to "error"
	Throttle time.Duration
}
