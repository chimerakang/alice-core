package fleet

import (
	"context"
	"fmt"
	"strings"
)

const EventQuotaTransferred = "quota.transferred"

// ConfigureScoutQuotaFallback sets the transfer policy before starting Fleet.
func (r *Runner) ConfigureScoutQuotaFallback(maxTransfers int, choose func(context.Context, Task, string) (string, bool)) {
	r.MaxQuotaTransfers = maxTransfers
	r.ScoutQuotaFallback = choose
}

func (r *Runner) scoutUSDLimited(task Task, project Project) bool {
	return task.Budget.CostUSD > 0 || project.Budget.Task.CostUSD > 0 || project.Budget.Daily.CostUSD > 0 ||
		(r.Budget != nil && (r.Budget.Task.CostUSD > 0 || r.Budget.ProjectDaily.CostUSD > 0 || r.Budget.Daily.CostUSD > 0))
}

func (r *Runner) quotaFallback(ctx context.Context, task Task, fresh bool, opts RunOptions, quota *QuotaExhaustion) (string, string) {
	if quota == nil || task.Shape != ShapeScout || !fresh || strings.TrimSpace(opts.Instruction) != "" ||
		strings.TrimSpace(task.Goal) == "" || r.ScoutQuotaFallback == nil || task.quotaTransfers >= r.MaxQuotaTransfers {
		return "", ""
	}
	var backend string
	switch quota.Backend {
	case "codex":
		backend = "claude"
	case "claude":
		backend = "codex"
		project, err := r.Store.GetProject(ctx, task.ProjectID)
		if err != nil || r.CodexScoutBinary == "" || r.scoutUSDLimited(task, project) {
			return "", ""
		}
	default:
		return "", ""
	}
	model, eligible := r.ScoutQuotaFallback(ctx, task, backend)
	if !eligible {
		return "", ""
	}
	return backend, model
}

// requeueQuotaTransfer is reserved for the active runner. It carries the
// destination and count across restarts and clears the previous session.
func (s *Store) requeueQuotaTransfer(ctx context.Context, task Task, from, to, model string) (Task, error) {
	var out Task
	reason := from + " quota exhausted; queued for " + to
	err := s.inTx(ctx, func(tx querier) error {
		now := toMillis(s.now())
		res, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET status = ?, status_reason = ?, updated_at = ?,
			started_at = NULL, finished_at = NULL, session_id = '',
			quota_backend = ?, quota_model = ?, quota_transfers = quota_transfers + 1
			WHERE id = ? AND status = ? AND quota_transfers = ?`,
			StatusQueued, reason, now, to, model, task.ID, StatusRunning, task.quotaTransfers)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("%w: task %s changed before quota transfer", ErrConflict, task.ID)
		}
		if _, err := appendEvent(ctx, tx, task.ID, EventStatus, StatusChange{From: StatusRunning, To: StatusQueued, Reason: reason}, now); err != nil {
			return err
		}
		if _, err := appendEvent(ctx, tx, task.ID, EventQuotaTransferred, map[string]any{
			"from": from, "to": to, "attempt": task.quotaTransfers + 1,
		}, now); err != nil {
			return err
		}
		out, err = getTask(ctx, tx, task.ID)
		return err
	})
	return out, err
}
