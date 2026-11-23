package fleet

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"sync"
	"testing"
	"time"
)

// storeWithProject returns a store holding one project with ID "app".
func storeWithProject(t *testing.T) *Store {
	t.Helper()
	store := newStore(t)
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "app")}); err != nil {
		t.Fatal(err)
	}
	return store
}

func mustCreate(t *testing.T, store *Store, in NewTask) Task {
	t.Helper()
	if in.ProjectID == "" {
		in.ProjectID = "app"
	}
	if in.Goal == "" && in.Source.Kind != SourceIssue {
		in.Goal = "do the thing"
	}
	task, err := store.CreateTask(ctx, in)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task
}

// walk moves a task through the given statuses in order.
func walk(t *testing.T, store *Store, id string, path ...Status) Task {
	t.Helper()
	var task Task
	var err error
	for _, to := range path {
		if task, err = store.Transition(ctx, id, to, "test"); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	return task
}

// The full transition matrix, written out independently of the implementation.
// Rows are "from", the listed statuses are the only legal "to".
var wantTransitions = map[Status][]Status{
	StatusQueued:        {StatusRunning, StatusCancelled},
	StatusRunning:       {StatusVerifying, StatusNeedsDecision, StatusFailed, StatusCancelled},
	StatusVerifying:     {StatusReviewReady, StatusDone, StatusRunning, StatusNeedsDecision, StatusFailed, StatusCancelled},
	StatusReviewReady:   {StatusDone, StatusRunning, StatusCancelled},
	StatusNeedsDecision: {StatusRunning, StatusQueued, StatusReviewReady, StatusFailed, StatusCancelled},
	StatusFailed:        {StatusQueued},
	StatusDone:          nil,
	StatusCancelled:     nil,
}

// pathTo is a legal route from a fresh task to each status.
var pathTo = map[Status][]Status{
	StatusQueued:        {},
	StatusRunning:       {StatusRunning},
	StatusVerifying:     {StatusRunning, StatusVerifying},
	StatusReviewReady:   {StatusRunning, StatusVerifying, StatusReviewReady},
	StatusNeedsDecision: {StatusRunning, StatusNeedsDecision},
	StatusDone:          {StatusRunning, StatusVerifying, StatusDone},
	StatusFailed:        {StatusRunning, StatusFailed},
	StatusCancelled:     {StatusCancelled},
}

func TestCanTransition_Matrix(t *testing.T) {
	if len(AllStatuses) != len(wantTransitions) {
		t.Fatalf("AllStatuses has %d entries, the matrix has %d", len(AllStatuses), len(wantTransitions))
	}
	for _, from := range AllStatuses {
		allowed := map[Status]bool{}
		for _, to := range wantTransitions[from] {
			allowed[to] = true
		}
		for _, to := range AllStatuses {
			if got := CanTransition(from, to); got != allowed[to] {
				t.Errorf("CanTransition(%s, %s) = %v, want %v", from, to, got, allowed[to])
			}
		}
		if got := len(NextStatuses(from)); got != len(wantTransitions[from]) {
			t.Errorf("NextStatuses(%s) has %d entries, want %d", from, got, len(wantTransitions[from]))
		}
		if CanTransition(from, "bogus") || CanTransition("bogus", from) || CanTransition(from, "") {
			t.Errorf("unknown statuses must never be reachable from or to %s", from)
		}
	}
}

// Every cell of the matrix, exercised against the database.
func TestTransition_Matrix(t *testing.T) {
	store := storeWithProject(t)
	for _, from := range AllStatuses {
		allowed := map[Status]bool{}
		for _, to := range wantTransitions[from] {
			allowed[to] = true
		}
		for _, to := range AllStatuses {
			t.Run(fmt.Sprintf("%s_to_%s", from, to), func(t *testing.T) {
				task := mustCreate(t, store, NewTask{})
				walk(t, store, task.ID, pathTo[from]...)
				eventsBefore, _ := store.ListEvents(ctx, task.ID, 0, 0)

				got, err := store.Transition(ctx, task.ID, to, "because")
				if allowed[to] {
					if err != nil {
						t.Fatalf("legal transition failed: %v", err)
					}
					if got.Status != to || got.StatusReason != "because" {
						t.Fatalf("task = %+v", got)
					}
					return
				}
				var te *TransitionError
				if !errors.As(err, &te) || !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("err = %v, want a *TransitionError", err)
				}
				if te.From != from || te.To != to || te.TaskID != task.ID {
					t.Errorf("TransitionError = %+v", te)
				}
				stored, _ := store.GetTask(ctx, task.ID)
				if stored.Status != from {
					t.Fatalf("a rejected transition changed the status to %s", stored.Status)
				}
				if eventsAfter, _ := store.ListEvents(ctx, task.ID, 0, 0); len(eventsAfter) != len(eventsBefore) {
					t.Fatalf("a rejected transition wrote an event")
				}
			})
		}
	}
}

func TestTransition_UnknownTargetAndTask(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})
	for _, to := range []Status{"", "bogus", "RUNNING"} {
		if _, err := store.Transition(ctx, task.ID, to, ""); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("to %q: err = %v, want ErrInvalidTransition", to, err)
		}
	}
	if _, err := store.Transition(ctx, "nope", StatusRunning, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("err = %v, want ErrTaskNotFound", err)
	}
}

func TestStatusPredicates(t *testing.T) {
	cases := map[Status][3]bool{ // terminal, active, needs owner
		StatusQueued:        {false, false, false},
		StatusRunning:       {false, true, false},
		StatusVerifying:     {false, true, false},
		StatusReviewReady:   {false, false, true},
		StatusNeedsDecision: {false, false, true},
		StatusDone:          {true, false, false},
		StatusFailed:        {true, false, false},
		StatusCancelled:     {true, false, false},
	}
	for status, want := range cases {
		got := [3]bool{status.IsTerminal(), status.IsActive(), status.NeedsOwner()}
		if got != want {
			t.Errorf("%s: terminal/active/needsOwner = %v, want %v", status, got, want)
		}
		if !status.Valid() {
			t.Errorf("%s must be valid", status)
		}
	}
	if Status("bogus").Valid() || Status("").Valid() {
		t.Error("unknown statuses must be invalid")
	}
}

func TestCreateTask(t *testing.T) {
	store := storeWithProject(t)
	task, err := store.CreateTask(ctx, NewTask{ProjectID: " app ", Goal: "  Fix the flaky login test  ", Model: " m ", Priority: 5})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{12}$`).MatchString(task.ID) {
		t.Errorf("ID = %q, want 12 hex characters", task.ID)
	}
	if task.ProjectID != "app" || task.Goal != "Fix the flaky login test" || task.Model != "m" || task.Priority != 5 {
		t.Errorf("task = %+v", task)
	}
	if task.Shape != ShapeShip || task.Source != (Source{Kind: SourceManual}) || task.Status != StatusQueued {
		t.Errorf("defaults: %+v", task)
	}
	if task.TrustMode != TrustLocalOnly {
		t.Errorf("TrustMode = %q, want the project's", task.TrustMode)
	}
	if task.StartedAt != nil || task.FinishedAt != nil || task.CreatedAt.IsZero() {
		t.Errorf("timestamps: %+v", task)
	}

	stored, err := store.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.CreatedAt, stored.UpdatedAt = task.CreatedAt, task.UpdatedAt
	if !reflect.DeepEqual(stored, task) {
		t.Fatalf("stored task =\n  %+v\nwant\n  %+v", stored, task)
	}

	events, _ := store.ListEvents(ctx, task.ID, 0, 0)
	if len(events) != 1 || events[0].Type != EventStatus {
		t.Fatalf("creation must record one status event: %+v", events)
	}
	var change StatusChange
	if err := json.Unmarshal(events[0].Payload, &change); err != nil || change != (StatusChange{To: StatusQueued, Reason: "created"}) {
		t.Fatalf("creation event = %+v, %v", change, err)
	}
}

func TestCreateTask_ScoutAndIssue(t *testing.T) {
	store := storeWithProject(t)
	scout := mustCreate(t, store, NewTask{Shape: ShapeScout, Goal: "Why is CI slow?"})
	if scout.Shape != ShapeScout {
		t.Errorf("shape = %q", scout.Shape)
	}
	issue := mustCreate(t, store, NewTask{Source: Source{Kind: SourceIssue, IssueNumber: 42}})
	if issue.Source != (Source{Kind: SourceIssue, IssueNumber: 42}) || issue.Goal != "" {
		t.Errorf("an issue task may omit the goal: %+v", issue)
	}
	stored, _ := store.GetTask(ctx, issue.ID)
	if stored.Source.IssueNumber != 42 || stored.Source.Kind != SourceIssue {
		t.Errorf("stored source = %+v", stored.Source)
	}
}

func TestCreateTask_Rejections(t *testing.T) {
	store := storeWithProject(t)
	cases := []struct {
		name string
		in   NewTask
		want error
	}{
		{"no project", NewTask{Goal: "x"}, ErrInvalidTask},
		{"unknown project", NewTask{ProjectID: "ghost", Goal: "x"}, ErrProjectNotFound},
		{"blank goal", NewTask{ProjectID: "app", Goal: "   "}, ErrInvalidTask},
		{"unknown shape", NewTask{ProjectID: "app", Goal: "x", Shape: "explore"}, ErrInvalidTask},
		{"unknown source", NewTask{ProjectID: "app", Goal: "x", Source: Source{Kind: "email"}}, ErrInvalidTask},
		{"issue without a number", NewTask{ProjectID: "app", Source: Source{Kind: SourceIssue}}, ErrInvalidTask},
		{"issue with a negative number", NewTask{ProjectID: "app", Source: Source{Kind: SourceIssue, IssueNumber: -1}}, ErrInvalidTask},
		{"manual with an issue number", NewTask{ProjectID: "app", Goal: "x", Source: Source{IssueNumber: 3}}, ErrInvalidTask},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.CreateTask(ctx, tc.in); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if n, _ := store.CountTasks(ctx, TaskFilter{}); n != 0 {
		t.Fatalf("rejected tasks were stored: %d", n)
	}
	if events, _ := store.ListEvents(ctx, "", 0, 0); len(events) != 0 {
		t.Fatalf("rejected tasks wrote events: %+v", events)
	}
}

// The trust mode is fixed at dispatch: changing the project later must not
// alter tasks that already exist.
func TestCreateTask_TrustModeIsSnapshotted(t *testing.T) {
	store := newStore(t)
	if _, err := store.RegisterProject(ctx, Project{ID: "app", RepoPath: newRepoWithRemote(t, "app")}); err != nil {
		t.Fatal(err)
	}
	before := mustCreate(t, store, NewTask{})
	if _, err := store.UpdateProject(ctx, "app", func(p *Project) error { p.TrustMode = TrustNoMistakes; return nil }); err != nil {
		t.Fatal(err)
	}
	after := mustCreate(t, store, NewTask{})

	if got, _ := store.GetTask(ctx, before.ID); got.TrustMode != TrustLocalOnly {
		t.Errorf("existing task TrustMode = %q, want it unchanged", got.TrustMode)
	}
	if after.TrustMode != TrustNoMistakes {
		t.Errorf("new task TrustMode = %q, want the updated mode", after.TrustMode)
	}
}

func TestTransition_Timestamps(t *testing.T) {
	store := storeWithProject(t)
	clock := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return clock }
	tick := func() time.Time { clock = clock.Add(time.Minute); return clock }

	task := mustCreate(t, store, NewTask{})
	created := clock

	startedAt := tick()
	running, _ := store.Transition(ctx, task.ID, StatusRunning, "slot free")
	if running.StartedAt == nil || !running.StartedAt.Equal(startedAt) || running.FinishedAt != nil {
		t.Fatalf("running: started=%v finished=%v", running.StartedAt, running.FinishedAt)
	}
	if !running.CreatedAt.Equal(created) || !running.UpdatedAt.Equal(startedAt) {
		t.Fatalf("running: created=%v updated=%v", running.CreatedAt, running.UpdatedAt)
	}

	// Going back to running after verification is the same run.
	tick()
	store.Transition(ctx, task.ID, StatusVerifying, "agent finished")
	tick()
	again, _ := store.Transition(ctx, task.ID, StatusRunning, "checks failed")
	if again.StartedAt == nil || !again.StartedAt.Equal(startedAt) {
		t.Fatalf("re-entering running must keep the run's start time: %v", again.StartedAt)
	}

	failedAt := tick()
	failed, _ := store.Transition(ctx, task.ID, StatusFailed, "agent died")
	if failed.FinishedAt == nil || !failed.FinishedAt.Equal(failedAt) || failed.StartedAt == nil {
		t.Fatalf("failed: started=%v finished=%v", failed.StartedAt, failed.FinishedAt)
	}

	// A retry is a new run: both marks are cleared until it starts again.
	tick()
	requeued, _ := store.Transition(ctx, task.ID, StatusQueued, "retry")
	if requeued.StartedAt != nil || requeued.FinishedAt != nil {
		t.Fatalf("requeued: started=%v finished=%v", requeued.StartedAt, requeued.FinishedAt)
	}
	secondStart := tick()
	second, _ := store.Transition(ctx, task.ID, StatusRunning, "slot free")
	if second.StartedAt == nil || !second.StartedAt.Equal(secondStart) {
		t.Fatalf("second run start = %v, want %v", second.StartedAt, secondStart)
	}

	tick()
	store.Transition(ctx, task.ID, StatusVerifying, "")
	doneAt := tick()
	done, _ := store.Transition(ctx, task.ID, StatusDone, "report written")
	if done.FinishedAt == nil || !done.FinishedAt.Equal(doneAt) || done.StatusReason != "report written" {
		t.Fatalf("done = %+v", done)
	}
}

func TestTransition_RecordsEvents(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})
	store.Transition(ctx, task.ID, StatusRunning, "slot free")
	store.Transition(ctx, task.ID, StatusNeedsDecision, "agent asked a question")
	store.Transition(ctx, task.ID, StatusRunning, "owner answered")

	events, err := store.ListEvents(ctx, task.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []StatusChange{
		{To: StatusQueued, Reason: "created"},
		{From: StatusQueued, To: StatusRunning, Reason: "slot free"},
		{From: StatusRunning, To: StatusNeedsDecision, Reason: "agent asked a question"},
		{From: StatusNeedsDecision, To: StatusRunning, Reason: "owner answered"},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %+v", events)
	}
	for i, ev := range events {
		var got StatusChange
		if err := json.Unmarshal(ev.Payload, &got); err != nil || got != want[i] || ev.Type != EventStatus || ev.TaskID != task.ID {
			t.Errorf("event %d = %+v (%s), want %+v", i, got, ev.Type, want[i])
		}
		if i > 0 && ev.ID <= events[i-1].ID {
			t.Errorf("event IDs must increase: %d after %d", ev.ID, events[i-1].ID)
		}
	}
}

// Two callers racing to move the same task: exactly one may win.
func TestTransition_ConcurrentCallersCannotBothWin(t *testing.T) {
	store := storeWithProject(t)
	for round := 0; round < 20; round++ {
		task := mustCreate(t, store, NewTask{})
		const callers = 8
		var wg sync.WaitGroup
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = store.Transition(ctx, task.ID, StatusRunning, "slot free")
			}(i)
		}
		wg.Wait()

		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrConflict):
			default:
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d callers started the task, want exactly 1", round, won)
		}
		if events, _ := store.ListEvents(ctx, task.ID, 0, 0); len(events) != 2 {
			t.Fatalf("round %d: %d events, want creation plus one transition", round, len(events))
		}
	}
}

func TestListAndCountTasks(t *testing.T) {
	store := storeWithProject(t)
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "other")}); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	store.now = func() time.Time { clock = clock.Add(time.Second); return clock }

	low := mustCreate(t, store, NewTask{Goal: "low"})
	urgent := mustCreate(t, store, NewTask{Goal: "urgent", Priority: 10})
	running := mustCreate(t, store, NewTask{Goal: "running"})
	elsewhere := mustCreate(t, store, NewTask{ProjectID: "other", Goal: "elsewhere"})
	late := mustCreate(t, store, NewTask{Goal: "late"})
	walk(t, store, running.ID, StatusRunning)

	ids := func(tasks []Task) []string {
		var out []string
		for _, task := range tasks {
			out = append(out, task.Goal)
		}
		return out
	}

	all, err := store.ListTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(all); !reflect.DeepEqual(got, []string{"late", "elsewhere", "running", "urgent", "low"}) {
		t.Errorf("newest first = %v", got)
	}

	queue, _ := store.ListTasks(ctx, TaskFilter{ProjectID: "app", Statuses: []Status{StatusQueued}, Order: QueueOrder})
	if got := ids(queue); !reflect.DeepEqual(got, []string{"urgent", "low", "late"}) {
		t.Errorf("queue order = %v, want priority first, then oldest", got)
	}

	limited, _ := store.ListTasks(ctx, TaskFilter{Limit: 2})
	if len(limited) != 2 {
		t.Errorf("limit: %v", ids(limited))
	}
	other, _ := store.ListTasks(ctx, TaskFilter{ProjectID: "other"})
	if len(other) != 1 || other[0].ID != elsewhere.ID {
		t.Errorf("project filter: %v", ids(other))
	}
	multi, _ := store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning, StatusDone}})
	if len(multi) != 1 || multi[0].ID != running.ID {
		t.Errorf("status filter: %v", ids(multi))
	}
	none, _ := store.ListTasks(ctx, TaskFilter{ProjectID: "ghost"})
	if none == nil || len(none) != 0 {
		t.Errorf("no match must be an empty, non-nil slice: %#v", none)
	}

	counts := map[string]int{}
	for name, f := range map[string]TaskFilter{
		"all":        {},
		"app":        {ProjectID: "app"},
		"app queued": {ProjectID: "app", Statuses: []Status{StatusQueued}},
		"active":     {Statuses: []Status{StatusRunning, StatusVerifying}},
		"limit":      {Limit: 1},
	} {
		n, err := store.CountTasks(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	want := map[string]int{"all": 5, "app": 4, "app queued": 3, "active": 1, "limit": 5}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("counts = %v, want %v", counts, want)
	}
	_, _ = low, urgent
	_ = late
}

func TestWorkspaceSessionAndUsage(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})

	if err := store.SetWorkspace(ctx, task.ID, "alice/"+task.ID, "/var/wt/app/"+task.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSession(ctx, task.ID, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUsage(ctx, task.ID, Usage{InputTokens: 100, OutputTokens: 20, CostUSD: 0.5}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUsage(ctx, task.ID, Usage{InputTokens: 1, OutputTokens: 2, CostUSD: 0.25}); err != nil {
		t.Fatal(err)
	}

	got, _ := store.GetTask(ctx, task.ID)
	if got.Branch != "alice/"+task.ID || got.WorktreePath != "/var/wt/app/"+task.ID || got.SessionID != "sess-1" {
		t.Errorf("workspace/session = %+v", got)
	}
	if got.InputTokens != 101 || got.OutputTokens != 22 || got.CostUSD != 0.75 {
		t.Errorf("usage = %d/%d/%v", got.InputTokens, got.OutputTokens, got.CostUSD)
	}
	if got.Status != StatusQueued {
		t.Errorf("runtime updates must not touch the status: %s", got.Status)
	}

	if err := store.AddUsage(ctx, task.ID, Usage{InputTokens: -1}); !errors.Is(err, ErrInvalidTask) {
		t.Errorf("negative usage: %v", err)
	}
	for name, err := range map[string]error{
		"SetWorkspace": store.SetWorkspace(ctx, "nope", "b", "p"),
		"SetSession":   store.SetSession(ctx, "nope", "s"),
		"AddUsage":     store.AddUsage(ctx, "nope", Usage{}),
	} {
		if !errors.Is(err, ErrTaskNotFound) {
			t.Errorf("%s on a missing task: %v", name, err)
		}
	}
}

func TestAddUsage_ConcurrentIncrementsAllCount(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})
	const callers, each = 10, 20
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				if err := store.AddUsage(ctx, task.ID, Usage{InputTokens: 3, OutputTokens: 1}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	got, _ := store.GetTask(ctx, task.ID)
	if got.InputTokens != 3*callers*each || got.OutputTokens != callers*each {
		t.Fatalf("usage = %d/%d, want %d/%d", got.InputTokens, got.OutputTokens, 3*callers*each, callers*each)
	}
}

func TestEvents_AppendAndRead(t *testing.T) {
	store := storeWithProject(t)
	a := mustCreate(t, store, NewTask{Goal: "a"})
	b := mustCreate(t, store, NewTask{Goal: "b"})

	type toolUse struct {
		Tool string `json:"tool"`
		Path string `json:"path"`
	}
	ev, err := store.AppendEvent(ctx, a.ID, "tool_use", toolUse{Tool: "Edit", Path: "main.go"})
	if err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ev.ID == 0 || ev.TaskID != a.ID || ev.Type != "tool_use" || string(ev.Payload) != `{"tool":"Edit","path":"main.go"}` {
		t.Fatalf("event = %+v", ev)
	}
	bare, err := store.AppendEvent(ctx, a.ID, "heartbeat", nil)
	if err != nil || bare.Payload != nil {
		t.Fatalf("event without a payload = %+v, %v", bare, err)
	}
	if _, err := store.AppendEvent(ctx, b.ID, "tool_use", toolUse{Tool: "Read"}); err != nil {
		t.Fatal(err)
	}

	forA, _ := store.ListEvents(ctx, a.ID, 0, 0)
	if len(forA) != 3 || forA[0].Type != EventStatus || forA[1].Type != "tool_use" || forA[2].Type != "heartbeat" {
		t.Fatalf("events for a = %+v", forA)
	}
	if forA[2].Payload != nil {
		t.Errorf("a stored event without a payload must read back as nil, got %s", forA[2].Payload)
	}

	// afterID resumes a reader; limit pages.
	resumed, _ := store.ListEvents(ctx, a.ID, forA[0].ID, 0)
	if len(resumed) != 2 || resumed[0].ID != forA[1].ID {
		t.Errorf("resume = %+v", resumed)
	}
	paged, _ := store.ListEvents(ctx, a.ID, 0, 1)
	if len(paged) != 1 || paged[0].ID != forA[0].ID {
		t.Errorf("page = %+v", paged)
	}

	// An empty task ID reads the whole fleet in write order.
	all, _ := store.ListEvents(ctx, "", 0, 0)
	if len(all) != 5 {
		t.Fatalf("all events = %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID <= all[i-1].ID {
			t.Fatalf("events out of order: %+v", all)
		}
	}
	if empty, _ := store.ListEvents(ctx, a.ID, all[len(all)-1].ID, 0); empty == nil || len(empty) != 0 {
		t.Errorf("nothing newer must be an empty, non-nil slice: %#v", empty)
	}

	if _, err := store.AppendEvent(ctx, "nope", "x", nil); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}
	if _, err := store.AppendEvent(ctx, a.ID, "  ", nil); !errors.Is(err, ErrInvalidTask) {
		t.Errorf("blank type: %v", err)
	}
	if _, err := store.AppendEvent(ctx, a.ID, "bad", func() {}); err == nil {
		t.Error("an unencodable payload must be rejected")
	}
}

// The event log is history: the database itself refuses to rewrite it.
func TestEvents_AreAppendOnly(t *testing.T) {
	store := storeWithProject(t)
	task := mustCreate(t, store, NewTask{})
	before, _ := store.ListEvents(ctx, task.ID, 0, 0)

	for name, stmt := range map[string]string{
		"update":     `UPDATE fleet_task_events SET type = 'tampered'`,
		"delete":     `DELETE FROM fleet_task_events`,
		"delete one": `DELETE FROM fleet_task_events WHERE task_id = '` + task.ID + `'`,
	} {
		if _, err := store.db.Exec(stmt); err == nil {
			t.Errorf("%s on the event log must fail", name)
		}
	}
	after, _ := store.ListEvents(ctx, task.ID, 0, 0)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the event log changed:\n  %+v\n  %+v", before, after)
	}
}

// fleet can live in a database another component already owns.
func TestNewStore_SharedDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "shared.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE someone_elses (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := NewStore(db); err != nil {
		t.Fatalf("preparing the schema twice must be harmless: %v", err)
	}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "app")}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Close must not close a database the store does not own: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO someone_elses DEFAULT VALUES`); err != nil {
		t.Fatalf("the other component's table must be untouched: %v", err)
	}
}

// The same race, but on a caller-owned database with a connection pool, where
// transitions really do run on separate connections.
func TestTransition_ConcurrentCallersOnPooledDatabase(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "pooled.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	store, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "app")}); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 20; round++ {
		task := mustCreate(t, store, NewTask{})
		const callers = 8
		var wg sync.WaitGroup
		errs := make([]error, callers)
		for i := 0; i < callers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = store.Transition(ctx, task.ID, StatusRunning, "slot free")
			}(i)
		}
		wg.Wait()

		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case errors.Is(err, ErrInvalidTransition), errors.Is(err, ErrConflict):
			default:
				t.Fatalf("round %d: unexpected error: %v", round, err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d callers started the task, want exactly 1", round, won)
		}
		got, _ := store.GetTask(ctx, task.ID)
		if got.Status != StatusRunning {
			t.Fatalf("round %d: status = %s", round, got.Status)
		}
		if events, _ := store.ListEvents(ctx, task.ID, 0, 0); len(events) != 2 {
			t.Fatalf("round %d: %d events, want creation plus one transition", round, len(events))
		}
	}
}
