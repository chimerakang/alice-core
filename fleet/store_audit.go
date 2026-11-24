package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AuditEntry records one command: who gave it, through what, and how it went.
type AuditEntry struct {
	ID int64
	// Actor is who gave the command.
	Actor string
	// Via is the surface it came through, e.g. "telegram" or "web".
	Via string
	// Command is the command's name, e.g. "dispatch".
	Command   string
	ProjectID string
	TaskID    string
	// Detail is a short description of what was asked.
	Detail string
	// OK is false when the command was refused or failed; Error says why.
	OK        bool
	Error     string
	CreatedAt time.Time
}

// AuditFilter narrows ListAudit. The zero value lists everything.
type AuditFilter struct {
	ProjectID string
	TaskID    string
	Actor     string
	// BeforeID, when positive, returns only entries older than that ID, for
	// paging backwards through history.
	BeforeID int64
	// Limit caps the result when positive.
	Limit int
}

// recordAudit writes an audit entry. It must not fail the command it
// describes, so errors are swallowed by callers.
func (s *Store) recordAudit(ctx context.Context, e AuditEntry) error {
	return s.inTx(ctx, func(tx querier) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO fleet_audit
			(actor, via, command, project_id, task_id, detail, ok, error, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.Actor, e.Via, e.Command, e.ProjectID, e.TaskID, e.Detail, e.OK, e.Error, toMillis(s.now()))
		if err != nil {
			return fmt.Errorf("fleet: record audit: %w", err)
		}
		return nil
	})
}

// ListAudit returns audit entries, newest first.
func (s *Store) ListAudit(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	var conds []string
	var args []any
	add := func(cond string, arg any) {
		conds = append(conds, cond)
		args = append(args, arg)
	}
	if filter.ProjectID != "" {
		add("project_id = ?", filter.ProjectID)
	}
	if filter.TaskID != "" {
		add("task_id = ?", filter.TaskID)
	}
	if filter.Actor != "" {
		add("actor = ?", filter.Actor)
	}
	if filter.BeforeID > 0 {
		add("id < ?", filter.BeforeID)
	}
	query := `SELECT id, actor, via, command, project_id, task_id, detail, ok, error, created_at FROM fleet_audit`
	if len(conds) > 0 {
		query += " WHERE " + strings.Join(conds, " AND ")
	}
	query += " ORDER BY id DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet: list audit: %w", err)
	}
	defer rows.Close()
	entries := []AuditEntry{}
	for rows.Next() {
		var (
			e    AuditEntry
			when int64
		)
		if err := rows.Scan(&e.ID, &e.Actor, &e.Via, &e.Command, &e.ProjectID, &e.TaskID, &e.Detail, &e.OK, &e.Error, &when); err != nil {
			return nil, fmt.Errorf("fleet: list audit: %w", err)
		}
		e.CreatedAt = fromMillis(when)
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list audit: %w", err)
	}
	return entries, nil
}
