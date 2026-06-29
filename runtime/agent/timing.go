package agent

import (
	"fmt"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

func emitTiming(cfg RunConfig, name string, started time.Time, detail string) {
	if cfg.OnTiming == nil || started.IsZero() {
		return
	}
	cfg.OnTiming(bs.TimingSpan{
		Name:       name,
		DurationMs: durationMs(time.Since(started)),
		Detail:     detail,
	})
}

func durationMs(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int(d / time.Millisecond)
}

func llmTimingDetail(cfg RunConfig, turn int, stopReason string, inputTokens, outputTokens int) string {
	return fmt.Sprintf("role=%s turn=%d model=%s stop=%s input_tokens=%d output_tokens=%d",
		cfg.Role, turn, cfg.Model, stopReason, inputTokens, outputTokens)
}

func toolTimingDetail(cfg RunConfig, turn int, name string, isError bool) string {
	return fmt.Sprintf("role=%s turn=%d tool=%s error=%t", cfg.Role, turn, name, isError)
}
