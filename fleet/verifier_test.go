package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/alice-core/worktree"
)

// verifyEnv is a runner environment whose project has verify commands.
func newVerifyEnv(t *testing.T, commands ...string) (*runnerEnv, *Verifier) {
	t.Helper()
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = commands
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e, &Verifier{Store: e.store, ReportRoot: e.runner.ReportRoot}
}

// finishedTask runs the fake agent once so the task is verifying in a worktree.
func (e *runnerEnv) finishedTask(in NewTask, script ...string) Task {
	e.t.Helper()
	if len(script) == 0 {
		script = []string{lineInit, lineResultOK}
	}
	e.script(script...)
	task := e.task(in)
	res := e.run(task.ID, RunOptions{})
	if res.Task.Status != StatusVerifying {
		e.t.Fatalf("setup: task is %s (%s), want verifying", res.Task.Status, res.Reason)
	}
	return res.Task
}

func TestVerify_ShipPasses(t *testing.T) {
	e, v := newVerifyEnv(t, "test -f fixed.txt", "grep -q made-by-agent fixed.txt")
	task := e.finishedTask(NewTask{}, lineInit, "#touch fixed.txt", lineResultOK)

	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.Passed || res.Retry || res.Attempt != 1 || len(res.Checks) != 2 {
		t.Fatalf("result = %+v", res)
	}
	if res.Task.Status != StatusReviewReady || res.Task.StatusReason != "verification passed" {
		t.Fatalf("task = %s (%s)", res.Task.Status, res.Task.StatusReason)
	}

	stored, err := e.store.ListVerifications(ctx, task.ID)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	first := stored[0]
	if first.TaskID != task.ID || first.Attempt != 1 || first.StepIndex != 0 || first.Command != "test -f fixed.txt" ||
		!first.Passed || first.ExitCode != 0 || first.Duration < 0 || first.CreatedAt.IsZero() {
		t.Errorf("first check = %+v", first)
	}
	if stored[1].StepIndex != 1 || stored[1].Command != "grep -q made-by-agent fixed.txt" {
		t.Errorf("second check = %+v", stored[1])
	}
	events := e.eventPayloads(task.ID, EventVerification)
	if len(events) != 1 || events[0]["passed"] != true || events[0]["attempt"] != float64(1) {
		t.Errorf("verify.result = %v", events)
	}
}

// Checks run in the task's worktree, not in the main working tree.
func TestVerify_RunsInTheWorktree(t *testing.T) {
	e, v := newVerifyEnv(t, "test -f only-in-worktree.txt")
	task := e.finishedTask(NewTask{}, lineInit, "#touch only-in-worktree.txt", lineResultOK)
	if _, err := os.Stat(filepath.Join(e.repo, "only-in-worktree.txt")); err == nil {
		t.Fatal("setup: the file must not exist in the main working tree")
	}
	res, err := v.Verify(ctx, task.ID)
	if err != nil || !res.Passed {
		t.Fatalf("result = %+v, %v", res, err)
	}
}

func TestVerify_NoCommandsConfigured(t *testing.T) {
	e, v := newVerifyEnv(t)
	task := e.finishedTask(NewTask{})
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || len(res.Checks) != 0 || res.Task.Status != StatusReviewReady ||
		res.Task.StatusReason != "no verification commands configured" {
		t.Fatalf("result = %+v", res)
	}
	if res.Attempt != 1 {
		t.Errorf("an empty round still counts as attempt 1, got %d", res.Attempt)
	}
}

func TestVerify_FailureAsksForARetryWithFeedback(t *testing.T) {
	e, v := newVerifyEnv(t, "echo building", "echo 'main.go:7: undefined: Foo' >&2; exit 2", "echo never-runs")
	task := e.finishedTask(NewTask{})

	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || !res.Retry || len(res.Checks) != 2 {
		t.Fatalf("result = %+v, want a retry after stopping at the failing check", res)
	}
	if res.Task.Status != StatusVerifying {
		t.Fatalf("a task awaiting a retry must stay verifying, got %s", res.Task.Status)
	}
	failed := res.Checks[1]
	if failed.Passed || failed.ExitCode != 2 || !strings.Contains(failed.OutputTail, "undefined: Foo") {
		t.Errorf("failed check = %+v", failed)
	}
	for _, want := range []string{
		"did not pass",
		"$ echo 'main.go:7: undefined: Foo' >&2; exit 2",
		"(exit status 2)",
		"main.go:7: undefined: Foo",
		"Do not weaken, skip or delete the check.",
	} {
		if !strings.Contains(res.Feedback, want) {
			t.Errorf("feedback is missing %q:\n%s", want, res.Feedback)
		}
	}
	if strings.Contains(res.Feedback, "never-runs") {
		t.Error("checks after the failure must not run")
	}
}

func TestVerify_RetriesRunOutAfterTheConfiguredCount(t *testing.T) {
	e, v := newVerifyEnv(t, "exit 1")
	v.MaxRetries = 2
	task := e.finishedTask(NewTask{})

	for round := 1; round <= 2; round++ {
		res, err := v.Verify(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Retry || res.Attempt != round {
			t.Fatalf("round %d: result = %+v, want a retry", round, res)
		}
		// The agent "tries again" and claims to be finished once more.
		e.run(task.ID, RunOptions{Instruction: res.Feedback})
	}

	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retry || res.Attempt != 3 {
		t.Fatalf("third round = %+v, want no more retries", res)
	}
	if res.Task.Status != StatusNeedsDecision ||
		res.Task.StatusReason != "verification failed after 3 attempts: exit 1 failed (exit 1)" {
		t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
	}
	if all, _ := e.store.ListVerifications(ctx, task.ID); len(all) != 3 {
		t.Errorf("stored rounds = %d, want 3", len(all))
	}
}

func TestVerify_RetriesDisabled(t *testing.T) {
	e, v := newVerifyEnv(t, "exit 1")
	v.MaxRetries = -1
	task := e.finishedTask(NewTask{})
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Retry || res.Task.Status != StatusNeedsDecision {
		t.Fatalf("result = %+v", res)
	}
}

// A retry from the queue is a new run and gets a fresh retry budget.
func TestVerify_RequeueResetsTheRetryBudget(t *testing.T) {
	e, v := newVerifyEnv(t, "exit 1")
	v.MaxRetries = -1
	e.store.now = tickingClock(time.Now())

	task := e.finishedTask(NewTask{})
	if res, _ := v.Verify(ctx, task.ID); res.Task.Status != StatusNeedsDecision {
		t.Fatalf("first run: %+v", res)
	}
	walk(t, e.store, task.ID, StatusQueued)
	e.run(task.ID, RunOptions{})

	v.MaxRetries = 1
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Retry || res.Attempt != 2 {
		t.Fatalf("after a requeue the earlier failure must not count: %+v", res)
	}
}

func TestVerify_CommandThatTimesOutOrCannotRun(t *testing.T) {
	e, v := newVerifyEnv(t, "sleep 30")
	v.StepTimeout = 300 * time.Millisecond
	v.MaxRetries = -1
	task := e.finishedTask(NewTask{})

	start := time.Now()
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the step timeout was not enforced, took %s", elapsed)
	}
	if res.Passed || !res.Checks[0].TimedOut || !strings.Contains(res.Task.StatusReason, "timed out after 300ms") {
		t.Fatalf("result = %+v (%q)", res, res.Task.StatusReason)
	}
	stored, _ := e.store.ListVerifications(ctx, task.ID)
	if len(stored) != 1 || !stored[0].TimedOut || stored[0].Err == "" {
		t.Errorf("stored = %+v", stored)
	}
}

func TestVerify_OutputTailIsBounded(t *testing.T) {
	e, v := newVerifyEnv(t, "i=0; while [ $i -lt 400 ]; do echo \"line $i 中文\"; i=$((i+1)); done; echo THE-END; exit 1")
	v.OutputTail = 200
	task := e.finishedTask(NewTask{})
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := res.Checks[0].OutputTail
	if !strings.Contains(out, "THE-END") || strings.Contains(out, "line 0 ") || !strings.Contains(out, "earlier bytes omitted") {
		t.Errorf("tail = %q, want only the end of the output", out)
	}
	if len(out) > 300 {
		t.Errorf("tail is %d bytes, want it bounded near 200", len(out))
	}
	if !strings.Contains(res.Feedback, "THE-END") {
		t.Error("the feedback must carry the end of the output, where the error is")
	}
}

func TestVerify_Scout(t *testing.T) {
	e, v := newVerifyEnv(t, "exit 1") // project commands do not apply to scouts
	task := e.finishedTask(NewTask{Shape: ShapeScout, Goal: "look"})

	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed || res.Task.Status != StatusDone || res.Task.StatusReason != "report verified" {
		t.Fatalf("result = %+v (%q)", res, res.Task.StatusReason)
	}
	var names []string
	for _, c := range res.Checks {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "report_exists,worktree_clean,no_new_commits" {
		t.Errorf("scout checks = %v", names)
	}
	if res.Task.FinishedAt == nil {
		t.Error("a verified scout is finished")
	}
}

func TestVerify_ScoutViolations(t *testing.T) {
	cases := []struct {
		name      string
		tamper    func(t *testing.T, e *runnerEnv, task Task)
		wantCheck string
		wantText  string
	}{
		{
			name: "report missing",
			tamper: func(t *testing.T, e *runnerEnv, task Task) {
				path, _ := e.runner.ReportPath(task.ProjectID, task.ID)
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "report_exists", wantText: "no report at",
		},
		{
			name: "report empty",
			tamper: func(t *testing.T, e *runnerEnv, task Task) {
				path, _ := e.runner.ReportPath(task.ProjectID, task.ID)
				if err := os.WriteFile(path, []byte("  \n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "report_exists", wantText: "is empty",
		},
		{
			name: "untracked file left in the checkout",
			tamper: func(t *testing.T, e *runnerEnv, task Task) {
				if err := os.WriteFile(filepath.Join(task.WorktreePath, "sneaky.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "worktree_clean", wantText: "?? sneaky.txt",
		},
		{
			name: "tracked file modified",
			tamper: func(t *testing.T, e *runnerEnv, task Task) {
				if err := os.WriteFile(filepath.Join(task.WorktreePath, "README.md"), []byte("edited\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			wantCheck: "worktree_clean", wantText: "README.md",
		},
		{
			name: "commit made on the branch",
			tamper: func(t *testing.T, e *runnerEnv, task Task) {
				if err := os.WriteFile(filepath.Join(task.WorktreePath, "committed.txt"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitT(t, task.WorktreePath, "add", ".")
				gitT(t, task.WorktreePath, "commit", "-q", "-m", "scouts must not commit")
			},
			wantCheck: "no_new_commits", wantText: "1 new commit(s)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, v := newVerifyEnv(t)
			task := e.finishedTask(NewTask{Shape: ShapeScout, Goal: "look"})
			tc.tamper(t, e, task)

			res, err := v.Verify(ctx, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if res.Passed || res.Retry {
				t.Fatalf("result = %+v, want a failure that is not retried", res)
			}
			if res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Task.StatusReason, "scout verification failed: "+tc.wantCheck) {
				t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
			}
			failed := res.Checks[len(res.Checks)-1]
			if failed.Name != tc.wantCheck || !strings.Contains(failed.OutputTail, tc.wantText) {
				t.Errorf("failing check = %+v, want %s mentioning %q", failed, tc.wantCheck, tc.wantText)
			}
		})
	}
}

func TestVerify_Refusals(t *testing.T) {
	e, v := newVerifyEnv(t, "true")
	queued := e.task(NewTask{})
	if _, err := v.Verify(ctx, queued.ID); err == nil || !strings.Contains(err.Error(), "not verifying") {
		t.Errorf("a queued task: err = %v", err)
	}
	if got, _ := e.store.GetTask(ctx, queued.ID); got.Status != StatusQueued {
		t.Errorf("a refused verification changed the task to %s", got.Status)
	}
	if _, err := v.Verify(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("a missing task: err = %v", err)
	}
	if _, err := (&Verifier{}).Verify(ctx, queued.ID); err == nil {
		t.Error("a Verifier without a Store must refuse")
	}
	if all, _ := e.store.ListVerifications(ctx, queued.ID); len(all) != 0 {
		t.Errorf("refused verifications must store nothing: %+v", all)
	}
}

// If the host shuts down mid-check, the task must not stay verifying.
func TestVerify_ContextCancelledDuringChecks(t *testing.T) {
	e, v := newVerifyEnv(t, "sleep 30")
	task := e.finishedTask(NewTask{})
	c, cancel := context.WithCancel(ctx)
	time.AfterFunc(300*time.Millisecond, cancel)

	res, err := v.Verify(c, task.ID)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Retry || res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Task.StatusReason, "verification interrupted") {
		t.Fatalf("result = %+v (%q)", res, res.Task.StatusReason)
	}
}

func TestRecordAndListVerifications(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})

	first, err := store.RecordVerification(ctx, task.ID, []Verification{
		{Name: "build", Command: "go build ./...", Passed: true, Duration: 1500 * time.Millisecond},
		{Name: "test", Command: "go test ./...", ExitCode: 1, OutputTail: "FAIL", Duration: 2 * time.Second},
	})
	if err != nil || first != 1 {
		t.Fatalf("first round = %d, %v", first, err)
	}
	second, err := store.RecordVerification(ctx, task.ID, []Verification{
		{Name: "lint", Command: "golangci-lint run", ExitCode: -1, TimedOut: true, Err: "timed out after 5m0s"},
	})
	if err != nil || second != 2 {
		t.Fatalf("second round = %d, %v", second, err)
	}
	empty, err := store.RecordVerification(ctx, task.ID, nil)
	if err != nil || empty != 3 {
		t.Fatalf("an empty round = %d, %v", empty, err)
	}

	all, err := store.ListVerifications(ctx, task.ID)
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	if all[0].Attempt != 1 || all[0].StepIndex != 0 || !all[0].Passed || all[0].Duration != 1500*time.Millisecond {
		t.Errorf("check 0 = %+v", all[0])
	}
	if all[1].StepIndex != 1 || all[1].Passed || all[1].ExitCode != 1 || all[1].OutputTail != "FAIL" {
		t.Errorf("check 1 = %+v", all[1])
	}
	if all[2].Attempt != 2 || !all[2].TimedOut || all[2].ExitCode != -1 || all[2].Err != "timed out after 5m0s" {
		t.Errorf("check 2 = %+v", all[2])
	}

	if _, err := store.RecordVerification(ctx, "missing", nil); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}
	other := mustCreate(t, store, NewTask{})
	if none, _ := store.ListVerifications(ctx, other.ID); none == nil || len(none) != 0 {
		t.Errorf("a task with no checks = %#v", none)
	}
	if n, _ := store.RecordVerification(ctx, other.ID, []Verification{{Name: "x", Passed: true}}); n != 1 {
		t.Errorf("attempt numbers are per task, got %d", n)
	}
}

func TestTail(t *testing.T) {
	if got := tail("short", 10); got != "short" {
		t.Errorf("tail under the limit = %q", got)
	}
	if got := tail("0123456789", 4); got != "[6 earlier bytes omitted] …6789" {
		t.Errorf("tail = %q", got)
	}
	// Cutting inside "é" (2 bytes) must move forward to the next character.
	if got := tail("aéz", 2); got != "[3 earlier bytes omitted] …z" {
		t.Errorf("tail on a rune boundary = %q", got)
	}
}

// A scout's checkout must be the one the runner created for it.
func TestVerify_ScoutWithoutReportRoot(t *testing.T) {
	e, v := newVerifyEnv(t)
	task := e.finishedTask(NewTask{Shape: ShapeScout, Goal: "look"})
	v.ReportRoot = ""
	res, err := v.Verify(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Checks[0].OutputTail, "ReportRoot is not set") {
		t.Fatalf("result = %+v", res)
	}
	if dirty, _ := worktree.IsDirty(ctx, task.WorktreePath); dirty {
		t.Error("verification must not dirty the checkout")
	}
}
