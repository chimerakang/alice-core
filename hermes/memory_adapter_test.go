package hermes

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/alice-core/memory"
)

type stubTaskReader struct {
	active    TaskState
	activeErr error
	history   []TaskState
	listErr   error
}

func (s stubTaskReader) GetActiveForChat(int64) (TaskState, error) { return s.active, s.activeErr }
func (s stubTaskReader) ListForChat(int64, int) ([]TaskState, error) {
	return s.history, s.listErr
}

func sampleState() TaskState {
	return TaskState{
		ID: "task-1", Goal: "Add worktree package", Status: TaskStatusExecuting, GithubIssueNumber: 42,
		Accumulated: "skeleton created", UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Plan: []SubTask{
			{ID: "s1", Description: "write code", Status: SubTaskDone, Result: "ok", TokensUsed: 10},
			{ID: "s2", Description: "write tests", Status: SubTaskPending},
		},
	}
}

func TestMemoryTaskSummary(t *testing.T) {
	want := memory.TaskSummary{
		ID: "task-1", Status: "executing", IssueNumber: 42, Goal: "Add worktree package",
		UpdatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Progress: "skeleton created",
		Steps: []memory.TaskStep{
			{Description: "write code", Status: "done", Result: "ok"},
			{Description: "write tests", Status: "pending"},
		},
	}
	if got := MemoryTaskSummary(sampleState()); !reflect.DeepEqual(got, want) {
		t.Fatalf("summary =\n  %+v\nwant\n  %+v", got, want)
	}
	if got := MemoryTaskSummary(TaskState{}); got.Steps != nil || got.ID != "" {
		t.Fatalf("zero state = %+v", got)
	}
}

func TestMemoryTaskSource(t *testing.T) {
	src := MemoryTaskSource(stubTaskReader{active: sampleState(), history: []TaskState{sampleState(), {ID: "task-0"}}})

	active, err := src.GetActiveForChat(1)
	if err != nil || active.ID != "task-1" || len(active.Steps) != 2 {
		t.Fatalf("active = %+v, err = %v", active, err)
	}
	history, err := src.ListForChat(1, 5)
	if err != nil || len(history) != 2 || history[1].ID != "task-0" {
		t.Fatalf("history = %+v, err = %v", history, err)
	}
}

func TestMemoryTaskSource_TranslatesErrNoTask(t *testing.T) {
	for name, err := range map[string]error{"sentinel": ErrNoTask, "wrapped": fmt.Errorf("lookup: %w", ErrNoTask)} {
		src := MemoryTaskSource(stubTaskReader{activeErr: err})
		if _, got := src.GetActiveForChat(1); !errors.Is(got, memory.ErrNoTask) {
			t.Errorf("%s: err = %v, want memory.ErrNoTask", name, got)
		}
	}

	boom := errors.New("database is locked")
	src := MemoryTaskSource(stubTaskReader{activeErr: boom, listErr: boom})
	if _, err := src.GetActiveForChat(1); !errors.Is(err, boom) || errors.Is(err, memory.ErrNoTask) {
		t.Errorf("other errors must pass through unchanged, got %v", err)
	}
	if _, err := src.ListForChat(1, 3); !errors.Is(err, boom) {
		t.Errorf("list error = %v", err)
	}
}

// A nil reader must become a nil interface, not a non-nil interface holding a
// nil reader, or the resolver would call into it and panic.
func TestMemoryTaskSource_NilReader(t *testing.T) {
	if src := MemoryTaskSource(nil); src != nil {
		t.Fatalf("MemoryTaskSource(nil) = %#v, want nil", src)
	}
}

func TestMemoryTaskSource_EndToEnd(t *testing.T) {
	resolver := memory.New(MemoryTaskSource(stubTaskReader{active: sampleState()}))
	bundle, err := resolver.Resolve(context.Background(), memory.Request{ChannelID: 1, IssueNumber: 42})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(bundle.Sections) != 1 {
		t.Fatalf("sections = %+v", bundle.Sections)
	}
	text := bundle.Sections[0].Text
	for _, want := range []string{"- Task ID: task-1", "- Status: executing", "1. [done] write code", "2. [pending] write tests"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered memory is missing %q:\n%s", want, text)
		}
	}
}
