package fleet

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func (e *runnerEnv) watcher() *Watcher {
	return &Watcher{Store: e.store, Runner: e.runner, Interval: 20 * time.Millisecond}
}

func (e *runnerEnv) scan(w *Watcher) []Wake {
	e.t.Helper()
	wakes, err := w.Scan(ctx)
	if err != nil {
		e.t.Fatalf("Scan: %v", err)
	}
	return wakes
}

type wakeSummary struct {
	TaskID string
	Kind   WakeKind
	Detail string
}

func summarize(wakes []Wake) []wakeSummary {
	var out []wakeSummary
	for _, w := range wakes {
		out = append(out, wakeSummary{w.TaskID, w.Kind, w.Detail})
	}
	return out
}

// settableClock is a clock tests can move by hand. Safe for concurrent use.
type settableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settableClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestWatcher_StatusChangesBecomeWakes(t *testing.T) {
	e := newRunnerEnv(t)
	w := e.watcher()
	s := e.scheduler(1)

	// A ship task that passes verification.
	e.script(lineInit, lineResultOK)
	shipped := e.task(NewTask{Goal: "ship it"})
	if _, err := s.RunTask(ctx, shipped.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// A scout that completes.
	scout := e.task(NewTask{Shape: ShapeScout, Goal: "look"})
	if _, err := s.RunTask(ctx, scout.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	// An agent that asks a question.
	e.script(lineInit, lineResultAsk)
	asking := e.task(NewTask{Goal: "ask"})
	e.run(asking.ID, RunOptions{})
	// An agent that hits the turn limit.
	e.script(lineInit, `{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"sess-1"}`)
	limited := e.task(NewTask{Goal: "long"})
	e.run(limited.ID, RunOptions{})
	// A run that cannot even start.
	e.script("#stderr not logged in", "#exit 1")
	broken := e.task(NewTask{Goal: "broken"})
	e.run(broken.ID, RunOptions{})

	got := summarize(e.scan(w))
	want := []wakeSummary{
		{shipped.ID, WakeReviewReady, "no verification commands configured"},
		{scout.ID, WakeCompleted, "report verified"},
		{asking.ID, WakeQuestion, "agent is waiting for an answer"},
		{limited.ID, WakeNeedsDecision, "reached the turn limit (100)"},
		{broken.ID, WakeFailed, "agent produced no output: not logged in"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("wakes =\n  %+v\nwant\n  %+v", got, want)
	}

	// A second pass over unchanged state adds nothing.
	if again := e.scan(w); len(again) != 0 {
		t.Fatalf("a repeat pass queued %+v", again)
	}
	pending, _ := e.store.PendingWakes(ctx, 0)
	if len(pending) != 5 || pending[0].ProjectID != "app" || pending[0].ConsumedAt != nil {
		t.Fatalf("pending = %+v", pending)
	}
}

// What the owner did by hand is not news to the owner.
func TestWatcher_OwnerActionsRaiseNoWakes(t *testing.T) {
	e := newRunnerEnv(t)
	w := e.watcher()

	// Accepted, cancelled and given-up tasks.
	e.script(lineInit, lineResultOK)
	accepted := e.task(NewTask{})
	if _, err := e.scheduler(1).RunTask(ctx, accepted.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	e.scan(w) // the review_ready wake; drain it
	walk(t, e.store, accepted.ID, StatusDone)

	withdrawn := e.task(NewTask{})
	walk(t, e.store, withdrawn.ID, StatusCancelled)

	e.script(lineInit, lineResultAsk)
	abandoned := e.task(NewTask{})
	e.run(abandoned.ID, RunOptions{})
	e.scan(w) // the question wake; drain it
	walk(t, e.store, abandoned.ID, StatusFailed)

	// A run the owner cancelled, and one the owner interrupted.
	e.script(lineInit, "#sleep 30")
	cancelled := e.task(NewTask{})
	out := e.runAsync(ctx, cancelled.ID)
	e.waitForStart(cancelled.ID)
	if err := e.runner.Cancel(cancelled.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)

	interrupted := e.task(NewTask{})
	out = e.runAsync(ctx, interrupted.ID)
	e.waitForStart(interrupted.ID)
	if err := e.runner.Interrupt(interrupted.ID, "wrong approach"); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)

	if got := e.scan(w); len(got) != 0 {
		t.Fatalf("owner actions raised wakes: %+v", summarize(got))
	}

	// But once the interrupted task runs again and then stops on its own, that is news.
	e.script(lineInit, lineResultAsk)
	e.run(interrupted.ID, RunOptions{Instruction: "try again"})
	got := summarize(e.scan(w))
	if len(got) != 1 || got[0].TaskID != interrupted.ID || got[0].Kind != WakeQuestion {
		t.Fatalf("wakes after the resumed run = %+v", got)
	}
}

func TestWatcher_CursorSurvivesARestart(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	first := e.task(NewTask{})
	e.run(first.ID, RunOptions{})
	if got := e.scan(e.watcher()); len(got) != 1 {
		t.Fatalf("first pass = %+v", got)
	}

	second := e.task(NewTask{})
	e.run(second.ID, RunOptions{})

	// A brand-new Watcher, as after a restart, picks up exactly where the
	// old one stopped: nothing repeated, nothing missed.
	got := summarize(e.scan(e.watcher()))
	if len(got) != 1 || got[0].TaskID != second.ID {
		t.Fatalf("after a restart the watcher queued %+v, want only the second task", got)
	}
	if pending, _ := e.store.PendingWakes(ctx, 0); len(pending) != 2 {
		t.Fatalf("pending = %+v", pending)
	}
}

func TestWakes_PendingUntilConsumed(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	for i := 0; i < 3; i++ {
		e.run(e.task(NewTask{}).ID, RunOptions{})
	}
	e.scan(e.watcher())

	pending, err := e.store.PendingWakes(ctx, 0)
	if err != nil || len(pending) != 3 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if pending[0].ID >= pending[1].ID || pending[1].ID >= pending[2].ID {
		t.Errorf("wakes must come oldest first: %+v", pending)
	}
	if limited, _ := e.store.PendingWakes(ctx, 2); len(limited) != 2 || limited[0].ID != pending[0].ID {
		t.Errorf("limit: %+v", limited)
	}

	// Reading does not consume: an unacknowledged wake is offered again.
	if again, _ := e.store.PendingWakes(ctx, 0); len(again) != 3 {
		t.Fatalf("a wake disappeared without being consumed: %+v", again)
	}

	if err := e.store.ConsumeWake(ctx, pending[1].ID); err != nil {
		t.Fatalf("ConsumeWake: %v", err)
	}
	left, _ := e.store.PendingWakes(ctx, 0)
	if len(left) != 2 || left[0].ID != pending[0].ID || left[1].ID != pending[2].ID {
		t.Fatalf("after consuming the middle one: %+v", left)
	}
	if err := e.store.ConsumeWake(ctx, pending[1].ID); err != nil {
		t.Errorf("consuming twice must be harmless: %v", err)
	}
	if err := e.store.ConsumeWake(ctx, 99999); !errors.Is(err, ErrWakeNotFound) {
		t.Errorf("unknown wake: %v", err)
	}
	if none, _ := e.store.PendingWakes(ctx, 0); none == nil {
		t.Error("PendingWakes must return an empty slice, not nil")
	}
}

func TestWatcher_LivenessOfARunningTask(t *testing.T) {
	e := newRunnerEnv(t)
	clock := &settableClock{now: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)}
	e.store.now = clock.Now
	w := e.watcher()
	w.StuckAfter = 10 * time.Minute

	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	// Fresh output and a live process: busy.
	if wakes := e.scan(w); len(wakes) != 0 {
		t.Fatalf("a healthy run raised %+v", summarize(wakes))
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Liveness != LivenessBusy {
		t.Fatalf("liveness = %q, want busy", got.Liveness)
	}

	// Still alive, but silent past the threshold: no longer vouched for.
	clock.Advance(11 * time.Minute)
	wakes := summarize(e.scan(w))
	if len(wakes) != 1 || wakes[0].Kind != WakeStuck || wakes[0].TaskID != task.ID || wakes[0].Detail != "no output for 11m0s" {
		t.Fatalf("wakes = %+v, want one stuck wake", wakes)
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Liveness != LivenessUnknown || got.Status != StatusRunning {
		t.Fatalf("a silent task = %s, liveness %q; want still running, liveness unknown", got.Status, got.Liveness)
	}

	// The same stall is reported once, however long it lasts.
	clock.Advance(30 * time.Minute)
	if again := e.scan(w); len(again) != 0 {
		t.Fatalf("the same stall was reported again: %+v", summarize(again))
	}

	// New output ends the stall; a later one is a new report.
	if _, err := e.store.RecordActivity(ctx, task.ID, EventAgentText, map[string]any{"text": "still here"}); err != nil {
		t.Fatal(err)
	}
	if wakes := e.scan(w); len(wakes) != 0 {
		t.Fatalf("fresh output raised %+v", summarize(wakes))
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Liveness != LivenessBusy {
		t.Fatalf("liveness after new output = %q, want busy", got.Liveness)
	}
	clock.Advance(11 * time.Minute)
	if wakes := summarize(e.scan(w)); len(wakes) != 1 || wakes[0].Kind != WakeStuck {
		t.Fatalf("a second stall must be reported: %+v", wakes)
	}

	if err := e.runner.Cancel(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)
}

// No evidence either way must read as unknown, never as idle or dead.
func TestWatcher_MissingSignalIsUnknown(t *testing.T) {
	e := newRunnerEnv(t)
	e.setProjectLimit("app", 5)

	// Stored as running, but the Runner has no process and no run was ever logged.
	noSignal := e.task(NewTask{Goal: "no signal"})
	walk(t, e.store, noSignal.ID, StatusRunning)
	if err := e.store.SetLiveness(ctx, noSignal.ID, LivenessIdle); err != nil {
		t.Fatal(err)
	}

	// A real agent, alive, but started by a different Runner sharing the
	// store: this Watcher's Runner cannot vouch for it.
	e.script(lineInit, "#sleep 30")
	foreign := e.task(NewTask{Goal: "someone else's agent"})
	out := e.runAsync(ctx, foreign.ID)
	e.waitForStart(foreign.ID)
	otherHost := &Runner{Store: e.store, Worktrees: e.runner.Worktrees, Binary: e.runner.Binary}
	w := &Watcher{Store: e.store, Runner: otherHost}

	if wakes := e.scan(w); len(wakes) != 0 {
		t.Fatalf("missing signals raised %+v", summarize(wakes))
	}
	for name, id := range map[string]string{"no signal": noSignal.ID, "agent of another runner": foreign.ID} {
		got, _ := e.store.GetTask(ctx, id)
		if got.Liveness != LivenessUnknown {
			t.Errorf("%s: liveness = %q, want unknown", name, got.Liveness)
		}
		if got.Status != StatusRunning {
			t.Errorf("%s: status = %s; a task of unknown liveness must not be moved", name, got.Status)
		}
	}
	if !e.runner.Running(foreign.ID) {
		t.Error("the watcher must not have stopped an agent it cannot vouch for")
	}

	if err := e.runner.Cancel(foreign.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)
}

// A recorded PID that now belongs to some other program means the agent is
// gone: the task is dead, and the unrelated program is left alone.
func TestWatcher_ReusedPIDMeansTheAgentIsGone(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning)
	// The PID on record is this test process: alive, and plainly not an agent.
	if _, err := e.store.AppendEvent(ctx, task.ID, EventRunStarted, map[string]any{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}

	e.scan(e.watcher())
	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Liveness != LivenessDead || got.Status != StatusNeedsDecision {
		t.Fatalf("task = %s, liveness %q; want needs_decision and dead", got.Status, got.Liveness)
	}
	// Reaching this line means the test process was not signalled.
}

// A recorded process that has verifiably gone is dead, and the slot is freed.
func TestWatcher_DeadProcessIsRecovered(t *testing.T) {
	e := newRunnerEnv(t)
	w := e.watcher()
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{}) // a real run: its process has exited, and its PID is on record
	walk(t, e.store, task.ID, StatusRunning)

	if wakes := e.scan(w); len(wakes) != 0 {
		// The wake for the first run's question, raised before the task went back to running.
		for _, wk := range wakes {
			if wk.Kind != WakeQuestion {
				t.Fatalf("unexpected wake on the first pass: %+v", summarize(wakes))
			}
		}
	}
	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusNeedsDecision || got.Liveness != LivenessDead || !strings.Contains(got.StatusReason, "agent process is gone") {
		t.Fatalf("task = %s (%q), liveness %q", got.Status, got.StatusReason, got.Liveness)
	}

	// The recovery is itself a status change, reported on the next pass.
	wakes := summarize(e.scan(w))
	if len(wakes) != 1 || wakes[0].Kind != WakeNeedsDecision || wakes[0].TaskID != task.ID {
		t.Fatalf("wakes = %+v, want the recovery reported", wakes)
	}
}

func TestWatcher_OverBudget(t *testing.T) {
	e := newRunnerEnv(t)
	w := e.watcher()
	w.MaxTaskCostUSD = 1.00
	w.MaxTaskTokens = 5000
	e.script(lineInit, "#sleep 30")

	cheap := e.task(NewTask{})
	costly := e.task(NewTask{})
	chatty := e.task(NewTask{})
	e.setProjectLimit("app", 5)
	outs := map[string]<-chan runOutcome{}
	for _, id := range []string{cheap.ID, costly.ID, chatty.ID} {
		outs[id] = e.runAsync(ctx, id)
		e.waitForStart(id)
	}
	if err := e.store.AddUsage(ctx, cheap.ID, Usage{InputTokens: 100, CostUSD: 0.10}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AddUsage(ctx, costly.ID, Usage{InputTokens: 100, CostUSD: 2.50}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.AddUsage(ctx, chatty.ID, Usage{InputTokens: 4000, OutputTokens: 1500, CostUSD: 0.20}); err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, wk := range e.scan(w) {
		if wk.Kind != WakeOverBudget {
			t.Fatalf("unexpected wake %+v", wk)
		}
		got[wk.TaskID] = wk.Detail
	}
	want := map[string]string{
		costly.ID: "cost $2.50 exceeds the task budget of $1.00",
		chatty.ID: "5500 tokens exceed the task budget of 5000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("over-budget wakes = %v, want %v", got, want)
	}
	// Reported once per run, not on every pass.
	if again := e.scan(w); len(again) != 0 {
		t.Fatalf("budget wakes repeated: %+v", summarize(again))
	}
	// The watcher reports; it does not stop the task.
	if !e.runner.Running(costly.ID) {
		t.Error("the watcher must not stop an over-budget task by itself")
	}

	for id, out := range outs {
		if err := e.runner.Cancel(id, ""); err != nil {
			t.Fatal(err)
		}
		awaitRun(t, out)
	}
}

func TestWatcher_Run(t *testing.T) {
	e := newRunnerEnv(t)
	w := e.watcher()
	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- w.Run(c) }()

	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})
	e.waitUntil("the running watcher to queue the wake", func() bool {
		pending, _ := e.store.PendingWakes(ctx, 0)
		return len(pending) == 1 && pending[0].TaskID == task.ID
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}

	if err := (&Watcher{}).Run(ctx); err == nil {
		t.Error("Run without a Store must fail")
	}
	if _, err := (&Watcher{}).Scan(ctx); err == nil {
		t.Error("Scan without a Store must fail")
	}
}

func TestWakeFor(t *testing.T) {
	cases := []struct {
		change StatusChange
		want   WakeKind
		ok     bool
	}{
		{StatusChange{From: StatusVerifying, To: StatusReviewReady}, WakeReviewReady, true},
		{StatusChange{From: StatusNeedsDecision, To: StatusReviewReady}, "", false}, // the owner waved it through
		{StatusChange{From: StatusVerifying, To: StatusDone}, WakeCompleted, true},
		{StatusChange{From: StatusReviewReady, To: StatusDone}, "", false}, // the owner accepted it
		{StatusChange{From: StatusRunning, To: StatusFailed}, WakeFailed, true},
		{StatusChange{From: StatusVerifying, To: StatusFailed}, WakeFailed, true},
		{StatusChange{From: StatusNeedsDecision, To: StatusFailed}, "", false}, // the owner gave up
		{StatusChange{From: StatusRunning, To: StatusNeedsDecision, Reason: reasonAgentWaiting}, WakeQuestion, true},
		{StatusChange{From: StatusVerifying, To: StatusNeedsDecision, Reason: "verification failed"}, WakeNeedsDecision, true},
		{StatusChange{From: StatusRunning, To: StatusCancelled}, "", false},
		{StatusChange{From: StatusQueued, To: StatusRunning}, "", false},
		{StatusChange{To: StatusQueued, Reason: "created"}, "", false},
	}
	for _, tc := range cases {
		got, ok := wakeFor(tc.change)
		if got != tc.want || ok != tc.ok {
			t.Errorf("wakeFor(%+v) = %q, %v; want %q, %v", tc.change, got, ok, tc.want, tc.ok)
		}
	}
}
