package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Verification is the stored outcome of one acceptance check on a task.
type Verification struct {
	ID     int64
	TaskID string
	// Attempt numbers the verification rounds of a task, starting at 1.
	Attempt int
	// StepIndex is the check's position within its round, starting at 0.
	StepIndex int
	Name      string
	// Command is the command line that ran.
	Command  string
	Passed   bool
	ExitCode int
	// OutputTail is the end of the check's combined output.
	OutputTail string
	Duration   time.Duration
	TimedOut   bool
	// Err says why the check produced no exit status, if it did not.
	Err       string
	CreatedAt time.Time
}

// RecordVerification stores one round of checks for a task and returns the
// round's attempt number, which is one more than the task's previous round.
// Each element of checks needs only the fields that describe the check;
// TaskID, Attempt, StepIndex and CreatedAt are assigned here.
func (s *Store) RecordVerification(ctx context.Context, taskID string, checks []Verification) (int, error) {
	var attempt int
	err := s.inTx(ctx, func(tx querier) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM fleet_tasks WHERE id = ?`, taskID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		if err != nil {
			return fmt.Errorf("fleet: load task: %w", err)
		}
		var last sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(attempt) FROM fleet_verifications WHERE task_id = ?`, taskID).Scan(&last); err != nil {
			return fmt.Errorf("fleet: load verification attempts: %w", err)
		}
		attempt = int(last.Int64) + 1
		now := toMillis(s.now())
		for i, c := range checks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_verifications
				(task_id, attempt, step_index, name, command, passed, exit_code, output_tail,
				 duration_ms, timed_out, error, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				taskID, attempt, i, c.Name, c.Command, c.Passed, c.ExitCode, c.OutputTail,
				c.Duration.Milliseconds(), c.TimedOut, c.Err, now); err != nil {
				return fmt.Errorf("fleet: record verification: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return attempt, nil
}

// ListVerifications returns every stored check of a task, oldest round first.
func (s *Store) ListVerifications(ctx context.Context, taskID string) ([]Verification, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, task_id, attempt, step_index, name, command, passed,
		exit_code, output_tail, duration_ms, timed_out, error, created_at
		FROM fleet_verifications WHERE task_id = ? ORDER BY attempt ASC, step_index ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("fleet: list verifications: %w", err)
	}
	defer rows.Close()
	out := []Verification{}
	for rows.Next() {
		var (
			v        Verification
			ms, when int64
		)
		if err := rows.Scan(&v.ID, &v.TaskID, &v.Attempt, &v.StepIndex, &v.Name, &v.Command, &v.Passed,
			&v.ExitCode, &v.OutputTail, &ms, &v.TimedOut, &v.Err, &when); err != nil {
			return nil, fmt.Errorf("fleet: list verifications: %w", err)
		}
		v.Duration, v.CreatedAt = time.Duration(ms)*time.Millisecond, fromMillis(when)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list verifications: %w", err)
	}
	return out, nil
}

// failedVerificationRounds counts the rounds since a point in time in which
// at least one check failed.
func (s *Store) failedVerificationRounds(ctx context.Context, taskID string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT attempt) FROM fleet_verifications
		WHERE task_id = ? AND passed = 0 AND created_at >= ?`, taskID, toMillis(since)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("fleet: count failed verifications: %w", err)
	}
	return n, nil
}
