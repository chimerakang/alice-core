package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// BackendQuotaMeasurement returns a fresh provider-reported remaining share.
func (s *Store) BackendQuotaMeasurement(ctx context.Context, backend string) (float64, time.Time, bool, error) {
	var remaining float64
	var reset int64
	now := s.now()
	err := s.db.QueryRowContext(ctx, `SELECT remaining, reset_at FROM fleet_provider_quota_measurements
		WHERE backend = ? AND reset_at > ? AND observed_at > ?`, backend, toMillis(now), toMillis(now.Add(-2*codexQuotaPollInterval))).Scan(&remaining, &reset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, time.Time{}, false, nil
	}
	return remaining, fromMillis(reset), err == nil, err
}

func (s *Store) recordProviderQuotaMeasurement(ctx context.Context, backend string, snapshot providerQuotaSnapshot, observed time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO fleet_provider_quota_measurements (backend, remaining, reset_at, observed_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(backend) DO UPDATE SET remaining = excluded.remaining,
		reset_at = excluded.reset_at, observed_at = excluded.observed_at
		WHERE fleet_provider_quota_measurements.observed_at < excluded.observed_at`,
		backend, snapshot.remaining, toMillis(snapshot.resetAt), toMillis(observed))
	return err
}

func (s *Store) recordCodexQuotaDenied(ctx context.Context, observedBefore, resetAt time.Time) error {
	var reset any
	if !resetAt.IsZero() {
		reset = toMillis(resetAt)
	}
	observed := toMillis(observedBefore)
	_, err := s.db.ExecContext(ctx, `INSERT INTO fleet_backend_quota (backend, reset_at, observed_at, episode_at) VALUES ('codex', ?, ?, ?)
		ON CONFLICT(backend) DO UPDATE SET observed_at = excluded.observed_at,
			episode_at = CASE WHEN fleet_backend_quota.reset_at <= excluded.observed_at THEN excluded.episode_at ELSE fleet_backend_quota.episode_at END,
			reset_at = COALESCE(excluded.reset_at, CASE WHEN fleet_backend_quota.reset_at > excluded.observed_at THEN fleet_backend_quota.reset_at END)
		WHERE fleet_backend_quota.observed_at < excluded.observed_at`, reset, observed, observed)
	return err
}

func (s *Store) bothBackendsQuotaWake(ctx context.Context) (newWake, bool, error) {
	now := toMillis(s.now())
	var claude, codex int64
	err := s.db.QueryRowContext(ctx, `SELECT a.episode_at, b.episode_at FROM fleet_backend_quota a, fleet_backend_quota b
		WHERE a.backend = 'claude' AND b.backend = 'codex'
		AND (a.reset_at IS NULL OR a.reset_at > ?) AND (b.reset_at IS NULL OR b.reset_at > ?)`, now, now).Scan(&claude, &codex)
	if errors.Is(err, sql.ErrNoRows) {
		return newWake{}, false, nil
	}
	return newWake{kind: WakeProviderQuota, dedupeKey: fmt.Sprintf("provider-quota:%d:%d", claude, codex)}, err == nil, err
}

func (s *Store) recordQuotaExhaustion(ctx context.Context, taskID string, quota QuotaExhaustion) error {
	return s.inTx(ctx, func(tx querier) error {
		var reset any
		now := toMillis(s.now())
		if !quota.ResetsAt.IsZero() {
			reset = toMillis(quota.ResetsAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_backend_quota (backend, reset_at, observed_at, episode_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(backend) DO UPDATE SET observed_at = excluded.observed_at,
				episode_at = CASE WHEN fleet_backend_quota.reset_at <= excluded.observed_at THEN excluded.episode_at ELSE fleet_backend_quota.episode_at END,
				reset_at = CASE
				WHEN excluded.reset_at IS NULL AND fleet_backend_quota.reset_at > ? THEN fleet_backend_quota.reset_at
				ELSE excluded.reset_at
			END`, quota.Backend, reset, now, now, now); err != nil {
			return err
		}
		_, err := appendEvent(ctx, tx, taskID, EventQuotaExhausted, quota, now)
		return err
	})
}
