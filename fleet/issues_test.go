package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/chimerakang/alice-core/ghissue"
	"github.com/chimerakang/alice-core/issueops"
)

// fakeIssues is an in-memory issue tracker holding one issue.
type fakeIssues struct {
	mu         sync.Mutex
	title      string
	body       string
	state      string
	labels     []string
	comments   []string
	fetchErr   error
	commentErr error
	syncErr    error
	syncCalls  int
}

func (f *fakeIssues) Fetch(_ context.Context, _ string, number int) (*ghissue.IssueContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	state := f.state
	if state == "" {
		state = "OPEN"
	}
	return &ghissue.IssueContext{
		Number: number, Title: f.title, Body: f.body, State: state, Labels: f.labels,
		Checklist: ghissue.ExtractChecklist(f.body),
	}, nil
}

func (f *fakeIssues) Comment(_ context.Context, _ string, _ int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.commentErr != nil {
		return f.commentErr
	}
	f.comments = append(f.comments, body)
	return nil
}

func (f *fakeIssues) SyncChecklist(_ context.Context, req issueops.SyncChecklistRequest) (issueops.SyncChecklistResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	if f.syncErr != nil {
		return issueops.SyncChecklistResult{}, f.syncErr
	}
	var done []ghissue.CompletedWork
	for _, item := range req.SubTasks {
		if item.Done {
			done = append(done, ghissue.CompletedWork{Description: item.Description, ChecklistItemIDs: item.ChecklistItemIDs})
		}
	}
	preview := ghissue.BuildChecklistSyncPreview(f.body, done)
	if !preview.Changed {
		return issueops.SyncChecklistResult{Outcome: issueops.SyncOutcomeNoChange}, nil
	}
	f.body = preview.BodyAfter
	return issueops.SyncChecklistResult{Wrote: true, Outcome: issueops.SyncOutcomeWrote}, nil
}

func (f *fakeIssues) snapshot() (body string, comments []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body, append([]string(nil), f.comments...)
}

const issueBody = `Uploads fail intermittently.

## 驗收條件

- [x] 重現問題
- [ ] 修正上傳重試
- [ ] 補上測試

## 子議題

- [ ] #99 另一個議題`

// Item IDs follow 0-based line numbers: 重現問題 = item-4, 修正上傳重試 = item-5, 補上測試 = item-6.

type issueEnv struct {
	*runnerEnv
	issues  *fakeIssues
	tracker *IssueTracker
	sched   *Scheduler
}

func newIssueEnv(t *testing.T, verifyCommands ...string) *issueEnv {
	t.Helper()
	e := newRunnerEnv(t)
	if _, err := e.store.UpdateProject(ctx, "app", func(p *Project) error { p.VerifyCommands = verifyCommands; return nil }); err != nil {
		t.Fatal(err)
	}
	issues := &fakeIssues{title: "修好不穩定的上傳", body: issueBody}
	tracker := &IssueTracker{Store: e.store, Host: issues}
	sched := e.scheduler(1)
	sched.Issues = tracker
	return &issueEnv{runnerEnv: e, issues: issues, tracker: tracker, sched: sched}
}

// resultSaying builds the agent's final stream line with the given message.
func resultSaying(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": text, "session_id": "sess-1",
		"total_cost_usd": 0.5, "usage": map[string]int{"input_tokens": 900, "output_tokens": 100},
	})
	return string(b)
}

func (e *issueEnv) dispatch() Task {
	e.t.Helper()
	task, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{})
	if err != nil {
		e.t.Fatalf("Dispatch: %v", err)
	}
	return task
}

func (e *issueEnv) runTask(id string, opts RunOptions) RunResult {
	e.t.Helper()
	res, err := e.sched.RunTask(ctx, id, opts)
	if err != nil {
		e.t.Fatalf("RunTask: %v", err)
	}
	return res
}

func TestDispatch_BuildsTheGoalFromTheIssue(t *testing.T) {
	e := newIssueEnv(t)
	task, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{Model: "m", Priority: 3})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if task.Source != (Source{Kind: SourceIssue, IssueNumber: 42}) || task.Status != StatusQueued ||
		task.Model != "m" || task.Priority != 3 || task.Shape != ShapeShip {
		t.Fatalf("task = %+v", task)
	}
	for _, want := range []string{
		"Work on GitHub issue #42: 修好不穩定的上傳",
		"Uploads fail intermittently.",
		"- [item-5] 修正上傳重試",
		"- [item-6] 補上測試",
		"Completed items: item-5, item-6",
		`write "Completed items: none"`,
		"Do not edit the issue yourself",
	} {
		if !strings.Contains(task.Goal, want) {
			t.Errorf("goal is missing %q:\n%s", want, task.Goal)
		}
	}
	// Already-checked items and items outside the acceptance section are not the task's to do.
	for _, unwanted := range []string{"[item-4]", "[item-10]"} {
		if strings.Contains(task.Goal, unwanted) {
			t.Errorf("goal must not offer %s as an open item:\n%s", unwanted, task.Goal)
		}
	}

	linked := e.eventPayloads(task.ID, EventIssueLinked)
	if len(linked) != 1 || linked[0]["number"] != float64(42) || linked[0]["title"] != "修好不穩定的上傳" {
		t.Fatalf("issue.linked = %v", linked)
	}
	if items := linked[0]["items"].([]any); len(items) != 2 || items[0].(map[string]any)["id"] != "item-5" {
		t.Errorf("linked items = %v", items)
	}
}

func TestDispatch_IssueWithoutOpenItems(t *testing.T) {
	e := newIssueEnv(t)
	e.issues.body = "Just a description, no checklist."
	task := e.dispatch()
	if strings.Contains(task.Goal, "Completed items") || !strings.Contains(task.Goal, "Just a description") {
		t.Fatalf("goal = %q", task.Goal)
	}
}

func TestDispatch_Refusals(t *testing.T) {
	e := newIssueEnv(t)

	if _, err := e.tracker.Dispatch(ctx, "app", 0, DispatchOptions{}); !errors.Is(err, ErrInvalidTask) {
		t.Errorf("issue number 0: %v", err)
	}
	if _, err := e.tracker.Dispatch(ctx, "ghost", 42, DispatchOptions{}); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("unknown project: %v", err)
	}
	if _, err := (&IssueTracker{}).Dispatch(ctx, "app", 42, DispatchOptions{}); err == nil {
		t.Error("a tracker without a Store must refuse")
	}

	e.issues.fetchErr = errors.New("HTTP 404")
	if _, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{}); err == nil || !strings.Contains(err.Error(), "load issue #42: HTTP 404") {
		t.Errorf("unreadable issue: %v", err)
	}
	e.issues.fetchErr = nil

	e.issues.state = "CLOSED"
	if _, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{}); !errors.Is(err, ErrIssueClosed) {
		t.Errorf("closed issue: %v", err)
	}
	e.issues.state = "OPEN"
	if n, _ := e.store.CountTasks(ctx, TaskFilter{}); n != 0 {
		t.Fatalf("refused dispatches created %d tasks", n)
	}

	// One unfinished task per issue.
	first := e.dispatch()
	if _, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{}); !errors.Is(err, ErrIssueBusy) || !strings.Contains(err.Error(), first.ID) {
		t.Errorf("second dispatch while the first is unfinished: %v", err)
	}
	if _, err := e.tracker.Dispatch(ctx, "app", 43, DispatchOptions{}); err != nil {
		t.Errorf("a different issue must be allowed: %v", err)
	}
	// Once the first is finished, the issue can be dispatched again.
	walk(t, e.store, first.ID, StatusCancelled)
	if _, err := e.tracker.Dispatch(ctx, "app", 42, DispatchOptions{}); err != nil {
		t.Errorf("dispatch after the earlier task finished: %v", err)
	}
}

func TestParseCompletedItems(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		want     []string
		declared bool
	}{
		{"no declaration", "All done, tests pass.", nil, false},
		{"plain", "Fixed it.\n\nCompleted items: item-5, item-6", []string{"item-5", "item-6"}, true},
		{"none", "Could not finish.\nCompleted items: none", []string{}, true},
		{"markdown decoration", "**Completed items:** `item-5`, `item-6`.", []string{"item-5", "item-6"}, true},
		{"case and spacing", "  completed ITEMS:   ITEM-5 item-6  ", []string{"item-5", "item-6"}, true},
		{"bracketed", "Completed items: [item-5] [item-6]", []string{"item-5", "item-6"}, true},
		{"duplicates collapse", "Completed items: item-5, item-5", []string{"item-5"}, true},
		{"a later line corrects an earlier one", "Completed items: item-5, item-6\nActually:\nCompleted items: item-5", []string{"item-5"}, true},
		{"words that are not IDs are ignored", "Completed items: item-5 and the tests", []string{"item-5"}, true},
		{"must be on its own line", "I think the completed items: are item-5", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, declared := ParseCompletedItems(tc.text)
			if declared != tc.declared || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseCompletedItems = %q, %v; want %q, %v", got, declared, tc.want, tc.declared)
			}
		})
	}
}

// The full path: dispatched from an issue, run, verified, and the issue told.
func TestIssueTask_VerifiedWorkTicksDeclaredItems(t *testing.T) {
	e := newIssueEnv(t, "true")
	task := e.dispatch()
	e.script(lineInit, resultSaying("Reworked the retry loop.\n\nCompleted items: item-5"))

	res := e.runTask(task.ID, RunOptions{})
	if res.Task.Status != StatusReviewReady {
		t.Fatalf("task = %s (%q)", res.Task.Status, res.Task.StatusReason)
	}

	body, comments := e.issues.snapshot()
	if !strings.Contains(body, "- [x] 修正上傳重試") {
		t.Errorf("the declared, verified item must be ticked:\n%s", body)
	}
	if !strings.Contains(body, "- [ ] 補上測試") || !strings.Contains(body, "- [ ] #99 另一個議題") {
		t.Errorf("undeclared items must stay open:\n%s", body)
	}

	if len(comments) != 2 {
		t.Fatalf("comments = %q, want a start and a finish", comments)
	}
	start, finish := comments[0], comments[1]
	if !strings.Contains(start, "Alice 開始處理") || !strings.Contains(start, task.ID) ||
		!strings.Contains(start, "alice/"+task.ID) || !strings.Contains(start, "local-only") {
		t.Errorf("start comment = %q", start)
	}
	for _, want := range []string{
		"✅ **Alice 完成**", task.ID, "等待你審核", "1000 tokens，$0.50",
		"**驗收**", "✅ `true`（exit 0",
		"**已勾選的項目**\n- 修正上傳重試",
		"**仍未完成的項目**\n- 補上測試",
	} {
		if !strings.Contains(finish, want) {
			t.Errorf("finish comment is missing %q:\n%s", want, finish)
		}
	}
	if synced := e.eventPayloads(task.ID, EventIssueSynced); len(synced) != 1 {
		t.Errorf("issue.synced = %v", synced)
	}
}

func TestIssueTask_NothingIsTickedWithoutBothKeys(t *testing.T) {
	cases := []struct {
		name     string
		verify   []string
		says     string
		wantNote string
	}{
		{"no verification commands", nil, "Done.\nCompleted items: item-5", "專案沒有設定驗收指令"},
		{"agent made no declaration", []string{"true"}, "Done, everything works.", "Agent 沒有回報完成了哪些項目"},
		{"agent declared none", []string{"true"}, "Blocked.\nCompleted items: none", "Agent 回報沒有完整完成任何項目"},
		{"agent declared an item it was not given", []string{"true"}, "Completed items: item-4, item-10", "已找不到"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newIssueEnv(t, tc.verify...)
			task := e.dispatch()
			e.script(lineInit, resultSaying(tc.says))
			e.runTask(task.ID, RunOptions{})

			body, comments := e.issues.snapshot()
			if body != issueBody {
				t.Fatalf("the checklist was changed:\n%s", body)
			}
			finish := comments[len(comments)-1]
			if !strings.Contains(finish, tc.wantNote) {
				t.Errorf("finish comment must explain why nothing was ticked (%q):\n%s", tc.wantNote, finish)
			}
			if strings.Contains(finish, "已勾選的項目") {
				t.Errorf("finish comment claims items were ticked:\n%s", finish)
			}
			if !strings.Contains(finish, "- 修正上傳重試") || !strings.Contains(finish, "- 補上測試") {
				t.Errorf("finish comment must list what is still open:\n%s", finish)
			}
		})
	}
}

// The agent says it finished an item, but the check fails: nothing is ticked.
func TestIssueTask_FailedVerificationTicksNothing(t *testing.T) {
	e := newIssueEnv(t, "exit 1")
	e.sched.Verifier = &Verifier{Store: e.store, MaxRetries: -1}
	task := e.dispatch()
	e.script(lineInit, resultSaying("All fixed!\nCompleted items: item-5, item-6"))

	res := e.runTask(task.ID, RunOptions{})
	if res.Task.Status != StatusNeedsDecision {
		t.Fatalf("task = %s", res.Task.Status)
	}
	body, comments := e.issues.snapshot()
	if body != issueBody {
		t.Fatalf("an unverified claim ticked the checklist:\n%s", body)
	}
	last := comments[len(comments)-1]
	if !strings.Contains(last, "⚠️ **Alice 需要你決定**") || !strings.Contains(last, "verification failed") {
		t.Errorf("problem comment = %q", last)
	}
	if e.issues.syncCalls != 0 {
		t.Error("the checklist must not even be attempted")
	}
}

// Item IDs are line numbers. If the issue is edited after dispatch, the
// right item must still be found.
func TestIssueTask_IssueEditedAfterDispatch(t *testing.T) {
	e := newIssueEnv(t, "true")
	task := e.dispatch()
	e.issues.mu.Lock()
	e.issues.body = "> 更新：優先處理這個\n\n新增的說明段落。\n\n" + issueBody // every line moves down by four
	e.issues.mu.Unlock()
	e.script(lineInit, resultSaying("Completed items: item-6")) // 補上測試, by its ID at dispatch

	e.runTask(task.ID, RunOptions{})
	body, _ := e.issues.snapshot()
	if !strings.Contains(body, "- [x] 補上測試") {
		t.Errorf("the declared item must be ticked despite the edit:\n%s", body)
	}
	if !strings.Contains(body, "- [ ] 修正上傳重試") || strings.Count(body, "- [x]") != 2 {
		t.Errorf("exactly the declared item must be ticked:\n%s", body)
	}
}

// A declaration from the first run still counts after a verification retry
// whose own message does not repeat it.
func TestIssueTask_DeclarationSurvivesAVerificationRetry(t *testing.T) {
	e := newIssueEnv(t, "test -f fixed.txt")
	task := e.dispatch()
	e.scriptFor(1, lineInit, resultSaying("Done.\nCompleted items: item-5"))
	e.scriptFor(2, lineInit, "#touch fixed.txt", resultSaying("Fixed the failing check."))

	res := e.runTask(task.ID, RunOptions{})
	if res.Task.Status != StatusReviewReady || e.invocations() != 2 {
		t.Fatalf("task = %s after %d runs", res.Task.Status, e.invocations())
	}
	body, comments := e.issues.snapshot()
	if !strings.Contains(body, "- [x] 修正上傳重試") {
		t.Errorf("the earlier declaration must be honoured once verification passes:\n%s", body)
	}
	if len(comments) != 2 {
		t.Errorf("comments = %d, want one start and one finish, not one per run", len(comments))
	}
}

func TestIssueTask_StartCommentOnlyOnAFreshRun(t *testing.T) {
	e := newIssueEnv(t, "true")
	task := e.dispatch()
	e.script(lineInit, lineResultAsk)
	e.runTask(task.ID, RunOptions{})

	_, comments := e.issues.snapshot()
	if len(comments) != 2 || !strings.Contains(comments[0], "開始處理") || !strings.Contains(comments[1], "需要你決定") {
		t.Fatalf("comments after the question = %q", comments)
	}
	if !strings.Contains(comments[1], "agent is waiting for an answer") {
		t.Errorf("the issue must be told why the task stopped: %q", comments[1])
	}

	// The owner answers; this is a continuation, not a new start.
	e.script(lineInit, resultSaying("Completed items: item-5, item-6"))
	e.runTask(task.ID, RunOptions{Instruction: "option B"})
	body, comments := e.issues.snapshot()
	if len(comments) != 3 || strings.Contains(comments[2], "開始處理") || !strings.Contains(comments[2], "Alice 完成") {
		t.Fatalf("comments after the answer = %q", comments)
	}
	if strings.Count(body, "- [x]") != 3 {
		t.Errorf("both declared items must now be ticked:\n%s", body)
	}
}

// An unreachable issue tracker must never fail the work itself.
func TestIssueTask_TrackerFailuresDoNotAffectTheTask(t *testing.T) {
	e := newIssueEnv(t, "true")
	task := e.dispatch()
	e.issues.commentErr = errors.New("HTTP 502")
	e.issues.syncErr = errors.New("HTTP 502")
	e.script(lineInit, resultSaying("Completed items: item-5"))

	res := e.runTask(task.ID, RunOptions{})
	if res.Task.Status != StatusReviewReady {
		t.Fatalf("task = %s; a tracker outage must not change the outcome", res.Task.Status)
	}
	var ops []string
	for _, p := range e.eventPayloads(task.ID, EventIssueError) {
		ops = append(ops, p["op"].(string))
	}
	if !reflect.DeepEqual(ops, []string{"comment", "sync_checklist", "comment"}) {
		t.Errorf("issue.error ops = %v", ops)
	}
	if body, _ := e.issues.snapshot(); body != issueBody {
		t.Error("the checklist changed despite the failed sync")
	}
}

func TestIssueTask_Switches(t *testing.T) {
	t.Run("NoComments still syncs the checklist", func(t *testing.T) {
		e := newIssueEnv(t, "true")
		e.tracker.NoComments = true
		task := e.dispatch()
		e.script(lineInit, resultSaying("Completed items: item-5"))
		e.runTask(task.ID, RunOptions{})
		body, comments := e.issues.snapshot()
		if len(comments) != 0 || !strings.Contains(body, "- [x] 修正上傳重試") {
			t.Fatalf("comments = %d, body:\n%s", len(comments), body)
		}
	})
	t.Run("NoChecklistSync still comments", func(t *testing.T) {
		e := newIssueEnv(t, "true")
		e.tracker.NoChecklistSync = true
		task := e.dispatch()
		e.script(lineInit, resultSaying("Completed items: item-5"))
		e.runTask(task.ID, RunOptions{})
		body, comments := e.issues.snapshot()
		if body != issueBody || len(comments) != 2 || e.issues.syncCalls != 0 {
			t.Fatalf("comments = %d, sync calls = %d, body:\n%s", len(comments), e.issues.syncCalls, body)
		}
	})
}

// Tasks that did not come from an issue are none of the tracker's business.
func TestIssueTracker_IgnoresOtherTasks(t *testing.T) {
	e := newIssueEnv(t, "true")
	e.script(lineInit, resultSaying("Completed items: item-5"))
	task := e.task(NewTask{Goal: "a manual task"})
	e.runTask(task.ID, RunOptions{})

	body, comments := e.issues.snapshot()
	if len(comments) != 0 || body != issueBody || e.issues.syncCalls != 0 {
		t.Fatalf("a manual task reached the issue tracker: %d comments, %d sync calls", len(comments), e.issues.syncCalls)
	}
	if len(e.eventPayloads(task.ID, EventIssueDeclared)) != 0 {
		t.Error("no declaration may be recorded for a manual task")
	}
	var none *IssueTracker
	none.Started(ctx, task)
	none.NoteResult(ctx, task, "Completed items: item-1")
	none.Finished(ctx, task) // a nil tracker is a no-op, not a panic
}

func TestRun_OnStartSeesTheWorkspace(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{})
	var seen Task
	calls := 0
	res := e.run(task.ID, RunOptions{OnStart: func(t Task) { seen = t; calls++ }})
	if calls != 1 || seen.ID != task.ID || seen.Branch != "alice/"+task.ID || seen.WorktreePath != res.Task.WorktreePath {
		t.Fatalf("OnStart saw %+v (%d calls)", seen, calls)
	}

	// Not called when the run never starts.
	e.runner.Binary = "/no/such/agent"
	other := e.task(NewTask{})
	e.run(other.ID, RunOptions{OnStart: func(Task) { calls++ }})
	if calls != 1 {
		t.Fatalf("OnStart was called for a run that never started")
	}
}
