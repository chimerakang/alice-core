package ghissue

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The lifecycle comments below are the write side of the signals that
// RecentSuccessfulHermesCompletion and BuildIssueStateSnapshot read back, so
// their wording is part of a contract: change both sides together.

// Artifact is a file changed by a run, listed in the completion comment.
type Artifact struct {
	Path string
	Hash string
}

// CompletionSummary is what a finished run reports on its issue.
type CompletionSummary struct {
	// Done and Total count the units of work completed and planned.
	Done  int
	Total int
	// TokensUsed is the run's token usage; MaxTokens is the budget, or 0 for none.
	TokensUsed int
	MaxTokens  int
	// StartedAt, when set, is used to report the elapsed time.
	StartedAt time.Time
	Artifacts []Artifact
}

// PlanEntry is one line of the generated plan section written to an issue.
type PlanEntry struct {
	Description      string
	ChecklistItemIDs []string
}

// CommentStarted builds the "run started" lifecycle comment.
func CommentStarted(plannerModel, executorModel string) string {
	return fmt.Sprintf("🤖 **Hermes 開始執行**\n\n- Planner: `%s`\n- Executor: `%s`\n- 開始時間: %s",
		plannerModel, executorModel,
		time.Now().Format("2006-01-02 15:04:05"))
}

// CommentDone builds the "run finished" lifecycle comment. note, when not
// blank, is appended under a Note heading.
func CommentDone(summary CompletionSummary, note string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("✅ **Hermes 完成** %d/%d SubTasks\n\n", summary.Done, summary.Total))
	sb.WriteString(fmt.Sprintf("- Token 用量: %d", summary.TokensUsed))
	if summary.MaxTokens > 0 {
		sb.WriteString(fmt.Sprintf("/%d", summary.MaxTokens))
	}
	sb.WriteString("\n")
	if !summary.StartedAt.IsZero() {
		sb.WriteString(fmt.Sprintf("- 執行時間: %s\n", time.Since(summary.StartedAt).Round(time.Second)))
	}

	if len(summary.Artifacts) > 0 {
		sb.WriteString("\n**修改的檔案：**\n")
		for _, a := range summary.Artifacts {
			if a.Hash != "" {
				hash := a.Hash
				if len(hash) > 8 {
					hash = hash[:8]
				}
				sb.WriteString(fmt.Sprintf("- `%s` (%s)\n", a.Path, hash))
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

// CommentFailed builds the "run failed" lifecycle comment.
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

// CommentProgress builds a progress comment for one finished unit of work.
// idx is 0-based.
func CommentProgress(idx, totalCount int, description, resultText string, tokens, completedCount int) string {
	return fmt.Sprintf("✅ **子任務進度** (%d/%d)\n\n"+
		"- 子任務: `%s`\n"+
		"- 結果: %s\n"+
		"- Token 用量: %d\n"+
		"- 完成進度: %d/%d\n",
		idx+1, totalCount, description, resultText, tokens, completedCount, totalCount)
}

// WritePlan replaces the generated plan section of the issue body with entries.
//
// The body is re-fetched from GitHub before mutation: any previously written
// plan section is stripped and a fresh one appended, so repeated runs do not
// accumulate duplicate sections.
func WritePlan(ctx context.Context, projectDir string, number int, entries []PlanEntry) error {
	body, err := FetchBody(ctx, projectDir, number)
	if err != nil {
		return fmt.Errorf("fetch body for plan write: %w", err)
	}
	if err := UpdateIssueBody(ctx, projectDir, number, RenderPlan(body, entries)); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

// RenderPlan returns body with its plan section replaced by entries.
func RenderPlan(body string, entries []PlanEntry) string {
	cleanBody := StripPlanSections(body)

	var sb strings.Builder
	sb.WriteString(cleanBody)
	if cleanBody != "" && !strings.HasSuffix(cleanBody, "\n") {
		sb.WriteString("\n")
	}
	sb.WriteString("\n## " + PlanSectionTitle + "\n\n")
	for idx, e := range entries {
		fmt.Fprintf(&sb, "%d. %s", idx+1, e.Description)
		if len(e.ChecklistItemIDs) > 0 {
			sb.WriteString(" _(covers: ")
			sb.WriteString(strings.Join(e.ChecklistItemIDs, ", "))
			sb.WriteString(")_")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}
