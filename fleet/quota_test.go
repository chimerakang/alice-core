package fleet

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestCodexQuotaUsesMostConstrainingWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	var response codexQuotaReply
	err := json.Unmarshal([]byte(`{
		"rateLimits":{"primary":{"usedPercent":1,"resetsAt":2000}},
		"rateLimitsByLimitId":{"codex":{
			"primary":{"usedPercent":100,"resetsAt":2000},
			"secondary":{"usedPercent":100,"resetsAt":3000}
		}}}`), &response)
	if err != nil {
		t.Fatal(err)
	}
	blocked := response.snapshot(now)
	if blocked.allowed == nil || *blocked.allowed || blocked.remaining != 0 || blocked.resetAt.Unix() != 3000 {
		t.Fatalf("quota snapshot = %+v", blocked)
	}
	available := response.snapshot(time.Unix(3000, 0))
	if available.allowed != nil || !available.resetAt.IsZero() {
		t.Fatalf("expired quota snapshot = %+v", available)
	}
}

func TestFleetQuotaWaitPreservesSessionAndDeduplicatesOwnerWake(t *testing.T) {
	e := newRunnerEnv(t)
	clock := &settableClock{now: time.Now()}
	e.store.now = clock.Now
	task := e.task(NewTask{ProjectID: "app", Goal: "Wait for quota", Shape: ShapeShip})
	if _, err := e.store.Transition(ctx, task.ID, StatusRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetSession(ctx, task.ID, "existing"); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"claude", "codex"} {
		if err := e.store.recordQuotaExhaustion(ctx, task.ID, QuotaExhaustion{Backend: backend, ResetsAt: clock.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	waiting, err := e.store.requeueQuotaWait(ctx, task, "claude")
	if err != nil || waiting.Status != StatusQueued || waiting.SessionID != "existing" || waiting.quotaTransfers != 0 {
		t.Fatalf("quota wait = %+v, err %v", waiting, err)
	}
	for i := 0; i < 2; i++ {
		clock.Advance(time.Minute)
		if err := e.store.recordCodexQuotaDenied(ctx, clock.Now(), clock.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		wake, bothBlocked, err := e.store.bothBackendsQuotaWake(ctx)
		if err != nil || !bothBlocked {
			t.Fatalf("both backends blocked = %v, err %v", bothBlocked, err)
		}
		added, err := e.store.raiseWakes(ctx, wake)
		if err != nil || len(added) != 1-i {
			t.Fatalf("new wakes at repeat %d = %v, err %v", i, added, err)
		}
	}
}

func TestFleetShipWaitsForQuotaAndResumesSession(t *testing.T) {
	e := newRunnerEnv(t)
	clock := &settableClock{now: time.Now()}
	e.store.now = clock.Now
	task := e.task(NewTask{ProjectID: "app", Goal: "Keep this session", Shape: ShapeShip})
	e.scriptFor(1, lineInit, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`)
	e.scriptFor(2, lineResultOK)
	e.runner.QuotaUnknownResetAfter = time.Minute
	first := e.run(task.ID, RunOptions{})
	if first.Outcome != OutcomeRequeued || first.Task.Status != StatusQueued || first.Task.SessionID == "" {
		t.Fatalf("first outcome = %s, task = %+v", first.Outcome, first.Task)
	}
	wait, _, err := e.store.BackendQuotaBlocked(ctx, "claude")
	if err != nil || !wait {
		t.Fatalf("blocked = %v, err %v", wait, err)
	}
	if started, err := e.scheduler(1).Tick(ctx); err != nil || len(started) != 0 {
		t.Fatalf("scheduler started %v during cooldown, err %v", started, err)
	}
	clock.Advance(time.Minute)
	_ = e.run(task.ID, RunOptions{})
	args := e.args(2)
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--resume" && args[i+1] == first.Task.SessionID {
			return
		}
	}
	t.Fatalf("resume args = %v", args)
}

func TestFleetScoutTransfersOnceThenWaitsForQuota(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.CodexScoutBinary = e.runner.Binary
	e.runner.QuotaUnknownResetAfter = time.Hour
	e.runner.ConfigureScoutQuotaAdmission(func(context.Context, Task, bool) (string, string, string) {
		return "claude", "codex-test", ""
	})
	e.runner.ConfigureScoutQuotaFallback(1, func(_ context.Context, _ Task, backend string) (string, bool) {
		return "codex-test", backend == "codex"
	})
	e.runner.MatchQuotaError = func(backend, diagnostic string) (bool, time.Time) {
		return backend == "codex" && diagnostic == "quota exhausted", time.Time{}
	}
	e.scriptFor(1, lineInit, `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`)
	e.scriptFor(2, "#stderr quota exhausted", "#exit 1")

	task := e.task(NewTask{ProjectID: "app", Goal: "Inspect the repository", Shape: ShapeScout})
	first := e.run(task.ID, RunOptions{scoutRoute: &scheduledScoutRoute{backend: "claude"}})
	if first.Outcome != OutcomeRequeued || first.Task.quotaBackend != "codex" || first.Task.quotaTransfers != 1 {
		t.Fatalf("quota transfer = %+v", first)
	}
	second := e.run(task.ID, RunOptions{})
	if second.Outcome != OutcomeRequeued || second.Task.StatusReason != quotaWaitPrefix+"codex" || second.Task.quotaTransfers != 1 {
		t.Fatalf("quota wait after transfer = %+v", second)
	}
	if !slices.Contains(e.args(2), "codex-test") {
		t.Fatalf("alternate invocation = %v", e.args(2))
	}
}
