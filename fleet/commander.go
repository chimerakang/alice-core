package fleet

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/process"
	"github.com/chimerakang/alice-core/worktree"
)

// Actor is who gives a command, and through which surface.
type Actor struct {
	// ID identifies the person, e.g. a Telegram user ID. Required.
	ID string
	// Via names the surface the command came through, e.g. "telegram".
	Via string
}

// Commander is the single entry point for operating the fleet. Every chat
// bot, web handler or script goes through it, so they all behave the same
// way and every command lands in the audit log with the name of whoever
// gave it.
//
// Deciding who is allowed to give commands is the caller's job; Commander
// records the actor it is told.
type Commander struct {
	// Store holds the fleet's state. Required.
	Store *Store
	// Scheduler starts and resumes tasks. Required for Dispatch to take
	// effect promptly and for Reply.
	Scheduler *Scheduler
	// Deliverer lands approved work. Defaults to the Scheduler's, or to one
	// built on Store.
	Deliverer *Deliverer
	// Issues dispatches from GitHub issues. Defaults to the Scheduler's, or
	// to one built on Store.
	Issues *IssueTracker
}

// ErrNoActor is returned by every command given without an actor.
var ErrNoActor = errors.New("fleet: a command needs an actor")

// Command names, as they appear in the audit log.
const (
	CmdRegisterProject = "register_project"
	CmdUpdateProject   = "update_project"
	CmdDispatch        = "dispatch"
	CmdCancel          = "cancel"
	CmdRetry           = "retry"
	CmdReply           = "reply"
	CmdApprove         = "approve"
	CmdMerge           = "merge"
	CmdDiscard         = "discard"
)

// EventDiscarded is recorded when a task's result is thrown away.
const EventDiscarded = "discarded"

// audit records a command and passes its error through, so a command can end
// with `return x, c.audit(...)`.
func (c *Commander) audit(ctx context.Context, actor Actor, command, projectID, taskID, detail string, err error) error {
	entry := AuditEntry{
		Actor: actor.ID, Via: actor.Via, Command: command,
		ProjectID: projectID, TaskID: taskID, Detail: clip(detail, 500), OK: err == nil,
	}
	if err != nil {
		entry.Error = clip(err.Error(), 500)
	}
	// The record must be written even if the request that carried the
	// command has already gone away.
	_ = c.Store.recordAudit(context.WithoutCancel(ctx), entry)
	return err
}

func (c *Commander) check(actor Actor) error {
	if c.Store == nil {
		return errors.New("fleet: Commander.Store is required")
	}
	if strings.TrimSpace(actor.ID) == "" {
		return ErrNoActor
	}
	return nil
}

// ── commands ─────────────────────────────────────────────────────────────────

// RegisterProject puts a repository under management.
func (c *Commander) RegisterProject(ctx context.Context, actor Actor, p Project) (Project, error) {
	if err := c.check(actor); err != nil {
		return Project{}, err
	}
	out, err := c.Store.RegisterProject(ctx, p)
	id := out.ID
	if id == "" {
		id = p.ID
	}
	return out, c.audit(ctx, actor, CmdRegisterProject, id, "", p.RepoPath, err)
}

// UpdateProject changes a project's settings. describe is a short note of
// what is being changed, for the audit log.
func (c *Commander) UpdateProject(ctx context.Context, actor Actor, projectID, describe string, change func(*Project) error) (Project, error) {
	if err := c.check(actor); err != nil {
		return Project{}, err
	}
	out, err := c.Store.UpdateProject(ctx, projectID, change)
	return out, c.audit(ctx, actor, CmdUpdateProject, projectID, "", describe, err)
}

// DispatchRequest describes a task to queue.
type DispatchRequest struct {
	ProjectID string
	// Goal is what to do. Leave it empty when dispatching from an issue.
	Goal string
	// Issue, when positive, dispatches from that GitHub issue: the goal is
	// built from the issue and its open checklist.
	Issue int
	// Shape defaults to ShapeShip.
	Shape    Shape
	Model    string
	Priority int
	// Budget, when set, caps this task's total usage. See Task.Budget.
	Budget Limit
}

// Dispatch queues a task and nudges the scheduler to look at it.
func (c *Commander) Dispatch(ctx context.Context, actor Actor, req DispatchRequest) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	var (
		task   Task
		err    error
		detail = firstLine(req.Goal, 200)
	)
	if req.Issue > 0 {
		detail = fmt.Sprintf("issue #%d", req.Issue)
		task, err = c.issues().Dispatch(ctx, req.ProjectID, req.Issue, DispatchOptions{Shape: req.Shape, Model: req.Model, Priority: req.Priority, Budget: req.Budget})
	} else {
		task, err = c.Store.CreateTask(ctx, NewTask{
			ProjectID: req.ProjectID, Goal: req.Goal, Shape: req.Shape, Model: req.Model, Priority: req.Priority, Budget: req.Budget,
		})
	}
	if err == nil && c.Scheduler != nil {
		c.Scheduler.Kick()
	}
	return task, c.audit(ctx, actor, CmdDispatch, req.ProjectID, task.ID, detail, err)
}

// Cancel stops a task for good. A task with a live agent is interrupted and
// becomes cancelled as soon as the agent exits, which may be a moment after
// this returns; the returned task is the task as it stands now.
func (c *Commander) Cancel(ctx context.Context, actor Actor, taskID, reason string) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	reason = firstNonEmpty(strings.TrimSpace(reason), "cancelled by "+actor.ID)
	task, err := c.cancel(ctx, taskID, reason)
	return task, c.audit(ctx, actor, CmdCancel, task.ProjectID, taskID, reason, err)
}

func (c *Commander) cancel(ctx context.Context, taskID, reason string) (Task, error) {
	task, err := c.Store.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	if runner := c.runner(); runner != nil && runner.Running(taskID) {
		if err := runner.Cancel(taskID, reason); err != nil && !errors.Is(err, ErrNotRunning) {
			return task, err
		} else if err == nil {
			return c.Store.GetTask(ctx, taskID)
		}
		// The run ended between the check and the stop; fall through.
	}
	updated, err := c.Store.Transition(ctx, taskID, StatusCancelled, reason)
	if err != nil {
		return task, err
	}
	return updated, nil
}

// Retry sends a failed or blocked task back to the queue for a fresh run.
// Its worktree and branch are kept; its agent session is not resumed.
func (c *Commander) Retry(ctx context.Context, actor Actor, taskID string) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	task, err := c.Store.Transition(ctx, taskID, StatusQueued, "retry requested by "+actor.ID)
	if err == nil && c.Scheduler != nil {
		c.Scheduler.Kick()
	}
	return task, c.audit(ctx, actor, CmdRetry, task.ProjectID, taskID, "", err)
}

// ReplyRequest is an answer or further instruction for a waiting task.
type ReplyRequest struct {
	TaskID string
	// Text is sent to the task's agent in its existing session. Empty means
	// "carry on from where you stopped".
	Text string
	// IgnoreBudget lets the task continue past a reached budget limit.
	IgnoreBudget bool
}

// Reply resumes a task that is waiting on the owner, with an answer to the
// agent's question or with further instructions. The agent runs in the
// background and the task then goes through verification and delivery as
// usual. It returns once the run has been accepted, not when it finishes.
func (c *Commander) Reply(ctx context.Context, actor Actor, req ReplyRequest) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	task, err := c.Store.GetTask(ctx, req.TaskID)
	if err == nil {
		switch {
		case c.Scheduler == nil:
			err = errors.New("fleet: Commander.Scheduler is required to resume a task")
		case task.Status == StatusQueued:
			// A queued task has no session to answer; it has not started.
			err = &TransitionError{TaskID: task.ID, From: task.Status, To: StatusRunning}
		default:
			err = c.Scheduler.Go(ctx, req.TaskID, RunOptions{Instruction: req.Text, IgnoreBudget: req.IgnoreBudget})
		}
	}
	return task, c.audit(ctx, actor, CmdReply, task.ProjectID, req.TaskID, firstLine(req.Text, 200), err)
}

// Approve is the owner accepting a task that is review_ready. It lands the
// work as the task's trust mode prescribes; see Deliverer.Approve.
func (c *Commander) Approve(ctx context.Context, actor Actor, taskID string) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	task, err := c.deliverer().Approve(ctx, taskID, actor.ID)
	return task, c.audit(ctx, actor, CmdApprove, c.projectOf(ctx, task, taskID), taskID, "", err)
}

// Merge lands a task's work wherever the task stands: it approves a task
// that is review_ready, and merges the pull request of a direct-pr task that
// is already done. See Deliverer.Merge.
func (c *Commander) Merge(ctx context.Context, actor Actor, taskID string) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	task, err := c.deliverer().Merge(ctx, taskID, actor.ID)
	return task, c.audit(ctx, actor, CmdMerge, c.projectOf(ctx, task, taskID), taskID, "", err)
}

// Discard throws a task's result away: the task is cancelled if it is not
// already finished, and its checkout is removed even if it holds uncommitted
// changes. The branch is kept, so committed work can still be recovered by
// hand. A task with a live agent must be cancelled first.
func (c *Commander) Discard(ctx context.Context, actor Actor, taskID string) (Task, error) {
	if err := c.check(actor); err != nil {
		return Task{}, err
	}
	task, err := c.discard(ctx, actor, taskID)
	return task, c.audit(ctx, actor, CmdDiscard, task.ProjectID, taskID, "", err)
}

func (c *Commander) discard(ctx context.Context, actor Actor, taskID string) (Task, error) {
	task, err := c.Store.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	if runner := c.runner(); (runner != nil && runner.Running(taskID)) || task.Status.IsActive() {
		return task, fmt.Errorf("%w: task %s is %s; cancel it before discarding", ErrConflict, taskID, task.Status)
	}
	if !task.Status.IsTerminal() {
		if task, err = c.Store.Transition(ctx, taskID, StatusCancelled, "discarded by "+actor.ID); err != nil {
			return task, err
		}
	}
	removed := false
	if task.WorktreePath != "" {
		switch err := worktree.Remove(ctx, task.WorktreePath, true); {
		case err == nil:
			removed = true
		case errors.Is(err, worktree.ErrNotWorktree):
			// Already gone.
		default:
			return task, fmt.Errorf("fleet: remove checkout of task %s: %w", taskID, err)
		}
	}
	_, _ = c.Store.AppendEvent(ctx, taskID, EventDiscarded, map[string]any{"by": actor.ID, "checkout_removed": removed})
	return task, nil
}

// ── queries ──────────────────────────────────────────────────────────────────

// ListProjects returns every project.
func (c *Commander) ListProjects(ctx context.Context) ([]Project, error) {
	return c.Store.ListProjects(ctx)
}

// GetProject returns one project.
func (c *Commander) GetProject(ctx context.Context, id string) (Project, error) {
	return c.Store.GetProject(ctx, id)
}

// ListTasks returns tasks, optionally narrowed by project and status.
func (c *Commander) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	return c.Store.ListTasks(ctx, filter)
}

// GetTask returns one task.
func (c *Commander) GetTask(ctx context.Context, id string) (Task, error) {
	return c.Store.GetTask(ctx, id)
}

// TaskEvents pages through a task's history in the order it was written.
// Pass the ID of the last event seen as afterID to get the next page; next
// is that cursor for the page returned, and more reports whether another
// page exists.
func (c *Commander) TaskEvents(ctx context.Context, taskID string, afterID int64, limit int) (events []Event, next int64, more bool, err error) {
	if _, err := c.Store.GetTask(ctx, taskID); err != nil {
		return nil, afterID, false, err
	}
	if limit <= 0 {
		limit = 100
	}
	events, err = c.Store.ListEvents(ctx, taskID, afterID, limit+1)
	if err != nil {
		return nil, afterID, false, err
	}
	if len(events) > limit {
		events, more = events[:limit], true
	}
	next = afterID
	if len(events) > 0 {
		next = events[len(events)-1].ID
	}
	return events, next, more, nil
}

// Verifications returns the stored acceptance checks of a task.
func (c *Commander) Verifications(ctx context.Context, taskID string) ([]Verification, error) {
	return c.Store.ListVerifications(ctx, taskID)
}

// Audit returns audit entries, newest first.
func (c *Commander) Audit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	return c.Store.ListAudit(ctx, filter)
}

// TaskDiff is what a task changed, relative to where its branch started.
type TaskDiff struct {
	// Base is the commit the comparison starts from: where the task's branch
	// left the project's default branch.
	Base string
	// Stat is the per-file summary, as `git diff --stat` prints it.
	Stat string
	// Patch is the unified diff. It includes changes not yet committed.
	Patch string
	// Untracked lists new files not yet added to git. They are not in Patch.
	Untracked []string
	// Truncated reports that Patch was cut at MaxDiffBytes.
	Truncated bool
}

// MaxDiffBytes caps the patch a TaskDiff carries.
const MaxDiffBytes = 1 << 20

// ErrNoWorkspace is returned by TaskDiff for a task that has no branch yet.
var ErrNoWorkspace = errors.New("fleet: task has no workspace yet")

// TaskDiff returns the changes a task has made. With the checkout present it
// covers committed and uncommitted work; with only the branch left, the
// committed work.
func (c *Commander) TaskDiff(ctx context.Context, taskID string) (TaskDiff, error) {
	task, err := c.Store.GetTask(ctx, taskID)
	if err != nil {
		return TaskDiff{}, err
	}
	if task.Branch == "" {
		return TaskDiff{}, fmt.Errorf("%w: %s", ErrNoWorkspace, taskID)
	}
	project, err := c.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return TaskDiff{}, err
	}

	dir, head := task.WorktreePath, "HEAD"
	live := false
	if dir != "" {
		if _, err := os.Stat(dir); err == nil {
			live = true
		}
	}
	if !live {
		dir, head = project.RepoPath, "refs/heads/"+task.Branch
	}
	base, err := runGit(ctx, dir, "merge-base", baseRef(ctx, project), head)
	if err != nil {
		return TaskDiff{}, fmt.Errorf("fleet: find where %s left %s: %s", task.Branch, project.DefaultBranch, orUnknown(base))
	}

	// Against the working tree when there is one, so uncommitted edits show.
	target := []string{base}
	if !live {
		target = []string{base, head}
	}
	out := TaskDiff{Base: base}
	if out.Stat, err = runGit(ctx, dir, append([]string{"diff", "--stat"}, target...)...); err != nil {
		return TaskDiff{}, fmt.Errorf("fleet: diff of task %s: %s", taskID, orUnknown(out.Stat))
	}
	patch, err := process.RunOutput(ctx, process.Options{
		Dir: dir, Timeout: gitTimeout, OutputLimit: MaxDiffBytes, LogPrefix: "fleet",
		Env: append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"),
	}, "git", append([]string{"diff"}, target...)...)
	if err != nil {
		return TaskDiff{}, fmt.Errorf("fleet: diff of task %s: %w", taskID, err)
	}
	out.Patch = string(patch)
	const marker = "\n[process output truncated]\n"
	if strings.HasSuffix(out.Patch, marker) {
		out.Patch, out.Truncated = strings.TrimSuffix(out.Patch, marker), true
	}
	if live {
		changes, err := worktree.Changes(ctx, dir)
		if err == nil {
			for _, line := range changes {
				if strings.HasPrefix(line, "?? ") {
					out.Untracked = append(out.Untracked, changedPaths([]string{line})...)
				}
			}
		}
	}
	return out, nil
}

// ── digest ───────────────────────────────────────────────────────────────────

// DigestCounts is how many tasks are in each state that matters at a glance.
type DigestCounts struct {
	Queued        int
	Running       int // an agent is working, or its result is being verified
	ReviewReady   int
	NeedsDecision int
	// FailedToday and DoneToday count tasks that reached that status today.
	FailedToday int
	DoneToday   int
}

// WaitingOnOwner is how many tasks cannot move until the owner acts.
func (c DigestCounts) WaitingOnOwner() int { return c.ReviewReady + c.NeedsDecision }

func (c *DigestCounts) add(o DigestCounts) {
	c.Queued += o.Queued
	c.Running += o.Running
	c.ReviewReady += o.ReviewReady
	c.NeedsDecision += o.NeedsDecision
	c.FailedToday += o.FailedToday
	c.DoneToday += o.DoneToday
}

// ProjectDigest is one project's line in the fleet digest.
type ProjectDigest struct {
	ProjectID string
	Name      string
	Counts    DigestCounts
	// Today is what the project has used since the day began.
	Today Usage
}

// Digest is the state of the whole fleet in one structure. It carries
// numbers only; each front end lays them out its own way.
type Digest struct {
	GeneratedAt time.Time
	// DayStart is when "today" began, in the budget's time zone.
	DayStart time.Time
	// Projects lists every project, ordered by ID, including idle ones.
	Projects []ProjectDigest
	// Totals sums the projects.
	Totals DigestCounts
	// Today is what the whole fleet has used since the day began.
	Today Usage
	// PendingWakes is how many notifications are waiting to be delivered.
	PendingWakes int
}

// Digest summarises the fleet: per project, how many tasks are queued,
// running, awaiting review, awaiting a decision, and failed or done today,
// with today's usage.
func (c *Commander) Digest(ctx context.Context) (Digest, error) {
	now := c.Store.now()
	var budget *Budget
	if runner := c.runner(); runner != nil {
		budget = runner.Budget
	}
	digest := Digest{GeneratedAt: now.UTC(), DayStart: budget.dayStart(now)}

	projects, err := c.Store.ListProjects(ctx)
	if err != nil {
		return Digest{}, err
	}
	tasks, err := c.Store.ListTasks(ctx, TaskFilter{})
	if err != nil {
		return Digest{}, err
	}
	counts := map[string]*DigestCounts{}
	for _, t := range tasks {
		dc := counts[t.ProjectID]
		if dc == nil {
			dc = &DigestCounts{}
			counts[t.ProjectID] = dc
		}
		today := t.FinishedAt != nil && !t.FinishedAt.Before(digest.DayStart)
		switch t.Status {
		case StatusQueued:
			dc.Queued++
		case StatusRunning, StatusVerifying:
			dc.Running++
		case StatusReviewReady:
			dc.ReviewReady++
		case StatusNeedsDecision:
			dc.NeedsDecision++
		case StatusFailed:
			if today {
				dc.FailedToday++
			}
		case StatusDone:
			if today {
				dc.DoneToday++
			}
		}
	}
	for _, p := range projects {
		line := ProjectDigest{ProjectID: p.ID, Name: p.Name}
		if dc := counts[p.ID]; dc != nil {
			line.Counts = *dc
		}
		if line.Today, err = c.Store.UsageSince(ctx, p.ID, digest.DayStart); err != nil {
			return Digest{}, err
		}
		digest.Totals.add(line.Counts)
		digest.Projects = append(digest.Projects, line)
	}
	if digest.Projects == nil {
		digest.Projects = []ProjectDigest{}
	}
	if digest.Today, err = c.Store.UsageSince(ctx, "", digest.DayStart); err != nil {
		return Digest{}, err
	}
	pending, err := c.Store.PendingWakes(ctx, 0)
	if err != nil {
		return Digest{}, err
	}
	digest.PendingWakes = len(pending)
	return digest, nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func (c *Commander) runner() *Runner {
	if c.Scheduler != nil {
		return c.Scheduler.Runner
	}
	return nil
}

func (c *Commander) deliverer() *Deliverer {
	switch {
	case c.Deliverer != nil:
		return c.Deliverer
	case c.Scheduler != nil && c.Scheduler.Deliverer != nil:
		return c.Scheduler.Deliverer
	}
	return &Deliverer{Store: c.Store}
}

func (c *Commander) issues() *IssueTracker {
	switch {
	case c.Issues != nil:
		return c.Issues
	case c.Scheduler != nil && c.Scheduler.Issues != nil:
		return c.Scheduler.Issues
	}
	return &IssueTracker{Store: c.Store}
}

// projectOf finds the project to file an audit entry under, even when the
// command failed before returning a task.
func (c *Commander) projectOf(ctx context.Context, task Task, taskID string) string {
	if task.ProjectID != "" {
		return task.ProjectID
	}
	if t, err := c.Store.GetTask(context.WithoutCancel(ctx), taskID); err == nil {
		return t.ProjectID
	}
	return ""
}
