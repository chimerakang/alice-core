package fleet

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// Store persists projects, tasks and task events in SQLite.
//
// A Store is safe for concurrent use. Status transitions are guarded so that
// two callers racing on the same task cannot both succeed.
type Store struct {
	db     *sql.DB
	ownsDB bool
	now    func() time.Time
}

// OpenStore opens (creating if needed) the SQLite database at path and
// prepares the fleet tables.
func OpenStore(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("fleet: database path is required")
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("fleet: open database: %w", err)
	}
	// SQLite allows one writer at a time; a single connection turns writer
	// contention into queueing instead of SQLITE_BUSY errors.
	db.SetMaxOpenConns(1)
	store, err := NewStore(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	store.ownsDB = true
	return store, nil
}

// NewStore prepares the fleet tables in an existing database, so fleet can
// share a database file with the rest of an application. The caller keeps
// ownership of db: Close does not close it.
func NewStore(db *sql.DB) (*Store, error) {
	if db == nil {
		return nil, fmt.Errorf("fleet: database is nil")
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close releases the database if the Store opened it.
func (s *Store) Close() error {
	if s.ownsDB {
		return s.db.Close()
	}
	return nil
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS fleet_projects (
		id              TEXT PRIMARY KEY,
		name            TEXT NOT NULL,
		repo_path       TEXT NOT NULL UNIQUE,
		remote          TEXT NOT NULL DEFAULT '',
		default_branch  TEXT NOT NULL,
		trust_mode      TEXT NOT NULL,
		verify_commands TEXT NOT NULL DEFAULT '[]',
		max_concurrent  INTEGER NOT NULL,
		branch_prefix   TEXT NOT NULL,
		created_at      INTEGER NOT NULL,
		updated_at      INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS fleet_project_bindings (
		project_id TEXT NOT NULL,
		key        TEXT NOT NULL,
		value      TEXT NOT NULL,
		PRIMARY KEY (project_id, key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_project_bindings_kv
		ON fleet_project_bindings (key, value)`,
	`CREATE TABLE IF NOT EXISTS fleet_tasks (
		id            TEXT PRIMARY KEY,
		project_id    TEXT NOT NULL,
		shape         TEXT NOT NULL,
		goal          TEXT NOT NULL,
		source_kind   TEXT NOT NULL,
		issue_number  INTEGER NOT NULL DEFAULT 0,
		trust_mode    TEXT NOT NULL,
		model         TEXT NOT NULL DEFAULT '',
		priority      INTEGER NOT NULL DEFAULT 0,
		status        TEXT NOT NULL,
		status_reason TEXT NOT NULL DEFAULT '',
		branch        TEXT NOT NULL DEFAULT '',
		worktree_path TEXT NOT NULL DEFAULT '',
		session_id    TEXT NOT NULL DEFAULT '',
		input_tokens  INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cost_usd      REAL NOT NULL DEFAULT 0,
		created_at    INTEGER NOT NULL,
		updated_at    INTEGER NOT NULL,
		started_at    INTEGER,
		finished_at   INTEGER,
		last_activity_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_tasks_project_status
		ON fleet_tasks (project_id, status)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_tasks_status
		ON fleet_tasks (status)`,
	`CREATE TABLE IF NOT EXISTS fleet_task_events (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id    TEXT NOT NULL,
		type       TEXT NOT NULL,
		payload    TEXT,
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_task_events_task
		ON fleet_task_events (task_id, id)`,
	`CREATE TABLE IF NOT EXISTS fleet_verifications (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     TEXT NOT NULL,
		attempt     INTEGER NOT NULL,
		step_index  INTEGER NOT NULL,
		name        TEXT NOT NULL,
		command     TEXT NOT NULL,
		passed      INTEGER NOT NULL,
		exit_code   INTEGER NOT NULL,
		output_tail TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL,
		timed_out   INTEGER NOT NULL DEFAULT 0,
		error       TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_verifications_task
		ON fleet_verifications (task_id, attempt, step_index)`,
	`CREATE TABLE IF NOT EXISTS fleet_wakes (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id     TEXT NOT NULL,
		project_id  TEXT NOT NULL,
		kind        TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT '',
		dedupe_key  TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		consumed_at INTEGER,
		UNIQUE (task_id, kind, dedupe_key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_wakes_pending
		ON fleet_wakes (consumed_at, id)`,
	`CREATE TABLE IF NOT EXISTS fleet_meta (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	// Every increment of usage, so spending can be totalled per day. Task
	// rows hold only running totals.
	`CREATE TABLE IF NOT EXISTS fleet_backend_quota (
		backend TEXT PRIMARY KEY,
		reset_at INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS fleet_provider_quota_measurements (
		backend TEXT PRIMARY KEY,
		remaining REAL NOT NULL,
		reset_at INTEGER NOT NULL,
		observed_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS fleet_usage (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		task_id       TEXT NOT NULL,
		project_id    TEXT NOT NULL,
		input_tokens  INTEGER NOT NULL,
		output_tokens INTEGER NOT NULL,
		cost_usd      REAL NOT NULL,
		created_at    INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_usage_time
		ON fleet_usage (created_at, project_id)`,
	// Who asked for what, and how it went.
	`CREATE TABLE IF NOT EXISTS fleet_audit (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		actor      TEXT NOT NULL,
		via        TEXT NOT NULL DEFAULT '',
		command    TEXT NOT NULL,
		project_id TEXT NOT NULL DEFAULT '',
		task_id    TEXT NOT NULL DEFAULT '',
		detail     TEXT NOT NULL DEFAULT '',
		ok         INTEGER NOT NULL,
		error      TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_audit_task ON fleet_audit (task_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_audit_project ON fleet_audit (project_id, id)`,
	// How each user wants to be notified, and what was done with each wake.
	`CREATE TABLE IF NOT EXISTS fleet_notify_prefs (
		user_id    TEXT PRIMARY KEY,
		mode       TEXT NOT NULL,
		grant_json TEXT NOT NULL DEFAULT '{}',
		since      INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS fleet_notify_log (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     TEXT NOT NULL,
		wake_id     INTEGER NOT NULL,
		task_id     TEXT NOT NULL DEFAULT '',
		project_id  TEXT NOT NULL DEFAULT '',
		kind        TEXT NOT NULL,
		detail      TEXT NOT NULL DEFAULT '',
		disposition TEXT NOT NULL,
		note        TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_notify_log_user ON fleet_notify_log (user_id, id)`,
	`CREATE INDEX IF NOT EXISTS idx_fleet_notify_log_task ON fleet_notify_log (user_id, task_id, disposition, id)`,
	// The event log is history: once written, a row is never changed or removed.
	`CREATE TRIGGER IF NOT EXISTS fleet_task_events_no_update
		BEFORE UPDATE ON fleet_task_events
		BEGIN SELECT RAISE(ABORT, 'fleet_task_events is append-only'); END`,
	`CREATE TRIGGER IF NOT EXISTS fleet_task_events_no_delete
		BEFORE DELETE ON fleet_task_events
		BEGIN SELECT RAISE(ABORT, 'fleet_task_events is append-only'); END`,
}

// addedColumns are columns introduced after a table first shipped. CREATE
// TABLE IF NOT EXISTS leaves an existing table alone, so they are added here.
var addedColumns = []struct{ table, column, definition string }{
	{"fleet_backend_quota", "episode_at", "INTEGER NOT NULL DEFAULT 0"},
	{"fleet_backend_quota", "observed_at", "INTEGER NOT NULL DEFAULT 0"},
	{"fleet_tasks", "quota_backend", "TEXT NOT NULL DEFAULT ''"},
	{"fleet_tasks", "quota_model", "TEXT NOT NULL DEFAULT ''"},
	{"fleet_tasks", "quota_transfers", "INTEGER NOT NULL DEFAULT 0"},
	{"fleet_usage", "backend", "TEXT NOT NULL DEFAULT ''"},
	{"fleet_usage", "model", "TEXT NOT NULL DEFAULT ''"},
	{"fleet_tasks", "liveness", "TEXT NOT NULL DEFAULT 'unknown'"},
	{"fleet_tasks", "pr_url", "TEXT NOT NULL DEFAULT ''"},
	{"fleet_tasks", "pr_number", "INTEGER NOT NULL DEFAULT 0"},
	{"fleet_projects", "budget", "TEXT NOT NULL DEFAULT '{}'"},
	{"fleet_tasks", "budget", "TEXT NOT NULL DEFAULT '{}'"},
	{"fleet_tasks", "target_branch", "TEXT NOT NULL DEFAULT ''"},
}

func (s *Store) migrate() error {
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("fleet: prepare schema: %w", err)
		}
	}
	for _, c := range addedColumns {
		has, err := s.hasColumn(c.table, c.column)
		if err != nil {
			return fmt.Errorf("fleet: inspect %s: %w", c.table, err)
		}
		if has {
			continue
		}
		if _, err := s.db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.column + " " + c.definition); err != nil {
			return fmt.Errorf("fleet: add %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

func (s *Store) hasColumn(table, column string) (bool, error) {
	rows, err := s.db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// inTx runs fn in a write transaction, committing when it returns nil.
//
// The transaction takes SQLite's write lock up front (BEGIN IMMEDIATE). A
// deferred transaction that reads and then writes fails with SQLITE_BUSY when
// another connection writes in between, and no busy timeout can rescue it;
// taking the lock first turns that failure into waiting. This matters when
// fleet shares a pooled database with other components.
func (s *Store) inTx(ctx context.Context, fn func(tx querier) error) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("fleet: acquire connection: %w", err)
	}
	defer conn.Close()

	if err := beginImmediate(ctx, conn); err != nil {
		return fmt.Errorf("fleet: begin transaction: %w", err)
	}
	if err := fn(conn); err != nil {
		rollback(conn)
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		rollback(conn)
		return fmt.Errorf("fleet: commit: %w", err)
	}
	return nil
}

// lockWait bounds how long a transaction waits for the write lock when the
// database was opened without a busy timeout of its own.
const lockWait = 10 * time.Second

func beginImmediate(ctx context.Context, conn *sql.Conn) error {
	deadline := time.Now().Add(lockWait)
	for delay := time.Millisecond; ; delay *= 2 {
		_, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE")
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			return err
		}
		if delay > 50*time.Millisecond {
			delay = 50 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// rollback must run even if the caller's context is already cancelled, or the
// connection would go back to the pool inside an open transaction.
func rollback(conn *sql.Conn) {
	_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
}

func isBusy(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "SQLITE_BUSY")
}

// querier is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Timestamps are stored as Unix milliseconds in UTC.

func toMillis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

func fromNullMillis(ms sql.NullInt64) *time.Time {
	if !ms.Valid {
		return nil
	}
	t := fromMillis(ms.Int64)
	return &t
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// newTaskID returns 12 hex characters of randomness: short enough to type
// and to fit in chat button payloads, wide enough not to collide.
func newTaskID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("fleet: generate task ID: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
