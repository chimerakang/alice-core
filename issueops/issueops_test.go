package issueops

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/chimerakang/alice-core/ghissue"
)

func issueWith(body string) *ghissue.IssueContext {
	return &ghissue.IssueContext{Number: 1, State: "OPEN", Body: body, Checklist: ghissue.ExtractChecklist(body)}
}

func TestBuildChecklistMapping_EarlyStates(t *testing.T) {
	svc := New()

	nilIssue := svc.BuildChecklistMapping(nil, []WorkItem{{Description: "x"}})
	if nilIssue.State != ghissue.IssueStateBlocked || !nilIssue.NeedsHumanConfirmation {
		t.Errorf("nil issue = %+v", nilIssue)
	}

	noChecklist := svc.BuildChecklistMapping(issueWith("just prose"), []WorkItem{{Description: "x"}})
	if noChecklist.State != ghissue.IssueStateDrafted || noChecklist.TotalChecklistItems != 0 {
		t.Errorf("no checklist = %+v", noChecklist)
	}

	noWork := svc.BuildChecklistMapping(issueWith("- [ ] a\n- [ ] b"), nil)
	if noWork.State != ghissue.IssueStatePlanned || len(noWork.UnmappedChecklistItems) != 2 {
		t.Errorf("no work items = %+v", noWork)
	}
}

func TestBuildChecklistMapping_FuzzyHighConfidence(t *testing.T) {
	issue := issueWith("## 驗收條件\n- [ ] Add worktree package\n- [ ] Write integration tests")
	res := New().BuildChecklistMapping(issue, []WorkItem{
		{ID: "s1", Description: "Write integration tests"},
		{ID: "s2", Description: "add worktree package."},
	})
	if res.State != ghissue.IssueStateChecklistSynced || !res.ChecklistSynced || res.NeedsHumanConfirmation {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Mappings) != 2 || len(res.UnmappedSubTasks) != 0 || len(res.UnmappedChecklistItems) != 0 {
		t.Fatalf("mappings = %+v", res.Mappings)
	}
	first := res.Mappings[0]
	if first.SubTaskID != "s1" || first.SubTaskIndex != 0 || first.ChecklistIndex != 1 ||
		first.ChecklistText != "Write integration tests" || first.ChecklistLineNumber != 2 ||
		first.Score != 100 || first.Confidence != ChecklistMappingConfidenceHigh || first.RequiresHumanConfirmation {
		t.Errorf("first mapping = %+v", first)
	}
	if res.Mappings[1].ChecklistIndex != 0 || res.Mappings[1].Score != 100 {
		t.Errorf("punctuation and case must not affect matching: %+v", res.Mappings[1])
	}
}

func TestBuildChecklistMapping_LowConfidenceBlocks(t *testing.T) {
	issue := issueWith("- [ ] Add retry logic to the uploader\n- [ ] Document the uploader")
	res := New().BuildChecklistMapping(issue, []WorkItem{{ID: "s1", Description: "Refactor the uploader module"}})
	if res.State != ghissue.IssueStateBlocked || !res.NeedsHumanConfirmation {
		t.Fatalf("an ambiguous mapping must block: %+v", res)
	}
	if len(res.Mappings) != 1 || res.Mappings[0].Confidence != ChecklistMappingConfidenceLow ||
		!res.Mappings[0].RequiresHumanConfirmation {
		t.Fatalf("mappings = %+v", res.Mappings)
	}
	if len(res.Notes) == 0 || !strings.Contains(res.Notes[0], "low confidence checklist mapping") {
		t.Errorf("notes = %q", res.Notes)
	}
}

func TestBuildChecklistMapping_UnmappedOnBothSides(t *testing.T) {
	issue := issueWith("- [ ] Add worktree package\n- [ ] Update README")
	res := New().BuildChecklistMapping(issue, []WorkItem{
		{ID: "s1", Description: "Add worktree package"},
		{ID: "s2", Description: "完全無關的工作"},
	})
	if res.State != ghissue.IssueStateChecklistUnsynced || res.ChecklistSynced {
		t.Fatalf("result = %+v", res)
	}
	if len(res.UnmappedSubTasks) != 1 || res.UnmappedSubTasks[0].ID != "s2" {
		t.Errorf("UnmappedSubTasks = %+v", res.UnmappedSubTasks)
	}
	if len(res.UnmappedChecklistItems) != 1 || res.UnmappedChecklistItems[0].Text != "Update README" {
		t.Errorf("UnmappedChecklistItems = %+v", res.UnmappedChecklistItems)
	}
}

func TestBuildChecklistMapping_Declared(t *testing.T) {
	issue := issueWith("- [ ] alpha\n- [ ] beta\n- [ ] gamma") // item-0, item-1, item-2

	t.Run("full coverage is synced regardless of description", func(t *testing.T) {
		res := New().BuildChecklistMapping(issue, []WorkItem{
			{ID: "s1", Description: "unrelated words", ChecklistItemIDs: []string{"item-0", " item-2 "}},
			{ID: "s2", Description: "more unrelated", ChecklistItemIDs: []string{"item-1", ""}},
		})
		if res.State != ghissue.IssueStateChecklistSynced || !res.ChecklistSynced || len(res.Mappings) != 3 {
			t.Fatalf("result = %+v", res)
		}
		for _, m := range res.Mappings {
			if m.Score != 100 || m.Confidence != ChecklistMappingConfidenceHigh || m.RequiresHumanConfirmation {
				t.Errorf("declared mapping must be high confidence: %+v", m)
			}
		}
	})

	t.Run("partial coverage is unsynced", func(t *testing.T) {
		res := New().BuildChecklistMapping(issue, []WorkItem{{ID: "s1", ChecklistItemIDs: []string{"item-1"}}})
		if res.State != ghissue.IssueStateChecklistUnsynced || len(res.UnmappedChecklistItems) != 2 {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("an unknown ID blocks", func(t *testing.T) {
		res := New().BuildChecklistMapping(issue, []WorkItem{{ID: "s1", Description: "d", ChecklistItemIDs: []string{"item-9"}}})
		if res.State != ghissue.IssueStateBlocked || !res.NeedsHumanConfirmation || len(res.Mappings) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("an ID claimed twice blocks", func(t *testing.T) {
		res := New().BuildChecklistMapping(issue, []WorkItem{
			{ID: "s1", ChecklistItemIDs: []string{"item-0"}},
			{ID: "s2", ChecklistItemIDs: []string{"item-0"}},
		})
		if res.State != ghissue.IssueStateBlocked || len(res.Mappings) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestScoreChecklistMapping(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"Add tests", "add tests!", 100},
		{"Add tests for the auth package", "tests for the auth", 95},
		{"add retry logic", "add retry tests", 66},
		{"alpha beta", "gamma delta", 0},
		{"", "anything", 0},
		{"!!!", "anything", 0},
		{"新增 測試", "新增 文件", 50},
	}
	for _, tc := range cases {
		if got, _ := scoreChecklistMapping(tc.left, tc.right); got != tc.want {
			t.Errorf("scoreChecklistMapping(%q, %q) = %d, want %d", tc.left, tc.right, got, tc.want)
		}
	}
}

func TestCompletedWorkAndPlanEntries(t *testing.T) {
	items := []WorkItem{
		{ID: "s1", Description: "done one", Done: true, ChecklistItemIDs: []string{"item-0"}},
		{ID: "s2", Description: "not done"},
		{ID: "s3", Description: "done two", Done: true},
	}
	wantDone := []ghissue.CompletedWork{
		{Description: "done one", ChecklistItemIDs: []string{"item-0"}},
		{Description: "done two"},
	}
	if got := completedWork(items); !reflect.DeepEqual(got, wantDone) {
		t.Errorf("completedWork = %+v, want %+v", got, wantDone)
	}
	if got := planEntries(items); len(got) != 3 || got[1].Description != "not done" {
		t.Errorf("planEntries must include every item: %+v", got)
	}
}

func TestBuildEvidenceRecords(t *testing.T) {
	req := RecordEvidenceRequest{
		Index: 1, Total: 3,
		SubTask:   WorkItem{ID: "s2", Description: "Add tests"},
		Result:    "  all green  ",
		Tokens:    1200,
		Completed: 2,
		Review:    &ReviewEvidence{Verdict: "pass", Score: 92, Summary: "solid", Detail: "no issues", Reference: "review:1"},
		Validation: &ValidationEvidence{
			Command: "go test ./...", Passed: true, Output: "ok\n", Reference: "ci:42",
		},
	}
	records := buildEvidenceRecords(req)
	if len(records) != 3 {
		t.Fatalf("records = %+v", records)
	}
	if records[0] != (EvidenceRecord{Kind: EvidenceKindSubTask, Summary: "Add tests",
		Detail: "result=all green, tokens=1200, completed=2", Reference: "subtask:s2"}) {
		t.Errorf("sub-task record = %+v", records[0])
	}
	if records[1].Kind != EvidenceKindReview || records[1].Summary != "pass score=92" || records[1].Reference != "review:1" {
		t.Errorf("review record = %+v", records[1])
	}
	if records[2] != (EvidenceRecord{Kind: EvidenceKindValidation, Summary: "go test ./...",
		Detail: "passed exit=0 | ok", Reference: "ci:42"}) {
		t.Errorf("validation record = %+v", records[2])
	}

	failed := buildEvidenceRecords(RecordEvidenceRequest{Validation: &ValidationEvidence{ExitCode: 2}})
	if len(failed) != 1 || failed[0].Summary != "validation" || failed[0].Detail != "failed exit=2" {
		t.Errorf("failed validation = %+v", failed)
	}

	noID := buildEvidenceRecords(RecordEvidenceRequest{Index: 0, Total: 4, Result: "x"})
	if noID[0].Summary != "sub-task 1/4" || noID[0].Reference != "subtask:1/4" {
		t.Errorf("an item without ID or description must fall back to its position: %+v", noID[0])
	}

	explicit := []EvidenceRecord{{Kind: EvidenceKindReview, Summary: "given"}}
	if got := buildEvidenceRecords(RecordEvidenceRequest{Evidence: explicit, SubTask: WorkItem{Description: "ignored"}}); !reflect.DeepEqual(got, explicit) {
		t.Errorf("explicit evidence must be used as-is: %+v", got)
	}
	if got := buildEvidenceRecords(RecordEvidenceRequest{}); len(got) != 0 {
		t.Errorf("an empty request yields no records: %+v", got)
	}
}

func TestBuildEvidenceComment(t *testing.T) {
	body := buildEvidenceComment(RecordEvidenceRequest{
		Index: 0, Total: 2, SubTask: WorkItem{ID: "s1", Description: "Add tests"}, Result: "green",
		Validation: &ValidationEvidence{Command: "go test ./...", Passed: true},
	})
	want := "Issue evidence:\n- subtask: Add tests [subtask:s1]\n  result=green\n- validation: go test ./...\n  passed exit=0"
	if body != want {
		t.Fatalf("comment =\n%s\nwant\n%s", body, want)
	}
	if got := buildEvidenceComment(RecordEvidenceRequest{}); got != "" {
		t.Fatalf("an empty request must produce no comment, got %q", got)
	}
}

// ---- gh-backed operations ---------------------------------------------------

const fakeGHScript = `#!/bin/sh
n=$(cat "$FAKE_GH_DIR/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$FAKE_GH_DIR/count"
: > "$FAKE_GH_DIR/args.$n"
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_GH_DIR/args.$n"; done
if [ -n "$FAKE_GH_FAIL_ON" ] && [ "$2" = "$FAKE_GH_FAIL_ON" ]; then echo "simulated gh failure" >&2; exit 1; fi
if [ -f "$FAKE_GH_DIR/stdout" ]; then cat "$FAKE_GH_DIR/stdout"; fi
`

type fakeGH struct {
	t   *testing.T
	dir string
}

func installFakeGH(t *testing.T, issueJSON string) *fakeGH {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte(issueJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", dir)
	t.Setenv("FAKE_GH_FAIL_ON", "")
	return &fakeGH{t: t, dir: dir}
}

// calls returns the gh subcommand ("view", "edit", "comment", ...) of each invocation.
func (f *fakeGH) calls() []string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "count"))
	if err != nil {
		return nil
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, strings.Split(f.args(i), "\n")[1])
	}
	return out
}

func (f *fakeGH) args(n int) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "args."+strconv.Itoa(n)))
	if err != nil {
		f.t.Fatalf("gh invocation %d was not recorded: %v", n, err)
	}
	return string(b)
}

func issueJSON(state string, labels []string, body string) string {
	var ls []string
	for _, l := range labels {
		ls = append(ls, `{"name":`+strconv.Quote(l)+`}`)
	}
	return `{"state":` + strconv.Quote(state) + `,"title":"t","body":` + strconv.Quote(body) +
		`,"labels":[` + strings.Join(ls, ",") + `],"comments":[]}`
}

const twoItemBody = "- [ ] alpha\n- [ ] beta"

func TestSyncChecklist_Outcomes(t *testing.T) {
	done := []WorkItem{{ID: "s1", Description: "alpha", Done: true}}
	cases := []struct {
		name        string
		state       string
		labels      []string
		body        string
		req         SyncChecklistRequest
		wantOutcome SyncOutcome
		wantState   ghissue.IssueState
		wantWrote   bool
		wantCalls   []string
	}{
		{name: "writes the ticked body", state: "OPEN", body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: done},
			wantOutcome: SyncOutcomeWrote, wantState: ghissue.IssueStateChecklistSynced, wantWrote: true,
			wantCalls: []string{"view", "edit"}},
		{name: "dry run does not write", state: "OPEN", body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: done, DryRun: true},
			wantOutcome: SyncOutcomeDryRun, wantState: ghissue.IssueStateChecklistSynced,
			wantCalls: []string{"view"}},
		{name: "items that are not done never tick", state: "OPEN", body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: []WorkItem{{Description: "alpha"}}},
			wantOutcome: SyncOutcomeNoMatch, wantState: ghissue.IssueStateDrafted,
			wantCalls: []string{"view"}},
		{name: "already ticked is no change", state: "OPEN", body: "- [x] alpha\n- [ ] beta",
			req:         SyncChecklistRequest{SubTasks: done},
			wantOutcome: SyncOutcomeNoChange, wantState: ghissue.IssueStateDrafted,
			wantCalls: []string{"view"}},
		{name: "closed issue is left alone", state: "CLOSED", body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: done},
			wantOutcome: SyncOutcomeIssueState, wantState: ghissue.IssueStateClosed,
			wantCalls: []string{"view"}},
		{name: "blocked label stops the write", state: "OPEN", labels: []string{"Blocked"}, body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: done},
			wantOutcome: SyncOutcomeIssueState, wantState: ghissue.IssueStateBlocked,
			wantCalls: []string{"view"}},
		{name: "a low-confidence mapping needs a human", state: "OPEN", body: twoItemBody,
			req: SyncChecklistRequest{SubTasks: done,
				ChecklistMapping: &ChecklistMappingResult{NeedsHumanConfirmation: true}},
			wantOutcome: SyncOutcomeNeedsHuman, wantState: ghissue.IssueStateBlocked,
			wantCalls: []string{"view"}},
		{name: "RequireHumanDecision holds even without a mapping", state: "OPEN", body: twoItemBody,
			req:         SyncChecklistRequest{SubTasks: done, RequireHumanDecision: true},
			wantOutcome: SyncOutcomeNeedsHuman, wantState: ghissue.IssueStateBlocked,
			wantCalls: []string{"view"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := installFakeGH(t, issueJSON(tc.state, tc.labels, tc.body))
			req := tc.req
			req.ProjectDir, req.IssueNumber = t.TempDir(), 5
			res, err := New().SyncChecklist(context.Background(), req)
			if err != nil {
				t.Fatalf("SyncChecklist: %v", err)
			}
			if res.Outcome != tc.wantOutcome || res.State != tc.wantState || res.Wrote != tc.wantWrote {
				t.Fatalf("outcome=%q state=%q wrote=%v reason=%q; want outcome=%q state=%q wrote=%v",
					res.Outcome, res.State, res.Wrote, res.Reason, tc.wantOutcome, tc.wantState, tc.wantWrote)
			}
			if got := fake.calls(); !reflect.DeepEqual(got, tc.wantCalls) {
				t.Fatalf("gh calls = %v, want %v", got, tc.wantCalls)
			}
			if tc.wantWrote {
				if want := "issue\nedit\n5\n--body\n- [x] alpha\n- [ ] beta\n"; fake.args(2) != want {
					t.Errorf("write args = %q, want %q", fake.args(2), want)
				}
			}
		})
	}
}

func TestSyncChecklist_Failures(t *testing.T) {
	done := []WorkItem{{Description: "alpha", Done: true}}

	t.Run("no issue number is a no-op", func(t *testing.T) {
		fake := installFakeGH(t, "")
		res, err := New().SyncChecklist(context.Background(), SyncChecklistRequest{SubTasks: done})
		if err != nil || res.Outcome != SyncOutcomeUnknown || len(fake.calls()) != 0 {
			t.Fatalf("res=%+v err=%v calls=%v", res, err, fake.calls())
		}
	})

	t.Run("load failure blocks with retry metadata", func(t *testing.T) {
		installFakeGH(t, "")
		t.Setenv("FAKE_GH_FAIL_ON", "view")
		res, err := New().SyncChecklist(context.Background(), SyncChecklistRequest{ProjectDir: t.TempDir(), IssueNumber: 5, SubTasks: done})
		if err == nil || res.Outcome != SyncOutcomeLoadFailed || res.State != ghissue.IssueStateBlocked {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if res.Recovery == nil || !res.Recovery.Retryable || res.Recovery.RetryAction != "retry-sync-checklist" {
			t.Fatalf("Recovery = %+v", res.Recovery)
		}
	})

	t.Run("write failure blocks with retry metadata", func(t *testing.T) {
		installFakeGH(t, issueJSON("OPEN", nil, twoItemBody))
		t.Setenv("FAKE_GH_FAIL_ON", "edit")
		res, err := New().SyncChecklist(context.Background(), SyncChecklistRequest{ProjectDir: t.TempDir(), IssueNumber: 5, SubTasks: done})
		if err == nil || res.Outcome != SyncOutcomeFailed || res.State != ghissue.IssueStateBlocked || res.Wrote {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		if res.Recovery == nil || res.Recovery.Operation != "sync-checklist" {
			t.Fatalf("Recovery = %+v", res.Recovery)
		}
	})
}

func TestPlanIssue(t *testing.T) {
	fake := installFakeGH(t, `{"body":"intro\n\n## Hermes 執行計劃\n\n1. old step"}`)
	err := New().PlanIssue(context.Background(), PlanIssueRequest{
		ProjectDir: t.TempDir(), IssueNumber: 8, PlannerModel: "p", ExecutorModel: "e",
		Tasks:        []WorkItem{{Description: "first"}, {Description: "second", ChecklistItemIDs: []string{"item-0", "item-2"}}},
		CommentStart: true, WritePlan: true,
	})
	if err != nil {
		t.Fatalf("PlanIssue: %v", err)
	}
	if got, want := fake.calls(), []string{"comment", "view", "edit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("gh calls = %v, want %v", got, want)
	}
	if !strings.Contains(fake.args(1), "Hermes 開始執行") {
		t.Errorf("start comment = %q", fake.args(1))
	}
	wantBody := "intro\n\n## Hermes 執行計劃\n\n1. first\n2. second _(covers: item-0, item-2)_\n"
	if want := "issue\nedit\n8\n--body\n" + wantBody + "\n"; fake.args(3) != want {
		t.Errorf("plan write = %q, want %q", fake.args(3), want)
	}

	quiet := installFakeGH(t, "")
	if err := New().PlanIssue(context.Background(), PlanIssueRequest{IssueNumber: 8}); err != nil || len(quiet.calls()) != 0 {
		t.Fatalf("with both switches off nothing must be written: err=%v calls=%v", err, quiet.calls())
	}
}

func TestCommentDone(t *testing.T) {
	fake := installFakeGH(t, "")
	summary := ghissue.CompletionSummary{Done: 2, Total: 2, TokensUsed: 500, MaxTokens: 1000,
		Artifacts: []ghissue.Artifact{{Path: "a.go", Hash: "0123456789abcdef"}, {Path: "b.go"}}}
	if err := New().CommentDone(context.Background(), t.TempDir(), 3, summary, "  remember to deploy  "); err != nil {
		t.Fatalf("CommentDone: %v", err)
	}
	want := "issue\ncomment\n3\n--body\n✅ **Hermes 完成** 2/2 SubTasks\n\n- Token 用量: 500/1000\n\n" +
		"**修改的檔案：**\n- `a.go` (01234567)\n- `b.go`\n\n**Note:**\nremember to deploy\n\n"
	if fake.args(1) != want {
		t.Fatalf("comment args =\n%q\nwant\n%q", fake.args(1), want)
	}

	// The comment must be readable back as a successful completion signal.
	body := ghissue.CommentDone(summary, "")
	sig, ok := ghissue.RecentSuccessfulHermesCompletion(&ghissue.IssueContext{Comments: []ghissue.IssueComment{{Body: body}}})
	if !ok || sig.Done != 2 || sig.Total != 2 {
		t.Fatalf("completion comment is not recognised as success: %+v %v", sig, ok)
	}
}
