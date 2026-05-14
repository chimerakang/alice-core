package agent

import "time"

// StorageBackend lets Agent persist tool executions and decision logs.
// Alice wires in its SQLiteStorage; other consumers can provide their own.
type StorageBackend interface {
	InsertToolExecution(ToolExecution) error
	InsertDecisionLog(DecisionLog) error
	GetToolExecutions(limit, offset int) ([]ToolExecution, error)
	GetDecisionLogs(limit, offset int) ([]DecisionLog, error)
	InsertRuntimeEvent(RuntimeEventRecord) error
	GetRuntimeEvents(limit, offset int) ([]RuntimeEventRecord, error)
}

// EventSink broadcasts tool and decision events (e.g. to a WebSocket hub).
type EventSink interface {
	OnToolEvent(eventType string, exec ToolExecution)
	OnDecisionEvent(decision DecisionLog)
}

// MetricsRecorder records per-call latency and token usage.
type MetricsRecorder interface {
	RecordAPICall(latency time.Duration, success bool, tokensUsed int, cost float64,
		channelID int64, projectPath, errorType, model string,
		inputTokens, cacheReadTokens, cacheWriteTokens, outputTokens int)
}

// PIIFilter removes or masks sensitive data from text.
// Alice injects security.SecurityManager; nil falls back to regex patterns.
type PIIFilter interface {
	DetectAndFilterPII(text string, logEvent bool, ctx interface{}) (filtered string, detected []string)
}

// SkillMatcher finds auto-skills relevant to the current message.
type SkillMatcher interface {
	FindRelevantSkills(message, projectDir string) []AutoSkill
}

// AutoSkill is a minimal description of an auto-injected skill.
type AutoSkill struct {
	Name    string
	Trigger string
	Prompt  string
}
