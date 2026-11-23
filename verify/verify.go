// Package verify runs deterministic acceptance checks against a working
// directory: build, type-check, lint, or any shell command. A check passes or
// fails on its exit code alone, so callers never have to trust an agent's own
// account of whether its work is correct.
//
// It has no dependency on any orchestrator package.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/process"
)

// Defaults applied when a CommandStep leaves the field unset.
const (
	DefaultTimeout     = 5 * time.Minute
	DefaultOutputLimit = 256 * 1024
)

// Result is the outcome of one Step.
type Result struct {
	// Name is the step's short label, e.g. "go_build".
	Name string
	// Command is the command line that ran, for display and audit.
	Command string
	// Passed is true only when the command started and exited with status 0.
	Passed bool
	// ExitCode is the process exit status, or -1 when the command did not
	// start or was killed (timeout, cancellation).
	ExitCode int
	// Output is the combined stdout and stderr, capped at the step's limit.
	Output string
	// Duration is the wall-clock time the step took.
	Duration time.Duration
	// TimedOut reports that the step was killed for exceeding its timeout.
	TimedOut bool
	// Err describes why the step could not produce an exit status (missing
	// binary, timeout, cancellation). Empty for an ordinary pass or fail.
	Err string
}

// Step is one acceptance check that can run against a directory.
type Step interface {
	// Name returns a short stable label for the step.
	Name() string
	// Run executes the check in dir and reports the outcome. It never panics
	// and never returns an error: every failure mode is expressed in Result.
	Run(ctx context.Context, dir string) Result
}

// CommandStep is a Step that runs an external command.
type CommandStep struct {
	// Label is the step name. Defaults to Bin when empty.
	Label string
	// Bin is the executable to run.
	Bin string
	// Args are passed to Bin verbatim; no shell is involved.
	Args []string
	// Env, when non-empty, replaces the inherited environment.
	Env []string
	// Timeout caps the run. Zero means DefaultTimeout.
	Timeout time.Duration
	// OutputLimit caps captured output in bytes. Zero means DefaultOutputLimit.
	OutputLimit int
}

// Command returns a Step that runs bin with args, without a shell.
func Command(label, bin string, args ...string) *CommandStep {
	return &CommandStep{Label: label, Bin: bin, Args: args}
}

// Shell returns a Step that runs script through `sh -c`, so pipes, `&&` and
// redirects work. Use it for project-defined verification commands.
func Shell(script string) *CommandStep {
	return &CommandStep{Label: script, Bin: "sh", Args: []string{"-c", script}}
}

// WithTimeout sets the step's timeout and returns the step.
func (s *CommandStep) WithTimeout(d time.Duration) *CommandStep {
	s.Timeout = d
	return s
}

// WithLabel sets the step's name and returns the step.
func (s *CommandStep) WithLabel(label string) *CommandStep {
	s.Label = label
	return s
}

// Name implements Step.
func (s *CommandStep) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Bin
}

// CommandLine returns the command as it would be typed, for display.
func (s *CommandStep) CommandLine() string {
	if s.Bin == "sh" && len(s.Args) == 2 && s.Args[0] == "-c" {
		return s.Args[1]
	}
	if len(s.Args) == 0 {
		return s.Bin
	}
	return s.Bin + " " + strings.Join(s.Args, " ")
}

// Run implements Step.
func (s *CommandStep) Run(ctx context.Context, dir string) Result {
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := s.OutputLimit
	if limit <= 0 {
		limit = DefaultOutputLimit
	}
	res := Result{Name: s.Name(), Command: s.CommandLine()}

	start := time.Now()
	out, err := process.RunCombinedOutput(ctx, process.Options{
		Dir:         dir,
		Env:         s.Env,
		Timeout:     timeout,
		OutputLimit: limit,
		LogPrefix:   "verify",
	}, s.Bin, s.Args...)
	res.Duration = time.Since(start)
	res.Output = string(out)

	if err == nil {
		res.Passed = true
		return res
	}

	res.ExitCode = -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode() // -1 when killed by a signal
	}
	switch {
	case ctx != nil && ctx.Err() != nil:
		res.Err = "cancelled: " + ctx.Err().Error()
	case res.ExitCode == -1 && res.Duration >= timeout:
		res.TimedOut = true
		res.Err = "timed out after " + timeout.String()
	case res.ExitCode == -1:
		res.Err = err.Error()
	}
	return res
}

// Report is the outcome of running a sequence of steps.
type Report struct {
	// Results holds one entry per step that ran, in order.
	Results []Result
	// Passed is true when every step ran and passed. An empty sequence passes.
	Passed bool
}

// Failed returns the first failing result, or nil when the report passed.
func (r Report) Failed() *Result {
	for i := range r.Results {
		if !r.Results[i].Passed {
			return &r.Results[i]
		}
	}
	return nil
}

// Run executes steps in order against dir and stops at the first failure, the
// behaviour wanted for an acceptance gate. Steps after a failure do not run.
func Run(ctx context.Context, dir string, steps ...Step) Report {
	return run(ctx, dir, steps, true)
}

// RunAll executes every step against dir even after a failure, for callers
// that want the complete picture rather than the first problem.
func RunAll(ctx context.Context, dir string, steps ...Step) Report {
	return run(ctx, dir, steps, false)
}

func run(ctx context.Context, dir string, steps []Step, stopOnFailure bool) Report {
	report := Report{Passed: true, Results: make([]Result, 0, len(steps))}
	for _, step := range steps {
		res := step.Run(ctx, dir)
		report.Results = append(report.Results, res)
		if !res.Passed {
			report.Passed = false
			if stopOnFailure {
				break
			}
		}
	}
	return report
}

// TrimLines returns at most n lines of s, with a trailing note when lines
// were dropped. Use it to keep long build output readable in messages.
func TrimLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if n <= 0 || len(lines) <= n {
		return s
	}
	kept := append([]string{}, lines[:n]...)
	kept = append(kept, fmt.Sprintf("... (%d more lines omitted)", len(lines)-n))
	return strings.Join(kept, "\n")
}
