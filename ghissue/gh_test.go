package ghissue

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fakeGHScript stands in for the gh CLI: it records each invocation and
// replays a canned response from FAKE_GH_DIR.
const fakeGHScript = `#!/bin/sh
n=$(cat "$FAKE_GH_DIR/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$FAKE_GH_DIR/count"
: > "$FAKE_GH_DIR/args.$n"
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_GH_DIR/args.$n"; done
pwd -P > "$FAKE_GH_DIR/cwd"
if [ -f "$FAKE_GH_DIR/stdout" ]; then cat "$FAKE_GH_DIR/stdout"; fi
if [ -f "$FAKE_GH_DIR/stderr" ]; then cat "$FAKE_GH_DIR/stderr" >&2; fi
exit "${FAKE_GH_EXIT:-0}"
`

type fakeGH struct {
	t   *testing.T
	dir string
}

func installFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", dir)
	return &fakeGH{t: t, dir: dir}
}

func (f *fakeGH) stdout(s string) *fakeGH {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, "stdout"), []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return f
}

func (f *fakeGH) calls() int {
	b, err := os.ReadFile(filepath.Join(f.dir, "count"))
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range strings.TrimSpace(string(b)) {
		n = n*10 + int(c-'0')
	}
	return n
}

// args returns the arguments of the n-th invocation (1-based), one per line.
func (f *fakeGH) args(n int) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "args."+string(rune('0'+n))))
	if err != nil {
		f.t.Fatalf("gh invocation %d was not recorded: %v", n, err)
	}
	return string(b)
}

func TestFetchIssue(t *testing.T) {
	fake := installFakeGH(t).stdout(`{
		"state": "OPEN",
		"title": "Add worktree package",
		"body": "## 驗收條件\n- [x] design\n- [ ] implement",
		"labels": [{"name": "enhancement"}, {"name": "P1"}],
		"comments": [{"author": {"login": "octo"}, "body": "looks good", "createdAt": "2026-01-02T03:04:05Z"}]
	}`)
	dir := t.TempDir()

	issue, err := FetchIssue(context.Background(), dir, 42)
	if err != nil {
		t.Fatalf("FetchIssue: %v", err)
	}
	if issue.Number != 42 || issue.Title != "Add worktree package" || issue.State != "OPEN" {
		t.Errorf("unexpected issue: %+v", issue)
	}
	if !reflect.DeepEqual(issue.Labels, []string{"enhancement", "P1"}) {
		t.Errorf("Labels = %q", issue.Labels)
	}
	if len(issue.Checklist) != 2 || !issue.Checklist[0].Checked || issue.Checklist[1].Checked ||
		issue.Checklist[1].Section != "驗收條件" {
		t.Errorf("Checklist = %+v", issue.Checklist)
	}
	if len(issue.Comments) != 1 || issue.Comments[0].Author != "octo" || issue.Comments[0].CreatedAt.Year() != 2026 {
		t.Errorf("Comments = %+v", issue.Comments)
	}
	if !HasLabel(issue, "p1") || HasLabel(issue, "bug") {
		t.Errorf("HasLabel must match case-insensitively")
	}

	if want := "issue\nview\n42\n--json\ntitle,body,state,labels,comments\n"; fake.args(1) != want {
		t.Errorf("gh args = %q, want %q", fake.args(1), want)
	}
	resolved, _ := filepath.EvalSymlinks(dir)
	if cwd, _ := os.ReadFile(filepath.Join(fake.dir, "cwd")); strings.TrimSpace(string(cwd)) != resolved {
		t.Errorf("gh ran in %q, want the project dir %q", cwd, resolved)
	}
}

func TestFetchIssue_Errors(t *testing.T) {
	installFakeGH(t).stdout("not json")
	if _, err := FetchIssue(context.Background(), t.TempDir(), 1); err == nil || !strings.Contains(err.Error(), "parse issue JSON") {
		t.Errorf("err = %v, want a parse error", err)
	}

	installFakeGH(t)
	t.Setenv("FAKE_GH_EXIT", "1")
	if _, err := FetchIssue(context.Background(), t.TempDir(), 7); err == nil || !strings.Contains(err.Error(), "gh issue view 7") {
		t.Errorf("err = %v, want a gh failure naming the issue", err)
	}
}

func TestListIssues_SortsByPriority(t *testing.T) {
	installFakeGH(t).stdout(`[
		{"number": 1, "title": "plain", "labels": [], "updatedAt": "2020-01-01T00:00:00Z"},
		{"number": 2, "title": "bug", "labels": [{"name": "Bug"}], "updatedAt": "2020-01-01T00:00:00Z"},
		{"number": 3, "title": "milestoned", "labels": [], "milestone": {"title": "v1"}, "updatedAt": "2020-01-01T00:00:00Z"},
		{"number": 4, "title": "blocked bug", "labels": [{"name": "bug"}, {"name": "blocked"}], "updatedAt": "2020-01-01T00:00:00Z"},
		{"number": 5, "title": "newer plain", "labels": [], "updatedAt": "2021-01-01T00:00:00Z"}
	]`)
	items, err := ListIssues(context.Background(), t.TempDir(), 0)
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	var order []int
	for _, it := range items {
		order = append(order, it.Number)
	}
	// bug(10) > milestone(5) = blocked bug(5) > plain(0); ties break on recency,
	// and equal timestamps keep their input order.
	if want := []int{2, 3, 4, 5, 1}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	if items[1].Milestone != "v1" {
		t.Errorf("Milestone = %q", items[1].Milestone)
	}
}

func TestListIssues_DefaultLimit(t *testing.T) {
	fake := installFakeGH(t).stdout("[]")
	if _, err := ListIssues(context.Background(), t.TempDir(), 0); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if !strings.Contains(fake.args(1), "--limit\n15\n") {
		t.Errorf("gh args = %q, want a default limit of 15", fake.args(1))
	}
}

func TestWriteOperations_Args(t *testing.T) {
	cases := []struct {
		name string
		run  func(dir string) error
		want string
	}{
		{"UpdateIssueBody", func(d string) error { return UpdateIssueBody(context.Background(), d, 9, "new\nbody") },
			"issue\nedit\n9\n--body\nnew\nbody\n"},
		{"PostComment", func(d string) error { return PostComment(context.Background(), d, 9, "hello") },
			"issue\ncomment\n9\n--body\nhello\n"},
		{"ApplyLabel", func(d string) error { return ApplyLabel(context.Background(), d, 9, "needs-review") },
			"issue\nedit\n9\n--add-label\nneeds-review\n"},
		{"CloseIssue", func(d string) error { return CloseIssue(context.Background(), d, 9) },
			"issue\nclose\n9\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := installFakeGH(t)
			if err := tc.run(t.TempDir()); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := fake.args(1); got != tc.want {
				t.Fatalf("gh args = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWriteOperations_ReportGHFailure(t *testing.T) {
	fake := installFakeGH(t)
	if err := os.WriteFile(filepath.Join(fake.dir, "stderr"), []byte("HTTP 403: forbidden\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_GH_EXIT", "1")
	err := PostComment(context.Background(), t.TempDir(), 9, "hello")
	if err == nil || !strings.Contains(err.Error(), "gh issue comment 9") || !strings.Contains(err.Error(), "HTTP 403: forbidden") {
		t.Fatalf("err = %v, want it to name the operation and include gh's output", err)
	}
}

func TestSyncChecklist(t *testing.T) {
	const bodyJSON = `{"body": "- [ ] alpha\n- [ ] beta"}`

	t.Run("writes the ticked body", func(t *testing.T) {
		fake := installFakeGH(t).stdout(bodyJSON)
		err := SyncChecklist(context.Background(), t.TempDir(), 5, []CompletedWork{{Description: "alpha"}})
		if err != nil {
			t.Fatalf("SyncChecklist: %v", err)
		}
		if fake.calls() != 2 {
			t.Fatalf("gh calls = %d, want a read then a write", fake.calls())
		}
		if want := "issue\nview\n5\n--json\nbody\n"; fake.args(1) != want {
			t.Errorf("read args = %q, want %q", fake.args(1), want)
		}
		if want := "issue\nedit\n5\n--body\n- [x] alpha\n- [ ] beta\n"; fake.args(2) != want {
			t.Errorf("write args = %q, want %q", fake.args(2), want)
		}
	})

	t.Run("does not write when nothing changes", func(t *testing.T) {
		for name, done := range map[string][]CompletedWork{
			"no completed work": nil,
			"no matching item":  {{Description: "gamma"}},
		} {
			fake := installFakeGH(t).stdout(bodyJSON)
			if err := SyncChecklist(context.Background(), t.TempDir(), 5, done); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if fake.calls() != 1 {
				t.Errorf("%s: gh calls = %d, want only the read", name, fake.calls())
			}
		}
	})
}

func TestStripPlanSections(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"no plan section", "intro\n\n## Tasks\n- [ ] a", "intro\n\n## Tasks\n- [ ] a"},
		{"plan at the end", "intro\n\n## Hermes 執行計劃\n\n1. step", "intro"},
		{"plan in the middle", "intro\n\n## Hermes 執行計劃\n\n1. step\n## Notes\nkeep", "intro\n\n## Notes\nkeep"},
		{"plan first, then another section", "## Hermes 執行計劃\n1. a\n## Notes\nkeep", "## Notes\nkeep"},
		{"duplicate plans", "intro\n\n## Hermes 執行計劃\n1. a\n\n## Hermes 執行計劃\n1. b", "intro"},
		{"only a plan", "## Hermes 執行計劃\n1. a", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StripPlanSections(tc.body)
			if got != tc.want {
				t.Fatalf("StripPlanSections = %q, want %q", got, tc.want)
			}
			if again := StripPlanSections(got); again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

func TestStripChecklistLines(t *testing.T) {
	got := StripChecklistLines("Intro\n- [ ] a\n\nmiddle\n- [x] b\n- plain bullet\n")
	if want := "Intro\n\nmiddle\n- plain bullet"; got != want {
		t.Fatalf("StripChecklistLines = %q, want %q", got, want)
	}
}

func TestRecentSuccessfulHermesCompletion(t *testing.T) {
	c := func(body string) IssueComment { return IssueComment{Author: "bot", Body: body} }
	cases := []struct {
		name     string
		comments []IssueComment
		wantOK   bool
		wantDone int
	}{
		{"no comments", nil, false, 0},
		{"complete run", []IssueComment{c("✅ **Hermes 完成** 3/3 SubTasks")}, true, 3},
		{"partial run is not success", []IssueComment{c("✅ **Hermes 完成** 2/3 SubTasks")}, false, 0},
		{"a later failure overrides success",
			[]IssueComment{c("✅ **Hermes 完成** 3/3 SubTasks"), c("❌ **Hermes 執行失敗**")}, false, 0},
		{"a later success overrides failure",
			[]IssueComment{c("❌ **Hermes 執行失敗**"), c("✅ **Hermes 完成** 4/4 SubTasks")}, true, 4},
		{"budget exhaustion counts as failure",
			[]IssueComment{c("✅ **Hermes 完成** 3/3 SubTasks"), c("⚠️ **Hermes Budget 耗盡**")}, false, 0},
		{"unrelated comments are ignored",
			[]IssueComment{c("✅ **Hermes 完成** 3/3 SubTasks"), c("thanks!"), c("  ")}, true, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok := RecentSuccessfulHermesCompletion(&IssueContext{Comments: tc.comments})
			if ok != tc.wantOK || sig.Done != tc.wantDone {
				t.Fatalf("got (%+v, %v), want done=%d ok=%v", sig, ok, tc.wantDone, tc.wantOK)
			}
		})
	}
	if _, ok := RecentSuccessfulHermesCompletion(nil); ok {
		t.Fatal("nil issue must not report success")
	}
}
