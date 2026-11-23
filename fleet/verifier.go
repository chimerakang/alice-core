package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/verify"
	"github.com/chimerakang/alice-core/worktree"
)

// Verifier is the acceptance gate: it decides whether a task that says it is
// finished really is, by running checks and looking only at their exit codes.
// It never asks a model and never trusts the agent's own account.
type Verifier struct {
	// Store holds the tasks. Required.
	Store *Store
	// ReportRoot is where scout reports live; the same value as
	// Runner.ReportRoot. Required to verify scouts.
	ReportRoot string
	// MaxRetries is how many times a failing ship task is sent back to its
	// agent with the failure before the owner is asked. Zero means
	// DefaultVerifyRetries; a negative value disables retries.
	MaxRetries int
	// StepTimeout caps each check. Zero means verify.DefaultTimeout.
	StepTimeout time.Duration
	// OutputTail is how many bytes of a check's output are stored and fed
	// back. Zero means DefaultOutputTail.
	OutputTail int
}

// Verifier defaults.
const (
	DefaultVerifyRetries = 2
	DefaultOutputTail    = 8 * 1024
)

// EventVerification is the event type recorded for each verification round.
const EventVerification = "verify.result"

// VerifyResult describes one verification round.
type VerifyResult struct {
	// Task is the task as stored after the round.
	Task Task
	// Attempt is the round's number for this task, starting at 1.
	Attempt int
	Passed  bool
	// Checks are the checks that ran, in order. A round stops at the first
	// failure, so checks after it are absent.
	Checks []Verification
	// Retry is true when the round failed and the task should go back to
	// its agent with Feedback. The task is still verifying; the caller
	// starts the next run.
	Retry bool
	// Feedback is the instruction to give the agent when Retry is true.
	Feedback string
}

// Verify runs the acceptance checks for a task that is verifying and moves it
// on according to the result:
//
//   - a ship task that passes becomes review_ready;
//   - a scout task that passes becomes done;
//   - a ship task that fails with retries left stays verifying and the result
//     has Retry set, with the failure as Feedback for the agent;
//   - anything else that fails becomes needs_decision.
//
// A ship task is checked with its project's VerifyCommands, run in order in
// the task's worktree. A scout task is checked for a non-empty report and for
// having left its checkout exactly as it found it.
func (v *Verifier) Verify(ctx context.Context, taskID string) (VerifyResult, error) {
	if v.Store == nil {
		return VerifyResult{}, errors.New("fleet: Verifier.Store is required")
	}
	task, err := v.Store.GetTask(ctx, taskID)
	if err != nil {
		return VerifyResult{}, err
	}
	if task.Status != StatusVerifying {
		return VerifyResult{}, fmt.Errorf("fleet: task %s is %s, not verifying", taskID, task.Status)
	}
	project, err := v.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return VerifyResult{}, err
	}

	var steps []verify.Step
	if task.Shape == ShapeScout {
		steps = v.scoutSteps(project, task)
	} else {
		for _, command := range project.VerifyCommands {
			step := verify.Shell(command)
			step.Timeout = v.StepTimeout
			steps = append(steps, step)
		}
	}
	report := verify.Run(ctx, task.WorktreePath, steps...)

	// From here the round is recorded and the task moved on even if ctx has
	// been cancelled: a task must not be left verifying with no one checking.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	checks := make([]Verification, 0, len(report.Results))
	for _, r := range report.Results {
		checks = append(checks, Verification{
			Name: r.Name, Command: r.Command, Passed: r.Passed, ExitCode: r.ExitCode,
			OutputTail: tail(r.Output, v.outputTail()), Duration: r.Duration, TimedOut: r.TimedOut, Err: r.Err,
		})
	}
	attempt, err := v.Store.RecordVerification(recCtx, taskID, checks)
	if err != nil {
		return VerifyResult{}, err
	}
	res := VerifyResult{Attempt: attempt, Passed: report.Passed, Checks: checks}
	v.recordEvent(recCtx, taskID, res)

	next, reason := v.decide(recCtx, ctx, task, report, &res)
	if next == "" {
		res.Task = task
		return res, nil
	}
	res.Task, err = v.Store.Transition(recCtx, taskID, next, reason)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("fleet: record verification outcome for task %s (%s: %s): %w", taskID, next, reason, err)
	}
	return res, nil
}

// decide picks the task's next status. An empty status means "stay
// verifying", used when the task goes back to its agent.
func (v *Verifier) decide(recCtx, runCtx context.Context, task Task, report verify.Report, res *VerifyResult) (Status, string) {
	if report.Passed {
		switch {
		case task.Shape == ShapeScout:
			return StatusDone, "report verified"
		case len(report.Results) == 0:
			return StatusReviewReady, "no verification commands configured"
		default:
			return StatusReviewReady, "verification passed"
		}
	}

	failed := report.Failed()
	what := fmt.Sprintf("%s failed", failed.Name)
	switch {
	case failed.TimedOut:
		what = fmt.Sprintf("%s %s", failed.Name, failed.Err)
	case failed.Err != "":
		what = fmt.Sprintf("%s could not run: %s", failed.Name, failed.Err)
	case failed.ExitCode > 0:
		what = fmt.Sprintf("%s failed (exit %d)", failed.Name, failed.ExitCode)
	}

	if runCtx.Err() != nil {
		return StatusNeedsDecision, "verification interrupted: " + runCtx.Err().Error()
	}
	if task.Shape == ShapeScout {
		// A scout that broke its rules is not sent back to try again.
		return StatusNeedsDecision, "scout verification failed: " + what
	}

	since := task.CreatedAt
	if task.StartedAt != nil {
		since = *task.StartedAt
	}
	failures, err := v.Store.failedVerificationRounds(recCtx, task.ID, since)
	if err != nil {
		return StatusNeedsDecision, "verification failed: " + what
	}
	if failures <= v.maxRetries() {
		res.Retry = true
		res.Feedback = feedbackFor(*failed, tail(failed.Output, v.outputTail()))
		return "", ""
	}
	return StatusNeedsDecision, fmt.Sprintf("verification failed after %d attempts: %s", failures, what)
}

func feedbackFor(failed verify.Result, output string) string {
	var sb strings.Builder
	sb.WriteString("Your work was checked and did not pass. This check failed:\n\n")
	fmt.Fprintf(&sb, "$ %s\n", failed.Command)
	switch {
	case failed.TimedOut || failed.Err != "":
		fmt.Fprintf(&sb, "(%s)\n", failed.Err)
	default:
		fmt.Fprintf(&sb, "(exit status %d)\n", failed.ExitCode)
	}
	if strings.TrimSpace(output) != "" {
		sb.WriteString("\nOutput:\n\n")
		sb.WriteString(strings.TrimRight(output, "\n"))
		sb.WriteString("\n")
	}
	sb.WriteString("\nFix the cause and make sure this check passes. Do not weaken, skip or delete the check.")
	return sb.String()
}

func (v *Verifier) recordEvent(ctx context.Context, taskID string, res VerifyResult) {
	steps := make([]map[string]any, 0, len(res.Checks))
	for _, c := range res.Checks {
		steps = append(steps, map[string]any{
			"name": c.Name, "passed": c.Passed, "exit_code": c.ExitCode, "duration_ms": c.Duration.Milliseconds(),
		})
	}
	_, _ = v.Store.AppendEvent(ctx, taskID, EventVerification, map[string]any{
		"attempt": res.Attempt, "passed": res.Passed, "checks": steps,
	})
}

// ── scout checks ─────────────────────────────────────────────────────────────

func (v *Verifier) scoutSteps(project Project, task Task) []verify.Step {
	return []verify.Step{
		checkStep{name: "report_exists", describe: "report.md exists and is not empty", check: func(ctx context.Context) (string, error) {
			if strings.TrimSpace(v.ReportRoot) == "" {
				return "", errors.New("Verifier.ReportRoot is not set")
			}
			path, err := (&Runner{ReportRoot: v.ReportRoot}).ReportPath(task.ProjectID, task.ID)
			if err != nil {
				return "", err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("no report at %s", path)
			}
			if strings.TrimSpace(string(body)) == "" {
				return "", fmt.Errorf("the report at %s is empty", path)
			}
			return fmt.Sprintf("%s (%d bytes)", path, len(body)), nil
		}},
		checkStep{name: "worktree_clean", describe: "the checkout has no uncommitted or untracked changes", check: func(ctx context.Context) (string, error) {
			changes, err := worktree.Changes(ctx, task.WorktreePath)
			if err != nil {
				return "", err
			}
			if len(changes) > 0 {
				return strings.Join(changes, "\n"), fmt.Errorf("%d file(s) changed", len(changes))
			}
			return "", nil
		}},
		checkStep{name: "no_new_commits", describe: "the branch has no commits of its own", check: func(ctx context.Context) (string, error) {
			out, err := runGit(ctx, task.WorktreePath, "rev-list", "--count", baseRef(ctx, project)+"..HEAD")
			if err != nil {
				return out, fmt.Errorf("could not compare with %s", project.DefaultBranch)
			}
			if out != "0" {
				return out, fmt.Errorf("%s new commit(s) on %s", out, task.Branch)
			}
			return "", nil
		}},
	}
}

// checkStep is a verify.Step implemented in Go rather than by a command.
type checkStep struct {
	name     string
	describe string
	check    func(ctx context.Context) (output string, err error)
}

func (c checkStep) Name() string { return c.name }

func (c checkStep) Run(ctx context.Context, _ string) verify.Result {
	start := time.Now()
	out, err := c.check(ctx)
	res := verify.Result{Name: c.name, Command: c.describe, Output: out, Duration: time.Since(start), Passed: err == nil}
	if err != nil {
		res.ExitCode = 1
		if out == "" {
			res.Output = err.Error()
		} else {
			res.Output = err.Error() + "\n" + out
		}
	}
	return res
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (v *Verifier) maxRetries() int {
	switch {
	case v.MaxRetries < 0:
		return 0
	case v.MaxRetries == 0:
		return DefaultVerifyRetries
	}
	return v.MaxRetries
}

func (v *Verifier) outputTail() int {
	if v.OutputTail > 0 {
		return v.OutputTail
	}
	return DefaultOutputTail
}

// tail returns the last max bytes of s, cut on a character boundary.
func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := len(s) - max
	for cut < len(s) && !isRuneStart(s[cut]) {
		cut++
	}
	return fmt.Sprintf("[%d earlier bytes omitted] …", cut) + s[cut:]
}
