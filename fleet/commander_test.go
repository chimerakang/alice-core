package fleet

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var owner = Actor{ID: "ck", Via: "telegram"}

func (e *runnerEnv) commander() *Commander {
	return &Commander{Store: e.store, Scheduler: e.scheduler(2)}
}

func (e *runnerEnv) audit(filter AuditFilter) []AuditEntry {
	e.t.Helper()
	entries, err := e.store.ListAudit(ctx, filter)
	if err != nil {
		e.t.Fatal(err)
	}
	return entries
}

func (e *runnerEnv) waitForStatus(taskID string, want Status) Task {
	e.t.Helper()
	var got Task
	e.waitUntil("task "+taskID+" to become "+string(want), func() bool {
		got, _ = e.store.GetTask(ctx, taskID)
		return got.Status == want
	})
	return got
}

func TestCommander_RequiresAnActor(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	task := e.task(NewTask{})
	nobody := Actor{Via: "web"}

	calls := map[string]func() error{
		"RegisterProject": func() error { _, err := c.RegisterProject(ctx, nobody, Project{RepoPath: e.repo}); return err },
		"UpdateProject": func() error {
			_, err := c.UpdateProject(ctx, nobody, "app", "", func(*Project) error { return nil })
			return err
		},
		"Dispatch": func() error {
			_, err := c.Dispatch(ctx, nobody, DispatchRequest{ProjectID: "app", Goal: "x"})
			return err
		},
		"Cancel":  func() error { _, err := c.Cancel(ctx, nobody, task.ID, ""); return err },
		"Retry":   func() error { _, err := c.Retry(ctx, nobody, task.ID); return err },
		"Reply":   func() error { _, err := c.Reply(ctx, nobody, ReplyRequest{TaskID: task.ID}); return err },
		"Approve": func() error { _, err := c.Approve(ctx, nobody, task.ID); return err },
		"Merge":   func() error { _, err := c.Merge(ctx, nobody, task.ID); return err },
		"Discard": func() error { _, err := c.Discard(ctx, nobody, task.ID); return err },
	}
	for name, call := range calls {
		if err := call(); !errors.Is(err, ErrNoActor) {
			t.Errorf("%s without an actor: %v", name, err)
		}
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusQueued {
		t.Errorf("an anonymous command changed the task to %s", got.Status)
	}
	if n, _ := e.store.CountTasks(ctx, TaskFilter{}); n != 1 {
		t.Errorf("an anonymous dispatch created a task")
	}
	if entries := e.audit(AuditFilter{}); len(entries) != 0 {
		t.Errorf("anonymous commands must not be audited as if someone gave them: %+v", entries)
	}
}

func TestCommander_ProjectCommandsAreAudited(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	repo := newRepo(t, "second")

	p, err := c.RegisterProject(ctx, owner, Project{RepoPath: repo})
	if err != nil || p.ID != "second" {
		t.Fatalf("RegisterProject = %+v, %v", p, err)
	}
	if _, err := c.RegisterProject(ctx, owner, Project{RepoPath: t.TempDir()}); !errors.Is(err, ErrNotGitRepo) {
		t.Fatalf("registering a non-repository: %v", err)
	}
	updated, err := c.UpdateProject(ctx, owner, "second", "allow three at once", func(p *Project) error { p.MaxConcurrent = 3; return nil })
	if err != nil || updated.MaxConcurrent != 3 {
		t.Fatalf("UpdateProject = %+v, %v", updated, err)
	}

	entries := e.audit(AuditFilter{}) // newest first
	if len(entries) != 3 {
		t.Fatalf("audit = %+v", entries)
	}
	if got := entries[0]; got.Command != CmdUpdateProject || got.ProjectID != "second" || got.Detail != "allow three at once" ||
		!got.OK || got.Actor != "ck" || got.Via != "telegram" || got.CreatedAt.IsZero() {
		t.Errorf("update entry = %+v", got)
	}
	if got := entries[1]; got.Command != CmdRegisterProject || got.OK || !strings.Contains(got.Error, "not a git repository") {
		t.Errorf("a refused command must be audited with its error: %+v", got)
	}
	if got := entries[2]; got.Command != CmdRegisterProject || !got.OK || got.ProjectID != "second" || got.Detail != repo {
		t.Errorf("register entry = %+v", got)
	}
}

func TestCommander_DispatchRunsThroughTheScheduler(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	e.script(lineInit, lineResultOK)
	stop := e.startScheduler(c.Scheduler)
	defer stop()

	task, err := c.Dispatch(ctx, owner, DispatchRequest{ProjectID: "app", Goal: "Add dark mode\n\nUsers asked.", Priority: 4})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if task.Status != StatusQueued || task.Priority != 4 || task.Source.Kind != SourceManual {
		t.Fatalf("task = %+v", task)
	}
	// PollInterval is short here, but the kick is what the command relies on.
	e.waitForStatus(task.ID, StatusReviewReady)

	entries := e.audit(AuditFilter{TaskID: task.ID})
	if len(entries) != 1 || entries[0].Command != CmdDispatch || entries[0].Detail != "Add dark mode" || entries[0].ProjectID != "app" {
		t.Fatalf("audit = %+v", entries)
	}

	if _, err := c.Dispatch(ctx, owner, DispatchRequest{ProjectID: "ghost", Goal: "x"}); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("dispatch to an unknown project: %v", err)
	}
	if failed := e.audit(AuditFilter{ProjectID: "ghost"}); len(failed) != 1 || failed[0].OK {
		t.Errorf("the refused dispatch must be audited: %+v", failed)
	}
}

func TestCommander_DispatchFromAnIssue(t *testing.T) {
	e := newIssueEnv(t)
	c := &Commander{Store: e.store, Scheduler: e.sched}
	task, err := c.Dispatch(ctx, owner, DispatchRequest{ProjectID: "app", Issue: 42, Model: "m"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if task.Source != (Source{Kind: SourceIssue, IssueNumber: 42}) || !strings.Contains(task.Goal, "修好不穩定的上傳") || task.Model != "m" {
		t.Fatalf("task = %+v", task)
	}
	if entries := e.audit(AuditFilter{TaskID: task.ID}); len(entries) != 1 || entries[0].Detail != "issue #42" {
		t.Errorf("audit = %+v", entries)
	}
}

func TestCommander_Cancel(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()

	queued := e.task(NewTask{})
	got, err := c.Cancel(ctx, owner, queued.ID, "")
	if err != nil || got.Status != StatusCancelled || got.StatusReason != "cancelled by ck" {
		t.Fatalf("cancel a queued task = %s (%q), %v", got.Status, got.StatusReason, err)
	}

	// A live run is interrupted and becomes cancelled when the agent exits.
	e.script(lineInit, "#sleep 30")
	running := e.task(NewTask{})
	out := e.runAsync(ctx, running.ID)
	e.waitForStart(running.ID)
	if _, err := c.Cancel(ctx, owner, running.ID, "changed my mind"); err != nil {
		t.Fatalf("cancel a running task: %v", err)
	}
	res := awaitRun(t, out)
	if res.Task.Status != StatusCancelled || res.Reason != "changed my mind" {
		t.Fatalf("running task = %s (%q)", res.Task.Status, res.Reason)
	}

	// Already finished: refused, and the refusal is on record.
	if _, err := c.Cancel(ctx, owner, queued.ID, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("cancel a cancelled task: %v", err)
	}
	if _, err := c.Cancel(ctx, owner, "missing", ""); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("cancel a missing task: %v", err)
	}
	entries := e.audit(AuditFilter{TaskID: queued.ID})
	if len(entries) != 2 || entries[0].OK || !entries[1].OK || entries[1].Detail != "cancelled by ck" {
		t.Errorf("audit = %+v", entries)
	}
}

func TestCommander_Retry(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning, StatusFailed)

	got, err := c.Retry(ctx, owner, task.ID)
	if err != nil || got.Status != StatusQueued || got.StatusReason != "retry requested by ck" {
		t.Fatalf("Retry = %s (%q), %v", got.Status, got.StatusReason, err)
	}
	if _, err := c.Retry(ctx, owner, task.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("retrying a queued task: %v", err)
	}
}

func TestCommander_ReplyResumesInTheBackground(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{}) // the agent asks a question

	e.script(lineInit, "#sleep 0.3", lineResultOK)
	start := time.Now()
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: task.ID, Text: "用方案 B"}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if time.Since(start) > 250*time.Millisecond {
		t.Errorf("Reply took %s; it must return once the run is accepted, not when it ends", time.Since(start))
	}
	// A second reply while the first is being worked on is refused.
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: task.ID, Text: "and also"}); !errors.Is(err, ErrConflict) && !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a reply to a task that is already running: %v", err)
	}

	// The resumed task goes on through verification on its own.
	done := e.waitForStatus(task.ID, StatusReviewReady)
	args := e.args(2)
	if args[len(args)-3] != "--resume" || args[len(args)-2] != "sess-1" || args[len(args)-1] != "用方案 B" {
		t.Errorf("resume args = %q", args)
	}
	if done.StatusReason != "no verification commands configured" {
		t.Errorf("reason = %q", done.StatusReason)
	}
	c.Scheduler.Wait()

	entries := e.audit(AuditFilter{TaskID: task.ID})
	if len(entries) != 2 || entries[1].Command != CmdReply || entries[1].Detail != "用方案 B" || !entries[1].OK || entries[0].OK {
		t.Errorf("audit = %+v", entries)
	}
}

func TestCommander_ReplyRefusals(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()

	queued := e.task(NewTask{})
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: queued.ID, Text: "hi"}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a reply to a task that has not started: %v", err)
	}
	finished := e.task(NewTask{})
	walk(t, e.store, finished.ID, StatusCancelled)
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: finished.ID}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a reply to a finished task: %v", err)
	}
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: "missing"}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("a reply to a missing task: %v", err)
	}
	if _, err := (&Commander{Store: e.store}).Reply(ctx, owner, ReplyRequest{TaskID: queued.ID}); err == nil {
		t.Error("a Commander without a Scheduler cannot resume tasks")
	}
	if e.invocations() != 0 {
		t.Fatalf("refused replies started %d agents", e.invocations())
	}

	// Over budget: refused, unless the owner says to go ahead anyway.
	e.runner.Budget = &Budget{Task: Limit{CostUSD: 0.05}}
	e.script(lineInit, lineResultAsk) // this run costs $0.10
	waiting := e.task(NewTask{})
	if _, err := e.runner.Run(ctx, waiting.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: waiting.ID, Text: "go on"}); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("a reply past the budget: %v", err)
	}
	e.script(lineInit, lineResultOK)
	if _, err := c.Reply(ctx, owner, ReplyRequest{TaskID: waiting.ID, Text: "go on", IgnoreBudget: true}); err != nil {
		t.Fatalf("the owner's override: %v", err)
	}
	e.waitForStatus(waiting.ID, StatusReviewReady)
	c.Scheduler.Wait()
}

func TestCommander_ApproveAndMerge(t *testing.T) {
	t.Run("approve fast-forwards a local-only task", func(t *testing.T) {
		e := newDeliveryEnv(t, TrustLocalOnly)
		c := &Commander{Store: e.store, Deliverer: e.deliverer}
		task := e.reviewReady("Add the feature")
		e.deliver(task.ID)

		done, err := c.Approve(ctx, owner, task.ID)
		if err != nil || done.Status != StatusDone || done.StatusReason != "approved by ck: fast-forwarded main" {
			t.Fatalf("Approve = %s (%q), %v", done.Status, done.StatusReason, err)
		}
		if entries := e.audit(AuditFilter{TaskID: task.ID}); len(entries) != 1 || entries[0].Command != CmdApprove || entries[0].ProjectID != "web" {
			t.Errorf("audit = %+v", entries)
		}
		if _, err := c.Approve(ctx, owner, task.ID); !errors.Is(err, ErrNothingToApprove) {
			t.Errorf("approving twice: %v", err)
		}
	})

	t.Run("merge lands a pull request the owner asks for", func(t *testing.T) {
		e := newDeliveryEnv(t, TrustDirectPR)
		c := &Commander{Store: e.store, Deliverer: e.deliverer}
		task := e.reviewReady("Add the feature")
		e.deliver(task.ID) // direct-pr: pull request opened, task done

		if _, err := c.Approve(ctx, owner, task.ID); !errors.Is(err, ErrNothingToApprove) {
			t.Fatalf("a direct-pr task has no approval step: %v", err)
		}
		merged, err := c.Merge(ctx, owner, task.ID)
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if !reflect.DeepEqual(e.forge.merged, []int{7}) || merged.Status != StatusDone {
			t.Fatalf("merged = %v, task %s", e.forge.merged, merged.Status)
		}
		approved := e.eventPayloads(task.ID, EventDeliveryApproved)
		if len(approved) != 1 || approved[0]["approver"] != "ck" {
			t.Errorf("delivery.approved = %v", approved)
		}
	})

	t.Run("merge approves a task that is waiting", func(t *testing.T) {
		e := newDeliveryEnv(t, TrustNoMistakes, "true")
		c := &Commander{Store: e.store, Deliverer: e.deliverer}
		task := e.reviewReady("Add the feature")
		e.deliver(task.ID)
		done, err := c.Merge(ctx, owner, task.ID)
		if err != nil || done.Status != StatusDone || !reflect.DeepEqual(e.forge.merged, []int{7}) {
			t.Fatalf("Merge = %s, %v, merged %v", done.Status, err, e.forge.merged)
		}
	})

	t.Run("merge with nothing to merge", func(t *testing.T) {
		e := newDeliveryEnv(t, TrustLocalOnly)
		c := &Commander{Store: e.store, Deliverer: e.deliverer}
		queued := e.task(NewTask{ProjectID: "web"})
		if _, err := c.Merge(ctx, owner, queued.ID); !errors.Is(err, ErrNothingToApprove) {
			t.Fatalf("Merge on a queued task: %v", err)
		}
		if entries := e.audit(AuditFilter{TaskID: queued.ID}); len(entries) != 1 || entries[0].OK || entries[0].ProjectID != "web" {
			t.Errorf("audit = %+v", entries)
		}
	})
}

func TestCommander_Discard(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	c := &Commander{Store: e.store, Scheduler: e.scheduler(1)}
	task := e.reviewReady("Add the feature")
	gitT(t, task.WorktreePath, "add", "-A")
	gitT(t, task.WorktreePath, "commit", "-q", "-m", "committed part")
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "uncommitted.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := c.Discard(ctx, owner, task.ID)
	if err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if got.Status != StatusCancelled || got.StatusReason != "discarded by ck" {
		t.Fatalf("task = %s (%q)", got.Status, got.StatusReason)
	}
	if _, err := os.Stat(task.WorktreePath); err == nil {
		t.Error("the checkout must be removed, uncommitted changes and all")
	}
	// The branch, and what was committed on it, can still be recovered by hand.
	if out := gitT(t, e.repo, "log", "-1", "--format=%s", task.Branch); out != "committed part" {
		t.Errorf("branch tip = %q", out)
	}
	if ev := e.eventPayloads(task.ID, EventDiscarded); len(ev) != 1 || ev[0]["checkout_removed"] != true || ev[0]["by"] != "ck" {
		t.Errorf("discarded event = %v", ev)
	}

	// Discarding again finds nothing left to remove, and says so without failing.
	if _, err := c.Discard(ctx, owner, task.ID); err != nil {
		t.Fatalf("discarding a task whose checkout is already gone: %v", err)
	}
	if ev := e.eventPayloads(task.ID, EventDiscarded); len(ev) != 2 || ev[1]["checkout_removed"] != false {
		t.Errorf("second discarded event = %v", ev)
	}

	// A task with a live agent must be cancelled first.
	e.script(lineInit, "#sleep 30")
	live := e.task(NewTask{ProjectID: "web"})
	out := e.runAsync(ctx, live.ID)
	e.waitForStart(live.ID)
	if _, err := c.Discard(ctx, owner, live.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("discarding a running task: %v", err)
	}
	if !e.runner.Running(live.ID) {
		t.Fatal("a refused discard must not stop the agent")
	}
	if err := e.runner.Cancel(live.ID, ""); err != nil {
		t.Fatal(err)
	}
	res := awaitRun(t, out)

	// Finished tasks can be discarded to clean up what they left behind.
	if _, err := c.Discard(ctx, owner, live.ID); err != nil {
		t.Fatalf("discarding a finished task: %v", err)
	}
	if _, err := os.Stat(res.Task.WorktreePath); err == nil {
		t.Error("a finished task's checkout must be removed by Discard")
	}
}

func TestCommander_TaskEventsPaging(t *testing.T) {
	e := newRunnerEnv(t)
	c := e.commander()
	e.script(lineInit, lineText, lineToolUse, lineToolRes, lineResultOK)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})
	all, _ := e.store.ListEvents(ctx, task.ID, 0, 0)

	var seen []int64
	cursor := int64(0)
	for page := 0; ; page++ {
		events, next, more, err := c.TaskEvents(ctx, task.ID, cursor, 4)
		if err != nil {
			t.Fatalf("TaskEvents: %v", err)
		}
		if len(events) > 4 {
			t.Fatalf("a page of %d exceeds the limit", len(events))
		}
		for _, ev := range events {
			seen = append(seen, ev.ID)
		}
		cursor = next
		if !more {
			break
		}
		if page > 10 {
			t.Fatal("paging does not terminate")
		}
	}
	if len(seen) != len(all) {
		t.Fatalf("paged %d events, the task has %d", len(seen), len(all))
	}
	for i := range all {
		if seen[i] != all[i].ID {
			t.Fatalf("event %d: got ID %d, want %d", i, seen[i], all[i].ID)
		}
	}
	// Nothing newer: an empty page, and the cursor stays where it was.
	events, next, more, err := c.TaskEvents(ctx, task.ID, cursor, 4)
	if err != nil || len(events) != 0 || next != cursor || more {
		t.Fatalf("past the end: %d events, next %d, more %v, %v", len(events), next, more, err)
	}
	if _, _, _, err := c.TaskEvents(ctx, "missing", 0, 10); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("events of a missing task: %v", err)
	}
}

func TestCommander_TaskDiff(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	c := &Commander{Store: e.store}

	queued := e.task(NewTask{ProjectID: "web"})
	if _, err := c.TaskDiff(ctx, queued.ID); !errors.Is(err, ErrNoWorkspace) {
		t.Fatalf("diff of a task that has not started: %v", err)
	}

	task := e.reviewReady("Add the feature", lineInit, lineResultOK)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(task.WorktreePath, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("committed.txt", "in a commit\n")
	gitT(t, task.WorktreePath, "add", "-A")
	gitT(t, task.WorktreePath, "commit", "-q", "-m", "work")
	write("README.md", "edited but not committed\n")
	write("brand new.txt", "untracked\n")

	diff, err := c.TaskDiff(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskDiff: %v", err)
	}
	if diff.Base != gitT(t, e.repo, "rev-parse", "main") {
		t.Errorf("Base = %s, want where the branch left main", diff.Base)
	}
	for _, want := range []string{"+in a commit", "+edited but not committed", "-hello"} {
		if !strings.Contains(diff.Patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, diff.Patch)
		}
	}
	if !strings.Contains(diff.Stat, "committed.txt") || !strings.Contains(diff.Stat, "README.md") {
		t.Errorf("stat = %q", diff.Stat)
	}
	if !reflect.DeepEqual(diff.Untracked, []string{"brand new.txt"}) {
		t.Errorf("Untracked = %q", diff.Untracked)
	}
	if diff.Truncated {
		t.Error("a small diff must not be marked truncated")
	}

	// With the checkout gone, the committed work is still shown from the branch.
	if _, err := c.Discard(ctx, owner, task.ID); err != nil {
		t.Fatal(err)
	}
	after, err := c.TaskDiff(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskDiff after the checkout was removed: %v", err)
	}
	if !strings.Contains(after.Patch, "+in a commit") || strings.Contains(after.Patch, "edited but not committed") || len(after.Untracked) != 0 {
		t.Errorf("after removal the diff must hold only committed work:\n%s", after.Patch)
	}
}

func TestCommander_TaskDiffIsCapped(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	c := &Commander{Store: e.store}
	task := e.reviewReady("Add a big file", lineInit, lineResultOK)
	big := strings.Repeat(strings.Repeat("x", 99)+"\n", 30000) // about 3 MB
	if err := os.WriteFile(filepath.Join(task.WorktreePath, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, task.WorktreePath, "add", "-A")

	diff, err := c.TaskDiff(ctx, task.ID)
	if err != nil {
		t.Fatalf("TaskDiff: %v", err)
	}
	if !diff.Truncated || len(diff.Patch) > MaxDiffBytes {
		t.Fatalf("patch is %d bytes, truncated = %v; want it cut at %d", len(diff.Patch), diff.Truncated, MaxDiffBytes)
	}
	if !strings.Contains(diff.Stat, "big.txt") {
		t.Errorf("the stat must still describe the whole change: %q", diff.Stat)
	}
}

func TestCommander_Digest(t *testing.T) {
	e := newRunnerEnv(t)
	e.addProject("idle", 1)
	e.addProject("web", 3)
	clock := &settableClock{now: time.Date(2026, 5, 1, 22, 0, 0, 0, time.UTC)}
	e.store.now = clock.Now
	e.runner.Budget = &Budget{Location: time.UTC}
	c := e.commander()

	// Yesterday: one task done and one failed, with usage.
	oldDone := e.task(NewTask{ProjectID: "web"})
	walk(t, e.store, oldDone.ID, StatusRunning, StatusVerifying, StatusDone)
	oldFailed := e.task(NewTask{ProjectID: "web"})
	walk(t, e.store, oldFailed.ID, StatusRunning, StatusFailed)
	if err := e.store.AddUsage(ctx, oldDone.ID, Usage{InputTokens: 9000, CostUSD: 9}); err != nil {
		t.Fatal(err)
	}

	clock.Advance(4 * time.Hour) // 02:00 the next day
	mk := func(project string, path ...Status) Task {
		task := e.task(NewTask{ProjectID: project})
		walk(t, e.store, task.ID, path...)
		return task
	}
	mk("web")                                 // queued
	mk("web", StatusRunning)                  // running
	mk("web", StatusRunning, StatusVerifying) // verifying counts as running
	spender := mk("web", StatusRunning, StatusVerifying, StatusReviewReady)
	mk("web", StatusRunning, StatusNeedsDecision)
	mk("web", StatusRunning, StatusFailed)                // failed today
	mk("web", StatusRunning, StatusVerifying, StatusDone) // done today
	mk("web", StatusCancelled)                            // not counted anywhere
	appTask := mk("app", StatusRunning, StatusNeedsDecision)
	if err := e.store.AddUsage(ctx, spender.ID, Usage{InputTokens: 800, OutputTokens: 200, CostUSD: 1.5}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AddUsage(ctx, appTask.ID, Usage{InputTokens: 100, CostUSD: 0.25}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.raiseWakes(ctx, newWake{taskID: appTask.ID, projectID: "app", kind: WakeQuestion, dedupeKey: "k"}); err != nil {
		t.Fatal(err)
	}

	digest, err := c.Digest(ctx)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if !digest.DayStart.Equal(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("DayStart = %s", digest.DayStart)
	}
	want := []ProjectDigest{
		{ProjectID: "app", Name: "app", Counts: DigestCounts{NeedsDecision: 1}, Today: Usage{InputTokens: 100, CostUSD: 0.25}},
		{ProjectID: "idle", Name: "idle"},
		{ProjectID: "web", Name: "web",
			Counts: DigestCounts{Queued: 1, Running: 2, ReviewReady: 1, NeedsDecision: 1, FailedToday: 1, DoneToday: 1},
			Today:  Usage{InputTokens: 800, OutputTokens: 200, CostUSD: 1.5}},
	}
	if !reflect.DeepEqual(digest.Projects, want) {
		t.Fatalf("projects =\n  %+v\nwant\n  %+v", digest.Projects, want)
	}
	wantTotals := DigestCounts{Queued: 1, Running: 2, ReviewReady: 1, NeedsDecision: 2, FailedToday: 1, DoneToday: 1}
	if digest.Totals != wantTotals || digest.Totals.WaitingOnOwner() != 3 {
		t.Errorf("totals = %+v, waiting on owner %d", digest.Totals, digest.Totals.WaitingOnOwner())
	}
	if digest.Today != (Usage{InputTokens: 900, OutputTokens: 200, CostUSD: 1.75}) {
		t.Errorf("fleet usage today = %+v; yesterday's must not count", digest.Today)
	}
	if digest.PendingWakes != 1 {
		t.Errorf("PendingWakes = %d", digest.PendingWakes)
	}
}

func TestCommander_DigestOfAnEmptyFleet(t *testing.T) {
	store := newStore(t)
	digest, err := (&Commander{Store: store}).Digest(ctx)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if digest.Projects == nil || len(digest.Projects) != 0 || digest.Totals != (DigestCounts{}) || digest.Today != (Usage{}) {
		t.Fatalf("digest = %+v", digest)
	}
}

func TestAudit_FiltersAndPaging(t *testing.T) {
	e := newRunnerEnv(t)
	e.addProject("other", 1)
	c := e.commander()
	web := Actor{ID: "ck", Via: "web"}
	bot := Actor{ID: "cron", Via: "script"}

	a, _ := c.Dispatch(ctx, web, DispatchRequest{ProjectID: "app", Goal: "one"})
	b, _ := c.Dispatch(ctx, bot, DispatchRequest{ProjectID: "other", Goal: "two"})
	c.Cancel(ctx, web, a.ID, "")
	c.Cancel(ctx, web, b.ID, "")
	c.Retry(ctx, bot, a.ID) // refused: a cancelled task cannot be retried

	all := e.audit(AuditFilter{})
	if len(all) != 5 {
		t.Fatalf("audit has %d entries, want 5", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("audit must be newest first: %+v", all)
		}
	}
	if got := e.audit(AuditFilter{ProjectID: "other"}); len(got) != 2 {
		t.Errorf("project filter: %+v", got)
	}
	if got := e.audit(AuditFilter{TaskID: a.ID}); len(got) != 3 || got[0].Command != CmdRetry || got[0].OK {
		t.Errorf("task filter: %+v", got)
	}
	if got := e.audit(AuditFilter{Actor: "cron"}); len(got) != 2 || got[0].Via != "script" {
		t.Errorf("actor filter: %+v", got)
	}
	first := e.audit(AuditFilter{Limit: 2})
	second := e.audit(AuditFilter{Limit: 2, BeforeID: first[1].ID})
	if len(first) != 2 || len(second) != 2 || second[0].ID >= first[1].ID {
		t.Errorf("paging: %+v then %+v", first, second)
	}
	if none := e.audit(AuditFilter{TaskID: "nope"}); none == nil || len(none) != 0 {
		t.Errorf("no match must be an empty slice: %#v", none)
	}
}

func TestCommander_Pass(t *testing.T) {
	t.Run("waves a blocked ship task through and delivers it", func(t *testing.T) {
		e := newDeliveryEnv(t, TrustLocalOnly)
		c := &Commander{Store: e.store, Deliverer: e.deliverer}
		e.script(lineInit, "#touch feature.txt", lineResultOK)
		task := e.task(NewTask{ProjectID: "web", Goal: "Add the feature"})
		e.run(task.ID, RunOptions{})
		walk(t, e.store, task.ID, StatusNeedsDecision) // its checks kept failing

		got, err := c.Pass(ctx, owner, task.ID)
		if err != nil || got.Status != StatusReviewReady {
			t.Fatalf("Pass = %s (%q), %v", got.Status, got.StatusReason, err)
		}
		events, err := e.store.ListEvents(ctx, task.ID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		var waved, committed bool
		for _, ev := range events {
			if ev.Type == EventStatus && strings.Contains(string(ev.Payload), "waved through by ck") {
				waved = true
			}
			committed = committed || ev.Type == EventDeliveryCommitted
		}
		if !waved || !committed {
			t.Fatalf("waved=%v committed=%v; want the override recorded and the work committed", waved, committed)
		}
		if entries := e.audit(AuditFilter{TaskID: task.ID}); len(entries) != 1 || entries[0].Command != CmdPass {
			t.Errorf("audit = %+v", entries)
		}
		// Landing it is still the owner's separate decision.
		if done, err := c.Approve(ctx, owner, task.ID); err != nil || done.Status != StatusDone {
			t.Fatalf("Approve after Pass = %s, %v", done.Status, err)
		}
	})

	t.Run("only a task that is waiting on a decision", func(t *testing.T) {
		e := newRunnerEnv(t)
		c := e.commander()
		task := e.task(NewTask{})
		for _, status := range []Status{StatusQueued, StatusRunning, StatusVerifying} {
			if status != StatusQueued {
				walk(t, e.store, task.ID, status)
			}
			if _, err := c.Pass(ctx, owner, task.ID); !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("Pass on a %s task: %v", status, err)
			}
			if got, _ := e.store.GetTask(ctx, task.ID); got.Status != status {
				t.Fatalf("a refused Pass moved the task to %s", got.Status)
			}
		}
	})

	t.Run("a scout is not delivered", func(t *testing.T) {
		e := newRunnerEnv(t)
		c := e.commander()
		task := e.task(NewTask{Shape: ShapeScout})
		walk(t, e.store, task.ID, StatusRunning, StatusNeedsDecision)
		got, err := c.Pass(ctx, owner, task.ID)
		if err != nil || got.Status != StatusReviewReady {
			t.Fatalf("Pass = %s, %v", got.Status, err)
		}
	})
}
