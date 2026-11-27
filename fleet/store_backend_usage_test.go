package fleet

import (
	"reflect"
	"testing"
	"time"
)

func TestUsageByBackend(t *testing.T) {
	store := storeWithProject(t)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	clock := &settableClock{now: start}
	store.now = clock.Now
	task := mustCreate(t, store, NewTask{})

	if err := store.AddUsage(ctx, task.ID, Usage{InputTokens: 2}); err != nil {
		t.Fatal(err)
	}
	add := func(backend string) {
		t.Helper()
		if err := store.AddBackendUsage(ctx, task.ID, backend, "test-model", Usage{InputTokens: 3, OutputTokens: 5, CostUSD: 0.25}); err != nil {
			t.Fatal(err)
		}
	}
	add("claude")
	add("claude")
	add("codex")
	end := start.Add(time.Hour)
	clock.Advance(time.Hour)
	add("codex")

	got, err := store.UsageByBackend(ctx, start, end)
	if err != nil {
		t.Fatal(err)
	}
	want := []BackendUsage{
		{InputTokens: 2, Tasks: 1},
		{Backend: "claude", InputTokens: 6, OutputTokens: 10, CostUSD: 0.5, Tasks: 1},
		{Backend: "codex", InputTokens: 3, OutputTokens: 5, CostUSD: 0.25, Tasks: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UsageByBackend = %+v, want %+v", got, want)
	}
}
