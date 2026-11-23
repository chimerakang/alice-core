package fleet

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chimerakang/alice-core/worktree"
)

func TestRecover_NothingToDo(t *testing.T) {
	e := newRunnerEnv(t)
	e.task(NewTask{}) // queued tasks are fine as they are
	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !report.Empty() {
		t.Fatalf("report = %+v, want it empty", report)
	}
}

// A task the database says is running, with no process behind it.
func TestRecover_StaleRunningTask(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})             // gives it a session and a worktree
	walk(t, e.store, task.ID, StatusRunning) // then "the host crashed mid-run"

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(report.Recovered) != 1 {
		t.Fatalf("recovered = %+v", report.Recovered)
	}
	got := report.Recovered[0]
	if got.From != StatusRunning || got.KilledPID != 0 || got.Task.ID != task.ID {
		t.Errorf("recovered = %+v", got)
	}
	if got.Task.Status != StatusNeedsDecision || !strings.Contains(got.Task.StatusReason, "host restarted") {
		t.Errorf("task = %s (%q)", got.Task.Status, got.Task.StatusReason)
	}
	if got.Task.Liveness != LivenessDead {
		t.Errorf("liveness = %q, want dead", got.Task.Liveness)
	}
	if got.Task.SessionID != "sess-1" || got.Task.WorktreePath == "" {
		t.Errorf("session and workspace must survive recovery: %+v", got.Task)
	}

	// The slot is free again, and the task can be picked up where it stopped.
	if n, _ := e.store.CountTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning, StatusVerifying}}); n != 0 {
		t.Errorf("%d tasks still hold a slot", n)
	}
	e.script(lineInit, lineResultOK)
	res := e.run(task.ID, RunOptions{})
	if res.Outcome != OutcomeFinished || res.Task.Liveness != LivenessIdle {
		t.Fatalf("resumed run = %+v (liveness %q)", res, res.Task.Liveness)
	}
	args := e.args(e.invocations())
	if args[len(args)-3] != "--resume" || args[len(args)-2] != "sess-1" {
		t.Errorf("recovery must leave the task resumable: %q", args)
	}
}

func TestRecover_StaleVerifyingTask(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.finishedTask(NewTask{}) // left verifying, as if the host died mid-check

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recovered) != 1 || report.Recovered[0].From != StatusVerifying {
		t.Fatalf("recovered = %+v", report.Recovered)
	}
	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusNeedsDecision || !strings.Contains(got.StatusReason, "verification was cut off") {
		t.Fatalf("task = %s (%q)", got.Status, got.StatusReason)
	}
}

func TestRecover_IsIdempotentAndOrdered(t *testing.T) {
	e := newRunnerEnv(t)
	e.store.now = tickingClock(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))
	first := e.task(NewTask{Goal: "first"})
	second := e.task(NewTask{Goal: "second"})
	walk(t, e.store, second.ID, StatusRunning)
	walk(t, e.store, first.ID, StatusRunning)

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range report.Recovered {
		ids = append(ids, r.Task.ID)
	}
	if !reflect.DeepEqual(ids, []string{first.ID, second.ID}) {
		t.Fatalf("recovered = %v, want oldest task first", ids)
	}

	again, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Recovered) != 0 {
		t.Fatalf("a second pass recovered %+v", again.Recovered)
	}
}

// A run that really is alive in this Runner must not be touched.
func TestRecover_LeavesLiveRunsAlone(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recovered) != 0 {
		t.Fatalf("a live run was recovered: %+v", report.Recovered)
	}
	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusRunning || got.Liveness != LivenessBusy {
		t.Fatalf("live task = %s, liveness %q", got.Status, got.Liveness)
	}
	if err := e.runner.Cancel(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)
}

// The host process died but the agent it started lived on. A new Runner
// knows nothing about it, so recovery has to find and stop it.
func TestRecover_StopsAnAgentLeftRunning(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#ignore-int", "#sleep 60")
	task := e.task(NewTask{})
	oldHost := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	started := e.eventPayloads(task.ID, EventRunStarted)
	pid := int(started[len(started)-1]["pid"].(float64))
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("setup: the agent process %d should be alive: %v", pid, err)
	}

	// A Runner as it would be after a restart: same store, no memory of runs.
	newHost := &Runner{Store: e.store, Worktrees: e.runner.Worktrees, ReportRoot: e.runner.ReportRoot, Binary: e.runner.Binary}
	report, err := newHost.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The old Runner sees its process die; either side may record the outcome.
	select {
	case <-oldHost:
	case <-time.After(20 * time.Second):
		t.Fatal("the leftover agent was not stopped")
	}
	e.waitUntil("the leftover process to disappear", func() bool { return syscall.Kill(pid, 0) != nil })

	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusNeedsDecision {
		t.Fatalf("task = %s (%q), want needs_decision", got.Status, got.StatusReason)
	}
	if len(report.Recovered) == 1 {
		if report.Recovered[0].KilledPID != pid {
			t.Errorf("KilledPID = %d, want %d", report.Recovered[0].KilledPID, pid)
		}
		if !strings.Contains(report.Recovered[0].Task.StatusReason, "without supervision") {
			t.Errorf("reason = %q", report.Recovered[0].Task.StatusReason)
		}
	}
	if n, _ := e.store.CountTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning, StatusVerifying}}); n != 0 {
		t.Errorf("%d tasks still hold a slot", n)
	}
}

// A recorded PID that now belongs to an unrelated process must not be killed.
func TestRecover_DoesNotKillAnUnrelatedProcess(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning)
	// Pretend the task's agent had the PID of this very test process.
	if _, err := e.store.AppendEvent(ctx, task.ID, EventRunStarted, map[string]any{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Recovered) != 1 || report.Recovered[0].KilledPID != 0 {
		t.Fatalf("recovered = %+v, want the task recovered without killing anything", report.Recovered)
	}
	// Reaching this line at all means the test process survived.
	if !strings.Contains(report.Recovered[0].Task.StatusReason, "agent process is gone") {
		t.Errorf("reason = %q", report.Recovered[0].Task.StatusReason)
	}
}

func TestRecover_ReportsOrphanWorktrees(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultOK)
	root := e.runner.Worktrees.Root

	// An unfinished task: its checkout is owned.
	owned := e.task(NewTask{Goal: "owned"})
	e.run(owned.ID, RunOptions{})

	// A finished task whose checkout was left behind, with uncommitted work.
	finished := e.task(NewTask{Goal: "finished"})
	finishedRes := e.run(finished.ID, RunOptions{})
	walk(t, e.store, finished.ID, StatusReviewReady, StatusDone)
	if err := os.WriteFile(filepath.Join(finishedRes.Task.WorktreePath, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A checkout under the root that no task accounts for.
	strayPath := filepath.Join(root, "app", "feedfacecafe")
	if _, err := worktree.Create(ctx, e.repo, strayPath, "alice/stray", "main"); err != nil {
		t.Fatal(err)
	}

	// A plain directory under the root that git knows nothing about.
	junkPath := filepath.Join(root, "app", "not-a-worktree")
	if err := os.MkdirAll(junkPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junkPath, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The owner's own worktree elsewhere: not fleet's business.
	elsewhere := filepath.Join(t.TempDir(), "my-own-checkout")
	if _, err := worktree.Create(ctx, e.repo, elsewhere, "my-own-branch", "main"); err != nil {
		t.Fatal(err)
	}

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	type summary struct {
		TaskID, Branch, Reason string
		Dirty                  bool
	}
	got := map[string]summary{}
	for _, o := range report.Orphans {
		if o.ProjectID != "app" {
			t.Errorf("orphan with project %q: %+v", o.ProjectID, o)
		}
		got[o.Path] = summary{o.TaskID, o.Branch, o.Reason, o.Dirty}
	}
	want := map[string]summary{
		finishedRes.Task.WorktreePath: {finished.ID, "alice/" + finished.ID, "task is done", true},
		strayPath:                     {"feedfacecafe", "alice/stray", "no such task", false},
		junkPath:                      {"not-a-worktree", "", "no such task; not a registered git worktree", true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("orphans =\n  %+v\nwant\n  %+v", got, want)
	}

	// Reported, never removed.
	for path := range want {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("recovery removed %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(finishedRes.Task.WorktreePath, "uncommitted.txt")); err != nil {
		t.Errorf("uncommitted work was lost: %v", err)
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Errorf("a worktree outside the root was touched: %v", err)
	}
	if len(report.Recovered) != 1 || report.Recovered[0].Task.ID != owned.ID {
		t.Errorf("recovered = %+v, want only the task left verifying", report.Recovered)
	}
}

func TestRecover_ReportsMissingWorktrees(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	waiting := e.task(NewTask{Goal: "waiting"})
	waitingRes := e.run(waiting.ID, RunOptions{}) // needs_decision, has a worktree
	gone := e.task(NewTask{Goal: "finished"})
	goneRes := e.run(gone.ID, RunOptions{})
	walk(t, e.store, gone.ID, StatusCancelled)

	for _, path := range []string{waitingRes.Task.WorktreePath, goneRes.Task.WorktreePath} {
		if err := worktree.Remove(ctx, path, true); err != nil {
			t.Fatal(err)
		}
	}

	report, err := e.runner.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingWorktrees) != 1 || report.MissingWorktrees[0].ID != waiting.ID {
		t.Fatalf("missing = %+v, want only the unfinished task", report.MissingWorktrees)
	}
	if len(report.Orphans) != 0 || len(report.Recovered) != 0 {
		t.Errorf("report = %+v", report)
	}
	if report.Empty() {
		t.Error("a report with a missing worktree is not empty")
	}
}

func TestRecover_WithoutAWorktreeRoot(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning)
	bare := &Runner{Store: e.store}
	report, err := bare.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(report.Recovered) != 1 || len(report.Orphans) != 0 {
		t.Fatalf("report = %+v", report)
	}
	if _, err := (&Runner{}).Recover(ctx); err == nil {
		t.Error("Recover without a Store must fail")
	}
}

// Starting the scheduler is enough to clear tasks a crash left active.
func TestScheduler_RecoversBeforeScheduling(t *testing.T) {
	e := newRunnerEnv(t) // "app" allows one task at a time
	e.script(lineInit, lineResultOK)
	stale := e.task(NewTask{Goal: "was running when the host died"})
	walk(t, e.store, stale.ID, StatusRunning)
	queued := e.task(NewTask{Goal: "waiting for the slot"})

	s := e.scheduler(1)
	var reported RecoveryReport
	s.OnRecovery = func(r RecoveryReport) { reported = r }
	e.runUntilIdle(s)

	if len(reported.Recovered) != 1 || reported.Recovered[0].Task.ID != stale.ID {
		t.Fatalf("OnRecovery got %+v", reported)
	}
	if got, _ := e.store.GetTask(ctx, stale.ID); got.Status != StatusNeedsDecision {
		t.Errorf("stale task = %s", got.Status)
	}
	// Without recovery the stale task would hold the project's only slot for ever.
	if got, _ := e.store.GetTask(ctx, queued.ID); got.Status != StatusReviewReady {
		t.Fatalf("queued task = %s (%q), want it to have run", got.Status, got.StatusReason)
	}
}

func TestScheduler_OnRecoveryIsSkippedWhenThereIsNothing(t *testing.T) {
	e := newRunnerEnv(t)
	s := e.scheduler(1)
	called := false
	s.OnRecovery = func(RecoveryReport) { called = true }
	e.runUntilIdle(s)
	if called {
		t.Error("OnRecovery must not fire for an empty report")
	}
}

func TestLiveness(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	if task.Liveness != LivenessUnknown {
		t.Errorf("a new task's liveness = %q, want unknown", task.Liveness)
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Liveness != LivenessUnknown {
		t.Errorf("stored liveness = %q, want unknown: a missing signal is never idle", got.Liveness)
	}

	for _, l := range []Liveness{LivenessBusy, LivenessIdle, LivenessDead, LivenessUnknown} {
		if err := e.store.SetLiveness(ctx, task.ID, l); err != nil {
			t.Fatalf("SetLiveness(%s): %v", l, err)
		}
		if got, _ := e.store.GetTask(ctx, task.ID); got.Liveness != l {
			t.Errorf("liveness = %q, want %q", got.Liveness, l)
		}
	}
	if err := e.store.SetLiveness(ctx, task.ID, "sleeping"); err == nil {
		t.Error("an unknown liveness must be rejected")
	}
	if err := e.store.SetLiveness(ctx, "missing", LivenessBusy); err == nil {
		t.Error("a missing task must be rejected")
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusQueued {
		t.Errorf("liveness changes must not touch the status: %s", got.Status)
	}
}

// A database created before the liveness column existed must keep working.
func TestStore_AddsLivenessToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// fleet_tasks as it was first released: no liveness column.
	if _, err := db.Exec(`CREATE TABLE fleet_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, shape TEXT NOT NULL, goal TEXT NOT NULL,
		source_kind TEXT NOT NULL, issue_number INTEGER NOT NULL DEFAULT 0, trust_mode TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '', priority INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL,
		status_reason TEXT NOT NULL DEFAULT '', branch TEXT NOT NULL DEFAULT '',
		worktree_path TEXT NOT NULL DEFAULT '', session_id TEXT NOT NULL DEFAULT '',
		input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
		started_at INTEGER, finished_at INTEGER, last_activity_at INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fleet_tasks
		(id, project_id, shape, goal, source_kind, trust_mode, status, created_at, updated_at)
		VALUES ('old-task', 'app', 'ship', 'from before', 'manual', 'local-only', 'queued', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("opening an older database: %v", err)
	}
	defer store.Close()
	got, err := store.GetTask(context.Background(), "old-task")
	if err != nil {
		t.Fatalf("reading a task from before the migration: %v", err)
	}
	if got.Liveness != LivenessUnknown || got.Goal != "from before" {
		t.Fatalf("task = %+v", got)
	}
	if err := store.SetLiveness(ctx, "old-task", LivenessDead); err != nil {
		t.Fatalf("writing the new column: %v", err)
	}
	store.Close()
	if again, err := OpenStore(path); err != nil {
		t.Fatalf("opening a second time must not try to add the column again: %v", err)
	} else {
		again.Close()
	}
}
