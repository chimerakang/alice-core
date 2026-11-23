package hermes

import (
	"reflect"
	"testing"

	"github.com/chimerakang/alice-core/ghissue"
)

// The hermes wrappers must feed only done sub-tasks into checklist sync.
func TestBuildChecklistSyncPreview_OnlyDoneSubTasksCount(t *testing.T) {
	const body = "## 驗收條件\n- [ ] 新增 worktree 套件\n- [ ] 補上測試\n- [x] 更新 README"

	t.Run("fuzzy mode", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []SubTask{
			{ID: "s1", Description: "新增 worktree 套件", Status: SubTaskDone},
			{ID: "s2", Description: "補上測試", Status: SubTaskFailed},
			{ID: "s3", Description: "補上測試", Status: SubTaskPending},
		})
		want := "## 驗收條件\n- [x] 新增 worktree 套件\n- [ ] 補上測試\n- [x] 更新 README"
		if !p.Changed || p.BodyAfter != want {
			t.Fatalf("BodyAfter =\n%q\nwant\n%q", p.BodyAfter, want)
		}
		if !reflect.DeepEqual(p.CompletedDescriptions, []string{"新增 worktree 套件"}) {
			t.Errorf("CompletedDescriptions = %q", p.CompletedDescriptions)
		}
	})

	t.Run("declared mode ignores declarations from sub-tasks that are not done", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []SubTask{
			{ID: "s1", Description: "setup", Status: SubTaskDone, ChecklistItemIDs: []string{"item-1"}},
			{ID: "s2", Description: "tests", Status: SubTaskFailed, ChecklistItemIDs: []string{"item-2"}},
		})
		want := "## 驗收條件\n- [x] 新增 worktree 套件\n- [ ] 補上測試\n- [x] 更新 README"
		if p.BodyAfter != want {
			t.Fatalf("BodyAfter =\n%q\nwant\n%q", p.BodyAfter, want)
		}
	})

	t.Run("no done sub-tasks changes nothing", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []SubTask{{ID: "s1", Description: "新增 worktree 套件", Status: SubTaskPending}})
		if p.Changed || p.BodyAfter != body {
			t.Fatalf("unexpected preview: %+v", p)
		}
	})
}

// The aliases must be the same types, so values cross the package boundary
// without conversion.
func TestGitHubAliasesAreIdentical(t *testing.T) {
	var issue *IssueContext = &ghissue.IssueContext{Number: 1, Labels: []string{"bug"}}
	if !HasLabel(issue, "BUG") {
		t.Fatal("HasLabel wrapper must delegate")
	}
	var state IssueState = ghissue.IssueStateBlocked
	if state != IssueStateBlocked || !state.NeedsHumanDecision() {
		t.Fatal("IssueState alias must share constants and methods")
	}
	cfg := &GithubCfg{Enabled: true, CommentOnDone: true}
	if !cfg.ShouldComment("complete") {
		t.Fatal("GithubCfg alias must keep its methods")
	}
	if got := ExtractChecklist("- [ ] a"); len(got) != 1 || got[0].ID != "item-0" {
		t.Fatalf("ExtractChecklist wrapper = %+v", got)
	}
}

func TestBuildGoalFromIssue_ListsUncheckedItemsWithIDs(t *testing.T) {
	body := "Background text.\n\n## 驗收條件\n- [x] done item\n- [ ] open item"
	goal := BuildGoalFromIssue(&IssueContext{Number: 12, Title: "Do the thing", Body: body, Checklist: ExtractChecklist(body)})
	for _, want := range []string{
		"[GitHub #12] Do the thing",
		"  - [ ] [item-4] open item",
		"  - [x] done item",
		"Background text.",
	} {
		if !contains(goal, want) {
			t.Errorf("goal is missing %q:\n%s", want, goal)
		}
	}
	if contains(goal, "- [ ] open item\n") {
		t.Errorf("raw checklist lines must be stripped from the description:\n%s", goal)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
