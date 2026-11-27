package fleet

import (
	"context"
	"os"
	"slices"
	"testing"
)

func TestRunCodexScout(t *testing.T) {
	for _, success := range []bool{true, false} {
		name := "failed turn"
		if success {
			name = "completed turn"
		}
		t.Run(name, func(t *testing.T) {
			e := newRunnerEnv(t)
			e.runner.CodexScoutBinary = e.runner.Binary
			e.runner.CodexScoutModel = "test-model"
			last := `{"type":"turn.failed","error":{"message":"service unavailable"}}`
			if success {
				last = `{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}`
			}
			e.script(`{"type":"thread.started","thread_id":"ephemeral"}`,
				`{"type":"item.completed","item":{"type":"agent_message","text":"Scout findings"}}`, last)
			task := e.task(NewTask{Shape: ShapeScout, Goal: "Inspect the repository."})
			e.run(task.ID, RunOptions{})
			for _, flag := range []string{"read-only", "never", "--ephemeral", "--ignore-user-config"} {
				if !slices.Contains(e.args(1), flag) {
					t.Errorf("missing Codex Scout argument %q", flag)
				}
			}
			report, err := e.runner.ReportPath(task.ProjectID, task.ID)
			if err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(report)
			if success && (err != nil || string(body) != "Scout findings\n") {
				t.Fatalf("report = %q, %v", body, err)
			}
			if !success && !os.IsNotExist(err) {
				t.Fatalf("failed turn published a report: %v", err)
			}
		})
	}
}

func TestRunScoutRouteHonorsUSDBudget(t *testing.T) {
	for _, limited := range []bool{false, true} {
		name := "without USD budget"
		if limited {
			name = "with USD budget"
		}
		t.Run(name, func(t *testing.T) {
			e := newRunnerEnv(t)
			e.runner.CodexScoutBinary = e.runner.Binary
			e.runner.ScoutRoute = func(_ context.Context, _ Task, hasLimit bool) (bool, string) {
				if hasLimit != limited {
					t.Errorf("route saw USD budget = %v, want %v", hasLimit, limited)
				}
				return !hasLimit, "configured-codex-model"
			}
			in := NewTask{Shape: ShapeScout, Goal: "Inspect repository."}
			if limited {
				in.Budget.CostUSD = 10
				e.script(lineInit, lineResultOK)
			} else {
				e.script(`{"type":"thread.started","thread_id":"ephemeral"}`,
					`{"type":"item.completed","item":{"type":"agent_message","text":"Findings"}}`,
					`{"type":"turn.completed","usage":{"input_tokens":12,"output_tokens":3}}`)
			}
			res := e.run(e.task(in).ID, RunOptions{})
			if res.Task.Status != StatusVerifying {
				t.Fatalf("task = %s (%s)", res.Task.Status, res.Reason)
			}
			args := e.args(1)
			if slices.Contains(args, "--ephemeral") == limited || slices.Contains(args, "configured-codex-model") == limited {
				t.Fatalf("unexpected Scout arguments under USD budget %v: %q", limited, args)
			}
		})
	}
}
