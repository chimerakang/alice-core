package engine

import (
	"reflect"
	"testing"

	"github.com/chimerakang/alice-core/hermes"
	"github.com/chimerakang/alice-core/issueops"
)

func TestWorkItems(t *testing.T) {
	tasks := []hermes.SubTask{
		{ID: "s1", Description: "done", Status: hermes.SubTaskDone, ChecklistItemIDs: []string{"item-1"}, Result: "ok", TokensUsed: 9},
		{ID: "s2", Description: "failed", Status: hermes.SubTaskFailed},
		{ID: "s3", Description: "pending", Status: hermes.SubTaskPending},
	}
	want := []issueops.WorkItem{
		{ID: "s1", Description: "done", Done: true, ChecklistItemIDs: []string{"item-1"}},
		{ID: "s2", Description: "failed"},
		{ID: "s3", Description: "pending"},
	}
	if got := workItems(tasks); !reflect.DeepEqual(got, want) {
		t.Fatalf("workItems =\n  %+v\nwant\n  %+v", got, want)
	}
	if got := workItems(nil); got == nil || len(got) != 0 {
		t.Fatalf("workItems(nil) = %#v, want an empty non-nil slice", got)
	}
}
