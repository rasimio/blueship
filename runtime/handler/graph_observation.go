package handler

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func renderGraphReport(ctx context.Context, task core.AgentTask, deps core.AgentDeps, report string) (string, error) {
	report, err := renderGraphCalculations(report)
	if err != nil || !strings.Contains(report, "{{observed_at:") {
		return report, err
	}
	if deps.DB == nil {
		return "", fmt.Errorf("saved source observation store unavailable")
	}
	db, err := deps.DB("ship")
	if err != nil {
		return "", err
	}
	observations, err := core.NewAgentTaskStore(db).TaskSourceObservations(ctx, task.ID, task.UserID, task.SoulID)
	if err != nil {
		return "", err
	}
	return renderGraphObservations(report, observations)
}

// Resolve dates only from the task's own genuine reads. If a URL was read at
// multiple times the template must identify one exact observed_at value; never
// silently attach the newest read's freshness to an older price or statement.
func renderGraphObservations(report string, observations []core.TaskSourceObservation) (string, error) {
	const prefix = "{{observed_at:"
	var out strings.Builder
	for count := 0; ; count++ {
		start := strings.Index(report, prefix)
		if start < 0 {
			out.WriteString(report)
			return out.String(), nil
		}
		if count >= 128 {
			return "", fmt.Errorf("too many observation markers")
		}
		out.WriteString(report[:start])
		rest := report[start+len(prefix):]
		end := strings.Index(rest, "}}")
		if end < 0 || end > 4096 {
			return "", fmt.Errorf("unclosed or oversized observation marker")
		}
		sourceURL := strings.TrimSpace(rest[:end])
		var selected time.Time
		explicit := false
		// Semicolons are valid URL characters. A selector uses "; " followed
		// by RFC3339; URL characters (even a date after ;) remain in the URL.
		if i := strings.LastIndex(sourceURL, "; "); i >= 0 {
			if stamp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(sourceURL[i+1:])); err == nil {
				selected = stamp
				explicit = true
				sourceURL = strings.TrimSpace(sourceURL[:i])
			}
		}
		parsed, err := url.Parse(sourceURL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || strings.ContainsAny(sourceURL, " \t\r\n") {
			return "", fmt.Errorf("observation marker requires a source URL and optional recorded RFC3339 timestamp")
		}
		var found time.Time
		for _, observation := range observations {
			matched := false
			for _, alias := range observation.URLs {
				if core.SameDocURL(sourceURL, alias) {
					matched = true
					break
				}
			}
			if !matched || observation.ObservedAt.IsZero() || (explicit && !observation.ObservedAt.Equal(selected)) {
				continue
			}
			if !found.IsZero() && !found.Equal(observation.ObservedAt) {
				return "", fmt.Errorf("source has multiple observations; specify its exact recorded timestamp")
			}
			found = observation.ObservedAt
		}
		if found.IsZero() {
			return "", fmt.Errorf("source observation not found in this task")
		}
		out.WriteString(found.UTC().Format(time.RFC3339Nano))
		report = rest[end+2:]
	}
}
