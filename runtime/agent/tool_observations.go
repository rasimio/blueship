package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	bs "github.com/rasimio/blueship/internal/core"
)

const (
	recentToolObservationLimit     = 3
	recentToolObservationMaxAge    = 2 * time.Hour
	maxToolObservationInputChars   = 500
	maxToolObservationOutputChars  = 1500
	maxErroredToolObservationChars = 1000
)

func (a *Loop) recentToolObservationContext(ctx context.Context, cfg RunConfig) string {
	if a == nil || a.store == nil || cfg.SessionID == "" {
		return ""
	}
	since := time.Now().Add(-recentToolObservationMaxAge)
	observations, err := a.store.RecentToolObservations(ctx, cfg.SessionID, since, recentToolObservationLimit)
	if err != nil {
		if a.logger != nil {
			a.logger.Warn("recent tool observations unavailable", "error", err)
		}
		return ""
	}
	return formatToolObservations(observations)
}

func formatToolObservations(observations []bs.ToolObservation) string {
	if len(observations) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("[recent tool observations]\n")
	b.WriteString("Past tool results from this session. Treat them as time-stamped snapshots; refresh tools if current state matters.\n")
	for i := len(observations) - 1; i >= 0; i-- {
		obs := observations[i]
		name := strings.TrimSpace(obs.Name)
		if name == "" {
			name = "unknown_tool"
		}
		fmt.Fprintf(&b, "- observed_at: %s\n", obs.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "  tool: %s\n", name)
		if input := truncateForPrompt(strings.TrimSpace(obs.Input), maxToolObservationInputChars); input != "" {
			fmt.Fprintf(&b, "  input: %s\n", indentObservationValue(input))
		}
		outputLimit := maxToolObservationOutputChars
		if obs.IsError {
			outputLimit = maxErroredToolObservationChars
			b.WriteString("  error: true\n")
		}
		output := compactToolResultForPrompt(name, obs.Output, obs.IsError)
		output = truncateForPrompt(strings.TrimSpace(output), outputLimit)
		if output != "" {
			fmt.Fprintf(&b, "  output: %s\n", indentObservationValue(output))
		}
	}
	b.WriteString("[/recent tool observations]")
	return b.String()
}

func indentObservationValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !strings.Contains(value, "\n") {
		return value
	}
	return "\n    " + strings.ReplaceAll(value, "\n", "\n    ")
}
