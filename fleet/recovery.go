package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/chimerakang/alice-core/process"
	"github.com/chimerakang/alice-core/worktree"
)

// RecoveryReport is what Recover found and did.
type RecoveryReport struct {
	// Recovered are tasks that claimed to be active with nothing behind
	// them. Each has been moved to needs_decision.
	Recovered []RecoveredTask
	// Orphans are checkouts under the worktree root that no unfinished task
	// owns. They are reported, never removed.
	Orphans []OrphanWorktree
	// MissingWorktrees are unfinished tasks whose recorded checkout is gone.
	// Running such a task again re-attaches its branch if the branch survives.
	MissingWorktrees []Task
	// Problems are things Recover could not check. They do not stop the
	// rest of the reconciliation.
	Problems []string
}

// RecoveredTask is one task Recover took out of an active status.
type RecoveredTask struct {
	// Task is the task as stored after recovery.
	Task Task
	// From is the status the task claimed to be in.
	From Status
	// KilledPID is the leftover agent process that was terminated, or 0.
	KilledPID int
}

// OrphanWorktree is a checkout with no unfinished task behind it.
type OrphanWorktree struct {
	ProjectID string
	// TaskID is the task the path names, whether or not such a task exists.
	TaskID string
	Path   string
	// Branch is the branch checked out there, when it is a git worktree.
	Branch string
	// Reason says why it counts as an orphan.
	Reason string
	// Dirty reports uncommitted or untracked changes: work that would be
	// lost if the checkout were removed.
	Dirty bool
}

// Empty reports whether recovery found nothing to act on or report.
func (r RecoveryReport) Empty() bool {
	return len(r.Recovered) == 0 && len(r.Orphans) == 0 && len(r.MissingWorktrees) == 0 && len(r.Problems) == 0
}

// Recover reconciles stored task state with reality. Call it when the host
// starts, before scheduling.
//
// A task stored as running or verifying that this Runner is not actually
// running was cut off, usually by a crash or restart. Its liveness is set to
// dead and it moves to needs_decision with its session intact, so the owner
// can resume it. If the agent process from before the restart is still
// alive, nothing is reading its output or able to stop it, so it is
// terminated first.
//
// Checkouts are then compared with tasks. Nothing is deleted: a checkout may
// hold the only copy of uncommitted work.
//
// Recover is safe to call at any time and more than once; tasks with a live
// run in this Runner are left alone.
func (r *Runner) Recover(ctx context.Context) (RecoveryReport, error) {
	if r.Store == nil {
		return RecoveryReport{}, errors.New("fleet: Runner.Store is required")
	}
	var report RecoveryReport

	active, err := r.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning, StatusVerifying}})
	if err != nil {
		return report, err
	}
	// Oldest first, so the report reads in the order things were started.
	sort.SliceStable(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })
	for _, task := range active {
		if r.Running(task.ID) {
			continue
		}
		recovered, ok, err := r.recoverTask(ctx, task)
		if err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("task %s: %v", task.ID, err))
			continue
		}
		if ok {
			report.Recovered = append(report.Recovered, recovered)
		}
	}

	if err := r.reconcileWorktrees(ctx, &report); err != nil {
		return report, err
	}
	return report, nil
}

func (r *Runner) recoverTask(ctx context.Context, task Task) (RecoveredTask, bool, error) {
	out := RecoveredTask{From: task.Status}

	reason := "agent process is gone: the host restarted while it was running"
	if task.Status == StatusVerifying {
		reason = "verification was cut off by a host restart"
	}
	if pid := r.leftoverAgent(ctx, task); pid != 0 {
		signalGroup(pid, syscall.SIGKILL)
		out.KilledPID = pid
		reason = "agent was left running without supervision by a host restart and has been stopped"
	}

	if err := r.Store.SetLiveness(ctx, task.ID, LivenessDead); err != nil {
		return out, false, err
	}
	updated, err := r.Store.Transition(ctx, task.ID, StatusNeedsDecision, reason)
	if errors.Is(err, ErrInvalidTransition) || errors.Is(err, ErrConflict) {
		// Something else resolved the task in the meantime; nothing to recover.
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	out.Task = updated
	return out, true, nil
}

// leftoverAgent returns the PID of the task's agent process from before the
// restart if it is still alive, or 0.
//
// A PID alone proves nothing, because PIDs are reused. The process must also
// look like an agent run: its command line has to name the agent binary and
// the stream-json output mode the Runner always asks for.
func (r *Runner) leftoverAgent(ctx context.Context, task Task) int {
	ev, ok, err := r.Store.lastEvent(ctx, task.ID, EventRunStarted)
	if err != nil || !ok {
		return 0
	}
	var started struct {
		PID int `json:"pid"`
	}
	if json.Unmarshal(ev.Payload, &started) != nil || started.PID <= 1 {
		return 0
	}
	if err := syscall.Kill(started.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return 0 // no such process
	}
	out, err := process.RunOutput(ctx, process.Options{Timeout: 10 * time.Second, LogPrefix: "fleet.recover"},
		"ps", "-o", "command=", "-p", fmt.Sprint(started.PID))
	if err != nil {
		return 0
	}
	command := string(out)
	if !strings.Contains(command, filepath.Base(r.binary())) || !strings.Contains(command, "stream-json") {
		return 0 // the PID now belongs to something else
	}
	return started.PID
}

// reconcileWorktrees compares the checkouts under the worktree root with the
// tasks that should own them.
func (r *Runner) reconcileWorktrees(ctx context.Context, report *RecoveryReport) error {
	tasks, err := r.Store.ListTasks(ctx, TaskFilter{})
	if err != nil {
		return err
	}
	byID := make(map[string]Task, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
		if t.WorktreePath == "" || t.Status.IsTerminal() {
			continue
		}
		if _, err := os.Stat(t.WorktreePath); errors.Is(err, os.ErrNotExist) {
			report.MissingWorktrees = append(report.MissingWorktrees, t)
		}
	}
	sort.SliceStable(report.MissingWorktrees, func(i, j int) bool {
		return report.MissingWorktrees[i].CreatedAt.Before(report.MissingWorktrees[j].CreatedAt)
	})

	root := strings.TrimSpace(r.Worktrees.Root)
	if root == "" {
		return nil
	}
	projects, err := r.Store.ListProjects(ctx)
	if err != nil {
		return err
	}
	for _, project := range projects {
		// What git knows about, keyed by path.
		known := map[string]worktree.Worktree{}
		if list, err := worktree.List(ctx, project.RepoPath); err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("project %s: list worktrees: %v", project.ID, err))
		} else {
			for _, wt := range list {
				known[canonicalPath(wt.Path)] = wt
			}
		}

		dir := filepath.Join(root, project.ID)
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			report.Problems = append(report.Problems, fmt.Sprintf("project %s: read %s: %v", project.ID, dir, err))
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			task, exists := byID[entry.Name()]
			if exists && task.ProjectID == project.ID && !task.Status.IsTerminal() {
				continue // owned by an unfinished task
			}

			orphan := OrphanWorktree{ProjectID: project.ID, TaskID: entry.Name(), Path: path}
			switch {
			case !exists:
				orphan.Reason = "no such task"
			case task.ProjectID != project.ID:
				orphan.Reason = "task belongs to project " + task.ProjectID
			default:
				orphan.Reason = "task is " + string(task.Status)
			}
			if wt, ok := known[canonicalPath(path)]; ok {
				orphan.Branch = wt.Branch
				if dirty, err := worktree.IsDirty(ctx, path); err == nil {
					orphan.Dirty = dirty
				}
			} else {
				orphan.Reason += "; not a registered git worktree"
				// Not a checkout git tracks, so "dirty" has no git meaning.
				// Anything in it is still unaccounted for.
				if inner, err := os.ReadDir(path); err == nil && len(inner) > 0 {
					orphan.Dirty = true
				}
			}
			report.Orphans = append(report.Orphans, orphan)
		}
	}
	sort.SliceStable(report.Orphans, func(i, j int) bool { return report.Orphans[i].Path < report.Orphans[j].Path })
	return nil
}

func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}
