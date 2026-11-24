package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/ghissue"
	"github.com/chimerakang/alice-core/issueops"
)

// IssueHost is where issues live. It is the part of GitHub that issue-driven
// tasks need: read an issue, comment on it, tick its checklist.
type IssueHost interface {
	Fetch(ctx context.Context, dir string, number int) (*ghissue.IssueContext, error)
	Comment(ctx context.Context, dir string, number int, body string) error
	SyncChecklist(ctx context.Context, req issueops.SyncChecklistRequest) (issueops.SyncChecklistResult, error)
}

// GitHubIssues is an IssueHost backed by the gh CLI.
type GitHubIssues struct{}

func (GitHubIssues) Fetch(ctx context.Context, dir string, number int) (*ghissue.IssueContext, error) {
	return ghissue.FetchIssue(ctx, dir, number)
}

func (GitHubIssues) Comment(ctx context.Context, dir string, number int, body string) error {
	return ghissue.PostComment(ctx, dir, number, body)
}

func (GitHubIssues) SyncChecklist(ctx context.Context, req issueops.SyncChecklistRequest) (issueops.SyncChecklistResult, error) {
	return issueops.New().SyncChecklist(ctx, req)
}

// IssueTracker turns GitHub issues into tasks and keeps each issue informed
// of what its task is doing.
//
// A checklist item is ticked only when two things agree: the agent declared
// the item complete, and the task passed verification with at least one real
// check. Neither alone is enough. An agent's word is not evidence, and a
// passing build does not say which items were done.
type IssueTracker struct {
	// Store holds the tasks. Required.
	Store *Store
	// Host talks to the issue tracker. Defaults to GitHub via gh.
	Host IssueHost
	// NoComments turns off the comments posted to the issue.
	NoComments bool
	// NoChecklistSync leaves the issue's checklist untouched.
	NoChecklistSync bool
}

var (
	// ErrIssueClosed is returned by Dispatch for an issue that is closed.
	ErrIssueClosed = errors.New("fleet: issue is closed")
	// ErrIssueBusy is returned by Dispatch when the issue already has an
	// unfinished task in the project.
	ErrIssueBusy = errors.New("fleet: issue already has an unfinished task")
)

// Event types written by the IssueTracker.
const (
	// EventIssueLinked records the issue a task was dispatched from and the
	// checklist items it was asked to complete.
	EventIssueLinked = "issue.linked"
	// EventIssueDeclared records the items the agent says it completed.
	EventIssueDeclared = "issue.declared"
	// EventIssueSynced records what was ticked on the issue.
	EventIssueSynced = "issue.synced"
	// EventIssueError records an issue-tracker call that failed. It never
	// changes the task: an unreachable tracker must not fail the work.
	EventIssueError = "issue.error"
)

// IssueItem is one checklist item a task was asked to complete.
type IssueItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type issueLink struct {
	Number int         `json:"number"`
	Title  string      `json:"title"`
	Items  []IssueItem `json:"items"`
}

// DispatchOptions adjusts the task created for an issue.
type DispatchOptions struct {
	// Shape defaults to ShapeShip.
	Shape    Shape
	Model    string
	Priority int
	// Budget, when set, caps the task's total usage. See Task.Budget.
	Budget Limit
}

// Dispatch queues a task for an issue of the project. The task's goal is the
// issue's title and body, followed by its open acceptance items, each with
// an ID the agent uses to report what it completed.
func (t *IssueTracker) Dispatch(ctx context.Context, projectID string, number int, opts DispatchOptions) (Task, error) {
	if t.Store == nil {
		return Task{}, errors.New("fleet: IssueTracker.Store is required")
	}
	if number <= 0 {
		return Task{}, fmt.Errorf("%w: issue number must be positive", ErrInvalidTask)
	}
	project, err := t.Store.GetProject(ctx, projectID)
	if err != nil {
		return Task{}, err
	}
	issue, err := t.host().Fetch(ctx, project.RepoPath, number)
	if err != nil {
		return Task{}, fmt.Errorf("fleet: load issue #%d: %w", number, err)
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), "closed") {
		return Task{}, fmt.Errorf("%w: #%d", ErrIssueClosed, number)
	}

	unfinished, err := t.Store.ListTasks(ctx, TaskFilter{ProjectID: projectID, Statuses: []Status{
		StatusQueued, StatusRunning, StatusVerifying, StatusReviewReady, StatusNeedsDecision,
	}})
	if err != nil {
		return Task{}, err
	}
	for _, other := range unfinished {
		if other.Source.Kind == SourceIssue && other.Source.IssueNumber == number {
			return Task{}, fmt.Errorf("%w: #%d is being handled by task %s (%s)", ErrIssueBusy, number, other.ID, other.Status)
		}
	}

	items := openItems(issue)
	task, err := t.Store.CreateTask(ctx, NewTask{
		ProjectID: projectID, Shape: opts.Shape, Model: opts.Model, Priority: opts.Priority, Budget: opts.Budget,
		Goal:   issueGoal(number, issue, items),
		Source: Source{Kind: SourceIssue, IssueNumber: number},
	})
	if err != nil {
		return Task{}, err
	}
	_, _ = t.Store.AppendEvent(ctx, task.ID, EventIssueLinked, issueLink{Number: number, Title: issue.Title, Items: items})
	return task, nil
}

// openItems returns the issue's unchecked acceptance items. Items under
// headings that are not acceptance criteria (a list of sub-issues, say) are
// not the task's to complete.
func openItems(issue *ghissue.IssueContext) []IssueItem {
	items := []IssueItem{}
	for _, item := range issue.Checklist {
		if item.Checked || !ghissue.IsAcceptanceSection(item.Section) {
			continue
		}
		items = append(items, IssueItem{ID: item.ID, Text: item.Text})
	}
	return items
}

func issueGoal(number int, issue *ghissue.IssueContext, items []IssueItem) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Work on GitHub issue #%d: %s\n", number, strings.TrimSpace(issue.Title))
	if body := strings.TrimSpace(issue.Body); body != "" {
		sb.WriteString("\n")
		sb.WriteString(body)
		sb.WriteString("\n")
	}
	if len(items) == 0 {
		sb.WriteString("\nDo not edit the issue yourself.")
		return sb.String()
	}
	sb.WriteString("\nAcceptance items still open. Each has an ID:\n\n")
	for _, item := range items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.ID, item.Text)
	}
	sb.WriteString("\nComplete these. Items already checked in the issue are done; do not redo them.\n\n")
	sb.WriteString("End your final message with one line naming the items you fully completed, in exactly this form:\n\n")
	fmt.Fprintf(&sb, "%s %s\n\n", completedPrefix, exampleIDs(items))
	fmt.Fprintf(&sb, "List an item only if it is completely done. If you completed none, write \"%s none\". ", completedPrefix)
	sb.WriteString("Do not edit the issue yourself; its checklist is updated for you once your work has been verified.")
	return sb.String()
}

func exampleIDs(items []IssueItem) string {
	ids := make([]string, 0, 2)
	for i := 0; i < len(items) && i < 2; i++ {
		ids = append(ids, items[i].ID)
	}
	return strings.Join(ids, ", ")
}

const completedPrefix = "Completed items:"

var completedRe = regexp.MustCompile(`(?mi)^[\s*_>-]*completed items:[\s*_]*(.*)$`)

// ParseCompletedItems reads the agent's declaration of finished checklist
// items from its final message. declared is false when the message has no
// such line; an explicit "none" is a declaration of nothing.
func ParseCompletedItems(text string) (ids []string, declared bool) {
	matches := completedRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil, false
	}
	last := matches[len(matches)-1][1] // a later line corrects an earlier one
	seen := map[string]bool{}
	ids = []string{}
	for _, field := range strings.FieldsFunc(last, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '`' || r == '*' || r == '.' || r == ';' || r == '[' || r == ']'
	}) {
		field = strings.ToLower(strings.TrimSpace(field))
		if strings.HasPrefix(field, "item-") && !seen[field] {
			seen[field] = true
			ids = append(ids, field)
		}
	}
	return ids, true
}

// isTracked reports whether the tracker follows this task.
func (t *IssueTracker) isTracked(task Task) bool {
	return t != nil && task.Source.Kind == SourceIssue && task.Source.IssueNumber > 0
}

// Started tells the issue that work on it has begun.
func (t *IssueTracker) Started(ctx context.Context, task Task) {
	if !t.isTracked(task) || t.NoComments {
		return
	}
	body := fmt.Sprintf("🤖 **Alice 開始處理**（任務 `%s`）\n\n- 分支: `%s`\n- 信任模式: %s", task.ID, task.Branch, task.TrustMode)
	t.comment(ctx, task, body)
}

// NoteResult records the checklist items the agent declared complete in a
// run's final message, if it declared any.
func (t *IssueTracker) NoteResult(ctx context.Context, task Task, text string) {
	if !t.isTracked(task) {
		return
	}
	if ids, declared := ParseCompletedItems(text); declared {
		_, _ = t.Store.AppendEvent(ctx, task.ID, EventIssueDeclared, map[string]any{"items": ids})
	}
}

// Finished reports how a run-and-verify cycle left the task, and, when the
// work was verified, ticks the checklist items the agent declared complete.
func (t *IssueTracker) Finished(ctx context.Context, task Task) {
	if !t.isTracked(task) {
		return
	}
	// The issue should hear the outcome even if the caller is shutting down.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	switch task.Status {
	case StatusReviewReady, StatusDone:
		checks := t.latestChecks(ctx, task)
		ticked, remaining, note := t.syncChecklist(ctx, task, checks)
		if !t.NoComments {
			t.comment(ctx, task, successComment(task, checks, ticked, remaining, note))
		}
	case StatusNeedsDecision, StatusFailed:
		if !t.NoComments {
			t.comment(ctx, task, problemComment(task))
		}
	}
}

// latestChecks returns the checks of the task's most recent verification round.
func (t *IssueTracker) latestChecks(ctx context.Context, task Task) []Verification {
	all, err := t.Store.ListVerifications(ctx, task.ID)
	if err != nil || len(all) == 0 {
		return nil
	}
	last := all[len(all)-1].Attempt
	var checks []Verification
	for _, c := range all {
		if c.Attempt == last {
			checks = append(checks, c)
		}
	}
	return checks
}

// syncChecklist ticks the items that were both declared complete and covered
// by a passing verification. It returns the texts ticked, the texts still
// open, and a note explaining anything that was held back.
func (t *IssueTracker) syncChecklist(ctx context.Context, task Task, checks []Verification) (ticked, remaining []string, note string) {
	link, ok := t.link(ctx, task)
	if !ok || len(link.Items) == 0 {
		return nil, nil, ""
	}
	for _, item := range link.Items {
		remaining = append(remaining, item.Text)
	}
	if t.NoChecklistSync {
		return nil, remaining, ""
	}
	if len(checks) == 0 {
		return nil, remaining, "專案沒有設定驗收指令，所以沒有自動勾選任何項目。"
	}
	for _, c := range checks {
		if !c.Passed {
			return nil, remaining, "驗收未通過，所以沒有勾選任何項目。"
		}
	}
	declared, ok := t.declared(ctx, task)
	if !ok {
		return nil, remaining, "Agent 沒有回報完成了哪些項目，所以沒有自動勾選。"
	}
	if len(declared) == 0 {
		return nil, remaining, "Agent 回報沒有完整完成任何項目。"
	}

	project, err := t.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return nil, remaining, ""
	}
	// Item IDs are line numbers, and the issue may have been edited since
	// dispatch. Match by text against the issue as it is now.
	current, err := t.host().Fetch(ctx, project.RepoPath, task.Source.IssueNumber)
	if err != nil {
		t.recordError(ctx, task, "fetch", err)
		return nil, remaining, "無法讀取 issue，checklist 未更新。"
	}
	textOf := map[string]string{}
	for _, item := range link.Items {
		textOf[item.ID] = item.Text
	}
	var ids, want []string
	for _, id := range declared {
		text, known := textOf[id]
		if !known {
			continue // not an item this task was given
		}
		for _, now := range current.Checklist {
			if now.Text == text {
				if !now.Checked {
					ids = append(ids, now.ID)
				}
				want = append(want, text)
				break
			}
		}
	}
	if len(want) == 0 {
		return nil, remaining, "Agent 回報的項目在 issue 中已找不到，checklist 未更新。"
	}

	if len(ids) > 0 {
		res, err := t.host().SyncChecklist(ctx, issueops.SyncChecklistRequest{
			ProjectDir: project.RepoPath, IssueNumber: task.Source.IssueNumber,
			SubTasks: []issueops.WorkItem{{ID: task.ID, Description: taskSubject(task), Done: true, ChecklistItemIDs: ids}},
		})
		if err != nil {
			t.recordError(ctx, task, "sync_checklist", err)
			return nil, remaining, "更新 checklist 失敗。"
		}
		if !res.Wrote && res.Outcome != issueops.SyncOutcomeNoChange {
			return nil, remaining, "checklist 未更新：" + firstNonEmpty(res.Reason, string(res.Outcome))
		}
	}

	done := map[string]bool{}
	for _, text := range want {
		done[text] = true
	}
	remaining = nil
	for _, item := range link.Items {
		if !done[item.Text] {
			remaining = append(remaining, item.Text)
		}
	}
	_, _ = t.Store.AppendEvent(ctx, task.ID, EventIssueSynced, map[string]any{"ticked": want})
	return want, remaining, ""
}

func (t *IssueTracker) link(ctx context.Context, task Task) (issueLink, bool) {
	ev, ok, err := t.Store.lastEvent(ctx, task.ID, EventIssueLinked)
	if err != nil || !ok {
		return issueLink{}, false
	}
	var link issueLink
	if json.Unmarshal(ev.Payload, &link) != nil {
		return issueLink{}, false
	}
	return link, true
}

func (t *IssueTracker) declared(ctx context.Context, task Task) ([]string, bool) {
	ev, ok, err := t.Store.lastEvent(ctx, task.ID, EventIssueDeclared)
	if err != nil || !ok {
		return nil, false
	}
	var payload struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(ev.Payload, &payload) != nil {
		return nil, false
	}
	return payload.Items, true
}

func (t *IssueTracker) comment(ctx context.Context, task Task, body string) {
	project, err := t.Store.GetProject(ctx, task.ProjectID)
	if err != nil {
		return
	}
	if err := t.host().Comment(ctx, project.RepoPath, task.Source.IssueNumber, body); err != nil {
		t.recordError(ctx, task, "comment", err)
	}
}

func (t *IssueTracker) recordError(ctx context.Context, task Task, op string, err error) {
	_, _ = t.Store.AppendEvent(ctx, task.ID, EventIssueError, map[string]any{"op": op, "error": err.Error()})
}

func (t *IssueTracker) host() IssueHost {
	if t.Host != nil {
		return t.Host
	}
	return GitHubIssues{}
}

func successComment(task Task, checks []Verification, ticked, remaining []string, note string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "✅ **Alice 完成**（任務 `%s`）\n\n", task.ID)
	switch {
	case task.PRURL != "" && task.Status == StatusDone:
		fmt.Fprintf(&sb, "- 狀態: 已開 PR %s\n", task.PRURL)
	case task.PRURL != "":
		fmt.Fprintf(&sb, "- 狀態: 已開 PR %s，等待你核准\n", task.PRURL)
	case task.Status == StatusDone:
		sb.WriteString("- 狀態: 已完成\n")
	default:
		sb.WriteString("- 狀態: 等待你審核\n")
	}
	if task.Branch != "" {
		fmt.Fprintf(&sb, "- 分支: `%s`\n", task.Branch)
	}
	fmt.Fprintf(&sb, "- 用量: %d tokens，$%.2f\n", task.InputTokens+task.OutputTokens, task.CostUSD)

	if len(checks) > 0 {
		sb.WriteString("\n**驗收**\n")
		for _, c := range checks {
			mark := "✅"
			if !c.Passed {
				mark = "❌"
			}
			fmt.Fprintf(&sb, "- %s `%s`（exit %d，%s）\n", mark, c.Command, c.ExitCode, c.Duration.Round(100*time.Millisecond))
		}
	}
	writeList(&sb, "已勾選的項目", ticked)
	writeList(&sb, "仍未完成的項目", remaining)
	if note != "" {
		sb.WriteString("\n" + note + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

func problemComment(task Task) string {
	title := "⚠️ **Alice 需要你決定**"
	if task.Status == StatusFailed {
		title = "❌ **Alice 執行失敗**"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s（任務 `%s`）\n\n- 原因: %s\n", title, task.ID, firstNonEmpty(task.StatusReason, "未說明"))
	if task.Branch != "" {
		fmt.Fprintf(&sb, "- 分支: `%s`\n", task.Branch)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func writeList(sb *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n**%s**\n", heading)
	for _, item := range items {
		fmt.Fprintf(sb, "- %s\n", item)
	}
}
