package blueship

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestAppendHostMetricSamplesUsesConfiguredCollector(t *testing.T) {
	called := false
	ship := New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostMetrics: func(context.Context) ([]MetricSample, error) {
			called = true
			return []MetricSample{{
				Name: "host_ready", Help: "Host readiness.", Type: MetricGauge, Value: 1,
			}}, nil
		},
	})
	var dst strings.Builder
	dst.WriteString("framework_metric 1\n")

	ship.appendHostMetricSamples(context.Background(), &dst)

	if !called {
		t.Fatal("configured host metric collector was not called")
	}
	if got := dst.String(); !strings.Contains(got, "framework_metric 1\n") ||
		!strings.Contains(got, "host_ready 1\n") {
		t.Fatalf("combined scrape = %q", got)
	}
}

func TestAppendHostMetricSamplesRejectsInvalidBatch(t *testing.T) {
	ship := New(Config{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		HostMetrics: func(context.Context) ([]MetricSample, error) {
			return []MetricSample{
				{Name: "host_ready", Help: "Valid.", Type: MetricGauge, Value: 1},
				{Name: "invalid-name", Help: "Invalid.", Type: MetricGauge, Value: 1},
			}, nil
		},
	})
	var dst strings.Builder
	dst.WriteString("framework_metric 1\n")

	ship.appendHostMetricSamples(context.Background(), &dst)

	if got, want := dst.String(), "framework_metric 1\n"; got != want {
		t.Fatalf("invalid host batch changed scrape: got %q, want %q", got, want)
	}
}

func TestRenderHostMetricSamplesGaugeCounterAndLabels(t *testing.T) {
	got, err := renderHostMetricSamples([]MetricSample{
		{
			Name:   "host_requests_total",
			Help:   "Completed host requests.",
			Type:   MetricCounter,
			Labels: map[string]string{"status": "ok", "method": "GET"},
			Value:  7,
		},
		{
			Name:  "host_feature_enabled",
			Help:  "Whether the host feature is enabled.",
			Type:  MetricGauge,
			Value: 1,
		},
		{
			Name:   "host_requests_total",
			Help:   "Completed host requests.",
			Type:   MetricCounter,
			Labels: map[string]string{"method": "GET", "status": "error"},
			Value:  2,
		},
	})
	if err != nil {
		t.Fatalf("renderHostMetricSamples: %v", err)
	}

	const want = "# HELP host_feature_enabled Whether the host feature is enabled.\n" +
		"# TYPE host_feature_enabled gauge\n" +
		"host_feature_enabled 1\n" +
		"# HELP host_requests_total Completed host requests.\n" +
		"# TYPE host_requests_total counter\n" +
		"host_requests_total{method=\"GET\",status=\"error\"} 2\n" +
		"host_requests_total{method=\"GET\",status=\"ok\"} 7\n"
	if got != want {
		t.Fatalf("rendered metrics mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestRenderHostMetricSamplesEscapesAndSortsLabels(t *testing.T) {
	got, err := renderHostMetricSamples([]MetricSample{{
		Name: "host_state",
		Help: "Host state\nby label.",
		Type: MetricGauge,
		Labels: map[string]string{
			"z_label": "line\n\"slash\\",
			"a_label": "first",
		},
		Value: 0.5,
	}})
	if err != nil {
		t.Fatalf("renderHostMetricSamples: %v", err)
	}
	for _, want := range []string{
		"# HELP host_state Host state\\nby label.\n",
		`host_state{a_label="first",z_label="line\n\"slash\\"} 0.5`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered metrics missing %q:\n%s", want, got)
		}
	}
}

func TestRenderHostMetricSamplesRejectsInvalidNamesAndTypes(t *testing.T) {
	tests := []struct {
		name   string
		sample MetricSample
	}{
		{
			name:   "metric starts with digit",
			sample: MetricSample{Name: "9host_metric", Help: "bad", Type: MetricGauge},
		},
		{
			name:   "metric contains dash",
			sample: MetricSample{Name: "host-metric", Help: "bad", Type: MetricGauge},
		},
		{
			name:   "unsupported type",
			sample: MetricSample{Name: "host_metric", Help: "bad", Type: MetricType("histogram")},
		},
		{
			name: "invalid label name",
			sample: MetricSample{
				Name: "host_metric", Help: "bad", Type: MetricGauge,
				Labels: map[string]string{"bad-label": "x"},
			},
		},
		{
			name:   "framework metric collision",
			sample: MetricSample{Name: "blueship_agent_tasks", Help: "bad", Type: MetricGauge},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := renderHostMetricSamples([]MetricSample{tt.sample}); err == nil {
				t.Fatalf("renderHostMetricSamples accepted invalid sample: %q", got)
			}
		})
	}
}

func TestRenderHostMetricSamplesRejectsInconsistentOrDuplicateFamily(t *testing.T) {
	base := MetricSample{
		Name:   "host_jobs_total",
		Help:   "Completed jobs.",
		Type:   MetricCounter,
		Labels: map[string]string{"state": "done"},
		Value:  1,
	}
	for _, second := range []MetricSample{
		{
			Name: "host_jobs_total", Help: "Different help.", Type: MetricCounter,
			Labels: map[string]string{"state": "failed"}, Value: 1,
		},
		{
			Name: "host_jobs_total", Help: "Completed jobs.", Type: MetricGauge,
			Labels: map[string]string{"state": "failed"}, Value: 1,
		},
		base,
	} {
		if _, err := renderHostMetricSamples([]MetricSample{base, second}); err == nil {
			t.Fatalf("accepted inconsistent/duplicate family: %#v", second)
		}
	}
}
