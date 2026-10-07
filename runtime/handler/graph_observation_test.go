package handler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rasimio/blueship/internal/core"
)

func TestGraphObservationsPreserveSourceIdentityAndPrecision(t *testing.T) {
	at := time.Date(2026, 9, 22, 21, 55, 49, 566826000, time.UTC)
	observations := []core.TaskSourceObservation{{
		URLs: []string{"https://example.test/requested", "https://example.test/item;a=1?q=x;y", "https://example.test/item;2026-09-22T21:55:49Z"}, ObservedAt: at,
	}}
	for _, source := range observations[0].URLs {
		for _, selector := range []string{"", "; " + at.Format(time.RFC3339Nano)} {
			got, err := renderGraphObservations("Seen {{observed_at: "+source+selector+"}}; total 10 EUR", observations)
			if err != nil || got != "Seen 2026-09-22T21:55:49.566826Z; total 10 EUR" {
				t.Fatal(got, err)
			}
		}
	}
	// A repeated identical observation is not a different source version.
	observations = append(observations, observations[0])
	marker := "{{observed_at: https://example.test/requested}}"
	if _, err := renderGraphObservations(marker, observations); err != nil {
		t.Fatal(err)
	}
	// Two genuine reads in the same second require an exact selector. The
	// rendered branch result must remain usable by the downstream finalizer.
	observations = append(observations, core.TaskSourceObservation{URLs: observations[0].URLs, ObservedAt: at.Add(time.Nanosecond)})
	if _, err := renderGraphObservations(marker, observations); err == nil {
		t.Fatal("silently selected one of two source versions")
	}
	branch, err := renderGraphObservations("{{observed_at: https://example.test/requested; "+at.Format(time.RFC3339Nano)+"}}", observations)
	if err != nil {
		t.Fatal(err)
	}
	final, err := renderGraphObservations("{{observed_at: https://example.test/requested; "+branch+"}}", observations)
	if err != nil || final != branch || final != at.Format(time.RFC3339Nano) {
		t.Fatal("downstream selector lost observation precision", final, err)
	}
}

func TestGraphObservationsRejectUnrecordedOrInvalidMarkers(t *testing.T) {
	at := time.Date(2026, 9, 22, 21, 55, 49, 566826000, time.UTC)
	observations := []core.TaskSourceObservation{{URLs: []string{"https://example.test/source"}, ObservedAt: at}}
	for _, report := range []string{
		"{{observed_at: https://example.test/other}}",
		"{{observed_at: https://example.test/source; 2026-09-22T21:55:49Z}}", // Rounded is not the saved clock.
		"{{observed_at: https://example.test/source; 0001-01-01T00:00:00Z}}",
		"{{observed_at: https://example.test/source; tomorrow}}",
		"{{observed_at: https://example.test/source",
		"{{observed_at: }}", "{{observed_at: https://}}", "{{observed_at: file:///tmp/source}}",
		"{{observed_at: https://example.test/" + strings.Repeat("x", 4096) + "}}",
		strings.Repeat("{{observed_at: https://example.test/source}}", 129),
	} {
		if got, err := renderGraphObservations(report, observations); err == nil || got != "" {
			t.Fatalf("accepted invalid marker: %q, %q, %v", report, got, err)
		}
	}
	if _, err := renderGraphObservations("{{observed_at: https://example.test/source}}", nil); err == nil {
		t.Fatal("invented timestamp without a saved observation")
	}
}

func TestGraphReportOnlyRequiresDatabaseForObservationMarkers(t *testing.T) {
	report, err := renderGraphReport(context.Background(), core.AgentTask{}, core.AgentDeps{}, "Total {{calc: 0.1+0.2; 2}}")
	if err != nil || report != "Total 0.30" {
		t.Fatal(report, err)
	}
	if _, err := renderGraphReport(context.Background(), core.AgentTask{}, core.AgentDeps{}, "{{observed_at: https://example.test/source}}"); err == nil {
		t.Fatal("accepted observation without its store")
	}
}
