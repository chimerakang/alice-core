package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Watcher supervises the fleet without spending a single token: it reads
// what tasks have recorded, works out which of them need attention, and
// queues a wake for each. Nothing here calls a model.
//
// It does two things on every pass:
//
//   - turns status changes in the event log into wakes (review ready,
//     completed, failed, a question, a decision needed);
//   - looks at every running task and judges its liveness from whether the
//     Runner has its process and how long ago it last produced output, and
//     pauses it if a daily budget has been reached (see Runner.Budget).
//
// Wakes go to a durable queue; see Store.PendingWakes and Store.ConsumeWake.
type Watcher struct {
	// Store holds the tasks. Required.
	Store *Store
	// Runner is asked which tasks have a live process. Required.
	Runner *Runner
	// Interval is the time between passes. Zero means DefaultWatchInterval.
	Interval time.Duration
	// StuckAfter is how long a running task may produce no output before it
	// is reported as stuck. Zero means DefaultStuckAfter.
	StuckAfter time.Duration
}

// Watcher defaults.
const (
	DefaultWatchInterval = 5 * time.Second
	DefaultStuckAfter    = 10 * time.Minute
)

const reasonAgentWaiting = "agent is waiting for an answer"

// Run watches until ctx is cancelled. All supervision happens on the calling
// goroutine, one pass at a time.
func (w *Watcher) Run(ctx context.Context) error {
	if w.Store == nil || w.Runner == nil {
		return errors.New("fleet: Watcher needs a Store and a Runner")
	}
	ticker := time.NewTicker(w.interval())
	defer ticker.Stop()
	for {
		// A failed pass is retried on the next tick. The cursor only moves
		// when a pass commits, so nothing is skipped.
		_, _ = w.Scan(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Scan makes one pass and returns the wakes it queued. A wake is queued at
// most once per occurrence, so repeated passes over the same state add
// nothing.
func (w *Watcher) Scan(ctx context.Context) ([]Wake, error) {
	if w.Store == nil || w.Runner == nil {
		return nil, errors.New("fleet: Watcher needs a Store and a Runner")
	}
	cursor, err := w.Store.watcherCursor(ctx)
	if err != nil {
		return nil, err
	}
	events, err := w.Store.ListEvents(ctx, "", cursor, 0)
	if err != nil {
		return nil, err
	}

	var wakes []newWake
	tasks := map[string]Task{}
	loadTask := func(id string) (Task, bool) {
		if t, ok := tasks[id]; ok {
			return t, true
		}
		t, err := w.Store.GetTask(ctx, id)
		if err != nil {
			return Task{}, false
		}
		tasks[id] = t
		return t, true
	}

	for _, ev := range events {
		cursor = ev.ID
		if ev.Type != EventStatus {
			continue
		}
		var change StatusChange
		if json.Unmarshal(ev.Payload, &change) != nil {
			continue
		}
		// A stop someone asked for is not news to them. The mark is on the
		// status change itself, so it holds however the stop request and the
		// agent's exit happen to be ordered, and across passes.
		if change.Requested {
			continue
		}
		kind, ok := wakeFor(change)
		if !ok {
			continue
		}
		task, found := loadTask(ev.TaskID)
		if !found {
			continue
		}
		wakes = append(wakes, newWake{
			taskID: task.ID, projectID: task.ProjectID, kind: kind, detail: change.Reason,
			dedupeKey: "event:" + strconv.FormatInt(ev.ID, 10),
		})
	}

	running, err := w.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning}})
	if err != nil {
		return nil, err
	}
	now := w.Store.now()
	budget := w.Runner.Budget.view(w.Store)
	projects := map[string]Project{}
	for _, task := range running {
		liveness, quietSince := w.judge(ctx, task, now)
		if liveness == LivenessDead {
			// A running task with no process holds its slot for nothing.
			// Recovery marks it dead and hands it to the owner; the status
			// change it writes becomes a wake on the next pass.
			if _, _, err := w.Runner.recoverTask(ctx, task); err != nil {
				return nil, err
			}
			continue
		}
		if liveness != task.Liveness {
			if err := w.Store.SetLiveness(ctx, task.ID, liveness); err != nil {
				return nil, err
			}
		}
		if !quietSince.IsZero() {
			wakes = append(wakes, newWake{
				taskID: task.ID, projectID: task.ProjectID, kind: WakeStuck,
				detail: fmt.Sprintf("no output for %s", now.Sub(quietSince).Round(time.Second)),
				// One wake per stall: new output starts a new stall.
				dedupeKey: "quiet-since:" + strconv.FormatInt(toMillis(quietSince), 10),
			})
		}
		// A daily limit reached while this task was running: pause it. The
		// agent keeps its session and can carry on when there is budget.
		if over := w.dailyLimitFor(ctx, budget, task, projects); over != nil {
			wakes = append(wakes, budgetWake(over, budget.day()))
			_ = w.Runner.Interrupt(task.ID, over.Reason())
		}
	}

	return w.Store.commitScan(ctx, wakes, cursor)
}

// wakeFor maps a status change onto the wake it deserves, if any. Changes
// the owner made by hand (accepting, cancelling, giving up) raise none.
func wakeFor(change StatusChange) (WakeKind, bool) {
	switch change.To {
	case StatusReviewReady:
		if change.From == StatusVerifying {
			return WakeReviewReady, true
		}
	case StatusDone:
		if change.From == StatusVerifying {
			return WakeCompleted, true
		}
	case StatusFailed:
		if change.From == StatusRunning || change.From == StatusVerifying {
			return WakeFailed, true
		}
	case StatusNeedsDecision:
		if change.Reason == reasonAgentWaiting {
			return WakeQuestion, true
		}
		return WakeNeedsDecision, true
	}
	return "", false
}

// judge works out a running task's liveness. quietSince is non-zero when the
// task has a live process that has produced nothing for longer than
// StuckAfter; it is the moment the silence began.
//
// The rule that matters: when there is no evidence either way the answer is
// unknown. Only a process the Runner can see makes a task busy, and only a
// recorded process that has verifiably gone makes it dead.
func (w *Watcher) judge(ctx context.Context, task Task, now time.Time) (Liveness, time.Time) {
	if w.Runner.Running(task.ID) {
		last := task.UpdatedAt
		switch {
		case task.LastActivityAt != nil && task.StartedAt != nil && task.LastActivityAt.Before(*task.StartedAt):
			last = *task.StartedAt // activity from an earlier run says nothing about this one
		case task.LastActivityAt != nil:
			last = *task.LastActivityAt
		case task.StartedAt != nil:
			last = *task.StartedAt
		}
		if now.Sub(last) > w.stuckAfter() {
			// Alive, but silent for too long to call busy.
			return LivenessUnknown, last
		}
		return LivenessBusy, time.Time{}
	}

	// The Runner has no process for it. Either this task is being run by
	// something else, or its process died without anyone recording it.
	pid, recorded := w.Runner.recordedPID(ctx, task)
	switch {
	case !recorded:
		return LivenessUnknown, time.Time{} // no signal at all
	case w.Runner.agentAlive(ctx, pid):
		return LivenessUnknown, time.Time{} // alive, but not ours to vouch for
	default:
		return LivenessDead, time.Time{}
	}
}

// dailyLimitFor returns the daily limit, the project's or the fleet's, that
// the task's scope has reached, or nil. A task's own limit is not checked
// here: its usage only changes when a run ends, and the next run is refused.
func (w *Watcher) dailyLimitFor(ctx context.Context, budget *budgetView, task Task, projects map[string]Project) *BudgetError {
	if budget == nil {
		return nil
	}
	project, ok := projects[task.ProjectID]
	if !ok {
		var err error
		if project, err = w.Store.GetProject(ctx, task.ProjectID); err != nil {
			return nil
		}
		projects[task.ProjectID] = project
	}
	if over, err := budget.projectExceeded(ctx, project); err == nil && over != nil {
		return over
	}
	if over, err := budget.fleetExceeded(ctx); err == nil && over != nil {
		return over
	}
	return nil
}

func (w *Watcher) interval() time.Duration {
	if w.Interval > 0 {
		return w.Interval
	}
	return DefaultWatchInterval
}

func (w *Watcher) stuckAfter() time.Duration {
	if w.StuckAfter > 0 {
		return w.StuckAfter
	}
	return DefaultStuckAfter
}
