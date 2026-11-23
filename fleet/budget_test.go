package fleet

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLimit(t *testing.T) {
	cases := []struct {
		name   string
		limit  Limit
		usage  Usage
		over   bool
		detail string
	}{
		{"no limit", Limit{}, Usage{InputTokens: 1 << 40, CostUSD: 1e9}, false, ""},
		{"under both", Limit{CostUSD: 5, Tokens: 1000}, Usage{InputTokens: 400, OutputTokens: 100, CostUSD: 1}, false, ""},
		{"cost reached exactly", Limit{CostUSD: 5}, Usage{CostUSD: 5}, true, "$5.00 spent of a $5.00 limit"},
		{"cost exceeded", Limit{CostUSD: 5}, Usage{CostUSD: 7.25}, true, "$7.25 spent of a $5.00 limit"},
		{"tokens count input plus output", Limit{Tokens: 1000}, Usage{InputTokens: 600, OutputTokens: 400}, true, "1000 tokens used of a 1000 limit"},
		{"only the limited dimension counts", Limit{Tokens: 1000}, Usage{InputTokens: 10, CostUSD: 999}, false, ""},
		{"cost is reported before tokens", Limit{CostUSD: 1, Tokens: 10}, Usage{InputTokens: 50, CostUSD: 2}, true, "$2.00 spent of a $1.00 limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail, over := tc.limit.exceededBy(tc.usage)
			if over != tc.over || detail != tc.detail {
				t.Fatalf("exceededBy = %q, %v; want %q, %v", detail, over, tc.detail, tc.over)
			}
		})
	}

	fallback := Limit{CostUSD: 9}
	if got := (Limit{}).or(fallback); got != fallback {
		t.Errorf("an empty limit must fall back, got %+v", got)
	}
	if got := (Limit{Tokens: 5}).or(fallback); got != (Limit{Tokens: 5}) {
		t.Errorf("a set limit must win whole, got %+v", got)
	}
	if !(Limit{}).IsZero() || (Limit{Tokens: 1}).IsZero() {
		t.Error("IsZero is wrong")
	}
}

func TestBudget_DayStart(t *testing.T) {
	taipei := time.FixedZone("UTC+8", 8*3600)
	// 2026-05-01 18:30 UTC is already 02:30 on the 2nd in UTC+8.
	now := time.Date(2026, 5, 1, 18, 30, 0, 0, time.UTC)

	utc := (&Budget{Location: time.UTC}).dayStart(now)
	if !utc.Equal(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("UTC day start = %s", utc)
	}
	local := (&Budget{Location: taipei}).dayStart(now)
	if !local.Equal(time.Date(2026, 5, 2, 0, 0, 0, 0, taipei)) {
		t.Errorf("UTC+8 day start = %s, want midnight of the 2nd there", local)
	}
}

func TestUsageLedger(t *testing.T) {
	store := storeWithProject(t)
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "other")}); err != nil {
		t.Fatal(err)
	}
	clock := &settableClock{now: time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)}
	store.now = clock.Now
	a := mustCreate(t, store, NewTask{})
	b := mustCreate(t, store, NewTask{ProjectID: "other"})
	add := func(id string, u Usage) {
		t.Helper()
		if err := store.AddUsage(ctx, id, u); err != nil {
			t.Fatal(err)
		}
	}

	add(a.ID, Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 1})
	clock.Advance(24 * time.Hour)
	midnight := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	add(a.ID, Usage{InputTokens: 200, OutputTokens: 20, CostUSD: 2})
	add(b.ID, Usage{InputTokens: 400, OutputTokens: 40, CostUSD: 4})

	check := func(name, project string, since time.Time, want Usage) {
		t.Helper()
		got, err := store.UsageSince(ctx, project, since)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s = %+v, want %+v", name, got, want)
		}
	}
	check("fleet, today", "", midnight, Usage{InputTokens: 600, OutputTokens: 60, CostUSD: 6})
	check("app, today", "app", midnight, Usage{InputTokens: 200, OutputTokens: 20, CostUSD: 2})
	check("other, today", "other", midnight, Usage{InputTokens: 400, OutputTokens: 40, CostUSD: 4})
	check("fleet, all time", "", time.Time{}, Usage{InputTokens: 700, OutputTokens: 70, CostUSD: 7})
	check("nothing yet", "", midnight.Add(48*time.Hour), Usage{})

	// The task row still carries the lifetime total.
	if got, _ := store.GetTask(ctx, a.ID); got.InputTokens != 300 || got.CostUSD != 3 {
		t.Errorf("task total = %d tokens, $%v", got.InputTokens, got.CostUSD)
	}
	if err := store.AddUsage(ctx, "missing", Usage{InputTokens: 1}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}
	check("a refused increment leaves no ledger row", "", time.Time{}, Usage{InputTokens: 700, OutputTokens: 70, CostUSD: 7})
}

func TestProjectBudget_StoredAndValidated(t *testing.T) {
	store := newStore(t)
	want := ProjectBudget{Task: Limit{CostUSD: 2.5}, Daily: Limit{CostUSD: 20, Tokens: 5_000_000}}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "app"), Budget: want}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetProject(ctx, "app"); got.Budget != want {
		t.Fatalf("stored budget = %+v, want %+v", got.Budget, want)
	}

	updated, err := store.UpdateProject(ctx, "app", func(p *Project) error { p.Budget.Task = Limit{}; return nil })
	if err != nil || updated.Budget.Task != (Limit{}) || updated.Budget.Daily != want.Daily {
		t.Fatalf("updated budget = %+v, %v", updated.Budget, err)
	}
	if listed, _ := store.ListProjects(ctx); listed[0].Budget.Daily != want.Daily {
		t.Errorf("listed budget = %+v", listed[0].Budget)
	}

	if _, err := store.UpdateProject(ctx, "app", func(p *Project) error { p.Budget.Daily.CostUSD = -1; return nil }); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("a negative budget: %v", err)
	}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "bad"), Budget: ProjectBudget{Task: Limit{Tokens: -5}}}); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("a negative budget at registration: %v", err)
	}
}

func TestRun_RefusedOverTaskBudget(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.Budget = &Budget{Task: Limit{CostUSD: 1.00}}
	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{})
	if err := e.store.AddUsage(ctx, task.ID, Usage{CostUSD: 1.20}); err != nil {
		t.Fatal(err)
	}

	_, err := e.runner.Run(ctx, task.ID, RunOptions{})
	var over *BudgetError
	if !errors.As(err, &over) || !errors.Is(err, ErrOverBudget) {
		t.Fatalf("err = %v, want a *BudgetError", err)
	}
	if over.Scope != BudgetScopeTask || over.TaskID != task.ID || over.Detail != "$1.20 spent of a $1.00 limit" {
		t.Errorf("BudgetError = %+v", over)
	}
	if over.Reason() != "over the task budget: $1.20 spent of a $1.00 limit" {
		t.Errorf("Reason = %q", over.Reason())
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusQueued {
		t.Fatalf("a refused run changed the task to %s", got.Status)
	}
	if e.invocations() != 0 || e.runner.Running(task.ID) {
		t.Fatal("no agent may start past the budget")
	}

	// The owner's explicit override runs it anyway.
	res, err := e.runner.Run(ctx, task.ID, RunOptions{IgnoreBudget: true})
	if err != nil || res.Outcome != OutcomeFinished {
		t.Fatalf("override: %+v, %v", res, err)
	}
}

func TestRun_BudgetScopes(t *testing.T) {
	cases := []struct {
		name      string
		budget    Budget
		project   ProjectBudget
		spendOn   string // which task's usage is recorded: "self", "sibling" (same project), "elsewhere"
		usage     Usage
		wantScope BudgetScope // "" means the run is allowed
	}{
		{"within every limit", Budget{Task: Limit{CostUSD: 5}, ProjectDaily: Limit{CostUSD: 5}, Daily: Limit{CostUSD: 5}},
			ProjectBudget{}, "self", Usage{CostUSD: 1}, ""},
		{"fleet default task limit", Budget{Task: Limit{Tokens: 1000}}, ProjectBudget{}, "self", Usage{InputTokens: 1000}, BudgetScopeTask},
		{"another task's usage does not count against this task", Budget{Task: Limit{CostUSD: 1}}, ProjectBudget{}, "sibling", Usage{CostUSD: 9}, ""},
		{"project's own task limit is stricter than the default", Budget{Task: Limit{CostUSD: 10}},
			ProjectBudget{Task: Limit{CostUSD: 1}}, "self", Usage{CostUSD: 2}, BudgetScopeTask},
		{"project's own task limit is looser than the default", Budget{Task: Limit{CostUSD: 1}},
			ProjectBudget{Task: Limit{CostUSD: 10}}, "self", Usage{CostUSD: 2}, ""},
		{"project daily limit counts every task of the project", Budget{ProjectDaily: Limit{CostUSD: 3}}, ProjectBudget{}, "sibling", Usage{CostUSD: 3}, BudgetScopeProject},
		{"project daily limit ignores other projects", Budget{ProjectDaily: Limit{CostUSD: 3}}, ProjectBudget{}, "elsewhere", Usage{CostUSD: 30}, ""},
		{"project's own daily limit overrides the default", Budget{ProjectDaily: Limit{CostUSD: 100}},
			ProjectBudget{Daily: Limit{CostUSD: 2}}, "sibling", Usage{CostUSD: 2}, BudgetScopeProject},
		{"fleet daily limit counts every project", Budget{Daily: Limit{CostUSD: 3}}, ProjectBudget{}, "elsewhere", Usage{CostUSD: 4}, BudgetScopeFleet},
		{"the narrowest scope is reported first", Budget{Task: Limit{CostUSD: 1}, Daily: Limit{CostUSD: 1}}, ProjectBudget{}, "self", Usage{CostUSD: 2}, BudgetScopeTask},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunnerEnv(t)
			e.addProject("other", 1)
			if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error { p.Budget = tc.project; return nil }); err != nil {
				t.Fatal(err)
			}
			budget := tc.budget
			e.runner.Budget = &budget
			e.script(lineInit, lineResultOK)

			task := e.task(NewTask{})
			spender := map[string]Task{
				"self":      task,
				"sibling":   e.task(NewTask{}),
				"elsewhere": e.task(NewTask{ProjectID: "other"}),
			}[tc.spendOn]
			if err := e.store.AddUsage(ctx, spender.ID, tc.usage); err != nil {
				t.Fatal(err)
			}

			_, err := e.runner.Run(ctx, task.ID, RunOptions{})
			var over *BudgetError
			switch {
			case tc.wantScope == "" && err != nil:
				t.Fatalf("the run was refused: %v", err)
			case tc.wantScope != "" && !errors.As(err, &over):
				t.Fatalf("err = %v, want a refusal for the %s scope", err, tc.wantScope)
			case tc.wantScope != "" && over.Scope != tc.wantScope:
				t.Fatalf("scope = %s (%s), want %s", over.Scope, over.Detail, tc.wantScope)
			}
		})
	}
}

// Yesterday's spending does not count against today.
func TestRun_DailyBudgetResetsAtMidnight(t *testing.T) {
	e := newRunnerEnv(t)
	clock := &settableClock{now: time.Date(2026, 5, 1, 23, 0, 0, 0, time.UTC)}
	e.store.now = clock.Now
	e.runner.Budget = &Budget{Daily: Limit{CostUSD: 1}, Location: time.UTC}
	e.script(lineInit, lineResultOK)

	spent := e.task(NewTask{})
	if err := e.store.AddUsage(ctx, spent.ID, Usage{CostUSD: 5}); err != nil {
		t.Fatal(err)
	}
	waiting := e.task(NewTask{})
	if _, err := e.runner.Run(ctx, waiting.ID, RunOptions{}); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("at 23:00 the day's budget is spent: %v", err)
	}

	clock.Advance(2 * time.Hour) // 01:00 the next day
	if res, err := e.runner.Run(ctx, waiting.ID, RunOptions{}); err != nil || res.Outcome != OutcomeFinished {
		t.Fatalf("after midnight the run must be allowed: %+v, %v", res, err)
	}
}

func TestScheduler_HoldsBackTasksOverBudget(t *testing.T) {
	e := newRunnerEnv(t)
	e.addProject("other", 1)
	clock := &settableClock{now: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)}
	e.store.now = clock.Now
	e.runner.Budget = &Budget{ProjectDaily: Limit{CostUSD: 2}, Location: time.UTC}
	e.script(lineInit, "#sleep 30")

	spender := e.task(NewTask{ProjectID: "app"})
	walk(t, e.store, spender.ID, StatusCancelled)
	if err := e.store.AddUsage(ctx, spender.ID, Usage{CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	heldA := e.task(NewTask{ProjectID: "app", Priority: 9})
	clock.Advance(time.Minute)
	heldB := e.task(NewTask{ProjectID: "app", Priority: 8})
	clock.Advance(time.Minute)
	free := e.task(NewTask{ProjectID: "other"})
	s := e.scheduler(5)

	started, err := s.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != free.ID {
		t.Fatalf("started = %v, want only the task of the project that still has budget", started)
	}
	for _, id := range []string{heldA.ID, heldB.ID} {
		if got, _ := e.store.GetTask(ctx, id); got.Status != StatusQueued {
			t.Errorf("a held task = %s, want it still queued", got.Status)
		}
	}
	e.waitForStart(free.ID)
	if _, err := s.Tick(ctx); err != nil {
		t.Fatal(err)
	}

	// One wake for the project's limit, not one per task or per pass.
	pending, _ := e.store.PendingWakes(ctx, 0)
	if len(pending) != 1 {
		t.Fatalf("pending wakes = %+v, want exactly one", summarize(pending))
	}
	w := pending[0]
	if w.Kind != WakeOverBudget || w.ProjectID != "app" || w.TaskID != "" ||
		w.Detail != "over the project's daily budget: $2.00 spent of a $2.00 limit" {
		t.Errorf("wake = %+v", w)
	}

	if err := e.runner.Cancel(free.ID, ""); err != nil {
		t.Fatal(err)
	}
	s.Wait()

	// The next day the held tasks start, in their queue order.
	clock.Advance(24 * time.Hour)
	started, _ = s.Tick(ctx)
	if len(started) != 1 || started[0] != heldA.ID {
		t.Fatalf("next day started = %v, want the higher-priority held task", started)
	}
	e.waitForStart(heldA.ID)
	if err := e.runner.Cancel(heldA.ID, ""); err != nil {
		t.Fatal(err)
	}
	s.Wait()
}

// The agent's own run can use up the task's budget. The retry that
// verification asks for must then not happen.
func TestRunTask_StopsRetryingWhenTheBudgetRunsOut(t *testing.T) {
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error {
		p.VerifyCommands = []string{"exit 1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// lineResultOK reports $0.25; the limit is reached by the first run.
	e.runner.Budget = &Budget{Task: Limit{CostUSD: 0.25}}
	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{})

	res, err := e.scheduler(1).RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.Task.Status != StatusNeedsDecision || res.Outcome != OutcomeNeedsDecision ||
		res.Task.StatusReason != "over the task budget: $0.25 spent of a $0.25 limit" {
		t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
	}
	if e.invocations() != 1 {
		t.Fatalf("agent invocations = %d; the retry must not have run", e.invocations())
	}
	pending, _ := e.store.PendingWakes(ctx, 0)
	if len(pending) != 1 || pending[0].Kind != WakeOverBudget || pending[0].TaskID != task.ID {
		t.Fatalf("wakes = %+v", summarize(pending))
	}
	// The failed check is on record even though it was not retried.
	if checks, _ := e.store.ListVerifications(ctx, task.ID); len(checks) != 1 || checks[0].Passed {
		t.Errorf("checks = %+v", checks)
	}
}

// When a daily limit is reached, runs already going are paused.
func TestWatcher_PausesRunningTasksAtTheDailyLimit(t *testing.T) {
	e := newRunnerEnv(t)
	e.setProjectLimit("app", 5)
	e.addProject("other", 5)
	e.runner.Budget = &Budget{ProjectDaily: Limit{CostUSD: 3}}
	e.script(lineInit, "#sleep 30")

	inScope1 := e.task(NewTask{ProjectID: "app"})
	inScope2 := e.task(NewTask{ProjectID: "app"})
	outOfScope := e.task(NewTask{ProjectID: "other"})
	outs := map[string]<-chan runOutcome{}
	for _, id := range []string{inScope1.ID, inScope2.ID, outOfScope.ID} {
		outs[id] = e.runAsync(ctx, id)
		e.waitForStart(id)
	}
	w := e.watcher()
	if wakes := e.scan(w); len(wakes) != 0 {
		t.Fatalf("under budget, the watcher raised %+v", summarize(wakes))
	}

	// A finished task of the same project reports its cost and uses up the day.
	done := e.task(NewTask{ProjectID: "app"})
	walk(t, e.store, done.ID, StatusCancelled)
	if err := e.store.AddUsage(ctx, done.ID, Usage{CostUSD: 3.50}); err != nil {
		t.Fatal(err)
	}

	wakes := e.scan(w)
	if len(wakes) != 1 || wakes[0].Kind != WakeOverBudget || wakes[0].ProjectID != "app" || wakes[0].TaskID != "" {
		t.Fatalf("wakes = %+v, want one project-wide budget wake", summarize(wakes))
	}
	for _, id := range []string{inScope1.ID, inScope2.ID} {
		res := awaitRun(t, outs[id])
		if res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Reason, "over the project's daily budget") {
			t.Errorf("paused task = %s (%q)", res.Task.Status, res.Reason)
		}
		if res.Task.SessionID == "" {
			t.Error("a paused task must keep its session so it can carry on later")
		}
	}
	if !e.runner.Running(outOfScope.ID) {
		t.Fatal("a task of another project must keep running")
	}

	// Pausing is reported once, as the budget wake, not again per task.
	if again := e.scan(w); len(again) != 0 {
		t.Fatalf("a later pass raised %+v", summarize(again))
	}
	if pending, _ := e.store.PendingWakes(ctx, 0); len(pending) != 1 {
		t.Fatalf("pending = %+v", summarize(pending))
	}

	if err := e.runner.Cancel(outOfScope.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, outs[outOfScope.ID])
}

func TestBudgetError(t *testing.T) {
	cases := []struct {
		err        *BudgetError
		wantError  string
		wantReason string
	}{
		{&BudgetError{Scope: BudgetScopeTask, TaskID: "t1", Detail: "d"}, "fleet: task t1 is over budget: d", "over the task budget: d"},
		{&BudgetError{Scope: BudgetScopeProject, ProjectID: "app", Detail: "d"}, "fleet: project app is over its daily budget: d", "over the project's daily budget: d"},
		{&BudgetError{Scope: BudgetScopeFleet, Detail: "d"}, "fleet: the daily budget is used up: d", "over the fleet's daily budget: d"},
	}
	for _, tc := range cases {
		if tc.err.Error() != tc.wantError || tc.err.Reason() != tc.wantReason {
			t.Errorf("%s: Error = %q, Reason = %q", tc.err.Scope, tc.err.Error(), tc.err.Reason())
		}
		if !errors.Is(tc.err, ErrOverBudget) {
			t.Errorf("%s must match ErrOverBudget", tc.err.Scope)
		}
	}
	// Without a Budget nothing is ever refused.
	var none *Budget
	if v := none.view(nil); v != nil {
		t.Fatal("a nil Budget must give a nil view")
	}
	var view *budgetView
	if over, err := view.admit(ctx, Task{}, Project{}); over != nil || err != nil {
		t.Fatalf("a nil view refused: %v, %v", over, err)
	}
}

// A projects table from before budgets existed must keep working.
func TestStore_AddsBudgetToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	repo := newRepo(t, "app")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE fleet_projects (
		id TEXT PRIMARY KEY, name TEXT NOT NULL, repo_path TEXT NOT NULL UNIQUE,
		remote TEXT NOT NULL DEFAULT '', default_branch TEXT NOT NULL, trust_mode TEXT NOT NULL,
		verify_commands TEXT NOT NULL DEFAULT '[]', max_concurrent INTEGER NOT NULL,
		branch_prefix TEXT NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO fleet_projects
		(id, name, repo_path, default_branch, trust_mode, max_concurrent, branch_prefix, created_at, updated_at)
		VALUES ('app', 'app', ?, 'main', 'local-only', 1, 'alice/', 1, 1)`, repo); err != nil {
		t.Fatal(err)
	}
	db.Close()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("opening an older database: %v", err)
	}
	defer store.Close()
	p, err := store.GetProject(ctx, "app")
	if err != nil || p.Budget != (ProjectBudget{}) {
		t.Fatalf("project = %+v, %v", p, err)
	}
	if _, err := store.UpdateProject(ctx, "app", func(p *Project) error { p.Budget.Daily = Limit{CostUSD: 5}; return nil }); err != nil {
		t.Fatalf("writing the new column: %v", err)
	}
}
