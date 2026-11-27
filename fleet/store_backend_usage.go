package fleet

import (
	"context"
	"time"
)

type BackendUsage struct {
	Backend      string  `json:"backend"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Tasks        int64   `json:"tasks"`
}

// UsageByBackend totals usage in [since, until). An empty backend denotes
// historical usage without attribution.
func (s *Store) UsageByBackend(ctx context.Context, since, until time.Time) ([]BackendUsage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT backend, SUM(input_tokens), SUM(output_tokens),
 SUM(cost_usd), COUNT(DISTINCT task_id) FROM fleet_usage
 WHERE created_at >= ? AND created_at < ? GROUP BY backend ORDER BY backend`, toMillis(since), toMillis(until))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BackendUsage{}
	for rows.Next() {
		var u BackendUsage
		if err := rows.Scan(&u.Backend, &u.InputTokens, &u.OutputTokens, &u.CostUSD, &u.Tasks); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// VisitBackendUsage exposes the backend totals to consumers that support
// both versions of the Fleet store during an upgrade.
func (s *Store) VisitBackendUsage(ctx context.Context, since, until time.Time, visit func(backend string, input, output int64, costUSD float64, tasks int64)) error {
	rows, err := s.UsageByBackend(ctx, since, until)
	if err != nil {
		return err
	}
	for _, row := range rows {
		visit(row.Backend, row.InputTokens, row.OutputTokens, row.CostUSD, row.Tasks)
	}
	return nil
}
