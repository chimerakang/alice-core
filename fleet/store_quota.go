package fleet

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// BackendQuotaBlocked reports whether a recorded rejection still applies.
// A zero reset time on a blocked backend means no reset time is known.
func (s *Store) BackendQuotaBlocked(ctx context.Context, backend string) (blocked bool, resetsAt time.Time, err error) {
	var reset sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT reset_at FROM fleet_backend_quota WHERE backend = ?`, backend).Scan(&reset)
	if errors.Is(err, sql.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, err
	}
	if !reset.Valid {
		return true, time.Time{}, nil
	}
	at := fromMillis(reset.Int64)
	return s.now().Before(at), at, nil
}

// ClearBackendQuota clears a rejection once a fresh provider snapshot
// confirms the backend is available.
func (s *Store) ClearBackendQuota(ctx context.Context, backend string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM fleet_backend_quota WHERE backend = ?`, backend)
	return err
}

func (s *Store) clearQuotaObservedBefore(ctx context.Context, backend string, before time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM fleet_backend_quota WHERE backend = ? AND observed_at < ?`, backend, toMillis(before))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *Store) recordCodexQuotaDenied(ctx context.Context, observedBefore time.Time) error {
	observed := toMillis(observedBefore)
	_, err := s.db.ExecContext(ctx, `INSERT INTO fleet_backend_quota (backend, reset_at, observed_at) VALUES ('codex', NULL, ?)
		ON CONFLICT(backend) DO UPDATE SET
			observed_at = excluded.observed_at,
			reset_at = NULL
		WHERE fleet_backend_quota.observed_at < excluded.observed_at`, observed)
	return err
}

func (s *Store) recordQuotaExhaustion(ctx context.Context, taskID string, quota QuotaExhaustion) error {
	return s.inTx(ctx, func(tx querier) error {
		var reset any
		now := toMillis(s.now())
		if !quota.ResetsAt.IsZero() {
			reset = toMillis(quota.ResetsAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_backend_quota (backend, reset_at, observed_at) VALUES (?, ?, ?)
			ON CONFLICT(backend) DO UPDATE SET observed_at = excluded.observed_at, reset_at = CASE
				WHEN excluded.reset_at IS NULL AND fleet_backend_quota.reset_at > ? THEN fleet_backend_quota.reset_at
				ELSE excluded.reset_at
			END`, quota.Backend, reset, now, now); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, taskID, EventQuotaExhausted, quota, now)
		return err
	})
}
