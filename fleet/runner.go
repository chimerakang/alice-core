package fleet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chimerakang/alice-core/chat"
	"github.com/chimerakang/alice-core/client"
	"github.com/chimerakang/alice-core/process"
	"github.com/chimerakang/alice-core/worktree"
)

// Runner executes tasks: one agent process per task, in the task's own
// worktree, with everything the agent emits recorded to the task's event log.
//
// A Runner does not decide when a task runs or what happens after it; it runs
// one task to the end of one agent turn and leaves it in the status that
// describes how the turn ended.
type Runner struct {
	// Store holds the tasks. Required.
	Store *Store
	// Worktrees decides where task checkouts live. Required.
	Worktrees worktree.Layout
	// ReportRoot is where scout reports are written, as
	// <ReportRoot>/<projectID>/<taskID>/report.md. Required to run scouts.
	ReportRoot string

	// Binary is the agent executable. Defaults to "claude".
	Binary string
	// CodexScoutBinary enables Codex for scouts. Ship still uses Binary.
	CodexScoutBinary string
	CodexScoutModel  string
	// ScoutRoute optionally selects the backend and model per Scout.
	ScoutRoute func(context.Context, Task, bool) (useCodex bool, model string)
	// MatchQuotaError optionally recognizes provider-specific diagnostics from a failed run.
	MatchQuotaError func(backend, diagnostic string) (exhausted bool, resetsAt time.Time)
	// ScoutQuotaFallback authorizes an alternate backend and selects its model.
	// MaxQuotaTransfers bounds the automatic transfers per task. Zero disables them.
	ScoutQuotaFallback func(context.Context, Task, string) (model string, eligible bool)
	MaxQuotaTransfers  int
	// DefaultModel is used for tasks that do not name a model. Empty lets
	// the CLI choose.
	DefaultModel string
	// MaxTurns caps agent turns per run. Zero means DefaultMaxTurns.
	MaxTurns int
	// ScoutTools are the only built-in tools a scout may use. Defaults to
	// DefaultScoutTools, which can read but not write or execute.
	ScoutTools []string
	// RunTimeout caps one run's wall-clock time. Zero means DefaultRunTimeout.
	RunTimeout time.Duration
	// StopGrace is how long a stopped agent gets to exit after the interrupt
	// signal before it is killed. Zero means DefaultStopGrace.
	StopGrace time.Duration
	// Budget, when set, is the spending policy. A run is refused with a
	// *BudgetError once the task, its project's day, or the fleet's day has
	// reached its limit. The Scheduler and Watcher enforce the same policy.
	Budget *Budget

	mu   sync.Mutex
	runs map[string]*activeRun
}

// Runner defaults.
const (
	DefaultMaxTurns   = 100
	DefaultRunTimeout = 2 * time.Hour
	DefaultStopGrace  = 10 * time.Second
)

// DefaultScoutTools is the read-only tool set given to scout tasks.
var DefaultScoutTools = []string{"Read", "Grep", "Glob"}

// Event types written by the Runner.
const (
	// EventAgentInit: the agent process reported its session.
	EventAgentInit = "agent.init"
	// EventAgentText: the agent said something.
	EventAgentText = "agent.text"
	// EventAgentToolUse: the agent called a tool.
	EventAgentToolUse = "agent.tool_use"
	// EventAgentToolResult: a tool call came back.
	EventAgentToolResult = "agent.tool_result"
	// EventAgentResult: the agent's turn ended and reported its totals.
	EventAgentResult = "agent.result"
	// EventRunStarted: a run was launched. Not a sign of agent life.
	EventRunStarted = "run.started"
	// EventRunStop: a stop was requested for the run.
	EventRunStop = "run.stop"
	// EventReport: a scout report was written.
	EventReport = "report.written"
)

// Outcome says how a run ended.
type Outcome string

const (
	// OutcomeFinished: the agent completed its turn; the task is verifying.
	OutcomeFinished Outcome = "finished"
	OutcomeRequeued Outcome = "requeued"
	// OutcomeNeedsDecision: the run stopped short and the owner must decide.
	OutcomeNeedsDecision Outcome = "needs_decision"
	// OutcomeFailed: the run could not be carried out at all.
	OutcomeFailed Outcome = "failed"
	// OutcomeCancelled: the owner cancelled the task.
	OutcomeCancelled Outcome = "cancelled"
)

// RunOptions adjusts one run.
type RunOptions struct {
	// Instruction is sent to the agent instead of the default prompt. On a
	// task that already has a session it is a follow-up: an answer to the
	// agent's question, or more instructions.
	Instruction string
	// IgnoreBudget lets this one run start even though a budget limit has
	// been reached. It is the owner's explicit override.
	IgnoreBudget bool
	// OnStart, when set, is called once the agent process is running, with
	// the task as it stands then (workspace assigned). It must return
	// promptly: the agent's output is not read until it does.
	OnStart       func(Task)
	startupResume *startupResume
}

// RunResult describes a finished run.
type RunResult struct {
	// Quota is set when the provider rejected a failed run for lack of capacity.
	Quota *QuotaExhaustion
	// Task is the task as stored after the run.
	Task    Task
	Outcome Outcome
	// Reason explains the outcome; it is also the task's StatusReason.
	Reason string
	// Text is the agent's final message, when it produced one.
	Text string
	// Usage is what this run consumed.
	Usage Usage
	// ReportPath is the scout report written by this run, if any.
	ReportPath string
}

// ErrNotRunning is returned by Cancel and Interrupt when the Runner has no
// live run for the task.
var ErrNotRunning = errors.New("fleet: task has no live run")

type stopMode int

const (
	stopNone stopMode = iota
	stopInterrupt
	stopCancel
)

type activeRun struct {
	mu     sync.Mutex
	mode   stopMode
	reason string
	pid    int
	timer  *time.Timer
	done   bool
}

// Run launches the task's agent and blocks until its turn ends.
//
// The task moves to running first; that transition is the claim on the task,
// so a second Run for the same task fails with a transition or conflict
// error and changes nothing. From queued the agent starts a fresh session;
// from any other status it resumes the task's existing session, which is how
// follow-up instructions and answers reach it.
//
// When the turn ends the task is left in:
//   - verifying, if the agent finished;
//   - needs_decision, if it asked a question, hit the turn limit, timed out,
//     errored, or was interrupted;
//   - cancelled, if Cancel was called;
//   - failed, if the run could not be set up at all.
//
// With a Budget set, a run that would start past a limit is refused with a
// *BudgetError and the task is left exactly as it was.
//
// The returned error is non-nil only when the task could not be claimed or
// its final status could not be stored. An agent that fails is a RunResult,
// not an error.
func (r *Runner) Run(ctx context.Context, taskID string, opts RunOptions) (RunResult, error) {
	if r.Store == nil {
		return RunResult{}, errors.New("fleet: Runner.Store is required")
	}
	before, err := r.Store.GetTask(ctx, taskID)
	if err != nil {
		return RunResult{}, err
	}
	if r.Budget != nil && !opts.IgnoreBudget {
		project, err := r.Store.GetProject(ctx, before.ProjectID)
		if err != nil {
			return RunResult{}, err
		}
		over, err := r.Budget.view(r.Store).admit(ctx, before, project)
		if err != nil {
			return RunResult{}, err
		}
		if over != nil {
			return RunResult{}, over
		}
	}
	fresh := before.Status == StatusQueued
	reason := "resumed"
	if fresh {
		reason = "started"
	}
	// The run is registered before the task is claimed, so there is never a
	// moment when the task is stored as running and Running reports false.
	// A supervisor seeing that would take the task for dead.
	run := &activeRun{}
	if !r.register(taskID, run) {
		return RunResult{}, fmt.Errorf("%w: task %s already has a live run", ErrConflict, taskID)
	}
	defer r.unregister(taskID, run)

	var task Task
	if opts.startupResume == nil {
		task, err = r.Store.Transition(ctx, taskID, StatusRunning, reason)
	} else {
		task, err = r.Store.transitionStartupResume(ctx, taskID, opts.startupResume)
		_ = r.Store.recordAudit(context.WithoutCancel(ctx), startupResumeAudit(before, err))
	}
	if err != nil {
		return RunResult{}, err
	}
	_ = r.Store.SetLiveness(ctx, taskID, LivenessBusy)

	// From here on the task is ours and must leave "running" whatever
	// happens, including after ctx is cancelled.
	end, err := r.execute(ctx, &task, fresh, opts, run)
	if err != nil {
		end = runEnd{status: StatusFailed, reason: err.Error()}
	}

	finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	// The process has exited and the Runner saw it happen.
	_ = r.Store.SetLiveness(finalCtx, taskID, LivenessIdle)
	quotaRecorded := end.quota == nil
	if end.quota != nil {
		quotaRecorded = r.Store.recordQuotaExhaustion(finalCtx, taskID, *end.quota) == nil
		if !quotaRecorded {
			end.reason += "; could not persist quota state"
		}
	}
	if backend, model := r.quotaFallback(finalCtx, task, fresh, opts, end.quota); quotaRecorded && ctx.Err() == nil && backend != "" {
		final, err := r.Store.requeueQuotaTransfer(finalCtx, task, end.quota.Backend, backend, model)
		if err == nil {
			return RunResult{Task: final, Outcome: OutcomeRequeued, Reason: final.StatusReason, Usage: end.usage, Quota: end.quota}, nil
		}
		// Leave the task visible for owner intervention if the transfer failed.
		end.reason += "; could not requeue on the alternate backend"
	}
	final, err := r.Store.transition(finalCtx, taskID, end.status, end.reason, end.requested)
	if err != nil {
		return RunResult{}, fmt.Errorf("fleet: record end of run for task %s (%s: %s): %w", taskID, end.status, end.reason, err)
	}
	return RunResult{
		Task:       final,
		Outcome:    outcomeFor(end.status),
		Reason:     end.reason,
		Text:       end.text,
		Usage:      end.usage,
		ReportPath: end.reportPath,
		Quota:      end.quota,
	}, nil
}

func outcomeFor(status Status) Outcome {
	switch status {
	case StatusVerifying:
		return OutcomeFinished
	case StatusCancelled:
		return OutcomeCancelled
	case StatusFailed:
		return OutcomeFailed
	default:
		return OutcomeNeedsDecision
	}
}

// runEnd is where a run leaves its task.
type runEnd struct {
	quota  *QuotaExhaustion
	status Status
	reason string
	// requested is true when the run ended because a stop was asked for.
	requested  bool
	text       string
	usage      Usage
	reportPath string
}

// execute sets up the workspace, runs the agent and classifies how it ended.
// A returned error means the run could not be set up.
func (r *Runner) execute(ctx context.Context, task *Task, fresh bool, opts RunOptions, run *activeRun) (runEnd, error) {
	project, err := r.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return runEnd{}, err
	}
	if task.Shape == ShapeScout && strings.TrimSpace(r.ReportRoot) == "" {
		return runEnd{}, errors.New("fleet: Runner.ReportRoot is required to run scout tasks")
	}
	dir, err := r.prepareWorkspace(ctx, task.deliveryProject(project), task)
	if err != nil {
		return runEnd{}, fmt.Errorf("prepare workspace: %w", err)
	}

	sessionID := task.SessionID
	if fresh {
		sessionID = ""
	}
	binary := r.binary()
	codex := task.Shape == ShapeScout && r.CodexScoutBinary != ""
	codexTask := *task
	if task.Shape == ShapeScout {
		usdLimited := r.scoutUSDLimited(*task, project)
		if task.quotaBackend != "" {
			codex = task.quotaBackend == "codex"
			if codex && r.CodexScoutBinary == "" {
				return runEnd{}, errors.New("queued quota transfer requires a configured Codex Scout")
			}
			codexTask.Model = task.quotaModel
			if task.quotaBackend == "claude" && task.quotaModel != "" {
				task.Model = task.quotaModel
			}
		} else if r.ScoutRoute != nil {
			codex, codexTask.Model = r.ScoutRoute(ctx, *task, usdLimited)
		}
		if codex {
			if firstNonEmpty(strings.TrimSpace(opts.Instruction), strings.TrimSpace(task.Goal)) == "" {
				return runEnd{}, errors.New("Codex Scout requires a written goal for offline investigation")
			}
			if usdLimited {
				return runEnd{}, errors.New("Codex Scout does not report USD usage and cannot run with a USD budget")
			}
			binary = r.CodexScoutBinary
			sessionID = ""
		}
	}
	args := r.buildArgs(*task, sessionID, r.prompt(*task, sessionID, opts))
	if codex {
		args = r.codexScoutArgs(codexTask, opts)
	}
	timeout := r.RunTimeout
	if timeout <= 0 {
		timeout = DefaultRunTimeout
	}

	cmd, cancel := process.Command(ctx, process.Options{
		Dir:       dir,
		Env:       client.CLIEnv(),
		Timeout:   timeout,
		LogPrefix: "fleet.run",
		// The prompt is in the task record; keep it out of the process log.
		LogLabel: fmt.Sprintf("%s (task %s, %s)", filepath.Base(binary), task.ID, task.Shape),
	}, binary, args...)
	defer cancel()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runEnd{}, fmt.Errorf("agent stdout: %w", err)
	}
	stderr := &tailBuffer{max: 4096}
	cmd.Stderr = stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return runEnd{}, fmt.Errorf("start agent: %w", err)
	}
	// If a stop arrived before the process existed, attach delivers it now.
	run.attach(cmd.Process.Pid, r.stopGrace())
	_, _ = r.Store.AppendEvent(ctx, task.ID, EventRunStarted, map[string]any{
		"dir": dir, "resumed": sessionID != "", "pid": cmd.Process.Pid,
	})

	if opts.OnStart != nil {
		opts.OnStart(*task)
	}

	var stream streamSummary
	if codex {
		stream = r.consumeCodexScout(ctx, &codexTask, stdout)
	} else {
		stream = r.consume(ctx, task, stdout)
	}
	waitErr := cmd.Wait()
	run.finish()
	elapsed := time.Since(started)

	end := runEnd{usage: stream.usage}
	if stream.result != nil {
		end.text = strings.TrimSpace(stream.result.Result)
	}
	mode, stopReason := run.stopped()
	exitDetail := strings.TrimSpace(stderr.String())

	var quota *QuotaExhaustion
	failed := waitErr != nil || stream.result == nil || stream.result.IsError
	if failed && mode == stopNone && ctx.Err() == nil && !(waitErr != nil && elapsed >= timeout) {
		quota = stream.quota
		if quota == nil && r.MatchQuotaError != nil {
			backend := "claude"
			if codex {
				backend = "codex"
			}
			diagnostic := exitDetail
			if stream.result != nil && stream.result.IsError {
				diagnostic = strings.TrimSpace(end.text + "\n" + exitDetail)
			}
			if diagnostic != "" {
				if exhausted, resetsAt := r.MatchQuotaError(backend, diagnostic); exhausted {
					quota = &QuotaExhaustion{Backend: backend, ResetsAt: resetsAt}
				}
			}
		}
	}

	switch {
	case mode == stopCancel:
		end.status, end.reason, end.requested = StatusCancelled, stopReason, true
	case mode == stopInterrupt:
		end.status, end.reason, end.requested = StatusNeedsDecision, stopReason, true
	case ctx.Err() != nil:
		end.status, end.reason = StatusNeedsDecision, runnerStoppedPrefix+ctx.Err().Error()
	case waitErr != nil && elapsed >= timeout:
		end.status, end.reason = StatusNeedsDecision, "timed out after "+timeout.String()
	case quota != nil:
		end.status, end.reason, end.quota = StatusNeedsDecision, quota.Backend+" quota exhausted", quota
	case stream.result == nil:
		detail := exitDetail
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		if stream.sessionID == "" {
			return runEnd{}, fmt.Errorf("agent produced no output: %s", orUnknown(detail))
		}
		end.status, end.reason = StatusNeedsDecision, "agent exited without a result: "+orUnknown(detail)
	case stream.result.IsError:
		end.status = StatusNeedsDecision
		if stream.result.Subtype == "error_max_turns" {
			end.reason = "reached the turn limit (" + strconv.Itoa(r.maxTurns()) + ")"
		} else {
			end.reason = "agent error: " + orUnknown(firstNonEmpty(end.text, stream.result.Subtype, exitDetail))
		}
	case waitErr != nil:
		end.status, end.reason = StatusNeedsDecision, "agent exited with an error: "+orUnknown(firstNonEmpty(exitDetail, waitErr.Error()))
	case task.Shape == ShapeScout:
		if end.text == "" {
			end.status, end.reason = StatusNeedsDecision, "scout produced no report"
			break
		}
		path, err := r.writeReport(*task, end.text)
		if err != nil {
			end.status, end.reason = StatusNeedsDecision, "could not write the report: "+err.Error()
			break
		}
		end.reportPath = path
		_, _ = r.Store.AppendEvent(context.WithoutCancel(ctx), task.ID, EventReport, map[string]any{"path": path})
		end.status, end.reason = StatusVerifying, "report written"
	case chat.AssistantAwaitsInput(end.text):
		end.status, end.reason = StatusNeedsDecision, "agent is waiting for an answer"
	default:
		end.status, end.reason = StatusVerifying, "agent finished"
	}
	return end, nil
}

// prepareWorkspace makes sure the task has a checkout and returns its path.
// An existing checkout is reused, so a resumed task sees its earlier work.
func (r *Runner) prepareWorkspace(ctx context.Context, project Project, task *Task) (string, error) {
	path := task.WorktreePath
	branch := task.Branch
	if path == "" {
		var err error
		if path, err = r.Worktrees.Path(project.ID, task.ID); err != nil {
			return "", err
		}
	}
	if branch == "" {
		branch = project.BranchName(task.ID)
	}

	if _, err := worktree.IsDirty(ctx, path); err == nil {
		// Already a working tree.
	} else if !errors.Is(err, worktree.ErrNotWorktree) {
		return "", err
	} else if task.Branch != "" {
		// The checkout is gone but its branch may still hold the work.
		if _, err := worktree.Attach(ctx, project.RepoPath, path, branch); err != nil {
			return "", err
		}
	} else {
		if _, err := worktree.Create(ctx, project.RepoPath, path, branch, baseRef(ctx, project)); err != nil {
			return "", err
		}
	}

	if task.Branch != branch || task.WorktreePath != path {
		if err := r.Store.SetWorkspace(ctx, task.ID, branch, path); err != nil {
			return "", err
		}
		task.Branch, task.WorktreePath = branch, path
	}
	return path, nil
}

// baseRef picks where a new task branch starts: the project's default branch,
// or its remote-tracking counterpart when there is no local one.
func baseRef(ctx context.Context, project Project) string {
	if _, err := runGit(ctx, project.RepoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+project.DefaultBranch); err == nil {
		return project.DefaultBranch
	}
	if project.Remote != "" {
		return project.Remote + "/" + project.DefaultBranch
	}
	return project.DefaultBranch
}

func (r *Runner) buildArgs(task Task, sessionID, prompt string) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if model := firstNonEmpty(task.Model, r.DefaultModel); model != "" {
		args = append(args, "--model", model)
	}
	args = append(args, "--max-turns", strconv.Itoa(r.maxTurns()))
	if task.Shape == ShapeScout {
		// A scout gets only tools that cannot change anything, no MCP
		// servers, and no permission bypass: whatever would need approval
		// is denied instead.
		args = append(args, "--tools", strings.Join(r.scoutTools(), ","), "--strict-mcp-config")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return append(args, prompt)
}

const scoutPreamble = `You are investigating, not changing anything. You have read-only tools and must not attempt to modify files, run commands, or push.

Your final message is the deliverable: it is saved verbatim as report.md. Write it as a complete, self-contained Markdown report with your findings, the evidence for them (file paths and line numbers), and what you recommend. Do not end with a question.

Investigate:

`

func (r *Runner) prompt(task Task, sessionID string, opts RunOptions) string {
	if instruction := strings.TrimSpace(opts.Instruction); instruction != "" {
		if sessionID == "" && task.Shape == ShapeScout {
			return scoutPreamble + instruction
		}
		return instruction
	}
	if sessionID != "" {
		return "Continue the task from where you left off."
	}
	goal := task.Goal
	if goal == "" && task.Source.Kind == SourceIssue {
		goal = fmt.Sprintf("Work on GitHub issue #%d of this repository. Read it first with `gh issue view %d`.",
			task.Source.IssueNumber, task.Source.IssueNumber)
	}
	if task.Shape == ShapeScout {
		return scoutPreamble + goal
	}
	return goal
}

// ReportPath returns where a scout task's report is written.
func (r *Runner) ReportPath(projectID, taskID string) (string, error) {
	dir, err := worktree.Layout{Root: r.ReportRoot}.Path(projectID, taskID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "report.md"), nil
}

func (r *Runner) writeReport(task Task, text string) (string, error) {
	path, err := r.ReportPath(task.ProjectID, task.ID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(text+"\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// ── stream handling ──────────────────────────────────────────────────────────

// agentEvent is one line of the agent's stream-json output. Unknown event
// types and fields are ignored, so new CLI versions do not break the runner.
type agentEvent struct {
	RateLimitInfo *struct {
		Status   string `json:"status"`
		ResetsAt int64  `json:"resetsAt"`
	} `json:"rate_limit_info"`
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Model     string `json:"model"`
	Message   *struct {
		Content []agentBlock `json:"content"`
	} `json:"message"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMs   int64   `json:"duration_ms"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

type agentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

type streamSummary struct {
	quota     *QuotaExhaustion
	sessionID string
	result    *agentEvent
	usage     Usage
}

// Limits on what one event stores, so a single large write cannot bloat the log.
const (
	maxEventText  = 16 * 1024
	maxEventInput = 4 * 1024
	maxStreamLine = 32 * 1024 * 1024
)

// consume reads the agent's output to the end, recording what it emits.
func (r *Runner) consume(ctx context.Context, task *Task, stdout interface{ Read([]byte) (int, error) }) streamSummary {
	// Recording must outlive ctx: output produced while shutting down is
	// still part of the task's history.
	recCtx := context.WithoutCancel(ctx)
	var sum streamSummary
	model := firstNonEmpty(task.Model, r.DefaultModel)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev agentEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.SessionID != "" && ev.SessionID != sum.sessionID {
			sum.sessionID = ev.SessionID
			if ev.SessionID != task.SessionID {
				// Stored at once, so a run that dies later can still be resumed.
				if err := r.Store.SetSession(recCtx, task.ID, ev.SessionID); err == nil {
					task.SessionID = ev.SessionID
				}
			}
		}

		switch ev.Type {
		case "rate_limit_event":
			if info := ev.RateLimitInfo; info != nil {
				switch info.Status {
				case "rejected":
					sum.quota = &QuotaExhaustion{Backend: "claude"}
					if info.ResetsAt > 0 {
						sum.quota.ResetsAt = time.Unix(info.ResetsAt, 0).UTC()
					}
				case "allowed", "allowed_warning":
					sum.quota = nil
				}
			}
		case "system":
			if ev.Subtype == "init" {
				model = firstNonEmpty(ev.Model, model)
				r.record(recCtx, task.ID, EventAgentInit, map[string]any{"session_id": ev.SessionID, "model": ev.Model})
			}
		case "assistant":
			if ev.Message == nil {
				continue
			}
			for _, blk := range ev.Message.Content {
				switch blk.Type {
				case "text":
					if strings.TrimSpace(blk.Text) != "" {
						r.record(recCtx, task.ID, EventAgentText, map[string]any{"text": clip(blk.Text, maxEventText)})
					}
				case "tool_use":
					r.record(recCtx, task.ID, EventAgentToolUse, map[string]any{
						"id": blk.ID, "tool": blk.Name, "input": clipJSON(blk.Input, maxEventInput),
					})
				}
			}
		case "user":
			if ev.Message == nil {
				continue
			}
			for _, blk := range ev.Message.Content {
				if blk.Type == "tool_result" {
					r.record(recCtx, task.ID, EventAgentToolResult, map[string]any{
						"tool_use_id": blk.ToolUseID, "is_error": blk.IsError,
					})
				}
			}
		case "result":
			result := ev
			sum.result = &result
			sum.usage = Usage{
				InputTokens:  ev.Usage.InputTokens + ev.Usage.CacheReadInputTokens + ev.Usage.CacheCreationInputTokens,
				OutputTokens: ev.Usage.OutputTokens,
				CostUSD:      ev.TotalCostUSD,
			}
			_ = r.Store.AddBackendUsage(recCtx, task.ID, "claude", model, sum.usage)
			r.record(recCtx, task.ID, EventAgentResult, map[string]any{
				"subtype": ev.Subtype, "is_error": ev.IsError, "num_turns": ev.NumTurns,
				"duration_ms": ev.DurationMs, "cost_usd": ev.TotalCostUSD,
				"input_tokens": sum.usage.InputTokens, "output_tokens": sum.usage.OutputTokens,
			})
		}
	}
	return sum
}

func (r *Runner) record(ctx context.Context, taskID, eventType string, payload any) {
	// A failed write must not stop the run; the agent's work matters more
	// than one missing log line.
	_, _ = r.Store.RecordActivity(ctx, taskID, eventType, payload)
}

func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… [%d more bytes]", len(s)-cut)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// clipJSON keeps small tool inputs as structured JSON and replaces large
// ones with a truncated preview.
func clipJSON(raw json.RawMessage, max int) any {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) <= max {
		return raw
	}
	return map[string]any{"truncated": true, "bytes": len(raw), "preview": clip(string(raw), max)}
}

// ── stopping ─────────────────────────────────────────────────────────────────

// Cancel stops the task's live run and leaves the task cancelled. The agent
// is sent an interrupt first and killed only if it has not exited within
// StopGrace.
func (r *Runner) Cancel(taskID, reason string) error {
	return r.stop(taskID, stopCancel, firstNonEmpty(strings.TrimSpace(reason), "cancelled"))
}

// Interrupt stops the task's live run and leaves the task needing a
// decision, with its session intact so it can be resumed with new
// instructions.
func (r *Runner) Interrupt(taskID, reason string) error {
	return r.stop(taskID, stopInterrupt, firstNonEmpty(strings.TrimSpace(reason), "interrupted"))
}

// Running reports whether the Runner has a live run for the task.
func (r *Runner) Running(taskID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.runs[taskID]
	return ok
}

func (r *Runner) stop(taskID string, mode stopMode, reason string) error {
	r.mu.Lock()
	run, ok := r.runs[taskID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotRunning, taskID)
	}
	if !run.requestStop(mode, reason, r.stopGrace()) {
		return fmt.Errorf("%w: %s", ErrNotRunning, taskID)
	}
	kind := "interrupt"
	if mode == stopCancel {
		kind = "cancel"
	}
	_, _ = r.Store.AppendEvent(context.Background(), taskID, EventRunStop, map[string]any{"kind": kind, "reason": reason})
	return nil
}

// register records a live run for the task. It reports false, changing
// nothing, when the task already has one.
func (r *Runner) register(taskID string, run *activeRun) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs == nil {
		r.runs = make(map[string]*activeRun)
	}
	if _, exists := r.runs[taskID]; exists {
		return false
	}
	r.runs[taskID] = run
	return true
}

func (r *Runner) unregister(taskID string, run *activeRun) {
	run.finish()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runs[taskID] == run {
		delete(r.runs, taskID)
	}
}

// requestStop records the stop and signals the process if it exists. A cancel
// overrides an earlier interrupt, never the other way round. It reports
// false when the run has already ended.
func (a *activeRun) requestStop(mode stopMode, reason string, grace time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.done {
		return false
	}
	if mode > a.mode {
		a.mode, a.reason = mode, reason
	}
	a.signalLocked(grace)
	return true
}

// attach gives the run its process, and signals it at once if a stop was
// requested before the process existed.
func (a *activeRun) attach(pid int, grace time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pid = pid
	if a.mode != stopNone {
		a.signalLocked(grace)
	}
}

// signalLocked interrupts the agent's process group and arms the kill timer.
func (a *activeRun) signalLocked(grace time.Duration) {
	if a.pid == 0 || a.timer != nil {
		return
	}
	pid := a.pid
	signalGroup(pid, syscall.SIGINT)
	a.timer = time.AfterFunc(grace, func() { signalGroup(pid, syscall.SIGKILL) })
}

func (a *activeRun) finish() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = true
	if a.timer != nil {
		a.timer.Stop()
	}
}

func (a *activeRun) stopped() (stopMode, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.mode, a.reason
}

// signalGroup signals the whole process group, so tools the agent spawned
// stop with it.
func signalGroup(pid int, sig syscall.Signal) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, sig); err != nil {
		_ = syscall.Kill(pid, sig)
	}
}

// ── small helpers ────────────────────────────────────────────────────────────

func (r *Runner) binary() string { return firstNonEmpty(strings.TrimSpace(r.Binary), "claude") }

func (r *Runner) maxTurns() int {
	if r.MaxTurns > 0 {
		return r.MaxTurns
	}
	return DefaultMaxTurns
}

func (r *Runner) stopGrace() time.Duration {
	if r.StopGrace > 0 {
		return r.StopGrace
	}
	return DefaultStopGrace
}

func (r *Runner) scoutTools() []string {
	if len(r.ScoutTools) > 0 {
		return r.ScoutTools
	}
	return DefaultScoutTools
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no details"
	}
	return clip(strings.TrimSpace(s), 500)
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
