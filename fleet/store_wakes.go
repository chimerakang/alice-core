package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// WakeKind says why a task needs someone's attention.
type WakeKind string

const (
	// WakeReviewReady: the task passed verification and awaits the owner.
	WakeReviewReady WakeKind = "review_ready"
	// WakeCompleted: the task finished with nothing left for the owner to do.
	WakeCompleted WakeKind = "completed"
	// WakeFailed: the task failed.
	WakeFailed WakeKind = "failed"
	// WakeQuestion: the agent asked something and is waiting for the answer.
	WakeQuestion WakeKind = "question"
	// WakeNeedsDecision: the task is blocked on the owner for another reason.
	WakeNeedsDecision WakeKind = "needs_decision"
	// WakeStuck: the task is running but its agent has gone quiet.
	WakeStuck WakeKind = "stuck"
	// WakeOverBudget: the task has used more than its budget allows.
	WakeOverBudget WakeKind = "over_budget"
	// WakeProviderQuota indicates both Scout backends currently reject work.
	WakeProviderQuota WakeKind = "provider_quota"
)

// Wake is one entry in the durable queue of things that need attention. A
// wake stays pending until a consumer marks it consumed, so a notification
// survives a crash between being raised and being delivered.
type Wake struct {
	ID int64
	// TaskID and ProjectID say what the wake is about. Both are empty for a
	// fleet-wide wake, and TaskID is empty for a project-wide one.
	TaskID    string
	ProjectID string
	Kind      WakeKind
	// Detail is a short human-readable explanation.
	Detail    string
	CreatedAt time.Time
	// ConsumedAt is when a consumer acknowledged the wake; nil while pending.
	ConsumedAt *time.Time
}

// EventWake is the event type recorded when a wake is raised. Its TaskID is
// empty for a wake that concerns a whole project or the whole fleet.
const EventWake = "wake"

// WakeRaised is the payload of an EventWake event.
type WakeRaised struct {
	WakeID    int64    `json:"wake_id"`
	Kind      WakeKind `json:"kind"`
	Detail    string   `json:"detail,omitempty"`
	ProjectID string   `json:"project_id,omitempty"`
}

// ErrWakeNotFound is returned when no wake has the given ID.
var ErrWakeNotFound = errors.New("fleet: wake not found")

// newWake is a wake about to be queued. dedupeKey identifies the occurrence:
// queueing the same task, kind and key twice keeps only the first.
type newWake struct {
	taskID, projectID string
	kind              WakeKind
	detail            string
	dedupeKey         string
}

// enqueueWakes stores wakes, skipping ones already queued, and returns the
// ones that were new.
func enqueueWakes(ctx context.Context, tx querier, wakes []newWake, nowMillis int64) ([]Wake, error) {
	var added []Wake
	for _, w := range wakes {
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO fleet_wakes
			(task_id, project_id, kind, detail, dedupe_key, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			w.taskID, w.projectID, string(w.kind), w.detail, w.dedupeKey, nowMillis)
		if err != nil {
			return nil, fmt.Errorf("fleet: queue wake: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("fleet: queue wake: %w", err)
		}
		// The wake also goes into the event log, so a subscriber following
		// one ordered stream sees it in sequence with everything else.
		if _, err := appendEvent(ctx, tx, w.taskID, EventWake, WakeRaised{
			WakeID: id, Kind: w.kind, Detail: w.detail, ProjectID: w.projectID,
		}, nowMillis); err != nil {
			return nil, err
		}
		added = append(added, Wake{
			ID: id, TaskID: w.taskID, ProjectID: w.projectID, Kind: w.kind, Detail: w.detail,
			CreatedAt: fromMillis(nowMillis),
		})
	}
	return added, nil
}

// raiseWakes queues wakes outside a watcher pass.
func (s *Store) raiseWakes(ctx context.Context, wakes ...newWake) ([]Wake, error) {
	var added []Wake
	err := s.inTx(ctx, func(tx querier) error {
		var err error
		added, err = enqueueWakes(ctx, tx, wakes, toMillis(s.now()))
		return err
	})
	return added, err
}

// budgetWake is the wake for a reached limit. It is raised once per scope
// per day, however many tasks run into the limit.
func budgetWake(over *BudgetError, day string) newWake {
	w := newWake{kind: WakeOverBudget, detail: over.Reason()}
	switch over.Scope {
	case BudgetScopeTask:
		w.taskID, w.projectID = over.TaskID, over.ProjectID
		w.dedupeKey = "budget:task:" + day
	case BudgetScopeProject:
		w.projectID = over.ProjectID
		w.dedupeKey = "budget:project:" + over.ProjectID + ":" + day
	default:
		w.dedupeKey = "budget:fleet:" + day
	}
	return w
}

// PendingWakes returns unconsumed wakes, oldest first. limit caps the result
// when positive.
func (s *Store) PendingWakes(ctx context.Context, limit int) ([]Wake, error) {
	query := `SELECT id, task_id, project_id, kind, detail, created_at, consumed_at
		FROM fleet_wakes WHERE consumed_at IS NULL ORDER BY id ASC`
	var args []any
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet: list wakes: %w", err)
	}
	defer rows.Close()
	wakes := []Wake{}
	for rows.Next() {
		var (
			w        Wake
			kind     string
			created  int64
			consumed sql.NullInt64
		)
		if err := rows.Scan(&w.ID, &w.TaskID, &w.ProjectID, &kind, &w.Detail, &created, &consumed); err != nil {
			return nil, fmt.Errorf("fleet: list wakes: %w", err)
		}
		w.Kind, w.CreatedAt, w.ConsumedAt = WakeKind(kind), fromMillis(created), fromNullMillis(consumed)
		wakes = append(wakes, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list wakes: %w", err)
	}
	return wakes, nil
}

// ConsumeWake marks a wake as handled. Call it only after the wake has
// really been acted on (the notification sent, say): until then the wake
// stays pending and will be offered again. Consuming twice is harmless.
func (s *Store) ConsumeWake(ctx context.Context, id int64) error {
	return s.inTx(ctx, func(tx querier) error {
		res, err := tx.ExecContext(ctx, `UPDATE fleet_wakes SET consumed_at = COALESCE(consumed_at, ?) WHERE id = ?`,
			toMillis(s.now()), id)
		if err != nil {
			return fmt.Errorf("fleet: consume wake: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %d", ErrWakeNotFound, id)
		}
		return nil
	})
}

// commitScan stores the wakes a watcher pass produced and advances the
// watcher's cursor, in one transaction, so a crash cannot separate them.
func (s *Store) commitScan(ctx context.Context, wakes []newWake, cursor int64) ([]Wake, error) {
	var added []Wake
	err := s.inTx(ctx, func(tx querier) error {
		var err error
		if added, err = enqueueWakes(ctx, tx, wakes, toMillis(s.now())); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO fleet_meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, metaWatcherCursor, strconv.FormatInt(cursor, 10))
		if err != nil {
			return fmt.Errorf("fleet: save watcher cursor: %w", err)
		}
		return nil
	})
	return added, err
}

const metaWatcherCursor = "watcher.cursor"

// watcherCursor returns the ID of the last event the watcher has processed.
func (s *Store) watcherCursor(ctx context.Context) (int64, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM fleet_meta WHERE key = ?`, metaWatcherCursor).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("fleet: load watcher cursor: %w", err)
	}
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("fleet: watcher cursor %q is not a number", value)
	}
	return cursor, nil
}
