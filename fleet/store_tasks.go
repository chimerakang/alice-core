package fleet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CreateTask queues a new task for a project. The project's trust mode at
// this moment is copied onto the task and stays with it.
func (s *Store) CreateTask(ctx context.Context, in NewTask) (Task, error) {
	t := Task{
		ProjectID: strings.TrimSpace(in.ProjectID),
		Shape:     in.Shape,
		Goal:      strings.TrimSpace(in.Goal),
		Source:    in.Source,
		Model:     strings.TrimSpace(in.Model),
		Priority:  in.Priority,
		Budget:    in.Budget,
		Status:    StatusQueued,
		Liveness:  LivenessUnknown,
	}
	if t.Shape == "" {
		t.Shape = ShapeShip
	}
	if !t.Shape.Valid() {
		return Task{}, fmt.Errorf("%w: unknown shape %q", ErrInvalidTask, t.Shape)
	}
	if t.Source.Kind == "" {
		t.Source.Kind = SourceManual
	}
	switch t.Source.Kind {
	case SourceManual:
		if t.Source.IssueNumber != 0 {
			return Task{}, fmt.Errorf("%w: a manual task cannot carry an issue number", ErrInvalidTask)
		}
		if t.Goal == "" {
			return Task{}, fmt.Errorf("%w: goal is required", ErrInvalidTask)
		}
	case SourceIssue:
		if t.Source.IssueNumber <= 0 {
			return Task{}, fmt.Errorf("%w: an issue task needs a positive issue number", ErrInvalidTask)
		}
	default:
		return Task{}, fmt.Errorf("%w: unknown source %q", ErrInvalidTask, t.Source.Kind)
	}
	if t.ProjectID == "" {
		return Task{}, fmt.Errorf("%w: project is required", ErrInvalidTask)
	}
	if t.Budget.CostUSD < 0 || t.Budget.Tokens < 0 {
		return Task{}, fmt.Errorf("%w: a budget cannot be negative", ErrInvalidTask)
	}
	budget, err := json.Marshal(t.Budget)
	if err != nil {
		return Task{}, fmt.Errorf("fleet: encode budget: %w", err)
	}

	now := s.now()
	t.CreatedAt, t.UpdatedAt = now.UTC(), now.UTC()

	// A random ID can in principle collide; retry rather than fail the dispatch.
	for attempt := 0; ; attempt++ {
		id, err := newTaskID()
		if err != nil {
			return Task{}, err
		}
		t.ID = id
		err = s.inTx(ctx, func(tx querier) error {
			var trust string
			err := tx.QueryRowContext(ctx, `SELECT trust_mode FROM fleet_projects WHERE id = ?`, t.ProjectID).Scan(&trust)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrProjectNotFound, t.ProjectID)
			}
			if err != nil {
				return fmt.Errorf("fleet: load project: %w", err)
			}
			t.TrustMode = TrustMode(trust)
			_, err = tx.ExecContext(ctx, `INSERT INTO fleet_tasks
				(id, project_id, shape, goal, source_kind, issue_number, trust_mode, model, priority,
				 status, created_at, updated_at, budget)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, t.ProjectID, string(t.Shape), t.Goal, string(t.Source.Kind), t.Source.IssueNumber,
				trust, t.Model, t.Priority, string(t.Status), toMillis(now), toMillis(now), string(budget))
			if err != nil {
				return err
			}
			_, err = appendEvent(ctx, tx, t.ID, EventStatus, StatusChange{To: StatusQueued, Reason: "created"}, toMillis(now))
			return err
		})
		if isUniqueViolation(err) && attempt < 4 {
			continue
		}
		if err != nil {
			if errors.Is(err, ErrProjectNotFound) {
				return Task{}, err
			}
			return Task{}, fmt.Errorf("fleet: create task: %w", err)
		}
		return t, nil
	}
}

const taskColumns = `id, project_id, shape, goal, source_kind, issue_number, trust_mode, model, priority,
	status, status_reason, branch, worktree_path, session_id,
	input_tokens, output_tokens, cost_usd, created_at, updated_at, started_at, finished_at, last_activity_at, liveness, pr_url, pr_number, budget`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var (
		t                          Task
		shape, kind, trust, status string
		liveness, budget           string
		created, updated           int64
		started, finished, active  sql.NullInt64
	)
	err := row.Scan(&t.ID, &t.ProjectID, &shape, &t.Goal, &kind, &t.Source.IssueNumber, &trust, &t.Model, &t.Priority,
		&status, &t.StatusReason, &t.Branch, &t.WorktreePath, &t.SessionID,
		&t.InputTokens, &t.OutputTokens, &t.CostUSD, &created, &updated, &started, &finished, &active, &liveness, &t.PRURL, &t.PRNumber, &budget)
	if err != nil {
		return Task{}, err
	}
	t.Shape, t.Source.Kind, t.TrustMode, t.Status = Shape(shape), SourceKind(kind), TrustMode(trust), Status(status)
	t.CreatedAt, t.UpdatedAt = fromMillis(created), fromMillis(updated)
	t.StartedAt, t.FinishedAt = fromNullMillis(started), fromNullMillis(finished)
	t.LastActivityAt = fromNullMillis(active)
	t.Liveness = Liveness(liveness)
	if err := json.Unmarshal([]byte(budget), &t.Budget); err != nil {
		return Task{}, fmt.Errorf("fleet: decode budget of %s: %w", t.ID, err)
	}
	return t, nil
}

func getTask(ctx context.Context, q querier, id string) (Task, error) {
	t, err := scanTask(q.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM fleet_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	if err != nil {
		return Task{}, fmt.Errorf("fleet: load task: %w", err)
	}
	return t, nil
}

// GetTask returns the task with the given ID.
func (s *Store) GetTask(ctx context.Context, id string) (Task, error) {
	return getTask(ctx, s.db, id)
}

// ListTasks returns the tasks matching filter.
func (s *Store) ListTasks(ctx context.Context, filter TaskFilter) ([]Task, error) {
	where, args := filter.where()
	query := `SELECT ` + taskColumns + ` FROM fleet_tasks` + where
	switch filter.Order {
	case QueueOrder:
		query += ` ORDER BY priority DESC, created_at ASC, id ASC`
	default:
		query += ` ORDER BY created_at DESC, id DESC`
	}
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet: list tasks: %w", err)
	}
	defer rows.Close()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("fleet: list tasks: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list tasks: %w", err)
	}
	return tasks, nil
}

// CountTasks returns how many tasks match filter. Order and Limit are ignored.
func (s *Store) CountTasks(ctx context.Context, filter TaskFilter) (int, error) {
	where, args := filter.where()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleet_tasks`+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("fleet: count tasks: %w", err)
	}
	return n, nil
}

func (f TaskFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.ProjectID != "" {
		conds = append(conds, "project_id = ?")
		args = append(args, f.ProjectID)
	}
	if len(f.Statuses) > 0 {
		conds = append(conds, "status IN ("+placeholders(len(f.Statuses))+")")
		for _, st := range f.Statuses {
			args = append(args, string(st))
		}
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// Transition moves a task to a new status and records the change in its
// event log, atomically. reason is stored on the task and in the event.
//
// A change the lifecycle forbids returns a *TransitionError (matching
// ErrInvalidTransition) and leaves the task untouched. If another caller
// changed the task's status first, ErrConflict is returned.
func (s *Store) Transition(ctx context.Context, taskID string, to Status, reason string) (Task, error) {
	return s.transition(ctx, taskID, to, reason, false)
}

// transition is Transition with the change optionally marked as requested:
// the result of a stop someone asked for, which therefore needs no wake.
func (s *Store) transition(ctx context.Context, taskID string, to Status, reason string, requested bool) (Task, error) {
	var out Task
	err := s.inTx(ctx, func(tx querier) error {
		current, err := getTask(ctx, tx, taskID)
		if err != nil {
			return err
		}
		if !to.Valid() || !CanTransition(current.Status, to) {
			return &TransitionError{TaskID: taskID, From: current.Status, To: to}
		}
		now := toMillis(s.now())

		// started_at marks the current run; finished_at marks a terminal state.
		started, finished := "started_at", "NULL"
		switch {
		case to == StatusRunning && current.StartedAt == nil:
			started = "?"
		case to == StatusQueued:
			started = "NULL"
		}
		args := []any{string(to), reason, now}
		if started == "?" {
			args = append(args, now)
		}
		if to.IsTerminal() {
			finished = "?"
			args = append(args, now)
		}
		args = append(args, taskID, string(current.Status))

		res, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET
			status = ?, status_reason = ?, updated_at = ?,
			started_at = `+started+`, finished_at = `+finished+`
			WHERE id = ? AND status = ?`, args...)
		if err != nil {
			return fmt.Errorf("fleet: update task status: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrConflict, taskID)
		}
		if _, err := appendEvent(ctx, tx, taskID, EventStatus,
			StatusChange{From: current.Status, To: to, Reason: reason, Requested: requested}, now); err != nil {
			return err
		}
		out, err = getTask(ctx, tx, taskID)
		return err
	})
	if err != nil {
		return Task{}, err
	}
	return out, nil
}

// SetWorkspace records the branch and worktree a task works in.
func (s *Store) SetWorkspace(ctx context.Context, taskID, branch, worktreePath string) error {
	return s.updateTask(ctx, taskID, `branch = ?, worktree_path = ?`, branch, worktreePath)
}

// SetSession records the agent session a task can be resumed from.
func (s *Store) SetSession(ctx context.Context, taskID, sessionID string) error {
	return s.updateTask(ctx, taskID, `session_id = ?`, sessionID)
}

// SetPullRequest records the pull request opened for a task.
func (s *Store) SetPullRequest(ctx context.Context, taskID, url string, number int) error {
	return s.updateTask(ctx, taskID, `pr_url = ?, pr_number = ?`, url, number)
}

// SetLiveness records what is known about the task's agent process. A change
// is written to the task's event log; setting the value it already has does
// nothing.
func (s *Store) SetLiveness(ctx context.Context, taskID string, liveness Liveness) error {
	if !liveness.Valid() {
		return fmt.Errorf("%w: unknown liveness %q", ErrInvalidTask, liveness)
	}
	return s.inTx(ctx, func(tx querier) error {
		var current string
		err := tx.QueryRowContext(ctx, `SELECT liveness FROM fleet_tasks WHERE id = ?`, taskID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		if err != nil {
			return fmt.Errorf("fleet: load task: %w", err)
		}
		if Liveness(current) == liveness {
			return nil
		}
		now := toMillis(s.now())
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET liveness = ?, updated_at = ? WHERE id = ?`,
			string(liveness), now, taskID); err != nil {
			return fmt.Errorf("fleet: update task liveness: %w", err)
		}
		_, err = appendEvent(ctx, tx, taskID, EventLiveness, LivenessChange{From: Liveness(current), To: liveness}, now)
		return err
	})
}

// lastEventID returns the ID of the newest event in the log, or 0.
func (s *Store) lastEventID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM fleet_task_events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("fleet: load last event: %w", err)
	}
	return id.Int64, nil
}

// lastEvent returns the task's most recent event of a type, if it has one.
func (s *Store) lastEvent(ctx context.Context, taskID, eventType string) (Event, bool, error) {
	var (
		ev      Event
		body    sql.NullString
		created int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, task_id, type, payload, created_at FROM fleet_task_events
		WHERE task_id = ? AND type = ? ORDER BY id DESC LIMIT 1`, taskID, eventType).
		Scan(&ev.ID, &ev.TaskID, &ev.Type, &body, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, fmt.Errorf("fleet: load event: %w", err)
	}
	if body.Valid {
		ev.Payload = json.RawMessage(body.String)
	}
	ev.CreatedAt = fromMillis(created)
	return ev, true, nil
}

// AddUsage adds to a task's token and cost totals. Increments from
// concurrent callers all count.
func (s *Store) AddUsage(ctx context.Context, taskID string, u Usage) error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CostUSD < 0 {
		return fmt.Errorf("%w: usage cannot be negative", ErrInvalidTask)
	}
	return s.inTx(ctx, func(tx querier) error {
		now := toMillis(s.now())
		var projectID string
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM fleet_tasks WHERE id = ?`, taskID).Scan(&projectID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		if err != nil {
			return fmt.Errorf("fleet: load task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET
			input_tokens = input_tokens + ?, output_tokens = output_tokens + ?, cost_usd = cost_usd + ?,
			updated_at = ? WHERE id = ?`, u.InputTokens, u.OutputTokens, u.CostUSD, now, taskID); err != nil {
			return fmt.Errorf("fleet: update task usage: %w", err)
		}
		// The ledger row is what makes "how much today" answerable.
		if _, err := tx.ExecContext(ctx, `INSERT INTO fleet_usage
			(task_id, project_id, input_tokens, output_tokens, cost_usd, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			taskID, projectID, u.InputTokens, u.OutputTokens, u.CostUSD, now); err != nil {
			return fmt.Errorf("fleet: record usage: %w", err)
		}
		return nil
	})
}

// UsageSince totals the usage recorded at or after since. projectID "" totals
// the whole fleet.
func (s *Store) UsageSince(ctx context.Context, projectID string, since time.Time) (Usage, error) {
	query := `SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), COALESCE(SUM(cost_usd), 0)
		FROM fleet_usage WHERE created_at >= ?`
	args := []any{toMillis(since)}
	if projectID != "" {
		query += ` AND project_id = ?`
		args = append(args, projectID)
	}
	var u Usage
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&u.InputTokens, &u.OutputTokens, &u.CostUSD); err != nil {
		return Usage{}, fmt.Errorf("fleet: total usage: %w", err)
	}
	return u, nil
}

func (s *Store) updateTask(ctx context.Context, taskID, set string, args ...any) error {
	args = append(args, toMillis(s.now()), taskID)
	return s.inTx(ctx, func(tx querier) error {
		res, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET `+set+`, updated_at = ? WHERE id = ?`, args...)
		if err != nil {
			return fmt.Errorf("fleet: update task: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		return nil
	})
}

// AppendEvent adds an entry to a task's history. payload is stored as JSON;
// nil stores no body. Events are never modified or deleted afterwards.
func (s *Store) AppendEvent(ctx context.Context, taskID, eventType string, payload any) (Event, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return Event{}, fmt.Errorf("%w: event type is required", ErrInvalidTask)
	}
	var ev Event
	err := s.inTx(ctx, func(tx querier) error {
		var one int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM fleet_tasks WHERE id = ?`, taskID).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		if err != nil {
			return fmt.Errorf("fleet: load task: %w", err)
		}
		ev, err = appendEvent(ctx, tx, taskID, eventType, payload, toMillis(s.now()))
		return err
	})
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

// RecordActivity appends an event produced by the task's agent and stamps the
// task's LastActivityAt, in one transaction. Use it for everything the agent
// emits; use AppendEvent for events that are not signs of life.
func (s *Store) RecordActivity(ctx context.Context, taskID, eventType string, payload any) (Event, error) {
	eventType = strings.TrimSpace(eventType)
	if eventType == "" {
		return Event{}, fmt.Errorf("%w: event type is required", ErrInvalidTask)
	}
	var ev Event
	err := s.inTx(ctx, func(tx querier) error {
		now := toMillis(s.now())
		res, err := tx.ExecContext(ctx, `UPDATE fleet_tasks SET last_activity_at = ?, updated_at = ? WHERE id = ?`,
			now, now, taskID)
		if err != nil {
			return fmt.Errorf("fleet: record activity: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: %s", ErrTaskNotFound, taskID)
		}
		ev, err = appendEvent(ctx, tx, taskID, eventType, payload, now)
		return err
	})
	if err != nil {
		return Event{}, err
	}
	return ev, nil
}

func appendEvent(ctx context.Context, q querier, taskID, eventType string, payload any, nowMillis int64) (Event, error) {
	var body sql.NullString
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return Event{}, fmt.Errorf("fleet: encode event payload: %w", err)
		}
		body, raw = sql.NullString{String: string(b), Valid: true}, b
	}
	res, err := q.ExecContext(ctx, `INSERT INTO fleet_task_events (task_id, type, payload, created_at)
		VALUES (?, ?, ?, ?)`, taskID, eventType, body, nowMillis)
	if err != nil {
		return Event{}, fmt.Errorf("fleet: append event: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("fleet: append event: %w", err)
	}
	return Event{ID: id, TaskID: taskID, Type: eventType, Payload: raw, CreatedAt: fromMillis(nowMillis)}, nil
}

// ListEvents returns events in the order they were written. taskID "" reads
// across all tasks. Only events with an ID greater than afterID are returned,
// so a reader resumes by passing the last ID it saw. limit caps the result
// when positive.
func (s *Store) ListEvents(ctx context.Context, taskID string, afterID int64, limit int) ([]Event, error) {
	query := `SELECT id, task_id, type, payload, created_at FROM fleet_task_events WHERE id > ?`
	args := []any{afterID}
	if taskID != "" {
		query += ` AND task_id = ?`
		args = append(args, taskID)
	}
	query += ` ORDER BY id ASC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("fleet: list events: %w", err)
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var (
			ev      Event
			body    sql.NullString
			created int64
		)
		if err := rows.Scan(&ev.ID, &ev.TaskID, &ev.Type, &body, &created); err != nil {
			return nil, fmt.Errorf("fleet: list events: %w", err)
		}
		if body.Valid {
			ev.Payload = json.RawMessage(body.String)
		}
		ev.CreatedAt = fromMillis(created)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fleet: list events: %w", err)
	}
	return events, nil
}
