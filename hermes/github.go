package hermes

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/ghissue"
)

// The GitHub issue helpers live in the ghissue package. The aliases and
// wrappers below keep the previous hermes API working for existing callers.

type (
	IssueContext           = ghissue.IssueContext
	IssueComment           = ghissue.IssueComment
	ChecklistItem          = ghissue.ChecklistItem
	IssueReconciliation    = ghissue.IssueReconciliation
	IssueListItem          = ghissue.IssueListItem
	ChecklistSyncPreview   = ghissue.ChecklistSyncPreview
	HermesCompletionSignal = ghissue.HermesCompletionSignal
	GithubCfg              = ghissue.Config
)

// ListIssues is ghissue.ListIssues.
func ListIssues(ctx context.Context, projectDir string, limit int) ([]IssueListItem, error) {
	return ghissue.ListIssues(ctx, projectDir, limit)
}

// FormatIssueList is ghissue.FormatIssueList.
func FormatIssueList(items []IssueListItem) string { return ghissue.FormatIssueList(items) }

// FetchIssue is ghissue.FetchIssue.
func FetchIssue(ctx context.Context, projectDir string, number int) (*IssueContext, error) {
	return ghissue.FetchIssue(ctx, projectDir, number)
}

// ExtractChecklist is ghissue.ExtractChecklist.
func ExtractChecklist(body string) []ChecklistItem { return ghissue.ExtractChecklist(body) }

// IsAcceptanceSection is ghissue.IsAcceptanceSection.
func IsAcceptanceSection(section string) bool { return ghissue.IsAcceptanceSection(section) }

// ReconcileIssueCompletion is ghissue.ReconcileIssueCompletion.
func ReconcileIssueCompletion(issue *IssueContext) IssueReconciliation {
	return ghissue.ReconcileIssueCompletion(issue)
}

// BuildIssueStateSnapshot is ghissue.BuildIssueStateSnapshot.
func BuildIssueStateSnapshot(issue *IssueContext) string {
	return ghissue.BuildIssueStateSnapshot(issue)
}

// RecentSuccessfulHermesCompletion is ghissue.RecentSuccessfulHermesCompletion.
func RecentSuccessfulHermesCompletion(issue *IssueContext) (HermesCompletionSignal, bool) {
	return ghissue.RecentSuccessfulHermesCompletion(issue)
}

// UpdateChecklistInBody is ghissue.UpdateChecklistInBody.
func UpdateChecklistInBody(body string, completedDescs []string) string {
	return ghissue.UpdateChecklistInBody(body, completedDescs)
}

// UpdateIssueBody is ghissue.UpdateIssueBody.
func UpdateIssueBody(ctx context.Context, projectDir string, number int, body string) error {
	return ghissue.UpdateIssueBody(ctx, projectDir, number, body)
}

// PostComment is ghissue.PostComment.
func PostComment(ctx context.Context, projectDir string, number int, body string) error {
	return ghissue.PostComment(ctx, projectDir, number, body)
}

// ApplyLabel is ghissue.ApplyLabel.
func ApplyLabel(ctx context.Context, projectDir string, number int, label string) error {
	return ghissue.ApplyLabel(ctx, projectDir, number, label)
}

// CloseIssue is ghissue.CloseIssue.
func CloseIssue(ctx context.Context, projectDir string, number int) error {
	return ghissue.CloseIssue(ctx, projectDir, number)
}

// HasLabel is ghissue.HasLabel.
func HasLabel(issue *IssueContext, label string) bool { return ghissue.HasLabel(issue, label) }

// completedWork maps the done sub-tasks onto the orchestrator-neutral input
// ghissue expects. Sub-tasks in any other status are ignored.
func completedWork(subtasks []SubTask) []ghissue.CompletedWork {
	done := make([]ghissue.CompletedWork, 0, len(subtasks))
	for _, st := range subtasks {
		if st.Status != SubTaskDone {
			continue
		}
		done = append(done, ghissue.CompletedWork{
			Description:      st.Description,
			ChecklistItemIDs: st.ChecklistItemIDs,
		})
	}
	return done
}

// BuildChecklistSyncPreview computes the checklist body patch for the done
// sub-tasks without mutating remote state. See ghissue.BuildChecklistSyncPreview
// for how declared item IDs and fuzzy text matching are chosen.
func BuildChecklistSyncPreview(body string, subtasks []SubTask) ChecklistSyncPreview {
	return ghissue.BuildChecklistSyncPreview(body, completedWork(subtasks))
}

// SyncChecklist fetches the current Issue body and checks off completed sub-tasks.
func SyncChecklist(ctx context.Context, projectDir string, number int, subtasks []SubTask) error {
	return ghissue.SyncChecklist(ctx, projectDir, number, completedWork(subtasks))
}

// BuildGoalFromIssue assembles the Planner goal string from an Issue.
// If the issue has an unchecked checklist, the Planner is instructed to use it.
func BuildGoalFromIssue(issue *IssueContext) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[GitHub #%d] %s\n\n", issue.Number, issue.Title))
	sb.WriteString(BuildIssueStateSnapshot(issue))
	sb.WriteString("\n")

	var unchecked, checked []ChecklistItem
	for _, item := range issue.Checklist {
		if !item.Checked {
			unchecked = append(unchecked, item)
		} else {
			checked = append(checked, item)
		}
	}

	if len(unchecked) > 0 {
		sb.WriteString("Remaining unchecked issue task list — plan ONLY these as SubTask descriptions.\n")
		sb.WriteString("Each item is prefixed with [item-N]; copy that ID into the sub-task's `checklist_item_ids` field per CHECKLIST DECLARATION RULE.\n")
		for _, item := range unchecked {
			id := strings.TrimSpace(item.ID)
			if id == "" {
				id = fmt.Sprintf("item-%d", item.LineNumber)
			}
			fmt.Fprintf(&sb, "  - [ ] [%s] %s\n", id, item.Text)
		}
		sb.WriteString("\n")
		if len(checked) > 0 {
			sb.WriteString("Already checked issue items — treat as completed context; DO NOT plan or redo these unless explicitly requested:\n")
			for _, item := range checked {
				sb.WriteString("  - [x] ")
				sb.WriteString(item.Text)
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	if issue.Body != "" {
		body := issue.Body
		if len(issue.Checklist) > 0 {
			body = ghissue.StripChecklistLines(body)
		}
		if strings.TrimSpace(body) != "" {
			sb.WriteString("Issue description (checklist lines omitted to avoid re-planning completed items):\n")
			sb.WriteString(body)
		}
	}

	return sb.String()
}

// WritePlanToIssue writes the generated sub-task plan to the Issue body.
//
// The body is always re-fetched from GitHub before mutation: any previously
// written plan section is stripped and a fresh one appended.
//
// originalBody is retained for backward compatibility but no longer used.
func WritePlanToIssue(ctx context.Context, projectDir string, number int, originalBody string, tasks []SubTask) error {
	_ = originalBody // intentionally ignored; see doc comment

	body, err := ghissue.FetchBody(ctx, projectDir, number)
	if err != nil {
		return fmt.Errorf("fetch body for plan write: %w", err)
	}
	cleanBody := ghissue.StripPlanSections(body)

	var sb strings.Builder
	sb.WriteString(cleanBody)
	if cleanBody != "" && !strings.HasSuffix(cleanBody, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("\n## " + ghissue.PlanSectionTitle + "\n\n")
	for idx, t := range tasks {
		fmt.Fprintf(&sb, "%d. %s", idx+1, t.Description)
		if len(t.ChecklistItemIDs) > 0 {
			sb.WriteString(" _(covers: ")
			sb.WriteString(strings.Join(t.ChecklistItemIDs, ", "))
			sb.WriteString(")_")
		}
		sb.WriteString("\n")
	}

	if err := ghissue.UpdateIssueBody(ctx, projectDir, number, sb.String()); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// ── Comment builders ──────────────────────────────────────────────────────────

// CommentStarted builds the "Hermes started" lifecycle comment.
func CommentStarted(plannerModel, executorModel string) string {
	return fmt.Sprintf("🤖 **Hermes 開始執行**\n\n- Planner: `%s`\n- Executor: `%s`\n- 開始時間: %s",
		plannerModel, executorModel,
		time.Now().Format("2006-01-02 15:04:05"))
}

// CommentDone builds the "all done" lifecycle comment.
func CommentDone(state TaskState) string {
	return CommentDoneWithNote(state, "")
}

func CommentDoneWithNote(state TaskState, note string) string {
	var sb strings.Builder
	done := 0
	for _, st := range state.Plan {
		if st.Status == SubTaskDone {
			done++
		}
	}
	elapsed := ""
	if !state.TokenBudget.StartedAt.IsZero() {
		elapsed = time.Since(state.TokenBudget.StartedAt).Round(time.Second).String()
	}

	sb.WriteString(fmt.Sprintf("✅ **Hermes 完成** %d/%d SubTasks\n\n", done, len(state.Plan)))
	sb.WriteString(fmt.Sprintf("- Token 用量: %d", state.TokenBudget.UsedTokens))
	if state.TokenBudget.MaxTotalTokens > 0 {
		sb.WriteString(fmt.Sprintf("/%d", state.TokenBudget.MaxTotalTokens))
	}
	sb.WriteString("\n")
	if elapsed != "" {
		sb.WriteString(fmt.Sprintf("- 執行時間: %s\n", elapsed))
	}

	if len(state.Artifacts) > 0 {
		sb.WriteString("\n**修改的檔案：**\n")
		for _, a := range state.Artifacts {
			if a.Hash != "" {
				sb.WriteString(fmt.Sprintf("- `%s` (%s)\n", a.Path, a.Hash[:min8(len(a.Hash))]))
			} else {
				sb.WriteString(fmt.Sprintf("- `%s`\n", a.Path))
			}
		}
	}
	if strings.TrimSpace(note) != "" {
		sb.WriteString("\n**Note:**\n")
		sb.WriteString(strings.TrimSpace(note))
		sb.WriteString("\n")
	}
	return sb.String()
}

// CommentFailed builds the "task failed" lifecycle comment.
func CommentFailed(failedDesc, reason string, doneCount, totalCount int) string {
	return fmt.Sprintf("❌ **Hermes 執行失敗**\n\n"+
		"- 失敗於子任務: `%s`\n"+
		"- 原因: %s\n"+
		"- 已完成: %d/%d\n\n"+
		"建議：修正上述問題後重新執行 `/hermes #<issue>`",
		failedDesc, reason, doneCount, totalCount)
}

// CommentBudgetExceeded builds the "budget exceeded" lifecycle comment.
func CommentBudgetExceeded(used, max int) string {
	return fmt.Sprintf("⚠️ **Hermes Budget 耗盡**\n\n"+
		"- Token 用量: %d / %d\n\n"+
		"可增加 `hermes.budget.max_total_tokens` 後重新執行。",
		used, max)
}

// CommentSubTaskProgress builds a per-subtask progress comment.
func CommentSubTaskProgress(idx, totalCount int, subTask SubTask, resultText string, execTokens, completedCount int) string {
	return fmt.Sprintf("✅ **子任務進度** (%d/%d)\n\n"+
		"- 子任務: `%s`\n"+
		"- 結果: %s\n"+
		"- Token 用量: %d\n"+
		"- 完成進度: %d/%d\n",
		idx+1, totalCount, subTask.Description, resultText, execTokens, completedCount, totalCount)
}

// PostCommentEvent posts a lifecycle event comment to the Issue.
// event must be one of: "start", "complete", "fail", "budget_exceeded".
// payload type depends on the event:
//   - "start": map[string]string with keys "planner_model", "executor_model"
//   - "complete": TaskState
//   - "fail": map[string]interface{} with keys "failed_description" (string), "reason" (string), "done_count" (int), "total_count" (int)
//   - "budget_exceeded": map[string]int with keys "used_tokens", "max_tokens"
func PostCommentEvent(ctx context.Context, projectDir string, number int, event string, payload interface{}) error {
	var body string
	switch event {
	case "start":
		pm := payload.(map[string]string)
		body = CommentStarted(pm["planner_model"], pm["executor_model"])
	case "complete":
		state := payload.(TaskState)
		body = CommentDone(state)
	case "fail":
		p := payload.(map[string]interface{})
		failedDesc := p["failed_description"].(string)
		reason := p["reason"].(string)
		doneCount := p["done_count"].(int)
		totalCount := p["total_count"].(int)
		body = CommentFailed(failedDesc, reason, doneCount, totalCount)
	case "budget_exceeded":
		p := payload.(map[string]int)
		body = CommentBudgetExceeded(p["used_tokens"], p["max_tokens"])
	default:
		return fmt.Errorf("unknown event type: %s", event)
	}

	return PostComment(ctx, projectDir, number, body)
}
