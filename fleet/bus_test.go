package fleet

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chimerakang/alice-core/agent"
)

func (e *runnerEnv) bus() *Bus {
	return &Bus{Store: e.store, Interval: 10 * time.Millisecond}
}

// collect reads events until none has arrived for a short while.
func collect(t *testing.T, ch <-chan BusEvent) []BusEvent {
	t.Helper()
	var got []BusEvent
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-time.After(300 * time.Millisecond):
			return got
		}
	}
}

func eventIDs(events []BusEvent) []int64 {
	ids := []int64{}
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

func storedIDs(t *testing.T, store *Store, taskID string) []int64 {
	t.Helper()
	events, err := store.ListEvents(ctx, taskID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, ev := range events {
		ids = append(ids, ev.ID)
	}
	return ids
}

func TestBus_LiveSubscriptionStartsFromNow(t *testing.T) {
	e := newRunnerEnv(t)
	before := e.task(NewTask{Goal: "before"}) // its creation event predates the subscription
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.bus().Subscribe(c, EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	after := e.task(NewTask{Goal: "after"})
	walk(t, e.store, before.ID, StatusRunning)

	got := collect(t, ch)
	if len(got) != 2 {
		t.Fatalf("got %d events, want the two written after subscribing: %+v", len(got), got)
	}
	if got[0].TaskID != after.ID || got[0].Type != EventStatus || got[0].ProjectID != "app" {
		t.Errorf("first event = %+v", got[0])
	}
	if got[1].TaskID != before.ID || got[1].ID <= got[0].ID {
		t.Errorf("second event = %+v; events must arrive in the order they were stored", got[1])
	}
}

func TestBus_ReplayAndResume(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	walk(t, e.store, task.ID, StatusRunning, StatusNeedsDecision, StatusRunning, StatusVerifying)
	all := storedIDs(t, e.store, "")

	c, cancel := context.WithCancel(ctx)
	fromStart, err := e.bus().Subscribe(c, EventFilter{FromStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := eventIDs(collect(t, fromStart)); !reflect.DeepEqual(got, all) {
		t.Fatalf("FromStart replayed %v, want %v", got, all)
	}
	cancel()

	// A subscriber that saw the first two events and then lost its connection.
	c2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	resumed, err := e.bus().Subscribe(c2, EventFilter{AfterID: all[1]})
	if err != nil {
		t.Fatal(err)
	}
	if got := eventIDs(collect(t, resumed)); !reflect.DeepEqual(got, all[2:]) {
		t.Fatalf("resuming after %d gave %v, want %v: no gap and no repeat", all[1], got, all[2:])
	}

	// And it stays live after the replay.
	walk(t, e.store, task.ID, StatusReviewReady)
	live := collect(t, resumed)
	if len(live) != 1 || live[0].Type != EventStatus {
		t.Fatalf("after catching up, new events must keep arriving: %+v", live)
	}
}

// A subscriber that keeps dropping and reconnecting sees every event once.
func TestBus_ReconnectingNeverLosesOrRepeats(t *testing.T) {
	e := newRunnerEnv(t)
	var mu sync.Mutex
	var seen []int64
	last := int64(0)
	done := make(chan struct{})

	go func() {
		defer close(done)
		for round := 0; round < 6; round++ {
			c, cancel := context.WithTimeout(ctx, 120*time.Millisecond)
			mu.Lock()
			filter := EventFilter{AfterID: last, FromStart: true}
			mu.Unlock()
			ch, err := e.bus().Subscribe(c, filter)
			if err != nil {
				cancel()
				return
			}
			for ev := range ch {
				mu.Lock()
				seen = append(seen, ev.ID)
				last = ev.ID
				mu.Unlock()
			}
			cancel()
		}
	}()

	for i := 0; i < 15; i++ {
		task := e.task(NewTask{})
		walk(t, e.store, task.ID, StatusRunning, StatusFailed)
		time.Sleep(20 * time.Millisecond)
	}
	<-done
	// One last connection to drain whatever was written after the final drop.
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, _ := e.bus().Subscribe(c, EventFilter{AfterID: last, FromStart: true})
	for _, ev := range collect(t, ch) {
		seen = append(seen, ev.ID)
	}

	if want := storedIDs(t, e.store, ""); !reflect.DeepEqual(seen, want) {
		t.Fatalf("across reconnects the subscriber saw %d events, the log has %d\n got %v\nwant %v", len(seen), len(want), seen, want)
	}
}

func TestBus_Filters(t *testing.T) {
	e := newRunnerEnv(t)
	e.addProject("other", 1)
	e.script(lineInit, lineText, lineToolUse, lineToolRes, lineResultOK)
	inApp := e.task(NewTask{ProjectID: "app"})
	inOther := e.task(NewTask{ProjectID: "other"})
	e.run(inApp.ID, RunOptions{})
	e.run(inOther.ID, RunOptions{})

	subscribe := func(f EventFilter) []BusEvent {
		t.Helper()
		f.FromStart = true
		c, cancel := context.WithCancel(ctx)
		defer cancel()
		ch, err := e.bus().Subscribe(c, f)
		if err != nil {
			t.Fatal(err)
		}
		return collect(t, ch)
	}

	byProject := subscribe(EventFilter{ProjectID: "other"})
	if len(byProject) == 0 {
		t.Fatal("no events for the project")
	}
	for _, ev := range byProject {
		if ev.TaskID != inOther.ID || ev.ProjectID != "other" {
			t.Fatalf("project filter let through %+v", ev)
		}
	}

	byTask := subscribe(EventFilter{TaskID: inApp.ID})
	if !reflect.DeepEqual(eventIDs(byTask), storedIDs(t, e.store, inApp.ID)) {
		t.Fatalf("task filter = %v, want every event of the task", eventIDs(byTask))
	}

	// Lifecycle only: no agent chatter.
	lifecycle := subscribe(EventFilter{TaskID: inApp.ID, Types: LifecycleEvents})
	var types []string
	for _, ev := range lifecycle {
		types = append(types, ev.Type)
	}
	want := []string{EventStatus, EventStatus, EventLiveness, EventLiveness, EventStatus}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("lifecycle events = %v, want %v", types, want)
	}

	tools := subscribe(EventFilter{Types: []string{EventAgentToolUse}})
	if len(tools) != 2 {
		t.Fatalf("type filter: %d tool_use events, want one per task", len(tools))
	}
	if none := subscribe(EventFilter{ProjectID: "ghost"}); len(none) != 0 {
		t.Fatalf("an unknown project matched %d events", len(none))
	}
}

// Lifecycle events that a front end follows all arrive on the one stream.
func TestBus_CarriesStatusLivenessVerificationAndWakes(t *testing.T) {
	e := newRunnerEnv(t)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.bus().Subscribe(c, EventFilter{Types: LifecycleEvents})
	if err != nil {
		t.Fatal(err)
	}

	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{})
	if _, err := e.scheduler(1).RunTask(ctx, task.ID, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	e.scan(e.watcher()) // raises the review_ready wake
	for _, kind := range []string{EventStartupResumed, EventStartupResumeCapped} {
		if _, err := e.store.AppendEvent(ctx, task.ID, kind, nil); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[string]int{}
	var wake BusEvent
	for _, ev := range collect(t, ch) {
		seen[ev.Type]++
		if ev.Type == EventWake {
			wake = ev
		}
	}
	for _, kind := range LifecycleEvents {
		if seen[kind] == 0 {
			t.Errorf("no %q event arrived; got %v", kind, seen)
		}
	}
	if wake.TaskID != task.ID || wake.ProjectID != "app" {
		t.Errorf("wake event = %+v", wake)
	}
}

// A wake that concerns a whole project has no task; it still reaches a
// subscriber filtering on that project.
func TestBus_ProjectWideWake(t *testing.T) {
	e := newRunnerEnv(t)
	e.addProject("other", 1)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.bus().Subscribe(c, EventFilter{ProjectID: "other"})
	if err != nil {
		t.Fatal(err)
	}
	over := &BudgetError{Scope: BudgetScopeProject, ProjectID: "other", Detail: "d"}
	if _, err := e.store.raiseWakes(ctx, budgetWake(over, "2026-05-01")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.raiseWakes(ctx, budgetWake(&BudgetError{Scope: BudgetScopeProject, ProjectID: "app", Detail: "d"}, "2026-05-01")); err != nil {
		t.Fatal(err)
	}
	got := collect(t, ch)
	if len(got) != 1 || got[0].Type != EventWake || got[0].ProjectID != "other" || got[0].TaskID != "" {
		t.Fatalf("got %+v, want only the wake for the subscribed project", got)
	}
}

func TestBus_ClosesWhenCancelled(t *testing.T) {
	e := newRunnerEnv(t)
	c, cancel := context.WithCancel(ctx)
	ch, err := e.bus().Subscribe(c, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("an event arrived on a cancelled subscription")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel was not closed after cancellation")
	}
	if _, err := (&Bus{}).Subscribe(ctx, EventFilter{}); err == nil {
		t.Error("a Bus without a Store must refuse")
	}
}

// A reader that stops reading must not make the bus drop events.
func TestBus_SlowSubscriberCatchesUp(t *testing.T) {
	e := newRunnerEnv(t)
	c, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := e.bus().Subscribe(c, EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		e.task(NewTask{})
	}
	time.Sleep(200 * time.Millisecond) // the subscriber is not reading at all
	got := collect(t, ch)
	if len(got) != 40 {
		t.Fatalf("a slow subscriber received %d of 40 events", len(got))
	}
}

func TestSetLiveness_RecordsOnlyChanges(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	for _, l := range []Liveness{LivenessBusy, LivenessBusy, LivenessUnknown, LivenessUnknown, LivenessDead} {
		if err := e.store.SetLiveness(ctx, task.ID, l); err != nil {
			t.Fatal(err)
		}
	}
	changes := e.eventPayloads(task.ID, EventLiveness)
	want := [][2]string{{"unknown", "busy"}, {"busy", "unknown"}, {"unknown", "dead"}}
	if len(changes) != len(want) {
		t.Fatalf("liveness events = %v, want one per actual change", changes)
	}
	for i, w := range want {
		if changes[i]["from"] != w[0] || changes[i]["to"] != w[1] {
			t.Errorf("change %d = %v, want %v", i, changes[i], w)
		}
	}
}

type recordingSink struct {
	mu    sync.Mutex
	tools []string
	execs []agent.ToolExecution
}

func (r *recordingSink) OnToolEvent(eventType string, exec agent.ToolExecution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = append(r.tools, eventType)
	r.execs = append(r.execs, exec)
}

func (r *recordingSink) OnDecisionEvent(agent.DecisionLog) {}

func (r *recordingSink) snapshot() ([]string, []agent.ToolExecution) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tools...), append([]agent.ToolExecution(nil), r.execs...)
}

// A dashboard wired to agent.EventSink shows fleet agents' tool calls as is.
func TestBus_ForwardToolsToAnEventSink(t *testing.T) {
	e := newRunnerEnv(t)
	sink := &recordingSink{}
	c, cancel := context.WithCancel(ctx)
	forwarded := make(chan error, 1)
	go func() { forwarded <- e.bus().ForwardTools(c, sink, EventFilter{FromStart: true}) }()

	failing := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu2","is_error":true}]},"session_id":"sess-1"}`
	second := `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu2","name":"Bash","input":{"command":"go test ./..."}}]},"session_id":"sess-1"}`
	e.script(lineInit, lineToolUse, lineToolRes, second, failing, lineResultOK)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})

	e.waitUntil("the sink to receive four tool events", func() bool {
		names, _ := sink.snapshot()
		return len(names) == 4
	})
	cancel()
	if err := <-forwarded; err != nil {
		t.Fatalf("ForwardTools: %v", err)
	}

	names, execs := sink.snapshot()
	if !reflect.DeepEqual(names, []string{SinkToolStart, SinkToolComplete, SinkToolStart, SinkToolComplete}) {
		t.Fatalf("event names = %v", names)
	}
	start, done, failed := execs[0], execs[1], execs[3]
	if start.ToolName != "Edit" || start.Status != "running" || start.Input["file_path"] != "login_test.go" || start.ProjectPath != e.repo {
		t.Errorf("start = %+v", start)
	}
	if done.ToolName != "Edit" || done.Status != "success" || done.Error != "" || done.Duration < 0 {
		t.Errorf("completion = %+v", done)
	}
	if failed.ToolName != "Bash" || failed.Status != "error" || failed.Error == "" || failed.Input["command"] != "go test ./..." {
		t.Errorf("failed tool = %+v", failed)
	}

	if err := e.bus().ForwardTools(ctx, nil, EventFilter{}); err == nil {
		t.Error("ForwardTools without a sink must fail")
	}
}
