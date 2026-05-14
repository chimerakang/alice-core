// Package storage provides a SQLite-backed StorageBackend and unified task store
// for alice-core. It has no Alice-specific dependencies (no Telegram, no security
// events, no scheduled tasks).
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/agent"
	"github.com/chimerakang/alice-core/engine"
	"github.com/chimerakang/alice-core/git"
	_ "modernc.org/sqlite" // Pure Go SQLite driver
)

// ==================== Types ====================

// UnifiedTask is a top-level task record in the unified task schema.
type UnifiedTask struct {
	ID                string     `json:"id"`
	ChannelID         int64      `json:"channel_id"`
	TopicID           int        `json:"topic_id"`
	ProjectDir        string     `json:"project_dir"`
	GithubIssueNumber int        `json:"github_issue_number,omitempty"`
	Goal              string     `json:"goal"`
	Engine            string     `json:"engine"`
	Backend           string     `json:"backend"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	TotalInputTokens  int        `json:"total_input_tokens"`
	TotalOutputTokens int        `json:"total_output_tokens"`
	TotalCostUSD      float64    `json:"total_cost_usd"`
}

// UnifiedSubTask is a single sub-task within a UnifiedTask.
type UnifiedSubTask struct {
	ID               string     `json:"id"`
	TaskID           string     `json:"task_id"`
	Idx              int        `json:"idx"`
	Description      string     `json:"description"`
	Model            string     `json:"model"`
	Status           string     `json:"status"`
	ResultText       string     `json:"result_text"`
	InputTokens      int        `json:"input_tokens"`
	OutputTokens     int        `json:"output_tokens"`
	CostUSD          float64    `json:"cost_usd"`
	StartedAt        time.Time  `json:"started_at"`
	EndedAt          *time.Time `json:"ended_at,omitempty"`
	RoutingReason    string     `json:"routing_reason"`
	RoutingLatencyMS int        `json:"routing_latency_ms"`
}

// UnifiedToolEvent records a single tool invocation linked to a sub-task.
type UnifiedToolEvent struct {
	ID         int64     `json:"id,omitempty"`
	SubTaskID  string    `json:"sub_task_id"`
	ToolName   string    `json:"tool_name"`
	InputJSON  string    `json:"input_json"`
	OutputJSON string    `json:"output_json"`
	Timestamp  time.Time `json:"ts"`
	Status     string    `json:"status"`
}

// UnifiedArtifact records a file artifact produced by a sub-task.
type UnifiedArtifact struct {
	ID        int64  `json:"id,omitempty"`
	SubTaskID string `json:"sub_task_id"`
	Path      string `json:"path"`
	Hash      string `json:"hash"`
}

// UnifiedReviewResult is the stored result of a review phase.
type UnifiedReviewResult struct {
	ID             int64     `json:"id,omitempty"`
	TaskID         string    `json:"task_id"`
	ReviewerModel  string    `json:"reviewer_model"`
	Verdict        string    `json:"verdict"`
	OverallScore   int       `json:"overall_score"`
	FeedbackText   string    `json:"feedback_text"`
	IssueTags      []string  `json:"issue_tags"`
	InputTokens    int       `json:"input_tokens"`
	OutputTokens   int       `json:"output_tokens"`
	CostUSD        float64   `json:"cost_usd"`
	BlockCount     int       `json:"block_count"`
	AutoFixedCount int       `json:"auto_fixed_count"`
	Source         string    `json:"source"`
	CreatedAt      time.Time `json:"created_at"`
}

// UnifiedReviewSubTaskResult is the per-sub-task result within a review.
type UnifiedReviewSubTaskResult struct {
	ID        int64    `json:"id,omitempty"`
	ReviewID  int64    `json:"review_id"`
	SubTaskID string   `json:"sub_task_id"`
	Score     int      `json:"score"`
	Feedback  string   `json:"feedback"`
	IssueTags []string `json:"issue_tags"`
}

// UnifiedReviewGraph is a review with its sub-task results.
type UnifiedReviewGraph struct {
	UnifiedReviewResult
	SubTaskResults []UnifiedReviewSubTaskResult `json:"sub_task_results"`
}

// UnifiedSubTaskGraph is a sub-task with its tool events and artifacts.
type UnifiedSubTaskGraph struct {
	UnifiedSubTask
	ToolEvents []UnifiedToolEvent `json:"tool_events"`
	Artifacts  []UnifiedArtifact  `json:"artifacts"`
}

// UnifiedTaskGraph is a task with all its sub-tasks and reviews.
type UnifiedTaskGraph struct {
	UnifiedTask
	SubTasks []UnifiedSubTaskGraph `json:"sub_tasks"`
	Reviews  []UnifiedReviewGraph  `json:"reviews"`
}

// UnifiedTaskQuery specifies filtering/pagination for ListUnifiedTaskGraphs.
type UnifiedTaskQuery struct {
	Limit      int
	Offset     int
	ID         string
	StartTime  *time.Time
	EndTime    *time.Time
	ProjectDir string
	Status     string
	HasReview  *bool
}

// ==================== SQLiteStorage ====================

// SQLiteStorage implements agent.StorageBackend and the unified task store.
type SQLiteStorage struct {
	db   *sql.DB
	path string
}

// NewSQLiteStorage creates a new SQLiteStorage at dbPath.
// It creates the directory if needed, opens the database, and initializes tables.
func NewSQLiteStorage(dbPath string) (*SQLiteStorage, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Hour)

	s := &SQLiteStorage{db: db, path: dbPath}
	if err := s.initTables(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize tables: %w", err)
	}
	return s, nil
}

// execWithRetry retries an operation on SQLITE_BUSY up to 5 times.
func (s *SQLiteStorage) execWithRetry(operation func() error) error {
	const maxRetries = 5
	const retryDelay = 50 * time.Millisecond

	for i := 0; i < maxRetries; i++ {
		err := operation()
		if err == nil {
			return nil
		}
		if strings.Contains(err.Error(), "database is locked") ||
			strings.Contains(err.Error(), "SQLITE_BUSY") {
			if i < maxRetries-1 {
				time.Sleep(retryDelay * time.Duration(i+1))
				continue
			}
		}
		return err
	}
	return fmt.Errorf("database operation failed after %d retries", maxRetries)
}

// formatTimeForSQLite formats a time.Time for SQLite string comparison.
func formatTimeForSQLite(t time.Time) string {
	return t.Local().Format("2006-01-02 15:04:05 -0700 MST")
}

// initTables creates all tables and runs migrations.
func (s *SQLiteStorage) initTables() error {
	if _, err := s.db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	if _, err := s.db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return err
	}

	toolExecutionsSQL := `
	CREATE TABLE IF NOT EXISTS tool_executions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		tool_name TEXT NOT NULL,
		input_json TEXT,
		status TEXT NOT NULL,
		duration_ms INTEGER,
		channel_id INTEGER,
		topic_id INTEGER,
		error TEXT,
		git_commit_hash TEXT,
		git_branch TEXT,
		project_path TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_tool_executions_timestamp ON tool_executions(timestamp);
	CREATE INDEX IF NOT EXISTS idx_tool_executions_channel_id ON tool_executions(channel_id);
	CREATE INDEX IF NOT EXISTS idx_tool_executions_tool_name ON tool_executions(tool_name);
	CREATE INDEX IF NOT EXISTS idx_tool_executions_project_path ON tool_executions(project_path);
	`

	decisionLogsSQL := `
	CREATE TABLE IF NOT EXISTS decision_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		session_id TEXT,
		project_path TEXT,
		channel_id INTEGER,
		topic_id INTEGER,
		user_prompt TEXT,
		agent_response TEXT,
		tool_calls_json TEXT,
		context_json TEXT,
		outcome_json TEXT,
		duration_ms INTEGER,
		tokens_input INTEGER,
		tokens_output INTEGER,
		tokens_total INTEGER,
		cost_usd REAL,
		git_commit_hash TEXT,
		git_branch TEXT,
		thinking_content TEXT DEFAULT '',
		source TEXT DEFAULT 'agent',
		model TEXT DEFAULT '',
		routing_reason TEXT DEFAULT '',
		routing_latency_ms INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_decision_logs_timestamp ON decision_logs(timestamp);
	CREATE INDEX IF NOT EXISTS idx_decision_logs_session_id ON decision_logs(session_id);
	CREATE INDEX IF NOT EXISTS idx_decision_logs_project_path ON decision_logs(project_path);
	CREATE INDEX IF NOT EXISTS idx_decision_logs_channel_id ON decision_logs(channel_id);
	CREATE INDEX IF NOT EXISTS idx_decision_logs_model ON decision_logs(model);
	`

	runtimeEventsSQL := `
	CREATE TABLE IF NOT EXISTS runtime_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME NOT NULL,
		type TEXT NOT NULL,
		channel_id INTEGER,
		topic_id INTEGER,
		task_id TEXT DEFAULT '',
		issue_number INTEGER DEFAULT 0,
		payload_json TEXT NOT NULL DEFAULT '{}',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);
	CREATE INDEX IF NOT EXISTS idx_runtime_events_timestamp ON runtime_events(timestamp);
	CREATE INDEX IF NOT EXISTS idx_runtime_events_type ON runtime_events(type);
	CREATE INDEX IF NOT EXISTS idx_runtime_events_task_id ON runtime_events(task_id);
	CREATE INDEX IF NOT EXISTS idx_runtime_events_channel_id ON runtime_events(channel_id);
	`

	tables := []string{toolExecutionsSQL, decisionLogsSQL, runtimeEventsSQL}
	for _, tableSQL := range tables {
		if _, err := s.db.Exec(tableSQL); err != nil {
			return fmt.Errorf("failed to create tables: %w", err)
		}
	}

	// Unified task tables
	if err := s.migrateUnifiedTaskTables(); err != nil {
		return err
	}

	// Migrations with duplicate column guard
	migrations := []string{
		`ALTER TABLE decision_logs ADD COLUMN thinking_content TEXT DEFAULT ''`,
		`ALTER TABLE decision_logs ADD COLUMN source TEXT DEFAULT 'agent'`,
	}
	for _, stmt := range migrations {
		if _, err := s.db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			log.Printf("[storage] migration warning: %v", err)
		}
	}

	log.Printf("[storage] database initialized at: %s", s.path)
	return nil
}

func (s *SQLiteStorage) migrateUnifiedTaskTables() error {
	for _, stmt := range unifiedTaskTablesSQL() {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("unified task migration: %w", err)
		}
	}
	migrations := []struct{ stmt, desc string }{
		{`ALTER TABLE review_results ADD COLUMN block_count INTEGER NOT NULL DEFAULT 0`, "review_results.block_count"},
		{`ALTER TABLE review_results ADD COLUMN auto_fixed_count INTEGER NOT NULL DEFAULT 0`, "review_results.auto_fixed_count"},
		{`ALTER TABLE tasks ADD COLUMN github_issue_number INTEGER NOT NULL DEFAULT 0`, "tasks.github_issue_number"},
	}
	for _, m := range migrations {
		if _, err := s.db.Exec(m.stmt); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("unified task %s migration: %w", m.desc, err)
		}
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_tasks_github_issue_number ON tasks(github_issue_number)`); err != nil {
		return fmt.Errorf("unified task github_issue_number index: %w", err)
	}
	return nil
}

func unifiedTaskTablesSQL() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS tasks (
			id                  TEXT PRIMARY KEY,
			channel_id          INTEGER NOT NULL DEFAULT 0,
			topic_id            INTEGER NOT NULL DEFAULT 0,
			project_dir         TEXT NOT NULL DEFAULT '',
			github_issue_number INTEGER NOT NULL DEFAULT 0,
			goal                TEXT NOT NULL DEFAULT '',
			engine              TEXT NOT NULL DEFAULT '',
			backend             TEXT NOT NULL DEFAULT '',
			status              TEXT NOT NULL DEFAULT '',
			started_at          TEXT NOT NULL,
			ended_at            TEXT,
			total_input_tokens  INTEGER NOT NULL DEFAULT 0,
			total_output_tokens INTEGER NOT NULL DEFAULT 0,
			total_cost_usd      REAL NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_started_at ON tasks(started_at)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_channel_topic ON tasks(channel_id, topic_id)`,
		`CREATE INDEX IF NOT EXISTS idx_tasks_project_dir ON tasks(project_dir)`,
		`CREATE TABLE IF NOT EXISTS sub_tasks (
			id                 TEXT PRIMARY KEY,
			task_id            TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			idx                INTEGER NOT NULL DEFAULT 0,
			description        TEXT NOT NULL DEFAULT '',
			model              TEXT NOT NULL DEFAULT '',
			status             TEXT NOT NULL DEFAULT '',
			result_text        TEXT NOT NULL DEFAULT '',
			input_tokens       INTEGER NOT NULL DEFAULT 0,
			output_tokens      INTEGER NOT NULL DEFAULT 0,
			cost_usd           REAL NOT NULL DEFAULT 0,
			started_at         TEXT NOT NULL,
			ended_at           TEXT,
			routing_reason     TEXT NOT NULL DEFAULT '',
			routing_latency_ms INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sub_tasks_task_idx ON sub_tasks(task_id, idx)`,
		`CREATE TABLE IF NOT EXISTS tool_events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			sub_task_id TEXT NOT NULL REFERENCES sub_tasks(id) ON DELETE CASCADE,
			tool_name   TEXT NOT NULL DEFAULT '',
			input_json  TEXT NOT NULL DEFAULT '{}',
			output_json TEXT NOT NULL DEFAULT '{}',
			ts          TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_tool_events_sub_task ON tool_events(sub_task_id)`,
		`CREATE TABLE IF NOT EXISTS artifacts (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			sub_task_id TEXT NOT NULL REFERENCES sub_tasks(id) ON DELETE CASCADE,
			path        TEXT NOT NULL,
			hash        TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_artifacts_sub_task ON artifacts(sub_task_id)`,
		`CREATE TABLE IF NOT EXISTS review_results (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			task_id         TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
			reviewer_model  TEXT NOT NULL DEFAULT '',
			verdict         TEXT NOT NULL DEFAULT '',
			overall_score   INTEGER NOT NULL DEFAULT 0,
			feedback_text   TEXT NOT NULL DEFAULT '',
			issue_tags      TEXT NOT NULL DEFAULT '[]',
			input_tokens    INTEGER NOT NULL DEFAULT 0,
			output_tokens   INTEGER NOT NULL DEFAULT 0,
			cost_usd        REAL NOT NULL DEFAULT 0,
			source          TEXT NOT NULL DEFAULT 'initial',
			created_at      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_results_task ON review_results(task_id)`,
		`CREATE TABLE IF NOT EXISTS review_subtask_results (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			review_id   INTEGER NOT NULL REFERENCES review_results(id) ON DELETE CASCADE,
			sub_task_id TEXT NOT NULL REFERENCES sub_tasks(id) ON DELETE CASCADE,
			score       INTEGER NOT NULL DEFAULT 0,
			feedback    TEXT NOT NULL DEFAULT '',
			issue_tags  TEXT NOT NULL DEFAULT '[]'
		)`,
		`CREATE INDEX IF NOT EXISTS idx_review_subtask_results_review ON review_subtask_results(review_id)`,
	}
}

// ==================== agent.StorageBackend ====================

// InsertToolExecution persists a tool execution record, enriching with git info.
func (s *SQLiteStorage) InsertToolExecution(exec agent.ToolExecution) error {
	inputJSON, _ := json.Marshal(exec.Input)

	var gitCommitHash, gitBranch sql.NullString
	if exec.ProjectPath != "" {
		if info, err := git.Get(exec.ProjectPath); err == nil && info != nil {
			gitCommitHash = sql.NullString{String: info.CommitHash, Valid: true}
			gitBranch = sql.NullString{String: info.Branch, Valid: true}
		}
	}

	_, err := s.db.Exec(`
		INSERT INTO tool_executions
		(timestamp, tool_name, input_json, status, duration_ms, channel_id, topic_id, error, git_commit_hash, git_branch, project_path)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exec.Timestamp, exec.ToolName, string(inputJSON), exec.Status,
		exec.Duration.Milliseconds(), exec.ChannelID, exec.TopicID, exec.Error,
		gitCommitHash, gitBranch, exec.ProjectPath)
	return err
}

// GetToolExecutions returns paginated tool execution records.
func (s *SQLiteStorage) GetToolExecutions(limit, offset int) ([]agent.ToolExecution, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, tool_name, input_json, status, duration_ms, channel_id, topic_id, error, COALESCE(project_path, '') as project_path
		FROM tool_executions
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToolExecutions(rows)
}

// GetToolExecutionsByTimeRange returns tool executions within a time range.
func (s *SQLiteStorage) GetToolExecutionsByTimeRange(start, end time.Time, limit int) ([]agent.ToolExecution, error) {
	startStr := formatTimeForSQLite(start)
	endStr := formatTimeForSQLite(end)
	rows, err := s.db.Query(`
		SELECT timestamp, tool_name, input_json, status, duration_ms, channel_id, topic_id, error, COALESCE(project_path, '') as project_path
		FROM tool_executions
		WHERE timestamp BETWEEN ? AND ?
		ORDER BY timestamp DESC
		LIMIT ?`, startStr, endStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanToolExecutions(rows)
}

func scanToolExecutions(rows *sql.Rows) ([]agent.ToolExecution, error) {
	var executions []agent.ToolExecution
	for rows.Next() {
		var exec agent.ToolExecution
		var inputJSON string
		var durationMs int64
		err := rows.Scan(&exec.Timestamp, &exec.ToolName, &inputJSON, &exec.Status,
			&durationMs, &exec.ChannelID, &exec.TopicID, &exec.Error, &exec.ProjectPath)
		if err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(inputJSON), &exec.Input)
		exec.Duration = time.Duration(durationMs) * time.Millisecond
		executions = append(executions, exec)
	}
	return executions, rows.Err()
}

// InsertDecisionLog persists a decision log record, enriching with git info.
func (s *SQLiteStorage) InsertDecisionLog(l agent.DecisionLog) error {
	toolCallsJSON, _ := json.Marshal(l.ToolCalls)
	contextJSON, _ := json.Marshal(l.Context)
	outcomeJSON, _ := json.Marshal(l.Outcome)

	var gitCommitHash, gitBranch sql.NullString
	if l.ProjectPath != "" {
		if info, err := git.Get(l.ProjectPath); err == nil && info != nil {
			gitCommitHash = sql.NullString{String: info.CommitHash, Valid: true}
			gitBranch = sql.NullString{String: info.Branch, Valid: true}
		}
	}

	source := l.Source
	if source == "" {
		source = "agent"
	}

	_, err := s.db.Exec(`
		INSERT INTO decision_logs
		(timestamp, session_id, project_path, channel_id, topic_id, user_prompt, agent_response,
		 tool_calls_json, context_json, outcome_json, duration_ms, tokens_input, tokens_output,
		 tokens_total, cost_usd, git_commit_hash, git_branch, thinking_content, source,
		 model, routing_reason, routing_latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		l.Timestamp, l.SessionID, l.ProjectPath, l.ChannelID, l.TopicID,
		l.UserPrompt, l.AgentResponse, string(toolCallsJSON), string(contextJSON),
		string(outcomeJSON), l.DurationMs, l.TokensUsed.TotalInputTokens, l.TokensUsed.TotalOutputTokens,
		l.TokensUsed.TotalInputTokens+l.TokensUsed.TotalOutputTokens, l.TokensUsed.TotalCostUSD,
		gitCommitHash, gitBranch, l.ThinkingContent, source,
		l.Model, l.RoutingReason, l.RoutingLatency)

	if err != nil {
		return err
	}
	if err := s.insertDecisionLogUnified(l); err != nil {
		return fmt.Errorf("insert unified task: %w", err)
	}
	return nil
}

// GetDecisionLogs returns paginated decision log records.
func (s *SQLiteStorage) GetDecisionLogs(limit, offset int) ([]agent.DecisionLog, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, session_id, project_path, channel_id, topic_id, user_prompt,
			   agent_response, tool_calls_json, context_json, outcome_json, duration_ms,
			   tokens_input, tokens_output, COALESCE(cost_usd, 0) as cost_usd,
			   COALESCE(thinking_content, '') as thinking_content,
			   COALESCE(git_commit_hash, '') as git_commit_hash, COALESCE(git_branch, '') as git_branch,
			   COALESCE(source, 'agent') as source,
			   COALESCE(model, '') as model, COALESCE(routing_reason, '') as routing_reason,
			   COALESCE(routing_latency_ms, 0) as routing_latency_ms
		FROM decision_logs
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecisionLogs(rows)
}

// GetDecisionLogsByTimeRange returns decision logs within a time range.
func (s *SQLiteStorage) GetDecisionLogsByTimeRange(start, end time.Time, limit int) ([]agent.DecisionLog, error) {
	startStr := formatTimeForSQLite(start)
	endStr := formatTimeForSQLite(end)
	rows, err := s.db.Query(`
		SELECT timestamp, session_id, project_path, channel_id, topic_id, user_prompt,
			   agent_response, tool_calls_json, context_json, outcome_json, duration_ms,
			   tokens_input, tokens_output, COALESCE(cost_usd, 0) as cost_usd,
			   COALESCE(thinking_content, '') as thinking_content,
			   COALESCE(git_commit_hash, '') as git_commit_hash, COALESCE(git_branch, '') as git_branch,
			   COALESCE(source, 'agent') as source,
			   COALESCE(model, '') as model, COALESCE(routing_reason, '') as routing_reason,
			   COALESCE(routing_latency_ms, 0) as routing_latency_ms
		FROM decision_logs
		WHERE timestamp BETWEEN ? AND ?
		ORDER BY timestamp DESC
		LIMIT ?`, startStr, endStr, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDecisionLogs(rows)
}

func scanDecisionLogs(rows *sql.Rows) ([]agent.DecisionLog, error) {
	var logs []agent.DecisionLog
	for rows.Next() {
		var l agent.DecisionLog
		var toolCallsJSON, contextJSON, outcomeJSON string
		var gitCommitHash, gitBranch string

		err := rows.Scan(&l.Timestamp, &l.SessionID, &l.ProjectPath,
			&l.ChannelID, &l.TopicID, &l.UserPrompt, &l.AgentResponse,
			&toolCallsJSON, &contextJSON, &outcomeJSON, &l.DurationMs,
			&l.TokensUsed.TotalInputTokens, &l.TokensUsed.TotalOutputTokens,
			&l.TokensUsed.TotalCostUSD,
			&l.ThinkingContent, &gitCommitHash, &gitBranch, &l.Source,
			&l.Model, &l.RoutingReason, &l.RoutingLatency)
		if err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(toolCallsJSON), &l.ToolCalls)
		json.Unmarshal([]byte(contextJSON), &l.Context)
		json.Unmarshal([]byte(outcomeJSON), &l.Outcome)
		l.GitCommitHash = gitCommitHash
		l.GitBranch = gitBranch
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// ==================== Runtime Events ====================

// InsertRuntimeEvent persists a runtime event record.
func (s *SQLiteStorage) InsertRuntimeEvent(event agent.RuntimeEventRecord) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	payloadJSON, err := json.Marshal(event.Payload)
	if err != nil {
		return fmt.Errorf("marshal runtime event payload: %w", err)
	}
	_, err = s.db.Exec(`
		INSERT INTO runtime_events
		(timestamp, type, channel_id, topic_id, task_id, issue_number, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		event.Timestamp, event.Type, event.ChannelID, event.TopicID, event.TaskID, event.Issue, string(payloadJSON))
	return err
}

// GetRuntimeEvents returns paginated runtime event records.
func (s *SQLiteStorage) GetRuntimeEvents(limit, offset int) ([]agent.RuntimeEventRecord, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, type, channel_id, topic_id, task_id, issue_number, payload_json
		FROM runtime_events
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuntimeEvents(rows)
}

// GetRuntimeEventsByType returns runtime events of a specific type.
func (s *SQLiteStorage) GetRuntimeEventsByType(eventType string, limit int) ([]agent.RuntimeEventRecord, error) {
	rows, err := s.db.Query(`
		SELECT timestamp, type, channel_id, topic_id, task_id, issue_number, payload_json
		FROM runtime_events
		WHERE type = ?
		ORDER BY timestamp DESC
		LIMIT ?`, eventType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuntimeEvents(rows)
}

// GetRuntimeEventsByTask returns runtime events for a specific task.
func (s *SQLiteStorage) GetRuntimeEventsByTask(taskID string, eventType string, limit int, offset int) ([]agent.RuntimeEventRecord, error) {
	taskID = strings.TrimSpace(taskID)
	eventType = strings.TrimSpace(eventType)
	if eventType != "" {
		rows, err := s.db.Query(`
			SELECT timestamp, type, channel_id, topic_id, task_id, issue_number, payload_json
			FROM runtime_events
			WHERE task_id = ? AND type = ?
			ORDER BY timestamp DESC
			LIMIT ? OFFSET ?`, taskID, eventType, limit, offset)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		return scanRuntimeEvents(rows)
	}
	rows, err := s.db.Query(`
		SELECT timestamp, type, channel_id, topic_id, task_id, issue_number, payload_json
		FROM runtime_events
		WHERE task_id = ?
		ORDER BY timestamp DESC
		LIMIT ? OFFSET ?`, taskID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuntimeEvents(rows)
}

func scanRuntimeEvents(rows *sql.Rows) ([]agent.RuntimeEventRecord, error) {
	var events []agent.RuntimeEventRecord
	for rows.Next() {
		var event agent.RuntimeEventRecord
		var payloadJSON string
		if err := rows.Scan(&event.Timestamp, &event.Type, &event.ChannelID, &event.TopicID, &event.TaskID, &event.Issue, &payloadJSON); err != nil {
			return nil, err
		}
		if strings.TrimSpace(payloadJSON) != "" {
			_ = json.Unmarshal([]byte(payloadJSON), &event.Payload)
		}
		if event.Payload == nil {
			event.Payload = map[string]interface{}{}
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// ==================== Unified Task Store ====================

// UpsertUnifiedTask inserts or updates a task record.
func (s *SQLiteStorage) UpsertUnifiedTask(task UnifiedTask) error {
	if task.StartedAt.IsZero() {
		task.StartedAt = time.Now()
	}
	_, err := s.db.Exec(`
		INSERT INTO tasks
			(id, channel_id, topic_id, project_dir, github_issue_number, goal, engine, backend, status,
			 started_at, ended_at, total_input_tokens, total_output_tokens, total_cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			channel_id = excluded.channel_id,
			topic_id = excluded.topic_id,
			project_dir = excluded.project_dir,
			github_issue_number = CASE
				WHEN excluded.github_issue_number > 0 THEN excluded.github_issue_number
				ELSE tasks.github_issue_number
			END,
			goal = excluded.goal,
			engine = excluded.engine,
			backend = excluded.backend,
			status = excluded.status,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			total_input_tokens = excluded.total_input_tokens,
			total_output_tokens = excluded.total_output_tokens,
			total_cost_usd = excluded.total_cost_usd`,
		task.ID, task.ChannelID, task.TopicID, task.ProjectDir, task.GithubIssueNumber, task.Goal,
		task.Engine, task.Backend, task.Status, task.StartedAt.Format(time.RFC3339Nano),
		formatNullableTime(task.EndedAt), task.TotalInputTokens, task.TotalOutputTokens,
		task.TotalCostUSD,
	)
	return err
}

// UpsertUnifiedSubTask inserts or updates a sub-task record.
func (s *SQLiteStorage) UpsertUnifiedSubTask(subTask UnifiedSubTask) error {
	if subTask.StartedAt.IsZero() {
		subTask.StartedAt = time.Now()
	}
	_, err := s.db.Exec(`
		INSERT INTO sub_tasks
			(id, task_id, idx, description, model, status, result_text, input_tokens,
			 output_tokens, cost_usd, started_at, ended_at, routing_reason, routing_latency_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			task_id = excluded.task_id,
			idx = excluded.idx,
			description = excluded.description,
			model = excluded.model,
			status = excluded.status,
			result_text = excluded.result_text,
			input_tokens = excluded.input_tokens,
			output_tokens = excluded.output_tokens,
			cost_usd = excluded.cost_usd,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			routing_reason = excluded.routing_reason,
			routing_latency_ms = excluded.routing_latency_ms`,
		subTask.ID, subTask.TaskID, subTask.Idx, subTask.Description, subTask.Model,
		subTask.Status, subTask.ResultText, subTask.InputTokens, subTask.OutputTokens,
		subTask.CostUSD, subTask.StartedAt.Format(time.RFC3339Nano),
		formatNullableTime(subTask.EndedAt), subTask.RoutingReason, subTask.RoutingLatencyMS,
	)
	return err
}

// InsertUnifiedToolEvent inserts a tool event record.
func (s *SQLiteStorage) InsertUnifiedToolEvent(event UnifiedToolEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now()
	}
	if event.InputJSON == "" {
		event.InputJSON = "{}"
	}
	if event.OutputJSON == "" {
		event.OutputJSON = "{}"
	}
	_, err := s.db.Exec(`
		INSERT INTO tool_events (sub_task_id, tool_name, input_json, output_json, ts, status)
		VALUES (?, ?, ?, ?, ?, ?)`,
		event.SubTaskID, event.ToolName, event.InputJSON, event.OutputJSON,
		event.Timestamp.Format(time.RFC3339Nano), event.Status,
	)
	return err
}

// InsertUnifiedArtifact inserts an artifact record.
func (s *SQLiteStorage) InsertUnifiedArtifact(artifact UnifiedArtifact) error {
	_, err := s.db.Exec(`
		INSERT INTO artifacts (sub_task_id, path, hash)
		VALUES (?, ?, ?)`,
		artifact.SubTaskID, artifact.Path, artifact.Hash,
	)
	return err
}

// InsertUnifiedReviewResult inserts a review result and returns its ID.
func (s *SQLiteStorage) InsertUnifiedReviewResult(review UnifiedReviewResult) (int64, error) {
	if review.CreatedAt.IsZero() {
		review.CreatedAt = time.Now()
	}
	if strings.TrimSpace(review.Source) == "" {
		review.Source = "initial"
	}
	tagsJSON, err := json.Marshal(review.IssueTags)
	if err != nil {
		return 0, err
	}
	res, err := s.db.Exec(`
		INSERT INTO review_results
			(task_id, reviewer_model, verdict, overall_score, feedback_text, issue_tags,
			 input_tokens, output_tokens, cost_usd, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		review.TaskID, review.ReviewerModel, review.Verdict, review.OverallScore,
		review.FeedbackText, string(tagsJSON), review.InputTokens, review.OutputTokens,
		review.CostUSD, review.Source, review.CreatedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertUnifiedReviewSubTaskResult inserts a per-sub-task review result.
func (s *SQLiteStorage) InsertUnifiedReviewSubTaskResult(result UnifiedReviewSubTaskResult) error {
	tagsJSON, err := json.Marshal(result.IssueTags)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`
		INSERT INTO review_subtask_results
			(review_id, sub_task_id, score, feedback, issue_tags)
		VALUES (?, ?, ?, ?, ?)`,
		result.ReviewID, result.SubTaskID, result.Score, result.Feedback, string(tagsJSON),
	)
	return err
}

// StoreReview persists a completed engine.ReviewResult into the unified task schema.
func (s *SQLiteStorage) StoreReview(ctx context.Context, taskID string, review engine.ReviewResult) error {
	if err := review.Validate(); err != nil {
		return fmt.Errorf("validate review: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin review transaction: %w", err)
	}
	defer tx.Rollback()

	createdAt := time.Now()
	tagsJSON, _ := json.Marshal(reviewTagsToStrings(review.IssueTags))
	res, err := tx.Exec(`
		INSERT INTO review_results
			(task_id, reviewer_model, verdict, overall_score, feedback_text, issue_tags,
			 input_tokens, output_tokens, cost_usd, block_count, auto_fixed_count, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, strings.TrimSpace(review.ReviewerModel), string(review.Verdict),
		review.OverallScore, strings.TrimSpace(review.Feedback), string(tagsJSON),
		review.InputTokens, review.OutputTokens, review.CostUSD,
		review.BlockCount, review.AutoFixedCount, "initial", createdAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return err
	}
	reviewID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	for idx, subTask := range review.SubTaskResults {
		subTaskTags, _ := json.Marshal(reviewTagsToStrings(subTask.IssueTags))
		subTaskID := reviewSubTaskStorageID(taskID, idx, subTask.SubTaskID)
		if _, err := tx.Exec(`
			INSERT INTO review_subtask_results
				(review_id, sub_task_id, score, feedback, issue_tags)
			VALUES (?, ?, ?, ?, ?)`,
			reviewID, subTaskID, subTask.Score, strings.TrimSpace(subTask.Feedback), string(subTaskTags),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func reviewSubTaskStorageID(taskID string, idx int, subTaskID string) string {
	subTaskID = strings.TrimSpace(subTaskID)
	if strings.HasPrefix(subTaskID, taskID+":") {
		return subTaskID
	}
	// Fallback: taskID:idx+1
	return fmt.Sprintf("%s:%d", taskID, idx+1)
}

func reviewTagsToStrings(tags []engine.ReviewIssueTag) []string {
	if len(tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, string(tag))
	}
	return out
}

// ListUnifiedTaskGraphs returns a list of task graphs matching the query.
func (s *SQLiteStorage) ListUnifiedTaskGraphs(query UnifiedTaskQuery) ([]UnifiedTaskGraph, error) {
	where, args := unifiedTaskWhere(query)
	limit := query.Limit
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.Query(`
		SELECT id, channel_id, topic_id, project_dir, goal, engine, backend, status,
		       github_issue_number, started_at, ended_at,
		       total_input_tokens, total_output_tokens, total_cost_usd
		FROM tasks`+where+`
		ORDER BY started_at DESC
		LIMIT ? OFFSET ?`, append(args, limit, maxInt(query.Offset, 0))...)
	if err != nil {
		return nil, err
	}

	tasks := make([]UnifiedTaskGraph, 0)
	for rows.Next() {
		task, err := scanUnifiedTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, UnifiedTaskGraph{UnifiedTask: task})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range tasks {
		subTasks, err := s.listUnifiedSubTaskGraphs(tasks[i].ID)
		if err != nil {
			return nil, err
		}
		reviews, err := s.listUnifiedReviewGraphs(tasks[i].ID)
		if err != nil {
			return nil, err
		}
		tasks[i].SubTasks = subTasks
		tasks[i].Reviews = reviews
	}
	return tasks, nil
}

// CountUnifiedTasks returns the count of tasks matching the query.
func (s *SQLiteStorage) CountUnifiedTasks(query UnifiedTaskQuery) (int64, error) {
	where, args := unifiedTaskWhere(query)
	var count int64
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tasks`+where, args...).Scan(&count)
	return count, err
}

func unifiedTaskWhere(query UnifiedTaskQuery) (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 6)
	if query.ID != "" {
		clauses = append(clauses, "id = ?")
		args = append(args, query.ID)
	}
	if query.StartTime != nil {
		clauses = append(clauses, "started_at >= ?")
		args = append(args, query.StartTime.Format(time.RFC3339Nano))
	}
	if query.EndTime != nil {
		clauses = append(clauses, "started_at <= ?")
		args = append(args, query.EndTime.Format(time.RFC3339Nano))
	}
	if query.ProjectDir != "" {
		clauses = append(clauses, "project_dir = ?")
		args = append(args, query.ProjectDir)
	}
	if query.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, query.Status)
	}
	if query.HasReview != nil {
		if *query.HasReview {
			clauses = append(clauses, "EXISTS (SELECT 1 FROM review_results rr WHERE rr.task_id = tasks.id)")
		} else {
			clauses = append(clauses, "NOT EXISTS (SELECT 1 FROM review_results rr WHERE rr.task_id = tasks.id)")
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

type sqlScanner interface {
	Scan(dest ...any) error
}

func scanUnifiedTask(scanner sqlScanner) (UnifiedTask, error) {
	var task UnifiedTask
	var startedAt, endedAt sql.NullString
	err := scanner.Scan(
		&task.ID, &task.ChannelID, &task.TopicID, &task.ProjectDir, &task.Goal,
		&task.Engine, &task.Backend, &task.Status, &task.GithubIssueNumber, &startedAt, &endedAt,
		&task.TotalInputTokens, &task.TotalOutputTokens, &task.TotalCostUSD,
	)
	if err != nil {
		return task, err
	}
	task.StartedAt = parseDBTime(startedAt.String)
	task.EndedAt = parseNullableDBTime(endedAt)
	return task, nil
}

func scanUnifiedSubTask(scanner sqlScanner) (UnifiedSubTask, error) {
	var subTask UnifiedSubTask
	var startedAt, endedAt sql.NullString
	err := scanner.Scan(
		&subTask.ID, &subTask.TaskID, &subTask.Idx, &subTask.Description,
		&subTask.Model, &subTask.Status, &subTask.ResultText,
		&subTask.InputTokens, &subTask.OutputTokens, &subTask.CostUSD,
		&startedAt, &endedAt, &subTask.RoutingReason, &subTask.RoutingLatencyMS,
	)
	if err != nil {
		return subTask, err
	}
	subTask.StartedAt = parseDBTime(startedAt.String)
	subTask.EndedAt = parseNullableDBTime(endedAt)
	return subTask, nil
}

func (s *SQLiteStorage) listUnifiedSubTaskGraphs(taskID string) ([]UnifiedSubTaskGraph, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, idx, description, model, status, result_text,
		       input_tokens, output_tokens, cost_usd, started_at, ended_at,
		       routing_reason, routing_latency_ms
		FROM sub_tasks
		WHERE task_id = ?
		ORDER BY idx ASC`, taskID)
	if err != nil {
		return nil, err
	}

	subTasks := make([]UnifiedSubTaskGraph, 0)
	for rows.Next() {
		subTask, err := scanUnifiedSubTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		subTasks = append(subTasks, UnifiedSubTaskGraph{UnifiedSubTask: subTask})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range subTasks {
		events, err := s.listUnifiedToolEvents(subTasks[i].ID)
		if err != nil {
			return nil, err
		}
		artifacts, err := s.listUnifiedArtifacts(subTasks[i].ID)
		if err != nil {
			return nil, err
		}
		subTasks[i].ToolEvents = events
		subTasks[i].Artifacts = artifacts
	}
	return subTasks, nil
}

func (s *SQLiteStorage) listUnifiedToolEvents(subTaskID string) ([]UnifiedToolEvent, error) {
	rows, err := s.db.Query(`
		SELECT id, sub_task_id, tool_name, input_json, output_json, ts, status
		FROM tool_events
		WHERE sub_task_id = ?
		ORDER BY ts ASC, id ASC`, subTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	events := make([]UnifiedToolEvent, 0)
	for rows.Next() {
		var event UnifiedToolEvent
		var ts string
		if err := rows.Scan(&event.ID, &event.SubTaskID, &event.ToolName, &event.InputJSON, &event.OutputJSON, &ts, &event.Status); err != nil {
			return nil, err
		}
		event.Timestamp = parseDBTime(ts)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *SQLiteStorage) listUnifiedArtifacts(subTaskID string) ([]UnifiedArtifact, error) {
	rows, err := s.db.Query(`
		SELECT id, sub_task_id, path, hash
		FROM artifacts
		WHERE sub_task_id = ?
		ORDER BY id ASC`, subTaskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	artifacts := make([]UnifiedArtifact, 0)
	for rows.Next() {
		var artifact UnifiedArtifact
		if err := rows.Scan(&artifact.ID, &artifact.SubTaskID, &artifact.Path, &artifact.Hash); err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

func (s *SQLiteStorage) listUnifiedReviewGraphs(taskID string) ([]UnifiedReviewGraph, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, reviewer_model, verdict, overall_score, feedback_text,
		       issue_tags, input_tokens, output_tokens, cost_usd, block_count, auto_fixed_count, source, created_at
		FROM review_results
		WHERE task_id = ?
		ORDER BY created_at DESC, id DESC`, taskID)
	if err != nil {
		return nil, err
	}

	reviews := make([]UnifiedReviewGraph, 0)
	for rows.Next() {
		var review UnifiedReviewGraph
		var tagsJSON, createdAt string
		if err := rows.Scan(
			&review.ID, &review.TaskID, &review.ReviewerModel, &review.Verdict,
			&review.OverallScore, &review.FeedbackText, &tagsJSON,
			&review.InputTokens, &review.OutputTokens, &review.CostUSD,
			&review.BlockCount, &review.AutoFixedCount, &review.Source, &createdAt,
		); err != nil {
			rows.Close()
			return nil, err
		}
		review.IssueTags = parseStringListJSON(tagsJSON)
		review.CreatedAt = parseDBTime(createdAt)
		reviews = append(reviews, review)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range reviews {
		subTaskResults, err := s.listUnifiedReviewSubTaskResults(reviews[i].ID)
		if err != nil {
			return nil, err
		}
		reviews[i].SubTaskResults = subTaskResults
	}
	return reviews, nil
}

func (s *SQLiteStorage) listUnifiedReviewSubTaskResults(reviewID int64) ([]UnifiedReviewSubTaskResult, error) {
	rows, err := s.db.Query(`
		SELECT id, review_id, sub_task_id, score, feedback, issue_tags
		FROM review_subtask_results
		WHERE review_id = ?
		ORDER BY id ASC`, reviewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]UnifiedReviewSubTaskResult, 0)
	for rows.Next() {
		var result UnifiedReviewSubTaskResult
		var tagsJSON string
		if err := rows.Scan(&result.ID, &result.ReviewID, &result.SubTaskID, &result.Score, &result.Feedback, &tagsJSON); err != nil {
			return nil, err
		}
		result.IssueTags = parseStringListJSON(tagsJSON)
		results = append(results, result)
	}
	return results, rows.Err()
}

// ==================== Connection Management ====================

// Close closes the underlying database connection.
func (s *SQLiteStorage) Close() error {
	return s.db.Close()
}

// Health checks whether the database is reachable.
func (s *SQLiteStorage) Health() error {
	return s.db.Ping()
}

// GetDB returns the underlying *sql.DB for advanced operations.
func (s *SQLiteStorage) GetDB() *sql.DB {
	return s.db
}

// ==================== Private helpers ====================

func (s *SQLiteStorage) insertDecisionLogUnified(decision agent.DecisionLog) error {
	taskID := unifiedDecisionTaskID(decision)
	subTaskID := taskID + ":1"
	startedAt := decision.Timestamp
	if startedAt.IsZero() {
		startedAt = time.Now()
	}
	endedAt := startedAt.Add(time.Duration(decision.DurationMs) * time.Millisecond)
	status := "done"
	if !decision.Outcome.Success {
		status = "failed"
	}
	if err := s.UpsertUnifiedTask(UnifiedTask{
		ID:                taskID,
		ChannelID:         decision.ChannelID,
		TopicID:           decision.TopicID,
		ProjectDir:        decision.ProjectPath,
		Goal:              decision.UserPrompt,
		Engine:            "direct",
		Backend:           backendForDecisionModel(decision.Model),
		Status:            status,
		StartedAt:         startedAt,
		EndedAt:           &endedAt,
		TotalInputTokens:  int(decision.TokensUsed.TotalInputTokens),
		TotalOutputTokens: int(decision.TokensUsed.TotalOutputTokens),
		TotalCostUSD:      decision.TokensUsed.TotalCostUSD,
	}); err != nil {
		return err
	}
	if err := s.UpsertUnifiedSubTask(UnifiedSubTask{
		ID:               subTaskID,
		TaskID:           taskID,
		Idx:              0,
		Description:      decision.UserPrompt,
		Model:            decision.Model,
		Status:           status,
		ResultText:       decision.AgentResponse,
		InputTokens:      int(decision.TokensUsed.TotalInputTokens),
		OutputTokens:     int(decision.TokensUsed.TotalOutputTokens),
		CostUSD:          decision.TokensUsed.TotalCostUSD,
		StartedAt:        startedAt,
		EndedAt:          &endedAt,
		RoutingReason:    decision.RoutingReason,
		RoutingLatencyMS: decision.RoutingLatency,
	}); err != nil {
		return err
	}
	for _, call := range decision.ToolCalls {
		inputJSON, _ := json.Marshal(call.Input)
		if err := s.InsertUnifiedToolEvent(UnifiedToolEvent{
			SubTaskID:  subTaskID,
			ToolName:   call.ToolName,
			InputJSON:  string(inputJSON),
			OutputJSON: "{}",
			Timestamp:  call.Timestamp,
			Status:     call.Status,
		}); err != nil {
			return err
		}
	}
	for _, path := range decision.Outcome.FilesChanged {
		if err := s.InsertUnifiedArtifact(UnifiedArtifact{
			SubTaskID: subTaskID,
			Path:      path,
		}); err != nil {
			return err
		}
	}
	return nil
}

// unifiedDecisionTaskID derives a stable, per-row task ID from a DecisionLog.
func unifiedDecisionTaskID(decision agent.DecisionLog) string {
	ts := decision.Timestamp.UTC().Format("20060102T150405.000000000Z")
	if decision.SessionID != "" {
		return "decision:" + decision.SessionID + ":" + ts
	}
	return "decision:" + ts
}

func backendForDecisionModel(model string) string {
	switch {
	case model == "":
		return ""
	case containsInsensitive(model, "gpt"), containsInsensitive(model, "codex"):
		return "codex"
	default:
		return "claude"
	}
}

func containsInsensitive(s, substr string) bool {
	return len(s) >= len(substr) && sqlContainsFold(s, substr)
}

func sqlContainsFold(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if equalFoldASCII(s[i:i+len(substr)], substr) {
			return true
		}
	}
	return false
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func parseDBTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func parseNullableDBTime(value sql.NullString) *time.Time {
	if !value.Valid || value.String == "" {
		return nil
	}
	parsed := parseDBTime(value.String)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}

func parseStringListJSON(raw string) []string {
	if raw == "" {
		return nil
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return nil
	}
	return values
}

func formatNullableTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return sql.NullString{}
	}
	return t.Format(time.RFC3339Nano)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
