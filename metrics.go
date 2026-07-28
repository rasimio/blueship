package blueship

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jmoiron/sqlx"
)

var (
	prometheusMetricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	prometheusLabelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

var frameworkMetricNames = map[string]struct{}{
	"blueship_agent_tasks":      {},
	"blueship_fleet_peer_cache": {},
	"blueship_a2a_calls_total":  {},
}

// handleShipMetrics exposes Prometheus-format metrics on the same port as the
// A2A server. Pulls counts directly from Postgres on each scrape — fine for
// low-frequency scraping (15s+ intervals).
//
// Series:
//   - blueship_agent_tasks{strategy,status}
//   - blueship_fleet_peer_cache
//   - blueship_a2a_calls_total{direction,state}
func (s *Ship) handleShipMetrics(db *sqlx.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		ctx := r.Context()

		var b strings.Builder

		// agent_tasks by (strategy, status)
		type tasksRow struct {
			Strategy string `db:"strategy"`
			Status   string `db:"status"`
			N        int    `db:"n"`
		}
		var trows []tasksRow
		if err := db.SelectContext(ctx, &trows,
			`SELECT strategy, status, count(*) AS n FROM agent_tasks GROUP BY strategy, status ORDER BY strategy, status`); err == nil {
			fmt.Fprintln(&b, "# HELP blueship_agent_tasks Tasks by strategy and lifecycle status.")
			fmt.Fprintln(&b, "# TYPE blueship_agent_tasks gauge")
			for _, row := range trows {
				fmt.Fprintf(&b, "blueship_agent_tasks{strategy=%q,status=%q} %d\n", row.Strategy, row.Status, row.N)
			}
		}

		// fleet_peer_cache size
		var peerCount int
		if err := db.GetContext(ctx, &peerCount,
			`SELECT count(*) FROM fleet_peer_cache WHERE status = 'active'`); err == nil {
			fmt.Fprintln(&b, "# HELP blueship_fleet_peer_cache Number of active peers known via Fleet.")
			fmt.Fprintln(&b, "# TYPE blueship_fleet_peer_cache gauge")
			fmt.Fprintf(&b, "blueship_fleet_peer_cache %d\n", peerCount)
		}

		// a2a_calls by (direction, state)
		type a2aRow struct {
			Direction string `db:"direction"`
			State     string `db:"state"`
			N         int    `db:"n"`
		}
		var arows []a2aRow
		if err := db.SelectContext(ctx, &arows,
			`SELECT direction, state, count(*) AS n FROM a2a_calls GROUP BY direction, state ORDER BY direction, state`); err == nil {
			fmt.Fprintln(&b, "# HELP blueship_a2a_calls_total Inter-agent calls by direction and final state.")
			fmt.Fprintln(&b, "# TYPE blueship_a2a_calls_total counter")
			for _, row := range arows {
				fmt.Fprintf(&b, "blueship_a2a_calls_total{direction=%q,state=%q} %d\n", row.Direction, row.State, row.N)
			}
		}

		s.appendHostMetricSamples(ctx, &b)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}
}

func (s *Ship) appendHostMetricSamples(ctx context.Context, dst *strings.Builder) {
	if s == nil || s.cfg.HostMetrics == nil || dst == nil {
		return
	}
	samples, err := s.cfg.HostMetrics(ctx)
	if err != nil {
		s.logger.Warn("host metrics collection failed", "error", err)
		return
	}
	rendered, err := renderHostMetricSamples(samples)
	if err != nil {
		s.logger.Warn("host metrics rejected", "error", err)
		return
	}
	dst.WriteString(rendered)
}

type hostMetricFamily struct {
	help    string
	typ     MetricType
	samples []hostMetricSeries
	seen    map[string]struct{}
}

type hostMetricSeries struct {
	labels string
	value  string
}

func renderHostMetricSamples(samples []MetricSample) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}

	families := make(map[string]*hostMetricFamily)
	for _, sample := range samples {
		if !prometheusMetricNameRE.MatchString(sample.Name) {
			return "", fmt.Errorf("invalid metric name %q", sample.Name)
		}
		if _, reserved := frameworkMetricNames[sample.Name]; reserved {
			return "", fmt.Errorf("metric name %q is reserved by BlueShip", sample.Name)
		}
		if sample.Type != MetricGauge && sample.Type != MetricCounter {
			return "", fmt.Errorf("invalid metric type %q for %q", sample.Type, sample.Name)
		}

		labels, err := formatPrometheusLabels(sample.Labels)
		if err != nil {
			return "", fmt.Errorf("metric %q: %w", sample.Name, err)
		}
		family := families[sample.Name]
		if family == nil {
			family = &hostMetricFamily{
				help: sample.Help,
				typ:  sample.Type,
				seen: make(map[string]struct{}),
			}
			families[sample.Name] = family
		} else if family.typ != sample.Type || family.help != sample.Help {
			return "", fmt.Errorf("inconsistent family metadata for metric %q", sample.Name)
		}
		if _, duplicate := family.seen[labels]; duplicate {
			return "", fmt.Errorf("duplicate series for metric %q with labels %s", sample.Name, labels)
		}
		family.seen[labels] = struct{}{}
		family.samples = append(family.samples, hostMetricSeries{
			labels: labels,
			value:  formatPrometheusValue(sample.Value),
		})
	}

	names := make([]string, 0, len(families))
	for name := range families {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	for _, name := range names {
		family := families[name]
		sort.Slice(family.samples, func(i, j int) bool {
			if family.samples[i].labels == family.samples[j].labels {
				return family.samples[i].value < family.samples[j].value
			}
			return family.samples[i].labels < family.samples[j].labels
		})
		fmt.Fprintf(&out, "# HELP %s %s\n", name, escapePrometheusHelp(family.help))
		fmt.Fprintf(&out, "# TYPE %s %s\n", name, family.typ)
		for _, series := range family.samples {
			fmt.Fprintf(&out, "%s%s %s\n", name, series.labels, series.value)
		}
	}
	return out.String(), nil
}

func formatPrometheusLabels(labels map[string]string) (string, error) {
	if len(labels) == 0 {
		return "", nil
	}
	names := make([]string, 0, len(labels))
	for name := range labels {
		if !prometheusLabelNameRE.MatchString(name) || strings.HasPrefix(name, "__") {
			return "", fmt.Errorf("invalid label name %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	out.WriteByte('{')
	for i, name := range names {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, `%s="%s"`, name, escapePrometheusLabelValue(labels[name]))
	}
	out.WriteByte('}')
	return out.String(), nil
}

func escapePrometheusHelp(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
	).Replace(value)
}

func escapePrometheusLabelValue(value string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.NewReplacer(
		`\`, `\\`,
		"\n", `\n`,
		`"`, `\"`,
	).Replace(value)
}

func formatPrometheusValue(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "+Inf"
	case math.IsInf(value, -1):
		return "-Inf"
	default:
		return strconv.FormatFloat(value, 'g', -1, 64)
	}
}
