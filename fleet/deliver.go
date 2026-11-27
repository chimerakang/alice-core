package fleet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/verify"
	"github.com/chimerakang/alice-core/worktree"
)

// Deliverer takes a verified ship task the rest of the way, as far as its
// trust mode allows:
//
//   - local-only: the work is committed on its branch and waits. Nothing is
//     pushed. Approve fast-forwards the default branch to it.
//   - direct-pr: the branch is pushed and a pull request opened, and the task
//     is done. Merging is the owner's to do on the forge.
//   - no-mistakes: the branch is pushed and a pull request opened, and the
//     task waits. Approve merges the pull request.
//
// The mode is the one copied onto the task at dispatch; changing the project
// afterwards does not change it. History is never rewritten: no force push,
// no rebase, only a fast-forward or a merge commit.
type Deliverer struct {
	// Store holds the tasks. Required.
	Store *Store
	// Forge opens and merges pull requests. Defaults to GitHub via gh.
	Forge Forge
	// RequireDirectPRReview keeps a delivered task reviewable until acceptance.
	RequireDirectPRReview func(context.Context, string) bool
	// BeforeLanding validates application-specific acceptance requirements.
	BeforeLanding func(context.Context, string) error
}

// Event types written by the Deliverer.
const (
	EventDeliveryCommitted = "delivery.committed"
	EventDeliveryPushed    = "delivery.pushed"
	EventDeliveryPROpened  = "delivery.pr_opened"
	EventDeliveryApproved  = "delivery.approved"
)

var (
	// ErrNotFastForward is returned by Approve when the default branch has
	// moved on and cannot be fast-forwarded to the task's branch.
	ErrNotFastForward = errors.New("fleet: default branch cannot be fast-forwarded to the task branch")
	// ErrNothingToApprove is returned by Approve for a task whose mode has
	// no approval step, or that has nothing awaiting approval.
	ErrNothingToApprove = errors.New("fleet: task has nothing awaiting approval")
)

// Delivery describes what Deliver did.
type Delivery struct {
	// Task is the task as stored afterwards.
	Task Task
	// Committed is true when leftover changes were committed for the agent.
	Committed bool
	// Pushed is true when the branch was pushed.
	Pushed bool
	// PRURL is the task's pull request, when it has one.
	PRURL string
}

// secretsGuard blocks the files that must never be committed on an agent's
// behalf, whatever the task was.
var secretsGuard = verify.NewPathGuardWith(".env", ".env.*", "*.pem", "*.key", "id_rsa", "id_ed25519")

// Deliver carries a ship task that is review_ready as far as its trust mode
// allows. Anything that stops it short (nothing to deliver, a protected file
// in the changes, a failed push) moves the task to needs_decision with the
// reason, and is reported in the returned task rather than as an error.
func (d *Deliverer) Deliver(ctx context.Context, taskID string) (Delivery, error) {
	if d.Store == nil {
		return Delivery{}, errors.New("fleet: Deliverer.Store is required")
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return Delivery{}, err
	}
	if task.Shape != ShapeShip {
		return Delivery{}, fmt.Errorf("fleet: task %s is a %s; only ship tasks are delivered", taskID, task.Shape)
	}
	if task.Status != StatusReviewReady {
		return Delivery{}, fmt.Errorf("fleet: task %s is %s, not review_ready", taskID, task.Status)
	}
	project, err := d.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return Delivery{}, err
	}

	project = task.deliveryProject(project)

	out := Delivery{PRURL: task.PRURL}
	// Past this point the task is always left in a definite state.
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	block := func(format string, args ...any) (Delivery, error) {
		reason := fmt.Sprintf(format, args...)
		out.Task, err = d.Store.Transition(recCtx, taskID, StatusNeedsDecision, reason)
		if err != nil {
			return Delivery{}, fmt.Errorf("fleet: record delivery outcome for task %s (%s): %w", taskID, reason, err)
		}
		return out, nil
	}

	if task.TrustMode == TrustNoMistakes && len(project.VerifyCommands) == 0 {
		return block("no-mistakes mode requires verification commands, and project %s has none", project.ID)
	}

	// Whatever the agent left uncommitted is committed for it, so the branch
	// holds the whole result and the checkout can later be removed safely.
	changes, err := worktree.Changes(ctx, task.WorktreePath)
	if err != nil {
		return block("delivery failed: %v", err)
	}
	if len(changes) > 0 {
		for _, path := range changedPaths(changes) {
			if pattern, blocked := secretsGuard.Check(path); blocked {
				return block("refusing to commit %s (matches %s): remove it from the changes or commit it by hand", path, pattern)
			}
		}
		if msg, err := runGit(ctx, task.WorktreePath, "add", "-A"); err != nil {
			return block("delivery failed: git add: %s", orUnknown(msg))
		}
		if msg, err := runGit(ctx, task.WorktreePath, "commit", "-m", commitMessage(task)); err != nil {
			return block("delivery failed: git commit: %s", orUnknown(msg))
		}
		out.Committed = true
		_, _ = d.Store.AppendEvent(recCtx, taskID, EventDeliveryCommitted, map[string]any{"files": len(changes)})
	}

	ahead, err := runGit(ctx, task.WorktreePath, "rev-list", "--count", baseRef(ctx, project)+"..HEAD")
	if err != nil {
		return block("delivery failed: cannot compare with %s: %s", project.DefaultBranch, orUnknown(ahead))
	}
	if ahead == "0" {
		return block("nothing to deliver: the task changed nothing")
	}

	if !task.TrustMode.NeedsRemote() {
		out.Task = task
		return out, nil // local-only: committed, and waiting for the owner
	}

	if project.Remote == "" {
		return block("delivery failed: project %s has no remote to push to", project.ID)
	}
	// A plain push: it fails rather than overwrite anything on the remote.
	if msg, err := runGit(ctx, task.WorktreePath, "push", "-u", project.Remote, task.Branch); err != nil {
		return block("delivery failed: git push: %s", orUnknown(msg))
	}
	out.Pushed = true
	_, _ = d.Store.AppendEvent(recCtx, taskID, EventDeliveryPushed, map[string]any{"remote": project.Remote, "branch": task.Branch})

	if task.PRURL == "" {
		url, number, err := d.forge().OpenPullRequest(ctx, task.WorktreePath, PullRequest{
			Branch: task.Branch, Base: project.DefaultBranch, Title: prTitle(task), Body: prBody(task, project),
		})
		if err != nil {
			return block("delivery failed: open pull request: %v", err)
		}
		if err := d.Store.SetPullRequest(recCtx, taskID, url, number); err != nil {
			return Delivery{}, err
		}
		out.PRURL = url
		_, _ = d.Store.AppendEvent(recCtx, taskID, EventDeliveryPROpened, map[string]any{"url": url, "number": number})
	}

	if task.TrustMode == TrustDirectPR && (d.RequireDirectPRReview == nil || !d.RequireDirectPRReview(recCtx, taskID)) {
		// Opening the pull request is the whole job in this mode.
		out.Task, err = d.Store.Transition(recCtx, taskID, StatusDone, "pull request opened: "+out.PRURL)
		if err != nil {
			return Delivery{}, err
		}
		return out, nil
	}
	out.Task, err = d.Store.GetTask(recCtx, taskID)
	if err != nil {
		return Delivery{}, err
	}
	return out, nil
}

// Approve is the owner's single act of acceptance for a task that is
// review_ready, and the only thing that lands its work:
//
//   - local-only: the project's default branch is fast-forwarded to the
//     task's branch. If that is not possible because the default branch has
//     moved on, ErrNotFastForward is returned and nothing changes.
//   - no-mistakes: the task's pull request is merged.
//
// approver is recorded for the audit trail. There is one approver, the
// project's owner; checking who may call this is the caller's job.
func (d *Deliverer) Approve(ctx context.Context, taskID, approver string) (Task, error) {
	if d.Store == nil {
		return Task{}, errors.New("fleet: Deliverer.Store is required")
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	if task.Status != StatusReviewReady || task.Shape != ShapeShip {
		return Task{}, fmt.Errorf("%w: task %s is a %s task in %s", ErrNothingToApprove, taskID, task.Shape, task.Status)
	}
	if d.BeforeLanding != nil {
		if err := d.BeforeLanding(ctx, taskID); err != nil {
			return Task{}, err
		}
	}
	project, err := d.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return Task{}, err
	}
	project = task.deliveryProject(project)
	approver = firstNonEmpty(strings.TrimSpace(approver), "owner")

	var landed string
	switch task.TrustMode {
	case TrustLocalOnly:
		if err := fastForward(ctx, project, task.Branch); err != nil {
			return Task{}, err
		}
		landed = "fast-forwarded " + project.DefaultBranch
	case TrustNoMistakes:
		if task.PRNumber == 0 {
			return Task{}, fmt.Errorf("%w: task %s has no pull request yet; deliver it first", ErrNothingToApprove, taskID)
		}
		if err := d.forge().MergePullRequest(ctx, task.WorktreePath, task.PRNumber); err != nil {
			return Task{}, err
		}
		landed = "merged " + task.PRURL
	default:
		return Task{}, fmt.Errorf("%w: %s mode has no approval step", ErrNothingToApprove, task.TrustMode)
	}

	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_, _ = d.Store.AppendEvent(recCtx, taskID, EventDeliveryApproved, map[string]any{"approver": approver, "landed": landed})
	return d.Store.Transition(recCtx, taskID, StatusDone, fmt.Sprintf("approved by %s: %s", approver, landed))
}

// Merge lands a task's work on the owner's say-so, wherever the task stands:
//
//   - a task that is review_ready is approved, exactly as Approve does;
//   - a task already done in direct-pr mode has its pull request merged, for
//     the owner who would rather say "merge it" than open the forge.
//
// Anything else has nothing to merge and returns ErrNothingToApprove.
func (d *Deliverer) Merge(ctx context.Context, taskID, approver string) (Task, error) {
	if d.Store == nil {
		return Task{}, errors.New("fleet: Deliverer.Store is required")
	}
	task, err := d.Store.GetTask(ctx, taskID)
	if err != nil {
		return Task{}, err
	}
	directReview := task.Status == StatusReviewReady && task.TrustMode == TrustDirectPR
	if task.Status == StatusReviewReady && !directReview {
		return d.Approve(ctx, taskID, approver)
	}
	if (task.Status != StatusDone && !directReview) || task.PRNumber == 0 {
		return Task{}, fmt.Errorf("%w: task %s is %s and has no open pull request to merge", ErrNothingToApprove, taskID, task.Status)
	}
	if d.BeforeLanding != nil {
		if err := d.BeforeLanding(ctx, taskID); err != nil {
			return Task{}, err
		}
	}
	if err := d.forge().MergePullRequest(ctx, task.WorktreePath, task.PRNumber); err != nil {
		return Task{}, err
	}
	approver = firstNonEmpty(strings.TrimSpace(approver), "owner")
	recCtx := context.WithoutCancel(ctx)
	_, _ = d.Store.AppendEvent(recCtx, taskID, EventDeliveryApproved,
		map[string]any{"approver": approver, "landed": "merged " + task.PRURL})
	if directReview {
		return d.Store.Transition(recCtx, taskID, StatusDone, "approved by "+approver+": merged "+task.PRURL)
	}
	return d.Store.GetTask(recCtx, taskID)
}

// fastForward moves the project's default branch to branch without creating
// a commit, and only if nothing would be lost.
func fastForward(ctx context.Context, project Project, branch string) error {
	repo, base := project.RepoPath, project.DefaultBranch
	if _, err := runGit(ctx, repo, "merge-base", "--is-ancestor", "refs/heads/"+base, "refs/heads/"+branch); err != nil {
		if isExitError(err) {
			return fmt.Errorf("%w: %s has commits that %s does not", ErrNotFastForward, base, branch)
		}
		return fmt.Errorf("fleet: compare %s with %s: %w", base, branch, err)
	}
	current, _ := runGit(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
	if current == base {
		// The default branch is checked out in the main working tree, so its
		// files have to move with it. git refuses if that would overwrite
		// local changes.
		if msg, err := runGit(ctx, repo, "merge", "--ff-only", "refs/heads/"+branch); err != nil {
			return fmt.Errorf("fleet: fast-forward %s: %s", base, orUnknown(msg))
		}
		return nil
	}
	// Not checked out: move the branch alone. A fetch into a branch only
	// ever fast-forwards.
	if msg, err := runGit(ctx, repo, "fetch", ".", "refs/heads/"+branch+":refs/heads/"+base); err != nil {
		return fmt.Errorf("fleet: fast-forward %s: %s", base, orUnknown(msg))
	}
	return nil
}

// changedPaths extracts the file paths from `git status --porcelain` lines.
func changedPaths(lines []string) []string {
	var paths []string
	for _, line := range lines {
		if len(line) < 4 {
			continue
		}
		entry := line[3:]
		// A rename reads "old -> new"; both names matter.
		for _, path := range strings.Split(entry, " -> ") {
			path = strings.Trim(strings.TrimSpace(path), `"`)
			if path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths
}

func firstLine(s string, max int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	line = strings.TrimSpace(line)
	if runes := []rune(line); len(runes) > max {
		line = string(runes[:max-1]) + "…"
	}
	return line
}

func taskSubject(task Task) string {
	if subject := firstLine(task.Goal, 72); subject != "" {
		return subject
	}
	if task.Source.Kind == SourceIssue {
		return fmt.Sprintf("Work on issue #%d", task.Source.IssueNumber)
	}
	return "Task " + task.ID
}

func commitMessage(task Task) string {
	return taskSubject(task) + "\n\nTask: " + task.ID
}

func prTitle(task Task) string { return taskSubject(task) }

func prBody(task Task, project Project) string {
	var sb strings.Builder
	if goal := strings.TrimSpace(task.Goal); goal != "" {
		sb.WriteString(goal)
		sb.WriteString("\n\n")
	}
	if task.Source.Kind == SourceIssue {
		fmt.Fprintf(&sb, "Refs #%d\n\n", task.Source.IssueNumber)
	}
	if len(project.VerifyCommands) > 0 {
		sb.WriteString("Verified before this pull request was opened:\n\n")
		for _, c := range project.VerifyCommands {
			fmt.Fprintf(&sb, "- `%s`\n", c)
		}
		sb.WriteString("\n")
	}
	fmt.Fprintf(&sb, "Task: %s", task.ID)
	return sb.String()
}

func (d *Deliverer) forge() Forge {
	if d.Forge != nil {
		return d.Forge
	}
	return GitHub{}
}
