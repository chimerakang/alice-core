package fleet

import (
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
