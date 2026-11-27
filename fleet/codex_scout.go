package fleet

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
)

func (r *Runner) codexScoutArgs(task Task, opts RunOptions) []string {
	args := []string{
		"--ask-for-approval", "never", "exec", "--json", "--sandbox", "read-only",
		"--ephemeral", "--ignore-user-config", "--ignore-rules",
		"--disable", "hooks", "--disable", "plugins",
		"-c", "web_search=\"disabled\"", "-c", "mcp_servers={}",
	}
	if model := firstNonEmpty(strings.TrimSpace(task.Model), strings.TrimSpace(r.CodexScoutModel)); model != "" {
		args = append(args, "--model", model)
	}
	goal := firstNonEmpty(strings.TrimSpace(opts.Instruction), task.Goal)
	return append(args, "--", "Investigate the checked-out repository using read-only shell commands. Do not modify files or contact external services. Write a self-contained final report with file and line references. Fleet saves your final answer as report.md.\n\n"+goal)
}

// A Scout's last agent message is its report, but only a completed turn may
// deliver it. Interrupted or failed turns must never publish partial reports.
func (r *Runner) consumeCodexScout(ctx context.Context, task *Task, stdout io.Reader) streamSummary {
	ctx = context.WithoutCancel(ctx)
	var summary streamSummary
	var final, lastError string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
	for scanner.Scan() {
		var event struct {
			Type    string
			Message string
			Error   struct{ Message string }
			Item    struct{ Type, Text string }
			Usage   struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			}
		}
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		switch event.Type {
		case "thread.started":
			r.record(ctx, task.ID, EventAgentInit, map[string]any{"backend": "codex"})
		case "item.completed":
			if event.Item.Type == "agent_message" && strings.TrimSpace(event.Item.Text) != "" {
				final = event.Item.Text
				r.record(ctx, task.ID, EventAgentText, map[string]any{"text": clip(final, maxEventText)})
			}
		case "error":
			lastError = event.Message
		case "turn.failed":
			summary.result = &agentEvent{IsError: true, Result: firstNonEmpty(event.Error.Message, lastError, "Codex turn failed")}
		case "turn.completed":
			summary.result = &agentEvent{Result: final}
			usage := Usage{InputTokens: event.Usage.Input, OutputTokens: event.Usage.Output}
			summary.usage.InputTokens += usage.InputTokens
			summary.usage.OutputTokens += usage.OutputTokens
			_ = r.Store.AddBackendUsage(ctx, task.ID, "codex", firstNonEmpty(task.Model, r.CodexScoutModel), usage)
			r.record(ctx, task.ID, EventAgentResult, map[string]any{
				"backend": "codex", "input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		summary.result = &agentEvent{IsError: true, Result: err.Error()}
	} else if summary.result == nil && lastError != "" {
		summary.result = &agentEvent{IsError: true, Result: lastError}
	}
	return summary
}
