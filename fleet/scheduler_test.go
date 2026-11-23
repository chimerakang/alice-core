package fleet

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// A fake agent run that marks when it starts and stops, with a pause between.
var timedRun = []string{lineInit, "#begin", "#sleep 0.3", "#end", lineResultOK}

func (e *runnerEnv) scheduler(maxConcurrent int) *Scheduler {
	return &Scheduler{Store: e.store, Runner: e.runner, MaxConcurrent: maxConcurrent, PollInterval: 20 * time.Millisecond}
}

func (e *runnerEnv) addProject(id string, maxConcurrent int) {
	e.t.Helper()
	if _, err := e.store.RegisterProject(ctx, Project{ID: id, RepoPath: newRepo(e.t, id), MaxConcurrent: maxConcurrent}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *runnerEnv) setProjectLimit(id string, maxConcurrent int) {
	e.t.Helper()
	if _, err := e.store.UpdateProject(ctx, id, func(p *Project) error { p.MaxConcurrent = maxConcurrent; return nil }); err != nil {
		e.t.Fatal(err)
	}
}

// runUntilIdle runs the scheduler until no task is queued or active.
func (e *runnerEnv) runUntilIdle(s *Scheduler) {
	e.t.Helper()
	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(c) }()
	e.waitUntil("the queue to drain", func() bool {
		n, _ := e.store.CountTasks(ctx, TaskFilter{Statuses: []Status{StatusQueued, StatusRunning, StatusVerifying}})
		return n == 0
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			e.t.Fatalf("Scheduler.Run: %v", err)
		}
	case <-time.After(20 * time.Second):
		e.t.Fatal("Scheduler.Run did not return after its context was cancelled")
	}
}

func (e *runnerEnv) waitUntil(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for %s", what)
}

// tickingClock returns a clock that advances one second per reading, so every
// stored timestamp is distinct and ordered. Safe for concurrent use.
func tickingClock(start time.Time) func() time.Time {
	var mu sync.Mutex
	now := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(time.Second)
		return now
	}
}

// agentLog is the fake agent's begin/end record, in the order it happened.
type agentLogEntry struct {
	begin   bool
	project string
	taskID  string
}

func (e *runnerEnv) agentLog() []agentLogEntry {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "log"))
	if err != nil {
		return nil
	}
	var out []agentLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		kind, path, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out = append(out, agentLogEntry{
			begin:   kind == "b",
			project: filepath.Base(filepath.Dir(path)),
			taskID:  filepath.Base(path),
		})
	}
	return out
}

// peak returns the most agents alive at once, overall and per project.
func peak(log []agentLogEntry) (overall int, perProject map[string]int) {
	perProject = map[string]int{}
	live, liveBy := 0, map[string]int{}
	for _, entry := range log {
		if entry.begin {
			live++
			liveBy[entry.project]++
		} else {
			live--
			liveBy[entry.project]--
		}
		if live > overall {
			overall = live
		}
		if liveBy[entry.project] > perProject[entry.project] {
			perProject[entry.project] = liveBy[entry.project]
		}
	}
	return overall, perProject
}

func startOrder(log []agentLogEntry) []string {
	var ids []string
	for _, entry := range log {
		if entry.begin {
			ids = append(ids, entry.taskID)
		}
	}
	return ids
}

func TestScheduler_GlobalLimit(t *testing.T) {
	e := newRunnerEnv(t)
	e.setProjectLimit("app", 10)
	e.script(timedRun...)
	for i := 0; i < 5; i++ {
		e.task(NewTask{})
	}

	e.runUntilIdle(e.scheduler(2))

	overall, _ := peak(e.agentLog())
	if overall != 2 {
		t.Fatalf("peak concurrency = %d, want exactly the global limit of 2", overall)
	}
	if n, _ := e.store.CountTasks(ctx, TaskFilter{Statuses: []Status{StatusReviewReady}}); n != 5 {
		t.Fatalf("%d tasks reached review_ready, want all 5", n)
	}
	if e.invocations() != 5 {
		t.Errorf("agent invocations = %d, want one per task", e.invocations())
	}
}

func TestScheduler_PerProjectLimit(t *testing.T) {
	e := newRunnerEnv(t) // "app" has MaxConcurrent 1
	e.addProject("other", 1)
	e.script(timedRun...)
	for i := 0; i < 3; i++ {
		e.task(NewTask{ProjectID: "app"})
		e.task(NewTask{ProjectID: "other"})
	}

	e.runUntilIdle(e.scheduler(4))

	overall, perProject := peak(e.agentLog())
	if perProject["app"] != 1 || perProject["other"] != 1 {
		t.Fatalf("per-project peaks = %v, want 1 each", perProject)
	}
	if overall != 2 {
		t.Fatalf("overall peak = %d, want 2: the projects run side by side", overall)
	}
	if n, _ := e.store.CountTasks(ctx, TaskFilter{Statuses: []Status{StatusReviewReady}}); n != 6 {
		t.Fatalf("%d tasks finished, want 6", n)
	}
}

func TestScheduler_QueueOrder(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#begin", "#end", lineResultOK)
	e.store.now = tickingClock(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))

	oldLow := e.task(NewTask{Goal: "old, low"})
	urgent := e.task(NewTask{Goal: "urgent", Priority: 10})
	newLow := e.task(NewTask{Goal: "new, low"})
	medium := e.task(NewTask{Goal: "medium", Priority: 5})

	e.runUntilIdle(e.scheduler(1))

	want := []string{urgent.ID, medium.ID, oldLow.ID, newLow.ID}
	if got := startOrder(e.agentLog()); !reflect.DeepEqual(got, want) {
		t.Fatalf("start order = %v\nwant        %v (priority first, then oldest)", got, want)
	}
}

// A project at its limit must not hold up tasks of other projects behind it.
func TestScheduler_FullProjectDoesNotBlockOthers(t *testing.T) {
	e := newRunnerEnv(t) // "app" has MaxConcurrent 1
	e.addProject("other", 1)
	e.script(lineInit, "#sleep 30")
	first := e.task(NewTask{ProjectID: "app", Priority: 9})
	second := e.task(NewTask{ProjectID: "app", Priority: 8})
	behind := e.task(NewTask{ProjectID: "other", Priority: 1})
	s := e.scheduler(5)

	started, err := s.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if !reflect.DeepEqual(started, []string{first.ID, behind.ID}) {
		t.Fatalf("started = %v, want %v", started, []string{first.ID, behind.ID})
	}
	if got, _ := e.store.GetTask(ctx, second.ID); got.Status != StatusQueued {
		t.Fatalf("the second task of a full project must wait, got %s", got.Status)
	}

	// Nothing more fits; a second pass must not start anything twice.
	e.waitForStart(first.ID)
	e.waitForStart(behind.ID)
	if again, _ := s.Tick(ctx); len(again) != 0 {
		t.Fatalf("second pass started %v", again)
	}
	if e.invocations() != 2 {
		t.Fatalf("agent invocations = %d, want 2", e.invocations())
	}

	for _, id := range []string{first.ID, behind.ID} {
		if err := e.runner.Cancel(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Wait()
}

func TestScheduler_OneTaskPerBranch(t *testing.T) {
	e := newRunnerEnv(t)
	e.setProjectLimit("app", 5)
	e.script(lineInit, "#sleep 30")
	e.store.now = tickingClock(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC))

	a := e.task(NewTask{Goal: "a"})
	b := e.task(NewTask{Goal: "b, same branch as a"})
	c := e.task(NewTask{Goal: "c, its own branch"})
	if err := e.store.SetWorkspace(ctx, b.ID, "alice/"+a.ID, ""); err != nil {
		t.Fatal(err)
	}
	s := e.scheduler(5)

	started, err := s.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(started, []string{a.ID, c.ID}) {
		t.Fatalf("started = %v, want a and c: b shares a's branch", started)
	}
	e.waitForStart(a.ID)
	e.waitForStart(c.ID)
	if again, _ := s.Tick(ctx); len(again) != 0 {
		t.Fatalf("b must keep waiting while a is active, but a pass started %v", again)
	}
	if got, _ := e.store.GetTask(ctx, b.ID); got.Status != StatusQueued {
		t.Fatalf("b = %s, want queued", got.Status)
	}

	for _, id := range []string{a.ID, c.ID} {
		if err := e.runner.Cancel(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	s.Wait()

	// With a finished, the branch is free and b becomes eligible.
	e.script(lineInit, "#sleep 30")
	started, _ = s.Tick(ctx)
	if !reflect.DeepEqual(started, []string{b.ID}) {
		t.Fatalf("after a finished, started = %v, want b", started)
	}
	s.Wait() // b fails to set up (its branch has no worktree here); the scheduling is what is under test
}

// Runs started outside the scheduler occupy slots too.
func TestScheduler_CountsRunsItDidNotStart(t *testing.T) {
	e := newRunnerEnv(t)
	e.setProjectLimit("app", 5)
	e.script(lineInit, "#sleep 30")
	direct := e.task(NewTask{})
	waiting := e.task(NewTask{})
	out := e.runAsync(ctx, direct.ID)
	e.waitForStart(direct.ID)

	s := e.scheduler(1)
	if started, _ := s.Tick(ctx); len(started) != 0 {
		t.Fatalf("the only slot is taken, but a pass started %v", started)
	}
	if got, _ := e.store.GetTask(ctx, waiting.ID); got.Status != StatusQueued {
		t.Fatalf("waiting task = %s", got.Status)
	}
	if err := e.runner.Cancel(direct.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)
}

func TestScheduler_KickWakesAnIdleScheduler(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultOK)
	s := e.scheduler(1)
	s.PollInterval = time.Hour // only a kick can wake it in time

	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(c) }()
	time.Sleep(100 * time.Millisecond) // let the first, empty pass happen

	task := e.task(NewTask{})
	s.Kick()
	e.waitUntil("the kicked task to finish", func() bool {
		got, _ := e.store.GetTask(ctx, task.ID)
		return got.Status == StatusReviewReady
	})
	cancel()
	<-done
}

func TestScheduler_StopsAgentsOnShutdown(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})
	s := e.scheduler(1)

	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(c) }()
	e.waitForStart(task.ID)

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return: it must wait for its agents, and they must stop")
	}
	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusNeedsDecision || !strings.Contains(got.StatusReason, "runner stopped") || got.SessionID == "" {
		t.Fatalf("task after shutdown = %s (%q), session %q", got.Status, got.StatusReason, got.SessionID)
	}
}

func TestScheduler_RequiresStoreAndRunner(t *testing.T) {
	if _, err := (&Scheduler{}).Tick(ctx); err == nil {
		t.Error("Tick without a Store must fail")
	}
	if err := (&Scheduler{}).Run(ctx); err == nil {
		t.Error("Run without a Store must fail")
	}
}

// The whole cycle: the agent says it is done, the check disagrees, the agent
// is told why, fixes it, and the check passes.
func TestRunTask_VerificationFailureIsFedBackAndFixed(t *testing.T) {
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = []string{"test -f fixed.txt"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.scriptFor(1, lineInit, lineResultOK)                     // claims success, changes nothing
	e.scriptFor(2, lineInit, "#touch fixed.txt", lineResultOK) // actually does the work
	task := e.task(NewTask{Goal: "create fixed.txt"})

	res, err := e.scheduler(1).RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.Task.Status != StatusReviewReady || res.Task.StatusReason != "verification passed" {
		t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
	}
	if e.invocations() != 2 {
		t.Fatalf("agent invocations = %d, want 2", e.invocations())
	}

	retry := e.args(2)
	if retry[len(retry)-3] != "--resume" || retry[len(retry)-2] != "sess-1" {
		t.Errorf("the retry must resume the same session: %q", retry)
	}
	feedback := retry[len(retry)-1]
	if !strings.Contains(feedback, "$ test -f fixed.txt") || !strings.Contains(feedback, "(exit status 1)") {
		t.Errorf("the agent must be told which check failed:\n%s", feedback)
	}

	checks, _ := e.store.ListVerifications(ctx, task.ID)
	if len(checks) != 2 || checks[0].Passed || !checks[1].Passed || checks[0].Attempt != 1 || checks[1].Attempt != 2 {
		t.Fatalf("stored checks = %+v", checks)
	}
	// Usage from both runs is on the task.
	if res.Task.InputTokens != 2*1050 {
		t.Errorf("InputTokens = %d, want both runs counted", res.Task.InputTokens)
	}
}

func TestRunTask_GivesUpAfterTheRetryBudget(t *testing.T) {
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = []string{"echo still-broken; exit 1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{})
	s := e.scheduler(1)
	s.Verifier = &Verifier{Store: e.store, MaxRetries: 1}

	res, err := s.RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Task.StatusReason, "verification failed after 2 attempts") {
		t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
	}
	if e.invocations() != 2 {
		t.Fatalf("agent invocations = %d, want the first run plus one retry", e.invocations())
	}
}

// A run that does not finish cleanly is not verified.
func TestRunTask_SkipsVerificationUnlessTheAgentFinished(t *testing.T) {
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = []string{"exit 1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})

	res, err := e.scheduler(1).RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.Status != StatusNeedsDecision || res.Reason != "agent is waiting for an answer" {
		t.Fatalf("result = %+v", res)
	}
	if checks, _ := e.store.ListVerifications(ctx, task.ID); len(checks) != 0 {
		t.Fatalf("no checks may run for an unfinished task: %+v", checks)
	}

	// The owner answers; the same call carries the task on through verification.
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = []string{"true"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.script(lineInit, lineResultOK)
	res, err = e.scheduler(1).RunTask(ctx, task.ID, RunOptions{Instruction: "option B"})
	if err != nil || res.Task.Status != StatusReviewReady {
		t.Fatalf("after the answer: %+v, %v", res, err)
	}
}

func TestScheduler_ScoutEndToEnd(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, `{"type":"result","subtype":"success","result":"# Findings\n\nAll good.","session_id":"sess-1"}`)
	task := e.task(NewTask{Shape: ShapeScout, Goal: "audit the config"})

	e.runUntilIdle(e.scheduler(1))

	got, _ := e.store.GetTask(ctx, task.ID)
	if got.Status != StatusDone || got.StatusReason != "report verified" {
		t.Fatalf("task = %s (%q)", got.Status, got.StatusReason)
	}
	path, _ := e.runner.ReportPath("app", task.ID)
	if body, err := os.ReadFile(path); err != nil || !strings.Contains(string(body), "All good.") {
		t.Fatalf("report = %q, %v", body, err)
	}
}
