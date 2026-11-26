package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/chimerakang/alice-core/worktree"
)

// fakeAgentScript stands in for the claude CLI. It records how it was
// invoked, then plays back "$FAKE_AGENT_DIR/script" line by line: plain lines
// go to stdout, lines starting with # are directives.
//
// Agents run side by side under the scheduler, so taking the next invocation
// number is done under a lock: without it two of them read the same count,
// and one invocation goes unrecorded.
const fakeAgentScript = `#!/bin/sh
D="$FAKE_AGENT_DIR"
until mkdir "$D/count.lock" 2>/dev/null; do sleep 0.01; done
n=$(cat "$D/count" 2>/dev/null || echo 0)
n=$((n+1))
echo "$n" > "$D/count"
rmdir "$D/count.lock"
: > "$D/args.$n"
for a in "$@"; do printf '%s\n' "$a" >> "$D/args.$n"; printf '\036\n' >> "$D/args.$n"; done
pwd -P > "$D/cwd.$n"
env > "$D/env.$n"
# A script for this exact invocation wins over the shared one.
script="$D/script"
if [ -f "$D/script.$n" ]; then script="$D/script.$n"; fi
while IFS= read -r line; do
  case "$line" in
    "#sleep "*)   sleep "${line#\#sleep }" ;;
    "#ignore-int") trap '' INT ;;
    "#on-int "*)  onint="${line#\#on-int }"; trap 'printf "%s\n" "$onint"; exit 130' INT ;;
    "#exit "*)    exit "${line#\#exit }" ;;
    "#stderr "*)  echo "${line#\#stderr }" >&2 ;;
    "#touch "*)   echo made-by-agent > "${line#\#touch }" ;;
    "#rm "*)      rm -f "${line#\#rm }" ;;
    "#begin")     echo "b $(pwd -P)" >> "$D/log" ;;
    "#end")       echo "e $(pwd -P)" >> "$D/log" ;;
    *)            printf '%s\n' "$line" ;;
  esac
done < "$script"
`

type runnerEnv struct {
	t      *testing.T
	store  *Store
	runner *Runner
	repo   string
	dir    string // fake agent's recording directory
}

func newRunnerEnv(t *testing.T) *runnerEnv {
	t.Helper()
	store := newStore(t)
	repo := newRepo(t, "app")
	if _, err := store.RegisterProject(ctx, Project{RepoPath: repo}); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	agent := filepath.Join(bin, "fake-claude")
	if err := os.WriteFile(agent, []byte(fakeAgentScript), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("FAKE_AGENT_DIR", dir)
	return &runnerEnv{
		t: t, store: store, repo: repo, dir: dir,
		runner: &Runner{
			Store:      store,
			Worktrees:  worktree.Layout{Root: filepath.Join(t.TempDir(), "worktrees")},
			ReportRoot: filepath.Join(t.TempDir(), "reports"),
			Binary:     agent,
			StopGrace:  5 * time.Second,
		},
	}
}

// script sets what the fake agent plays back on its next invocations.
func (e *runnerEnv) script(lines ...string) {
	e.t.Helper()
	if err := os.WriteFile(filepath.Join(e.dir, "script"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// scriptFor sets what the fake agent plays back on its n-th invocation only.
func (e *runnerEnv) scriptFor(n int, lines ...string) {
	e.t.Helper()
	path := filepath.Join(e.dir, "script."+strconv.Itoa(n))
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *runnerEnv) invocations() int {
	b, err := os.ReadFile(filepath.Join(e.dir, "count"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// args returns the arguments of the n-th invocation (1-based).
func (e *runnerEnv) args(n int) []string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "args."+strconv.Itoa(n)))
	if err != nil {
		e.t.Fatalf("agent invocation %d was not recorded: %v", n, err)
	}
	parts := strings.Split(string(b), "\n\x1e\n")
	return parts[:len(parts)-1]
}

func (e *runnerEnv) cwd(n int) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.dir, "cwd."+strconv.Itoa(n)))
	if err != nil {
		e.t.Fatalf("agent invocation %d was not recorded: %v", n, err)
	}
	return strings.TrimSpace(string(b))
}

func (e *runnerEnv) task(in NewTask) Task {
	e.t.Helper()
	return mustCreate(e.t, e.store, in)
}

func (e *runnerEnv) run(taskID string, opts RunOptions) RunResult {
	e.t.Helper()
	res, err := e.runner.Run(ctx, taskID, opts)
	if err != nil {
		e.t.Fatalf("Run: %v", err)
	}
	return res
}

func (e *runnerEnv) eventTypes(taskID string) []string {
	e.t.Helper()
	events, err := e.store.ListEvents(ctx, taskID, 0, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	var types []string
	for _, ev := range events {
		types = append(types, ev.Type)
	}
	return types
}

func (e *runnerEnv) eventPayloads(taskID, eventType string) []map[string]any {
	e.t.Helper()
	events, _ := e.store.ListEvents(ctx, taskID, 0, 0)
	var out []map[string]any
	for _, ev := range events {
		if ev.Type != eventType {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(ev.Payload, &m); err != nil {
			e.t.Fatalf("decode %s payload: %v", eventType, err)
		}
		out = append(out, m)
	}
	return out
}

// Stream lines as the real CLI emits them.
const (
	lineGoal      = `{"type":"active_goal","value":null,"uuid":"u0","session_id":"sess-1"}`
	lineInit      = `{"type":"system","subtype":"init","cwd":"/x","session_id":"sess-1","tools":["Read"],"model":"model-x"}`
	lineText      = `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Looking at the login test."}]},"session_id":"sess-1"}`
	lineToolUse   = `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"tu1","name":"Edit","input":{"file_path":"login_test.go","old_string":"a","new_string":"b"}}]},"session_id":"sess-1"}`
	lineToolRes   = `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tu1","is_error":false,"content":"ok"}]},"session_id":"sess-1"}`
	lineResultOK  = `{"type":"result","subtype":"success","is_error":false,"num_turns":3,"result":"Fixed the flaky test by removing the shared fixture.","session_id":"sess-1","total_cost_usd":0.25,"duration_ms":4200,"usage":{"input_tokens":100,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":50}}`
	lineResultAsk = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"有兩個方案：(A) 重寫 (B) 修補。你想先做哪個？","session_id":"sess-1","total_cost_usd":0.1,"usage":{"input_tokens":10,"output_tokens":5}}`
)

func TestRun_ShipLifecycle(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineGoal, lineInit, lineText, "this line is not json", "", lineToolUse, "#touch fixed.txt", lineToolRes, lineResultOK)
	task := e.task(NewTask{Goal: "Fix the flaky login test"})

	res := e.run(task.ID, RunOptions{})

	if res.Outcome != OutcomeFinished || res.Reason != "agent finished" ||
		res.Text != "Fixed the flaky test by removing the shared fixture." {
		t.Fatalf("result = %+v", res)
	}
	if res.Usage != (Usage{InputTokens: 1050, OutputTokens: 40, CostUSD: 0.25}) {
		t.Errorf("usage = %+v, want input to include cache tokens", res.Usage)
	}

	stored, _ := e.store.GetTask(ctx, task.ID)
	if stored.Status != StatusVerifying || stored.StatusReason != "agent finished" {
		t.Errorf("status = %s (%s)", stored.Status, stored.StatusReason)
	}
	if stored.SessionID != "sess-1" || stored.InputTokens != 1050 || stored.OutputTokens != 40 || stored.CostUSD != 0.25 {
		t.Errorf("session/usage = %+v", stored)
	}
	if stored.LastActivityAt == nil || stored.StartedAt == nil {
		t.Errorf("activity/start not recorded: %+v", stored)
	}

	// The agent ran inside a dedicated worktree on its own branch.
	wantPath, _ := e.runner.Worktrees.Path("app", task.ID)
	if stored.WorktreePath != wantPath || stored.Branch != "alice/"+task.ID {
		t.Errorf("workspace = %q on %q", stored.WorktreePath, stored.Branch)
	}
	resolved, _ := filepath.EvalSymlinks(wantPath)
	if e.cwd(1) != resolved {
		t.Errorf("agent cwd = %q, want the worktree %q", e.cwd(1), resolved)
	}
	if _, err := os.Stat(filepath.Join(wantPath, "fixed.txt")); err != nil {
		t.Errorf("the agent's change must land in the worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "fixed.txt")); err == nil {
		t.Error("the agent's change must not appear in the main working tree")
	}
	if got := gitT(t, wantPath, "rev-parse", "--abbrev-ref", "HEAD"); got != "alice/"+task.ID {
		t.Errorf("worktree branch = %q", got)
	}

	wantArgs := []string{"-p", "--output-format", "stream-json", "--verbose", "--max-turns", "100",
		"--dangerously-skip-permissions", "Fix the flaky login test"}
	if got := e.args(1); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("args =\n  %q\nwant\n  %q", got, wantArgs)
	}

	wantEvents := []string{
		EventStatus,   // created
		EventStatus,   // queued -> running
		EventLiveness, // unknown -> busy
		EventRunStarted,
		EventAgentInit, EventAgentText, EventAgentToolUse, EventAgentToolResult, EventAgentResult,
		EventLiveness, // busy -> idle
		EventStatus,   // running -> verifying
	}
	if got := e.eventTypes(task.ID); !reflect.DeepEqual(got, wantEvents) {
		t.Errorf("events =\n  %v\nwant\n  %v", got, wantEvents)
	}
	tool := e.eventPayloads(task.ID, EventAgentToolUse)[0]
	if tool["tool"] != "Edit" || tool["input"].(map[string]any)["file_path"] != "login_test.go" {
		t.Errorf("tool_use payload = %v", tool)
	}
	if text := e.eventPayloads(task.ID, EventAgentText)[0]["text"]; text != "Looking at the login test." {
		t.Errorf("text payload = %v (thinking must not be recorded as text)", text)
	}
	if result := e.eventPayloads(task.ID, EventAgentResult)[0]; result["num_turns"] != float64(3) || result["cost_usd"] != 0.25 {
		t.Errorf("result payload = %v", result)
	}
	if e.runner.Running(task.ID) {
		t.Error("a finished run must not be listed as running")
	}
}

func TestRun_CleansNestedAgentEnvironment(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultOK)
	t.Setenv("CLAUDECODE", "1")
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})

	env, _ := os.ReadFile(filepath.Join(e.dir, "env.1"))
	if strings.Contains(string(env), "\nCLAUDECODE=") || strings.HasPrefix(string(env), "CLAUDECODE=") {
		t.Error("CLAUDECODE leaked into the agent's environment")
	}
	if !strings.Contains(string(env), "ALICE_SKIP_HOOKS=1") {
		t.Error("ALICE_SKIP_HOOKS=1 must be set for the agent")
	}
}

func TestRun_ModelAndTurnLimitFlags(t *testing.T) {
	flagValue := func(args []string, flag string) (string, bool) {
		for i, a := range args {
			if a == flag && i+1 < len(args) {
				return args[i+1], true
			}
		}
		return "", false
	}
	cases := []struct {
		name         string
		taskModel    string
		defaultModel string
		maxTurns     int
		wantModel    string
		wantTurns    string
	}{
		{"no model anywhere lets the CLI choose", "", "", 0, "", "100"},
		{"runner default", "", "default-model", 0, "default-model", "100"},
		{"task model wins", "task-model", "default-model", 25, "task-model", "25"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunnerEnv(t)
			e.script(lineInit, lineResultOK)
			e.runner.DefaultModel, e.runner.MaxTurns = tc.defaultModel, tc.maxTurns
			task := e.task(NewTask{Model: tc.taskModel})
			e.run(task.ID, RunOptions{})

			model, hasModel := flagValue(e.args(1), "--model")
			if model != tc.wantModel || hasModel != (tc.wantModel != "") {
				t.Errorf("--model = %q (present=%v), want %q", model, hasModel, tc.wantModel)
			}
			if turns, _ := flagValue(e.args(1), "--max-turns"); turns != tc.wantTurns {
				t.Errorf("--max-turns = %q, want %q", turns, tc.wantTurns)
			}
		})
	}
}

func TestRun_IssueTaskPrompt(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultOK)
	task := e.task(NewTask{Source: Source{Kind: SourceIssue, IssueNumber: 42}})
	e.run(task.ID, RunOptions{})
	args := e.args(1)
	if prompt := args[len(args)-1]; !strings.Contains(prompt, "GitHub issue #42") || !strings.Contains(prompt, "gh issue view 42") {
		t.Fatalf("prompt = %q", prompt)
	}
}

// A question ends the turn successfully, but the task is not finished.
func TestRun_QuestionThenFollowUpResumesSession(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#touch draft.txt", lineResultAsk)
	task := e.task(NewTask{Goal: "Refactor the uploader"})

	first := e.run(task.ID, RunOptions{})
	if first.Outcome != OutcomeNeedsDecision || first.Reason != "agent is waiting for an answer" {
		t.Fatalf("first run = %+v", first)
	}
	if first.Task.Status != StatusNeedsDecision || first.Task.SessionID != "sess-1" {
		t.Fatalf("task after question = %+v", first.Task)
	}

	e.script(lineInit, lineResultOK)
	second := e.run(task.ID, RunOptions{Instruction: "  用方案 B  "})
	if second.Outcome != OutcomeFinished {
		t.Fatalf("second run = %+v", second)
	}

	wantArgs := []string{"-p", "--output-format", "stream-json", "--verbose", "--max-turns", "100",
		"--dangerously-skip-permissions", "--resume", "sess-1", "用方案 B"}
	if got := e.args(2); !reflect.DeepEqual(got, wantArgs) {
		t.Errorf("follow-up args =\n  %q\nwant\n  %q", got, wantArgs)
	}
	if e.cwd(2) != e.cwd(1) {
		t.Errorf("the follow-up must run in the same worktree: %q vs %q", e.cwd(2), e.cwd(1))
	}
	if _, err := os.Stat(filepath.Join(second.Task.WorktreePath, "draft.txt")); err != nil {
		t.Errorf("work from the first run must still be there: %v", err)
	}
	// Usage accumulates across runs.
	if second.Task.InputTokens != 10+1050 || second.Task.OutputTokens != 5+40 {
		t.Errorf("accumulated usage = %d/%d", second.Task.InputTokens, second.Task.OutputTokens)
	}
	if got := len(e.eventPayloads(task.ID, EventRunStarted)); got != 2 {
		t.Errorf("run.started events = %d, want 2", got)
	}
	if resumed := e.eventPayloads(task.ID, EventRunStarted)[1]["resumed"]; resumed != true {
		t.Errorf("second run.started must be marked resumed, got %v", resumed)
	}
}

func TestRun_ResumeWithoutInstructionContinues(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})

	e.script(lineInit, lineResultOK)
	e.run(task.ID, RunOptions{})
	args := e.args(2)
	if args[len(args)-1] != "Continue the task from where you left off." || args[len(args)-3] != "--resume" {
		t.Fatalf("args = %q", args)
	}
}

// A retry goes back through the queue and starts a fresh session.
func TestRun_FromQueuedNeverResumes(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{Goal: "the goal"})
	e.run(task.ID, RunOptions{})
	walk(t, e.store, task.ID, StatusFailed, StatusQueued)

	e.script(`{"type":"system","subtype":"init","session_id":"sess-2"}`,
		`{"type":"result","subtype":"success","result":"done","session_id":"sess-2"}`)
	res := e.run(task.ID, RunOptions{})

	args := e.args(2)
	for _, a := range args {
		if a == "--resume" {
			t.Fatalf("a run from queued must not resume: %q", args)
		}
	}
	if args[len(args)-1] != "the goal" {
		t.Errorf("a fresh run must send the goal again, got %q", args[len(args)-1])
	}
	if res.Task.SessionID != "sess-2" {
		t.Errorf("SessionID = %q, want the new session", res.Task.SessionID)
	}
	if e.cwd(2) != e.cwd(1) {
		t.Error("a retry keeps the task's worktree")
	}
}

func TestRun_FailureModes(t *testing.T) {
	cases := []struct {
		name       string
		script     []string
		wantStatus Status
		wantReason string // substring
	}{
		{
			name: "turn limit",
			script: []string{lineInit,
				`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":100,"session_id":"sess-1"}`},
			wantStatus: StatusNeedsDecision, wantReason: "reached the turn limit (100)",
		},
		{
			name: "agent reports an error",
			script: []string{lineInit,
				`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API overloaded","session_id":"sess-1"}`},
			wantStatus: StatusNeedsDecision, wantReason: "agent error: API overloaded",
		},
		{
			name:       "dies after starting a session",
			script:     []string{lineInit, lineText, "#stderr segfault in tool", "#exit 3"},
			wantStatus: StatusNeedsDecision, wantReason: "agent exited without a result: segfault in tool",
		},
		{
			name:       "non-zero exit after a successful result",
			script:     []string{lineInit, lineResultOK, "#stderr cleanup failed", "#exit 1"},
			wantStatus: StatusNeedsDecision, wantReason: "agent exited with an error: cleanup failed",
		},
		{
			name:       "never produces output",
			script:     []string{"#stderr not logged in", "#exit 1"},
			wantStatus: StatusFailed, wantReason: "agent produced no output: not logged in",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunnerEnv(t)
			e.script(tc.script...)
			task := e.task(NewTask{})
			res := e.run(task.ID, RunOptions{})
			if res.Task.Status != tc.wantStatus || !strings.Contains(res.Reason, tc.wantReason) {
				t.Fatalf("status = %s, reason = %q; want %s containing %q", res.Task.Status, res.Reason, tc.wantStatus, tc.wantReason)
			}
			if res.Task.StatusReason != res.Reason {
				t.Errorf("stored reason %q != result reason %q", res.Task.StatusReason, res.Reason)
			}
			if res.Outcome != outcomeFor(tc.wantStatus) {
				t.Errorf("outcome = %s", res.Outcome)
			}
			if e.runner.Running(task.ID) {
				t.Error("the run must be released")
			}
		})
	}
}

func TestRun_SetupFailures(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		e := newRunnerEnv(t)
		e.runner.Binary = filepath.Join(t.TempDir(), "no-such-agent")
		task := e.task(NewTask{})
		res := e.run(task.ID, RunOptions{})
		if res.Task.Status != StatusFailed || !strings.Contains(res.Reason, "start agent") {
			t.Fatalf("result = %+v", res)
		}
	})

	t.Run("worktree cannot be created", func(t *testing.T) {
		e := newRunnerEnv(t)
		e.script(lineInit, lineResultOK)
		task := e.task(NewTask{})
		// Occupy the branch name the task would use.
		gitT(t, e.repo, "branch", "alice/"+task.ID)
		res := e.run(task.ID, RunOptions{})
		if res.Task.Status != StatusFailed || !strings.Contains(res.Reason, "prepare workspace") {
			t.Fatalf("result = %+v", res)
		}
		if e.invocations() != 0 {
			t.Error("the agent must not start without a workspace")
		}
	})

	t.Run("scout without a report root", func(t *testing.T) {
		e := newRunnerEnv(t)
		e.runner.ReportRoot = ""
		task := e.task(NewTask{Shape: ShapeScout})
		res := e.run(task.ID, RunOptions{})
		if res.Task.Status != StatusFailed || !strings.Contains(res.Reason, "ReportRoot") || e.invocations() != 0 {
			t.Fatalf("result = %+v, invocations = %d", res, e.invocations())
		}
	})
}

func TestRun_ClaimErrors(t *testing.T) {
	e := newRunnerEnv(t)
	if _, err := e.runner.Run(ctx, "missing", RunOptions{}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("missing task: %v", err)
	}

	done := e.task(NewTask{})
	walk(t, e.store, done.ID, StatusCancelled)
	if _, err := e.runner.Run(ctx, done.ID, RunOptions{}); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("finished task: %v", err)
	}
	if got, _ := e.store.GetTask(ctx, done.ID); got.Status != StatusCancelled {
		t.Errorf("a refused run changed the task to %s", got.Status)
	}
	if e.invocations() != 0 {
		t.Error("no agent may start for a task that could not be claimed")
	}
	if _, err := (&Runner{}).Run(ctx, "x", RunOptions{}); err == nil {
		t.Error("a Runner without a Store must refuse to run")
	}
}

// waitForStart blocks until the fake agent has been launched for the task.
func (e *runnerEnv) waitForStart(taskID string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.eventPayloads(taskID, EventAgentInit)) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatal("the agent did not start in time")
}

type runOutcome struct {
	res RunResult
	err error
}

func (e *runnerEnv) runAsync(c context.Context, taskID string) <-chan runOutcome {
	out := make(chan runOutcome, 1)
	go func() {
		res, err := e.runner.Run(c, taskID, RunOptions{})
		out <- runOutcome{res, err}
	}()
	return out
}

func awaitRun(t *testing.T, out <-chan runOutcome) RunResult {
	t.Helper()
	select {
	case o := <-out:
		if o.err != nil {
			t.Fatalf("Run: %v", o.err)
		}
		return o.res
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
		return RunResult{}
	}
}

func TestCancel_InterruptsGracefully(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, `#on-int {"type":"result","subtype":"error_during_execution","is_error":true,"result":"interrupted","session_id":"sess-1"}`, "#sleep 30", lineResultOK)
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	if !e.runner.Running(task.ID) {
		t.Fatal("the run must be listed while the agent is alive")
	}
	start := time.Now()
	if err := e.runner.Cancel(task.ID, "owner withdrew it"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := awaitRun(t, out)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a cooperative agent must stop on the interrupt, took %s (grace is %s)", elapsed, e.runner.StopGrace)
	}

	if res.Outcome != OutcomeCancelled || res.Task.Status != StatusCancelled || res.Reason != "owner withdrew it" {
		t.Fatalf("result = %+v", res)
	}
	if res.Task.FinishedAt == nil {
		t.Error("a cancelled task must have a finish time")
	}
	stop := e.eventPayloads(task.ID, EventRunStop)
	if len(stop) != 1 || stop[0]["kind"] != "cancel" || stop[0]["reason"] != "owner withdrew it" {
		t.Errorf("run.stop = %v", stop)
	}
	// What the agent printed while shutting down is still recorded.
	if len(e.eventPayloads(task.ID, EventAgentResult)) != 1 {
		t.Error("output emitted during shutdown must be kept")
	}
	if err := e.runner.Cancel(task.ID, ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Cancel after the run ended: %v", err)
	}
}

func TestCancel_KillsAnAgentThatIgnoresTheInterrupt(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.StopGrace = 400 * time.Millisecond
	e.script(lineInit, "#ignore-int", "#sleep 30", lineResultOK)
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	start := time.Now()
	if err := e.runner.Cancel(task.ID, ""); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	res := awaitRun(t, out)
	elapsed := time.Since(start)

	if elapsed < 350*time.Millisecond {
		t.Errorf("the agent was killed after %s, before the grace period ended", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("the kill took %s", elapsed)
	}
	if res.Task.Status != StatusCancelled || res.Reason != "cancelled" {
		t.Fatalf("result = %+v", res)
	}
	if len(e.eventPayloads(task.ID, EventAgentResult)) != 0 {
		t.Error("the agent must not have reached its result line")
	}
}

func TestInterrupt_LeavesTheSessionResumable(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#touch partial.txt", "#sleep 30")
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	if err := e.runner.Interrupt(task.ID, "wrong approach"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	res := awaitRun(t, out)
	if res.Outcome != OutcomeNeedsDecision || res.Task.Status != StatusNeedsDecision || res.Reason != "wrong approach" {
		t.Fatalf("result = %+v", res)
	}
	if res.Task.SessionID != "sess-1" {
		t.Fatalf("the session must survive an interrupt, got %q", res.Task.SessionID)
	}
	if stop := e.eventPayloads(task.ID, EventRunStop); len(stop) != 1 || stop[0]["kind"] != "interrupt" {
		t.Errorf("run.stop = %v", stop)
	}

	e.script(lineInit, lineResultOK)
	again := e.run(task.ID, RunOptions{Instruction: "try the other way"})
	if again.Outcome != OutcomeFinished {
		t.Fatalf("resumed run = %+v", again)
	}
	args := e.args(2)
	if args[len(args)-3] != "--resume" || args[len(args)-2] != "sess-1" || args[len(args)-1] != "try the other way" {
		t.Errorf("resume args = %q", args)
	}
	if _, err := os.Stat(filepath.Join(again.Task.WorktreePath, "partial.txt")); err != nil {
		t.Errorf("partial work must survive the interrupt: %v", err)
	}
}

// A cancel must win over an earlier interrupt, never the other way round.
func TestCancel_OverridesInterrupt(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.StopGrace = 300 * time.Millisecond
	e.script(lineInit, "#ignore-int", "#sleep 30")
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	if err := e.runner.Interrupt(task.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if err := e.runner.Cancel(task.ID, "actually, drop it"); err != nil {
		t.Fatal(err)
	}
	if err := e.runner.Interrupt(task.ID, "too late"); err != nil {
		t.Fatal(err)
	}
	res := awaitRun(t, out)
	if res.Task.Status != StatusCancelled || res.Reason != "actually, drop it" {
		t.Fatalf("result = %+v", res)
	}
}

func TestStop_NotRunning(t *testing.T) {
	e := newRunnerEnv(t)
	task := e.task(NewTask{})
	if err := e.runner.Cancel(task.ID, ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Cancel on a queued task: %v", err)
	}
	if err := e.runner.Interrupt("missing", ""); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Interrupt on a missing task: %v", err)
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusQueued {
		t.Errorf("a refused stop changed the task to %s", got.Status)
	}
}

// When the host shuts down, the task must not be left claiming to run.
func TestRun_ContextCancelLeavesTaskRecoverable(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})
	c, cancel := context.WithCancel(ctx)
	out := e.runAsync(c, task.ID)
	e.waitForStart(task.ID)

	cancel()
	res := awaitRun(t, out)
	if res.Task.Status != StatusNeedsDecision || !strings.Contains(res.Reason, "runner stopped") {
		t.Fatalf("result = %+v", res)
	}
	if res.Task.SessionID != "sess-1" {
		t.Errorf("the session must be kept for a later resume, got %q", res.Task.SessionID)
	}
}

func TestRun_Timeout(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.RunTimeout = 400 * time.Millisecond
	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})

	start := time.Now()
	res := e.run(task.ID, RunOptions{})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the timeout was not enforced, took %s", elapsed)
	}
	if res.Task.Status != StatusNeedsDecision || res.Reason != "timed out after 400ms" {
		t.Fatalf("result = %+v", res)
	}
}

// A second Run for a task that is already running must not start a second agent.
func TestRun_SecondRunIsRefusedWhileRunning(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#sleep 30")
	task := e.task(NewTask{})
	out := e.runAsync(ctx, task.ID)
	e.waitForStart(task.ID)

	if _, err := e.runner.Run(ctx, task.ID, RunOptions{}); !errors.Is(err, ErrInvalidTransition) && !errors.Is(err, ErrConflict) {
		t.Fatalf("second Run: err = %v, want a refused claim", err)
	}
	if e.invocations() != 1 {
		t.Fatalf("agent invocations = %d, want 1", e.invocations())
	}
	if !e.runner.Running(task.ID) {
		t.Fatal("the refused Run must not release the first run")
	}
	if err := e.runner.Cancel(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	awaitRun(t, out)
}

func TestRun_Scout(t *testing.T) {
	e := newRunnerEnv(t)
	report := "# Why CI is slow\\n\\nThe integration suite runs serially (ci.yml:42)."
	e.script(lineInit, `{"type":"result","subtype":"success","result":"`+report+`","session_id":"sess-1","total_cost_usd":0.05}`)
	task := e.task(NewTask{Shape: ShapeScout, Goal: "Why is CI slow?"})

	res := e.run(task.ID, RunOptions{})

	if res.Outcome != OutcomeFinished || res.Task.Status != StatusVerifying || res.Reason != "report written" {
		t.Fatalf("result = %+v", res)
	}
	wantPath, _ := e.runner.ReportPath("app", task.ID)
	if res.ReportPath != wantPath || !strings.HasSuffix(wantPath, filepath.Join("app", task.ID, "report.md")) {
		t.Errorf("ReportPath = %q, want %q", res.ReportPath, wantPath)
	}
	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("report was not written: %v", err)
	}
	if want := "# Why CI is slow\n\nThe integration suite runs serially (ci.yml:42).\n"; string(body) != want {
		t.Errorf("report =\n%q\nwant\n%q", body, want)
	}
	if written := e.eventPayloads(task.ID, EventReport); len(written) != 1 || written[0]["path"] != wantPath {
		t.Errorf("report.written = %v", written)
	}

	args := e.args(1)
	joined := strings.Join(args[:len(args)-1], " ")
	if !strings.Contains(joined, "--tools Read,Grep,Glob --strict-mcp-config") {
		t.Errorf("a scout must be limited to read-only tools: %q", joined)
	}
	if strings.Contains(joined, "--dangerously-skip-permissions") {
		t.Errorf("a scout must not bypass permissions: %q", joined)
	}
	prompt := args[len(args)-1]
	if !strings.Contains(prompt, "read-only") || !strings.Contains(prompt, "report.md") || !strings.HasSuffix(prompt, "Why is CI slow?") {
		t.Errorf("scout prompt = %q", prompt)
	}

	// The report lives outside the checkout, so the checkout stays clean.
	if dirty, err := worktree.IsDirty(ctx, res.Task.WorktreePath); err != nil || dirty {
		t.Errorf("scout worktree dirty = %v, %v", dirty, err)
	}
}

func TestRun_ScoutCustomToolsAndEmptyReport(t *testing.T) {
	e := newRunnerEnv(t)
	e.runner.ScoutTools = []string{"Read", "WebFetch"}
	e.script(lineInit, `{"type":"result","subtype":"success","result":"   ","session_id":"sess-1"}`)
	task := e.task(NewTask{Shape: ShapeScout, Goal: "look"})

	res := e.run(task.ID, RunOptions{})
	if res.Task.Status != StatusNeedsDecision || res.Reason != "scout produced no report" || res.ReportPath != "" {
		t.Fatalf("result = %+v", res)
	}
	path, _ := e.runner.ReportPath("app", task.ID)
	if _, err := os.Stat(path); err == nil {
		t.Error("no report file may be written for an empty report")
	}
	if joined := strings.Join(e.args(1), " "); !strings.Contains(joined, "--tools Read,WebFetch ") {
		t.Errorf("custom scout tools not used: %q", joined)
	}
}

// A scout that ends with a question still produced a report; it is not asking.
func TestRun_ScoutQuestionIsStillAReport(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, lineResultAsk)
	task := e.task(NewTask{Shape: ShapeScout, Goal: "compare options"})
	res := e.run(task.ID, RunOptions{})
	if res.Task.Status != StatusVerifying || res.ReportPath == "" {
		t.Fatalf("result = %+v", res)
	}
}

// If the checkout disappears but the branch survives, a resume re-attaches it.
func TestRun_ReattachesAMissingWorktree(t *testing.T) {
	e := newRunnerEnv(t)
	e.script(lineInit, "#touch kept.txt", lineResultAsk)
	task := e.task(NewTask{})
	first := e.run(task.ID, RunOptions{})

	gitT(t, first.Task.WorktreePath, "add", ".")
	gitT(t, first.Task.WorktreePath, "commit", "-q", "-m", "wip")
	if err := worktree.Remove(ctx, first.Task.WorktreePath, false); err != nil {
		t.Fatal(err)
	}

	e.script(lineInit, lineResultOK)
	second := e.run(task.ID, RunOptions{})
	if second.Outcome != OutcomeFinished {
		t.Fatalf("result = %+v", second)
	}
	if second.Task.WorktreePath != first.Task.WorktreePath || second.Task.Branch != first.Task.Branch {
		t.Errorf("workspace changed: %q/%q", second.Task.WorktreePath, second.Task.Branch)
	}
	if _, err := os.Stat(filepath.Join(second.Task.WorktreePath, "kept.txt")); err != nil {
		t.Errorf("committed work must come back with the branch: %v", err)
	}
}

func TestRun_LargeOutputIsClipped(t *testing.T) {
	e := newRunnerEnv(t)
	bigText := strings.Repeat("字", 20000) // 60 KB of 3-byte runes
	bigInput := strings.Repeat("x", 10000)
	e.script(lineInit,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"`+bigText+`"},{"type":"tool_use","id":"t","name":"Write","input":{"content":"`+bigInput+`"}}]},"session_id":"sess-1"}`,
		lineResultOK)
	task := e.task(NewTask{})
	e.run(task.ID, RunOptions{})

	text := e.eventPayloads(task.ID, EventAgentText)[0]["text"].(string)
	if len(text) > maxEventText+64 || !strings.Contains(text, "more bytes]") {
		t.Errorf("text event is %d bytes, want it clipped near %d", len(text), maxEventText)
	}
	if !strings.HasPrefix(text, "字字字") || !utf8.ValidString(text) {
		t.Error("clipping must not split a multi-byte character")
	}
	input := e.eventPayloads(task.ID, EventAgentToolUse)[0]["input"].(map[string]any)
	if input["truncated"] != true || input["bytes"] != float64(len(`{"content":"`+bigInput+`"}`)) {
		t.Errorf("large tool input must be replaced by a preview: %v", input)
	}
}

func TestClip(t *testing.T) {
	if got := clip("short", 10); got != "short" {
		t.Errorf("clip under the limit = %q", got)
	}
	if got := clip("abcdefghij", 4); got != "abcd… [6 more bytes]" {
		t.Errorf("clip = %q", got)
	}
	// "é" is two bytes; cutting inside it must back up to the boundary.
	if got := clip("aé", 2); got != "a… [2 more bytes]" {
		t.Errorf("clip on a rune boundary = %q", got)
	}
	if got := clipJSON(nil, 10); got != nil {
		t.Errorf("clipJSON(nil) = %v", got)
	}
	small := json.RawMessage(`{"a":1}`)
	if got := clipJSON(small, 100); fmt.Sprint(got) != fmt.Sprint(small) {
		t.Errorf("small input must be kept as-is: %v", got)
	}
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 5}
	for _, chunk := range []string{"abc", "defg", "h"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got := b.String(); got != "defgh" {
		t.Fatalf("tail = %q, want the last 5 bytes", got)
	}
}
