package fleet

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// inbox collects what a Notifier delivers.
type inbox struct {
	mu   sync.Mutex
	got  []Notification
	fail error
}

func (b *inbox) deliver(_ context.Context, n Notification) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return b.fail
	}
	b.got = append(b.got, n)
	return nil
}

func (b *inbox) messages() []Notification {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Notification(nil), b.got...)
}

func (b *inbox) setFail(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fail = err
}

type notifyEnv struct {
	*runnerEnv
	clock    *settableClock
	inbox    *inbox
	notifier *Notifier
	n        int
}

func newNotifyEnv(t *testing.T) *notifyEnv {
	t.Helper()
	e := newRunnerEnv(t)
	clock := &settableClock{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	e.store.now = clock.Now
	box := &inbox{}
	return &notifyEnv{
		runnerEnv: e, clock: clock, inbox: box,
		notifier: &Notifier{Store: e.store, User: "ck", Deliver: box.deliver, MinInterval: time.Minute},
	}
}

// raise queues a wake about a task, as the watcher would.
func (e *notifyEnv) raise(task Task, kind WakeKind, detail string) {
	e.t.Helper()
	e.n++
	if _, err := e.store.raiseWakes(ctx, newWake{
		taskID: task.ID, projectID: task.ProjectID, kind: kind, detail: detail,
		dedupeKey: "test:" + string(rune('a'+e.n)),
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *notifyEnv) process() []Notification {
	e.t.Helper()
	sent, err := e.notifier.Process(ctx)
	if err != nil {
		e.t.Fatalf("Process: %v", err)
	}
	return sent
}

func (e *notifyEnv) pending() int {
	e.t.Helper()
	wakes, err := e.store.PendingWakes(ctx, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return len(wakes)
}

func kinds(n Notification) []WakeKind {
	var out []WakeKind
	for _, w := range n.Wakes {
		out = append(out, w.Kind)
	}
	return out
}

func TestNotifier_NormalDeliversAndMergesPerTask(t *testing.T) {
	e := newNotifyEnv(t)
	a, b := e.task(NewTask{Goal: "a"}), e.task(NewTask{Goal: "b"})
	e.raise(a, WakeStuck, "no output for 11m0s")
	e.raise(b, WakeCompleted, "report verified")
	e.raise(a, WakeNeedsDecision, "timed out after 2h0m0s")

	sent := e.process()
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want one per task: %+v", len(sent), sent)
	}
	if sent[0].TaskID != a.ID || sent[0].ProjectID != "app" || sent[0].User != "ck" ||
		!reflect.DeepEqual(kinds(sent[0]), []WakeKind{WakeStuck, WakeNeedsDecision}) {
		t.Errorf("first message = %+v; both wakes about the task belong in it", sent[0])
	}
	if sent[1].TaskID != b.ID || !reflect.DeepEqual(kinds(sent[1]), []WakeKind{WakeCompleted}) {
		t.Errorf("second message = %+v", sent[1])
	}
	if e.pending() != 0 {
		t.Fatalf("%d wakes still pending after delivery", e.pending())
	}
	if again := e.process(); len(again) != 0 {
		t.Fatalf("a second pass delivered again: %+v", again)
	}
	if got := e.inbox.messages(); len(got) != 2 {
		t.Fatalf("inbox has %d messages", len(got))
	}
}

// A wake is consumed only once it has really been delivered.
func TestNotifier_FailedDeliveryLeavesWakesPending(t *testing.T) {
	e := newNotifyEnv(t)
	task := e.task(NewTask{})
	e.raise(task, WakeQuestion, "agent is waiting for an answer")

	e.inbox.setFail(errors.New("telegram is down"))
	if sent := e.process(); len(sent) != 0 {
		t.Fatalf("reported %d messages as sent while delivery was failing", len(sent))
	}
	if e.pending() != 1 {
		t.Fatalf("pending = %d; an undelivered wake must stay in the queue", e.pending())
	}

	e.inbox.setFail(nil)
	if sent := e.process(); len(sent) != 1 || sent[0].TaskID != task.ID {
		t.Fatalf("after recovery: %+v", sent)
	}
	if e.pending() != 0 {
		t.Fatal("the wake must be consumed once delivered")
	}
}

func TestNotifier_MinIntervalPerTask(t *testing.T) {
	e := newNotifyEnv(t)
	noisy, other := e.task(NewTask{Goal: "noisy"}), e.task(NewTask{Goal: "other"})

	e.raise(noisy, WakeStuck, "first")
	if sent := e.process(); len(sent) != 1 {
		t.Fatalf("the first message about a task goes out at once: %+v", sent)
	}

	// Two more about the same task within the interval, and one about another task.
	e.clock.Advance(20 * time.Second)
	e.raise(noisy, WakeNeedsDecision, "second")
	e.raise(other, WakeCompleted, "unrelated")
	e.clock.Advance(20 * time.Second)
	e.raise(noisy, WakeFailed, "third")

	sent := e.process()
	if len(sent) != 1 || sent[0].TaskID != other.ID {
		t.Fatalf("within the interval only the other task may be messaged: %+v", sent)
	}
	if e.pending() != 2 {
		t.Fatalf("pending = %d; the held-back wakes must wait, not vanish", e.pending())
	}

	// Once the interval has passed, both arrive as one message.
	e.clock.Advance(30 * time.Second) // 70s since the first message
	sent = e.process()
	if len(sent) != 1 || sent[0].TaskID != noisy.ID || !reflect.DeepEqual(kinds(sent[0]), []WakeKind{WakeNeedsDecision, WakeFailed}) {
		t.Fatalf("after the interval: %+v, want one merged message", sent)
	}
	if e.pending() != 0 {
		t.Fatalf("pending = %d", e.pending())
	}
}

func TestNotifier_IntervalDisabledAndFleetWideWakes(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.MinInterval = -1
	task := e.task(NewTask{})
	e.raise(task, WakeStuck, "one")
	e.process()
	e.raise(task, WakeStuck, "two")
	if sent := e.process(); len(sent) != 1 {
		t.Fatalf("with the limit disabled every pass delivers: %+v", sent)
	}

	// Wakes about no task are never merged with each other or rate-limited.
	e.notifier.MinInterval = time.Hour
	for _, over := range []*BudgetError{
		{Scope: BudgetScopeFleet, Detail: "d"},
		{Scope: BudgetScopeProject, ProjectID: "app", Detail: "d"},
	} {
		if _, err := e.store.raiseWakes(ctx, budgetWake(over, "2026-05-01")); err != nil {
			t.Fatal(err)
		}
	}
	sent := e.process()
	if len(sent) != 2 || sent[0].TaskID != "" || sent[1].ProjectID != "app" || len(sent[0].Wakes) != 1 {
		t.Fatalf("fleet-wide wakes: %+v", sent)
	}
}

func TestNotifier_QuietDeliversOnlyWhatNeedsTheUser(t *testing.T) {
	e := newNotifyEnv(t)
	if _, err := e.notifier.SetMode(ctx, NotifyQuiet, nil); err != nil {
		t.Fatal(err)
	}
	wants := map[WakeKind]bool{
		WakeQuestion: true, WakeNeedsDecision: true, WakeFailed: true, WakeOverBudget: true,
		WakeReviewReady: false, WakeCompleted: false, WakeStuck: false,
	}
	tasks := map[string]WakeKind{}
	for kind := range wants {
		task := e.task(NewTask{Goal: string(kind)})
		tasks[task.ID] = kind
		e.raise(task, kind, "detail of "+string(kind))
	}

	delivered := map[WakeKind]bool{}
	for _, n := range e.process() {
		delivered[tasks[n.TaskID]] = true
	}
	for kind, want := range wants {
		if delivered[kind] != want {
			t.Errorf("%s: delivered = %v, want %v", kind, delivered[kind], want)
		}
	}
	if e.pending() != 0 {
		t.Fatalf("held wakes must be settled, not left pending: %d", e.pending())
	}

	// What was held is waiting in the summary.
	e.clock.Advance(time.Hour)
	summary, err := e.notifier.Back(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Mode != NotifyQuiet || len(summary.Held) != 3 || len(summary.Handled) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	held := map[WakeKind]bool{}
	for _, r := range summary.Held {
		held[r.Kind] = true
		if r.Detail != "detail of "+string(r.Kind) || r.TaskID == "" {
			t.Errorf("held record = %+v", r)
		}
	}
	if !held[WakeReviewReady] || !held[WakeCompleted] || !held[WakeStuck] {
		t.Errorf("held kinds = %v", held)
	}
	if prefs, _ := e.notifier.Prefs(ctx); prefs.Mode != NotifyNormal {
		t.Errorf("after Back the mode = %s", prefs.Mode)
	}
	if again, _ := e.notifier.Back(ctx); !again.Empty() {
		t.Errorf("Back in normal mode must have nothing to report: %+v", again)
	}
}

func TestNotifier_SetModeValidation(t *testing.T) {
	e := newNotifyEnv(t)
	future := e.clock.Now().Add(time.Hour)
	cases := []struct {
		name  string
		mode  NotifyMode
		grant *AFKGrant
	}{
		{"unknown mode", "vacation", nil},
		{"afk without a grant", NotifyAFK, nil},
		{"afk with no expiry", NotifyAFK, &AFKGrant{Actions: []AFKAction{AFKApprove}}},
		{"afk already expired", NotifyAFK, &AFKGrant{ExpiresAt: e.clock.Now().Add(-time.Minute)}},
		{"afk with an unknown action", NotifyAFK, &AFKGrant{Actions: []AFKAction{"merge_everything"}, ExpiresAt: future}},
		{"quiet with a grant", NotifyQuiet, &AFKGrant{ExpiresAt: future}},
	}
	for _, tc := range cases {
		if _, err := e.notifier.SetMode(ctx, tc.mode, tc.grant); !errors.Is(err, ErrInvalidNotifyMode) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if prefs, _ := e.notifier.Prefs(ctx); prefs.Mode != NotifyNormal {
		t.Fatalf("rejected settings changed the mode to %s", prefs.Mode)
	}

	grant := AFKGrant{Actions: []AFKAction{AFKApprove, AFKResume}, Projects: []string{"app"}, ExpiresAt: future}
	prefs, err := e.notifier.SetMode(ctx, NotifyAFK, &grant)
	if err != nil {
		t.Fatalf("a valid grant: %v", err)
	}
	if prefs.Mode != NotifyAFK || !prefs.Since.Equal(e.clock.Now()) {
		t.Errorf("prefs = %+v", prefs)
	}
	// A different Notifier, as after a restart, sees the same setting.
	again := &Notifier{Store: e.store, User: "ck", Deliver: e.inbox.deliver}
	stored, err := again.Prefs(ctx)
	if err != nil || stored.Mode != NotifyAFK || !reflect.DeepEqual(stored.Grant.Actions, grant.Actions) ||
		!reflect.DeepEqual(stored.Grant.Projects, grant.Projects) || !stored.Grant.ExpiresAt.Equal(future) {
		t.Fatalf("stored prefs = %+v, %v", stored, err)
	}
	// Another user is unaffected.
	if other, _ := (&Notifier{Store: e.store, User: "someone-else", Deliver: e.inbox.deliver}).Prefs(ctx); other.Mode != NotifyNormal {
		t.Errorf("another user's mode = %s", other.Mode)
	}

	for name, n := range map[string]*Notifier{
		"no store":   {User: "ck", Deliver: e.inbox.deliver},
		"no user":    {Store: e.store, Deliver: e.inbox.deliver},
		"no deliver": {Store: e.store, User: "ck"},
	} {
		if _, err := n.Process(ctx); err == nil {
			t.Errorf("%s: Process must refuse", name)
		}
	}
}

func TestGrant_Allows(t *testing.T) {
	all := AFKGrant{Actions: []AFKAction{AFKApprove}}
	if !all.allows(AFKApprove, "anything") || all.allows(AFKResume, "anything") {
		t.Error("a grant allows only its listed actions")
	}
	scoped := AFKGrant{Actions: []AFKAction{AFKApprove, AFKResume}, Projects: []string{"app"}}
	if !scoped.allows(AFKResume, "app") || scoped.allows(AFKResume, "other") {
		t.Error("a grant limited to projects must not reach others")
	}
	if (AFKGrant{}).allows(AFKApprove, "app") {
		t.Error("an empty grant allows nothing")
	}
}

func TestResumableWithoutJudgement(t *testing.T) {
	yes := []string{
		"timed out after 2h0m0s", "reached the turn limit (100)", "runner stopped: context canceled",
		"agent exited without a result: no details", "agent exited with an error: boom",
		"agent process is gone: it died or the host restarted while it was running",
	}
	no := []string{
		"agent is waiting for an answer", "verification failed after 3 attempts: go test failed (exit 1)",
		"delivery failed: git push: rejected", "over the task budget: $2.00 spent of a $1.00 limit",
		"nothing to deliver: the task changed nothing", "scout produced no report", "wrong approach", "",
	}
	for _, r := range yes {
		if !resumableWithoutJudgement(r) {
			t.Errorf("%q should be resumable without a person", r)
		}
	}
	for _, r := range no {
		if resumableWithoutJudgement(r) {
			t.Errorf("%q needs a person and must not be resumed automatically", r)
		}
	}
}

// Away with nothing granted: nothing is delivered and nothing is done.
func TestNotifier_AFKWithoutActionsOnlyHolds(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.Commander = e.commander()
	if _, err := e.notifier.SetMode(ctx, NotifyAFK, &AFKGrant{ExpiresAt: e.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ready := e.task(NewTask{})
	walk(t, e.store, ready.ID, StatusRunning, StatusVerifying, StatusReviewReady)
	e.raise(ready, WakeReviewReady, "verification passed")
	asking := e.task(NewTask{})
	e.raise(asking, WakeQuestion, "agent is waiting for an answer")

	if sent := e.process(); len(sent) != 0 {
		t.Fatalf("away means no messages: %+v", sent)
	}
	if got, _ := e.store.GetTask(ctx, ready.ID); got.Status != StatusReviewReady {
		t.Fatalf("nothing was granted, but the task is now %s", got.Status)
	}
	summary, _ := e.notifier.Back(ctx)
	if len(summary.Held) != 2 || len(summary.Handled) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestNotifier_AFKApprovesWithinItsGrant(t *testing.T) {
	d := newDeliveryEnv(t, TrustLocalOnly)
	d.addProject("other", 1)
	clock := &settableClock{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	box := &inbox{}
	commander := &Commander{Store: d.store, Deliverer: d.deliverer}
	n := &Notifier{Store: d.store, User: "ck", Deliver: box.deliver, Commander: commander}

	covered := d.reviewReady("Add the feature")
	d.deliver(covered.ID)
	d.store.now = clock.Now
	if _, err := n.SetMode(ctx, NotifyAFK, &AFKGrant{
		Actions: []AFKAction{AFKApprove}, Projects: []string{"web"}, ExpiresAt: clock.Now().Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	outside := mustCreate(t, d.store, NewTask{ProjectID: "other"})
	walk(t, d.store, outside.ID, StatusRunning, StatusVerifying, StatusReviewReady)

	raise := func(task Task, key string) {
		if _, err := d.store.raiseWakes(ctx, newWake{taskID: task.ID, projectID: task.ProjectID, kind: WakeReviewReady, detail: "verification passed", dedupeKey: key}); err != nil {
			t.Fatal(err)
		}
	}
	raise(covered, "a")
	raise(outside, "b")

	sent, err := n.Process(ctx)
	if err != nil || len(sent) != 0 {
		t.Fatalf("Process = %+v, %v; away means no messages", sent, err)
	}
	approved, _ := d.store.GetTask(ctx, covered.ID)
	if approved.Status != StatusDone || approved.StatusReason != "approved by ck: fast-forwarded main" {
		t.Fatalf("covered task = %s (%q), want it approved", approved.Status, approved.StatusReason)
	}
	if got := gitT(t, d.repo, "rev-parse", "main"); got != gitT(t, covered.WorktreePath, "rev-parse", "HEAD") {
		t.Error("the approved work was not landed")
	}
	if got, _ := d.store.GetTask(ctx, outside.ID); got.Status != StatusReviewReady {
		t.Fatalf("a task outside the grant's projects was touched: %s", got.Status)
	}
	// The audit log shows it was done under the grant, not by hand.
	entries, _ := d.store.ListAudit(ctx, AuditFilter{TaskID: covered.ID})
	if len(entries) != 1 || entries[0].Command != CmdApprove || entries[0].Via != "afk" || entries[0].Actor != "ck" {
		t.Fatalf("audit = %+v", entries)
	}

	clock.Advance(time.Hour)
	summary, err := n.Back(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Handled) != 1 || summary.Handled[0].TaskID != covered.ID || summary.Handled[0].Disposition != AutoApproved {
		t.Errorf("handled = %+v", summary.Handled)
	}
	if len(summary.Held) != 1 || summary.Held[0].TaskID != outside.ID {
		t.Errorf("held = %+v", summary.Held)
	}
	if !summary.Until.Equal(clock.Now()) || summary.Mode != NotifyAFK {
		t.Errorf("summary period = %+v", summary)
	}
}

// An approval that cannot go through is held, with the reason, not lost.
func TestNotifier_AFKApprovalThatFailsIsHeld(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.Commander = e.commander()
	if _, err := e.notifier.SetMode(ctx, NotifyAFK, &AFKGrant{Actions: []AFKAction{AFKApprove}, ExpiresAt: e.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// review_ready, but with no branch to land: approval will fail.
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning, StatusVerifying, StatusReviewReady)
	e.raise(task, WakeReviewReady, "verification passed")

	e.process()
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusReviewReady {
		t.Fatalf("task = %s", got.Status)
	}
	summary, _ := e.notifier.Back(ctx)
	if len(summary.Held) != 1 || !strings.Contains(summary.Held[0].Note, "could not approve automatically") {
		t.Fatalf("held = %+v", summary.Held)
	}
}

func TestNotifier_AFKResumesOncePerTask(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.Commander = e.commander()
	e.script(lineInit, lineResultOK)
	if _, err := e.notifier.SetMode(ctx, NotifyAFK, &AFKGrant{Actions: []AFKAction{AFKResume}, ExpiresAt: e.clock.Now().Add(8 * time.Hour)}); err != nil {
		t.Fatal(err)
	}

	stalled := e.task(NewTask{Goal: "stalled"})
	walk(t, e.store, stalled.ID, StatusRunning)
	if _, err := e.store.Transition(ctx, stalled.ID, StatusNeedsDecision, "timed out after 2h0m0s"); err != nil {
		t.Fatal(err)
	}
	asking := e.task(NewTask{Goal: "asking"})
	walk(t, e.store, asking.ID, StatusRunning)
	if _, err := e.store.Transition(ctx, asking.ID, StatusNeedsDecision, reasonAgentWaiting); err != nil {
		t.Fatal(err)
	}
	e.raise(stalled, WakeNeedsDecision, "timed out after 2h0m0s")
	e.raise(asking, WakeQuestion, reasonAgentWaiting)

	if sent := e.process(); len(sent) != 0 {
		t.Fatalf("away means no messages: %+v", sent)
	}
	// The stalled task is carried on; the question is left for a person.
	e.waitForStatus(stalled.ID, StatusReviewReady)
	e.notifier.Commander.Scheduler.Wait()
	if e.invocations() != 1 {
		t.Fatalf("agent invocations = %d, want only the stalled task resumed", e.invocations())
	}
	if got, _ := e.store.GetTask(ctx, asking.ID); got.Status != StatusNeedsDecision {
		t.Fatalf("a question was answered automatically: %s", got.Status)
	}

	// It stalls again. Once was the allowance.
	walk(t, e.store, stalled.ID, StatusRunning)
	if _, err := e.store.Transition(ctx, stalled.ID, StatusNeedsDecision, "timed out after 2h0m0s"); err != nil {
		t.Fatal(err)
	}
	e.raise(stalled, WakeNeedsDecision, "timed out after 2h0m0s")
	e.process()
	e.notifier.Commander.Scheduler.Wait()
	if e.invocations() != 1 {
		t.Fatalf("the task was resumed a second time (%d invocations)", e.invocations())
	}

	summary, _ := e.notifier.Back(ctx)
	if len(summary.Handled) != 1 || summary.Handled[0].Disposition != AutoResumed || summary.Handled[0].TaskID != stalled.ID {
		t.Errorf("handled = %+v", summary.Handled)
	}
	var notes []string
	for _, r := range summary.Held {
		notes = append(notes, string(r.Kind)+":"+r.Note)
	}
	want := []string{"question:", "needs_decision:already resumed once while away"}
	if !reflect.DeepEqual(notes, want) {
		t.Errorf("held = %v, want %v", notes, want)
	}
}

// The grant ends on the fleet's clock, whether or not the user came back.
func TestNotifier_AFKGrantExpires(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.Commander = e.commander()
	if _, err := e.notifier.SetMode(ctx, NotifyAFK, &AFKGrant{Actions: []AFKAction{AFKApprove}, ExpiresAt: e.clock.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	during := e.task(NewTask{Goal: "during"})
	e.raise(during, WakeQuestion, reasonAgentWaiting)
	if sent := e.process(); len(sent) != 0 {
		t.Fatalf("during the grant: %+v", sent)
	}

	e.clock.Advance(61 * time.Minute)
	// Something that the grant would have approved arrives after it lapsed.
	late := e.task(NewTask{Goal: "late"})
	walk(t, e.store, late.ID, StatusRunning, StatusVerifying, StatusReviewReady)
	e.raise(late, WakeReviewReady, "verification passed")

	sent := e.process()
	if len(sent) != 2 {
		t.Fatalf("after expiry want the summary and the new wake, got %+v", sent)
	}
	if sent[0].Summary == nil || len(sent[0].Summary.Held) != 1 || sent[0].Summary.Held[0].TaskID != during.ID || len(sent[0].Wakes) != 0 {
		t.Errorf("first message must be the away summary: %+v", sent[0])
	}
	if sent[1].Summary != nil || sent[1].TaskID != late.ID {
		t.Errorf("second message must be the new wake, delivered normally: %+v", sent[1])
	}
	if got, _ := e.store.GetTask(ctx, late.ID); got.Status != StatusReviewReady {
		t.Fatalf("an expired grant approved a task: %s", got.Status)
	}
	if prefs, _ := e.notifier.Prefs(ctx); prefs.Mode != NotifyNormal {
		t.Fatalf("mode after expiry = %s", prefs.Mode)
	}
	if entries, _ := e.store.ListAudit(ctx, AuditFilter{}); len(entries) != 0 {
		t.Errorf("no command may be given under an expired grant: %+v", entries)
	}
}

func TestNotifier_Run(t *testing.T) {
	e := newNotifyEnv(t)
	e.notifier.Interval = 10 * time.Millisecond
	c, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.notifier.Run(c) }()

	task := e.task(NewTask{})
	e.raise(task, WakeFailed, "agent produced no output")
	e.waitUntil("the running notifier to deliver", func() bool { return len(e.inbox.messages()) == 1 })

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop")
	}
	if err := (&Notifier{}).Run(ctx); err == nil {
		t.Error("Run without a Store must fail")
	}
}
