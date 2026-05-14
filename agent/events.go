package agent

import (
	"sync"
	"time"
)

// ToolExecution records a single tool invocation.
type ToolExecution struct {
	Timestamp   time.Time              `json:"timestamp"`
	ToolName    string                 `json:"tool_name"`
	Input       map[string]interface{} `json:"input"`
	Status      string                 `json:"status"` // "running", "success", "error"
	Duration    time.Duration          `json:"duration_ms"`
	ChannelID   int64                  `json:"channel_id"`
	TopicID     int                    `json:"topic_id"`
	Error       string                 `json:"error,omitempty"`
	ProjectPath string                 `json:"project_path,omitempty"`
}

// ExecutionOutcome summarises the result of an AI interaction.
type ExecutionOutcome struct {
	Success      bool     `json:"success"`
	ErrorMessage string   `json:"error_message,omitempty"`
	TaskType     string   `json:"task_type"`
	FilesChanged []string `json:"files_changed"`
	Summary      string   `json:"summary"`
}

// DecisionLog captures the full context behind an AI-generated decision.
type DecisionLog struct {
	Timestamp       time.Time              `json:"timestamp"`
	SessionID       string                 `json:"session_id"`
	ProjectPath     string                 `json:"project_path"`
	ChannelID       int64                  `json:"channel_id"`
	TopicID         int                    `json:"topic_id"`
	UserPrompt      string                 `json:"user_prompt"`
	AgentResponse   string                 `json:"agent_response"`
	ThinkingContent string                 `json:"thinking_content"`
	ToolCalls       []ToolExecution        `json:"tool_calls"`
	Context         map[string]interface{} `json:"context"`
	Outcome         ExecutionOutcome       `json:"outcome"`
	DurationMs      int                    `json:"duration_ms"`
	TokensUsed      TokenStats             `json:"tokens_used"`
	GitCommitHash   string                 `json:"git_commit_hash,omitempty"`
	GitBranch       string                 `json:"git_branch,omitempty"`
	Source          string                 `json:"source"`
	Model           string                 `json:"model"`
	RoutingReason   string                 `json:"routing_reason"`
	RoutingLatency  int                    `json:"routing_latency_ms"`
}

// ToolLogger keeps the most recent tool executions in memory.
type ToolLogger struct {
	executions []ToolExecution
	mu         sync.RWMutex
	maxSize    int
}

// NewToolLogger creates a ToolLogger with the given capacity.
func NewToolLogger(maxSize int) *ToolLogger {
	return &ToolLogger{executions: make([]ToolExecution, 0, maxSize), maxSize: maxSize}
}

// LogStart records the beginning of a tool execution and returns an index handle.
func (tl *ToolLogger) LogStart(toolName string, input map[string]interface{}, channelID int64, topicID int, projectPath string) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	exec := ToolExecution{
		Timestamp:   time.Now(),
		ToolName:    toolName,
		Input:       input,
		Status:      "running",
		ChannelID:   channelID,
		TopicID:     topicID,
		ProjectPath: projectPath,
	}
	if len(tl.executions) >= tl.maxSize {
		tl.executions = tl.executions[1:]
	}
	tl.executions = append(tl.executions, exec)
}

// LogEnd updates the most recent matching execution with its result.
func (tl *ToolLogger) LogEnd(toolName string, duration time.Duration, err error) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	for i := len(tl.executions) - 1; i >= 0; i-- {
		if tl.executions[i].ToolName == toolName && tl.executions[i].Status == "running" {
			tl.executions[i].Duration = duration
			if err != nil {
				tl.executions[i].Status = "error"
				tl.executions[i].Error = err.Error()
			} else {
				tl.executions[i].Status = "success"
			}
			return
		}
	}
}

// Recent returns up to n recent executions (newest last).
func (tl *ToolLogger) Recent(n int) []ToolExecution {
	tl.mu.RLock()
	defer tl.mu.RUnlock()
	if n >= len(tl.executions) {
		out := make([]ToolExecution, len(tl.executions))
		copy(out, tl.executions)
		return out
	}
	out := make([]ToolExecution, n)
	copy(out, tl.executions[len(tl.executions)-n:])
	return out
}

// RuntimeEventRecord captures engine lifecycle events for persistence.
type RuntimeEventRecord struct {
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
	ChannelID int64                  `json:"channel_id,omitempty"`
	TopicID   int                    `json:"topic_id,omitempty"`
	TaskID    string                 `json:"task_id,omitempty"`
	Issue     int                    `json:"issue,omitempty"`
	Payload   map[string]interface{} `json:"payload,omitempty"`
}

// DecisionLogger keeps the most recent AI decisions in memory.
type DecisionLogger struct {
	decisions []DecisionLog
	mu        sync.RWMutex
	maxSize   int
	enabled   bool
}

// NewDecisionLogger creates a DecisionLogger with the given capacity.
func NewDecisionLogger(maxSize int) *DecisionLogger {
	return &DecisionLogger{decisions: make([]DecisionLog, 0, maxSize), maxSize: maxSize, enabled: true}
}

// SetEnabled controls whether logging is active.
func (dl *DecisionLogger) SetEnabled(v bool) {
	dl.mu.Lock()
	dl.enabled = v
	dl.mu.Unlock()
}

// Log appends a decision record.
func (dl *DecisionLogger) Log(d DecisionLog) {
	dl.mu.Lock()
	defer dl.mu.Unlock()
	if !dl.enabled {
		return
	}
	if len(dl.decisions) >= dl.maxSize {
		dl.decisions = dl.decisions[1:]
	}
	dl.decisions = append(dl.decisions, d)
}

// Recent returns up to n recent decisions (newest last).
func (dl *DecisionLogger) Recent(n int) []DecisionLog {
	dl.mu.RLock()
	defer dl.mu.RUnlock()
	if n >= len(dl.decisions) {
		out := make([]DecisionLog, len(dl.decisions))
		copy(out, dl.decisions)
		return out
	}
	out := make([]DecisionLog, n)
	copy(out, dl.decisions[len(dl.decisions)-n:])
	return out
}
