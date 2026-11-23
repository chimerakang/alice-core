package ghissue

import (
	"reflect"
	"strings"
	"testing"
)

func TestExtractChecklist(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []ChecklistItem
	}{
		{name: "empty body", body: "", want: []ChecklistItem{}},
		{name: "no checklist", body: "Just a description.\n\n- a plain bullet", want: []ChecklistItem{}},
		{
			name: "flat list above any heading",
			body: "- [ ] first\n- [x] second\n- [X] third",
			want: []ChecklistItem{
				{ID: "item-0", Text: "first", LineNumber: 0},
				{ID: "item-1", Text: "second", Checked: true, LineNumber: 1},
				{ID: "item-2", Text: "third", Checked: true, LineNumber: 2},
			},
		},
		{
			name: "items carry their nearest heading and 0-indexed line",
			body: "Intro text\n\n## 驗收條件\n\n- [ ] 新增測試\n- [x] 更新文件\n\n### Notes ###\n- [ ] follow up",
			want: []ChecklistItem{
				{ID: "item-4", Text: "新增測試", LineNumber: 4, Section: "驗收條件"},
				{ID: "item-5", Text: "更新文件", Checked: true, LineNumber: 5, Section: "驗收條件"},
				{ID: "item-8", Text: "follow up", LineNumber: 8, Section: "Notes"},
			},
		},
		{
			name: "item text is trimmed and keeps inline markdown",
			body: "- [ ]   run `go test ./...` and **verify**  ",
			want: []ChecklistItem{
				{ID: "item-0", Text: "run `go test ./...` and **verify**", LineNumber: 0},
			},
		},
		{
			name: "Hermes plan section is ignored, later sections are not",
			body: "## Tasks\n- [ ] real item\n## Hermes 執行計劃\n- [ ] generated plan step\n## Done criteria\n- [ ] another real item",
			want: []ChecklistItem{
				{ID: "item-1", Text: "real item", LineNumber: 1, Section: "Tasks"},
				{ID: "item-5", Text: "another real item", LineNumber: 5, Section: "Done criteria"},
			},
		},
		{
			name: "lines that are not task-list items are skipped",
			body: "- [] no space\n- [y] wrong mark\n-[ ] no leading space\n- [ ]\n- [ ] valid",
			want: []ChecklistItem{
				{ID: "item-4", Text: "valid", LineNumber: 4},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractChecklist(tc.body)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractChecklist =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestIsAcceptanceSection(t *testing.T) {
	cases := []struct {
		section string
		want    bool
	}{
		{"", true},
		{"   ", true},
		{"Acceptance Criteria", true},
		{"acceptance", true},
		{"✅ Definition of Done", true},
		{"Requirements", true},
		{"驗收條件", true},
		{"完成條件", true},
		{"驗收清單（必填）", true},
		{"Tasks", false},
		{"Notes", false},
		{"子議題", false},
		{"相依", false},
		{"Hermes 執行計劃", false},
	}
	for _, tc := range cases {
		if got := IsAcceptanceSection(tc.section); got != tc.want {
			t.Errorf("IsAcceptanceSection(%q) = %v, want %v", tc.section, got, tc.want)
		}
	}
}

func TestMatchesChecklistItem(t *testing.T) {
	cases := []struct {
		name string
		item string
		desc string
		want bool
	}{
		{"exact", "Add unit tests", "Add unit tests", true},
		{"case-insensitive", "Add Unit Tests", "add unit tests", true},
		{"surrounding whitespace", "  Add unit tests ", "Add unit tests  ", true},
		{"description is a prefix of the item", "Add unit tests for auth", "Add unit", true},
		{"only the first 16 runes of the description are compared",
			"Add unit tests for the auth package", "Add unit tests for something else entirely", true},
		{"differs inside the first 16 runes", "Add unit tests for auth", "Add integration tests", false},
		{"16-rune cutoff counts runes, not bytes",
			"新增使用者登入流程的單元測試並更新文件說明", "新增使用者登入流程的單元測試並更新另一份", true},
		{"item shorter than the compared prefix", "Add unit", "Add unit tests", false},
		{"empty item", "", "Add", false},
		{"empty description", "Add", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matchesChecklistItem(tc.item, tc.desc); got != tc.want {
				t.Fatalf("matchesChecklistItem(%q, %q) = %v, want %v", tc.item, tc.desc, got, tc.want)
			}
		})
	}
}

func TestUpdateChecklistInBody(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		completed []string
		want      string
	}{
		{
			name:      "ticks only matching items and preserves everything else",
			body:      "Intro\n\n## 驗收條件\n- [ ] 新增測試\n- [ ] 更新文件\n\ntrailing text\n",
			completed: []string{"新增測試"},
			want:      "Intro\n\n## 驗收條件\n- [x] 新增測試\n- [ ] 更新文件\n\ntrailing text\n",
		},
		{
			name:      "already checked items keep their original mark",
			body:      "- [X] done before\n- [ ] done now",
			completed: []string{"done before", "done now"},
			want:      "- [X] done before\n- [x] done now",
		},
		{
			name:      "no completed descriptions leaves the body untouched",
			body:      "- [ ] one\n- [ ] two",
			completed: nil,
			want:      "- [ ] one\n- [ ] two",
		},
		{
			name:      "no match leaves the body untouched",
			body:      "- [ ] one\n- [ ] two",
			completed: []string{"three"},
			want:      "- [ ] one\n- [ ] two",
		},
		{
			name:      "Hermes plan section is never ticked",
			body:      "## Tasks\n- [ ] ship it\n## Hermes 執行計劃\n- [ ] ship it",
			completed: []string{"ship it"},
			want:      "## Tasks\n- [x] ship it\n## Hermes 執行計劃\n- [ ] ship it",
		},
		{
			name:      "one description can tick several items sharing a prefix",
			body:      "- [ ] Add tests for auth\n- [ ] Add tests for billing",
			completed: []string{"Add tests"},
			want:      "- [x] Add tests for auth\n- [x] Add tests for billing",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := UpdateChecklistInBody(tc.body, tc.completed); got != tc.want {
				t.Fatalf("UpdateChecklistInBody =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestUpdateChecklistInBody_RoundTrip(t *testing.T) {
	body := "## Acceptance\n- [ ] alpha\n- [ ] beta\n- [x] gamma\n"
	updated := UpdateChecklistInBody(body, []string{"beta"})

	before, after := ExtractChecklist(body), ExtractChecklist(updated)
	if len(before) != len(after) {
		t.Fatalf("item count changed: %d -> %d", len(before), len(after))
	}
	wantChecked := []bool{false, true, true}
	for i := range after {
		if after[i].ID != before[i].ID || after[i].Text != before[i].Text || after[i].Section != before[i].Section {
			t.Errorf("item %d identity changed: %+v -> %+v", i, before[i], after[i])
		}
		if after[i].Checked != wantChecked[i] {
			t.Errorf("item %d Checked = %v, want %v", i, after[i].Checked, wantChecked[i])
		}
	}
	if again := UpdateChecklistInBody(updated, []string{"beta"}); again != updated {
		t.Errorf("update is not idempotent:\n%q\n->\n%q", updated, again)
	}
}

func TestBuildChecklistSyncPreview(t *testing.T) {
	const body = "## 驗收條件\n- [ ] 新增 worktree 套件\n- [ ] 補上測試\n- [x] 更新 README"
	// item IDs follow line numbers: item-1, item-2, item-3.

	t.Run("no completed work changes nothing", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, nil)
		if p.Changed || p.BodyAfter != body || p.BodyBefore != body || len(p.UpdatedChecklistItems) != 0 {
			t.Fatalf("unexpected preview: %+v", p)
		}
	})

	t.Run("fuzzy mode ticks by description when nothing is declared", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []CompletedWork{{Description: "新增 worktree 套件"}})
		want := "## 驗收條件\n- [x] 新增 worktree 套件\n- [ ] 補上測試\n- [x] 更新 README"
		if !p.Changed || p.BodyAfter != want {
			t.Fatalf("BodyAfter =\n%q\nwant\n%q (Changed=%v)", p.BodyAfter, want, p.Changed)
		}
		if !reflect.DeepEqual(p.UpdatedChecklistItems, []string{"新增 worktree 套件"}) {
			t.Errorf("UpdatedChecklistItems = %q", p.UpdatedChecklistItems)
		}
		if !reflect.DeepEqual(p.CompletedDescriptions, []string{"新增 worktree 套件"}) {
			t.Errorf("CompletedDescriptions = %q", p.CompletedDescriptions)
		}
		if p.BodyBefore != body {
			t.Errorf("BodyBefore was modified")
		}
	})

	t.Run("declared mode ticks by ID and ignores description overlap", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []CompletedWork{
			// Description matches item-1 textually, but the declaration owns item-2.
			{Description: "新增 worktree 套件", ChecklistItemIDs: []string{" item-2 ", ""}},
		})
		want := "## 驗收條件\n- [ ] 新增 worktree 套件\n- [x] 補上測試\n- [x] 更新 README"
		if !p.Changed || p.BodyAfter != want {
			t.Fatalf("BodyAfter =\n%q\nwant\n%q (Changed=%v)", p.BodyAfter, want, p.Changed)
		}
		if !reflect.DeepEqual(p.UpdatedChecklistItems, []string{"補上測試"}) {
			t.Errorf("UpdatedChecklistItems = %q", p.UpdatedChecklistItems)
		}
	})

	t.Run("one declaration switches every entry to declared mode", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []CompletedWork{
			{Description: "新增 worktree 套件"}, // undeclared: not ticked in declared mode
			{Description: "tests", ChecklistItemIDs: []string{"item-2"}},
		})
		want := "## 驗收條件\n- [ ] 新增 worktree 套件\n- [x] 補上測試\n- [x] 更新 README"
		if p.BodyAfter != want {
			t.Fatalf("BodyAfter =\n%q\nwant\n%q", p.BodyAfter, want)
		}
	})

	t.Run("declared IDs that are unknown or already checked change nothing", func(t *testing.T) {
		p := BuildChecklistSyncPreview(body, []CompletedWork{
			{Description: "x", ChecklistItemIDs: []string{"item-99", "item-3"}},
		})
		if p.Changed || p.BodyAfter != body {
			t.Fatalf("unexpected change: %+v", p)
		}
	})
}

func TestReconcileIssueCompletion(t *testing.T) {
	if rec := ReconcileIssueCompletion(nil); rec.ChecklistTotal != 0 || rec.HasUnchecked() || rec.ChecklistComplete() {
		t.Fatalf("nil issue = %+v", rec)
	}

	open := &IssueContext{Number: 7, State: "OPEN", Checklist: ExtractChecklist("- [x] a\n- [ ] b\n- [ ] c")}
	rec := ReconcileIssueCompletion(open)
	if rec.IssueNumber != 7 || rec.ChecklistTotal != 3 || rec.CheckedCount != 1 || len(rec.Unchecked) != 2 {
		t.Fatalf("partial issue = %+v", rec)
	}
	if !rec.HasUnchecked() || rec.ChecklistComplete() {
		t.Fatalf("partial issue flags wrong: %+v", rec)
	}
	note := rec.CommentNote()
	if !strings.Contains(note, "1/3 checked") || !strings.Contains(note, "- [ ] b") || !strings.Contains(note, "- [ ] c") {
		t.Errorf("partial note = %q", note)
	}

	done := ReconcileIssueCompletion(&IssueContext{Checklist: ExtractChecklist("- [x] a\n- [X] b")})
	if done.HasUnchecked() || !done.ChecklistComplete() {
		t.Fatalf("complete issue flags wrong: %+v", done)
	}
	if !strings.Contains(done.CommentNote(), "2/2 checked") {
		t.Errorf("complete note = %q", done.CommentNote())
	}

	empty := ReconcileIssueCompletion(&IssueContext{})
	if empty.ChecklistComplete() {
		t.Fatal("an issue without a checklist must not count as checklist-complete")
	}
	if !strings.Contains(empty.CommentNote(), "no GitHub issue checklist items") {
		t.Errorf("empty note = %q", empty.CommentNote())
	}
}
