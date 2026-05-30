package blueship

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jmoiron/sqlx"
)

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

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(b.String()))
	}
}
