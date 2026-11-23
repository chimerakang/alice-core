package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type stubTaskSource struct {
	active    TaskSummary
	activeErr error
	history   []TaskSummary
	listErr   error
	gotLimit  int
}

func (s *stubTaskSource) GetActiveForChat(int64) (TaskSummary, error) { return s.active, s.activeErr }

func (s *stubTaskSource) ListForChat(_ int64, limit int) ([]TaskSummary, error) {
	s.gotLimit = limit
	return s.history, s.listErr
}

func taskSection(t *testing.T, src TaskSource, req Request) (Section, bool) {
	t.Helper()
	bundle, err := New(src).Resolve(context.Background(), req)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, s := range bundle.Sections {
		if s.Source == "issue_task" || s.Source == "hermes_task" {
			return s, true
		}
	}
	return Section{}, false
}

func TestResolve_RendersIssueTask(t *testing.T) {
	updated := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	src := &stubTaskSource{active: TaskSummary{
		ID: "task-1", Status: "executing", IssueNumber: 42, Goal: "Add the worktree package",
		UpdatedAt: updated, Progress: "Created the package skeleton.",
		Steps: []TaskStep{
			{Description: "Write Create/Remove", Status: "done", Result: "implemented and tested"},
			{Description: "  ", Status: "pending"},
		},
	}}
	section, ok := taskSection(t, src, Request{ChannelID: 1, IssueNumber: 42, UserMessage: "continue"})
	if !ok {
		t.Fatal("expected a task section")
	}
	if section.Source != "issue_task" || section.Scope != "issue:42" || section.Priority != 100 {
		t.Errorf("section meta = %+v", section)
	}
	for _, want := range []string{
		"Persisted Hermes work memory (2026-03-04T05:06:07Z):",
		"- Task ID: task-1",
		"- Status: executing",
		"- GitHub issue: #42",
		"- Original request: Add the worktree package",
		"Accumulated summary:\n  Created the package skeleton.",
		"1. [done] Write Create/Remove",
		"Result: implemented and tested",
		"2. [pending] (no description)",
	} {
		if !strings.Contains(section.Text, want) {
			t.Errorf("section text is missing %q:\n%s", want, section.Text)
		}
	}
}

func TestResolve_TaskWithoutSteps(t *testing.T) {
	src := &stubTaskSource{active: TaskSummary{ID: "t", IssueNumber: 7, Goal: "Investigate the flaky test"}}
	section, ok := taskSection(t, src, Request{ChannelID: 1, IssueNumber: 7})
	if !ok {
		t.Fatal("expected a task section")
	}
	if !strings.Contains(section.Text, "(No stored progress details.)") || strings.Contains(section.Text, "Subtasks:") {
		t.Errorf("a task with no steps must not render a sub-task list:\n%s", section.Text)
	}
}

func TestResolve_NoIssueScopeMeansNoTaskSection(t *testing.T) {
	src := &stubTaskSource{active: TaskSummary{ID: "t", IssueNumber: 7, Goal: "g"}}
	if _, ok := taskSection(t, src, Request{ChannelID: 1, UserMessage: "just chatting"}); ok {
		t.Fatal("task memory must only be injected for an explicit issue scope")
	}
}

func TestResolve_IssueScopeFiltersOtherIssues(t *testing.T) {
	src := &stubTaskSource{
		active: TaskSummary{ID: "active", IssueNumber: 99, Goal: "another issue"},
		history: []TaskSummary{
			{ID: "h1", IssueNumber: 42, Goal: "first run"},
			{ID: "h2", IssueNumber: 99, Goal: "unrelated"},
			{ID: "h3", IssueNumber: 42, Goal: "second run"},
			{ID: "h4", IssueNumber: 42, Goal: "third run"},
			{ID: "h5", IssueNumber: 42, Goal: "fourth run"},
		},
	}
	section, ok := taskSection(t, src, Request{ChannelID: 1, IssueNumber: 42})
	if !ok {
		t.Fatal("expected a task section")
	}
	if src.gotLimit != 10 {
		t.Errorf("history limit = %d, want 10 for an issue scope", src.gotLimit)
	}
	for _, want := range []string{"first run", "second run", "third run"} {
		if !strings.Contains(section.Text, want) {
			t.Errorf("missing %q:\n%s", want, section.Text)
		}
	}
	for _, unwanted := range []string{"another issue", "unrelated", "fourth run"} {
		if strings.Contains(section.Text, unwanted) {
			t.Errorf("must not include %q (other issue, or past the cap of 3):\n%s", unwanted, section.Text)
		}
	}
}

func TestResolve_ActiveTaskIsNotRepeatedFromHistory(t *testing.T) {
	task := TaskSummary{ID: "same", IssueNumber: 42, Goal: "only once please"}
	src := &stubTaskSource{active: task, history: []TaskSummary{task}}
	section, _ := taskSection(t, src, Request{ChannelID: 1, IssueNumber: 42})
	if got := strings.Count(section.Text, "only once please"); got != 1 {
		t.Fatalf("task rendered %d times, want 1:\n%s", got, section.Text)
	}
}

func TestResolve_ErrNoTaskIsNotAnError(t *testing.T) {
	for name, err := range map[string]error{
		"sentinel": ErrNoTask,
		"wrapped":  fmt.Errorf("store: %w", ErrNoTask),
	} {
		t.Run(name, func(t *testing.T) {
			src := &stubTaskSource{activeErr: err, history: []TaskSummary{{ID: "h", IssueNumber: 42, Goal: "from history"}}}
			section, ok := taskSection(t, src, Request{ChannelID: 1, IssueNumber: 42})
			if !ok || !strings.Contains(section.Text, "from history") {
				t.Fatalf("history must still be used when there is no active task: %+v", section)
			}
		})
	}
}

func TestResolve_SourceErrorsPropagate(t *testing.T) {
	boom := errors.New("database is locked")

	_, err := New(&stubTaskSource{activeErr: boom}).Resolve(context.Background(), Request{ChannelID: 1, IssueNumber: 42})
	if !errors.Is(err, boom) {
		t.Errorf("active-task error = %v, want it to wrap the source error", err)
	}

	_, err = New(&stubTaskSource{activeErr: ErrNoTask, listErr: boom}).Resolve(context.Background(), Request{ChannelID: 1, IssueNumber: 42})
	if !errors.Is(err, boom) {
		t.Errorf("list error = %v, want it to wrap the source error", err)
	}
}

func TestResolve_NilTaskSource(t *testing.T) {
	bundle, err := New(nil).Resolve(context.Background(), Request{ChannelID: 1, IssueNumber: 42})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(bundle.Sections) != 0 {
		t.Fatalf("a nil source must contribute nothing: %+v", bundle.Sections)
	}
}

// HermesTaskSource remains usable as a name for the same interface.
func TestHermesTaskSourceAlias(t *testing.T) {
	var legacy HermesTaskSource = &stubTaskSource{activeErr: ErrNoTask}
	var current TaskSource = legacy
	if _, err := New(current).Resolve(context.Background(), Request{ChannelID: 1, IssueNumber: 1}); err != nil {
		t.Fatal(err)
	}
}
