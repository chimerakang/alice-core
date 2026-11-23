package fleet

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Limit caps spending. A zero field means that dimension is not limited.
type Limit struct {
	// CostUSD is the most that may be spent, in US dollars.
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Tokens is the most input plus output tokens that may be used.
	Tokens int64 `json:"tokens,omitempty"`
}

// IsZero reports whether the limit limits nothing.
func (l Limit) IsZero() bool { return l.CostUSD <= 0 && l.Tokens <= 0 }

// exceededBy says whether usage has reached the limit, and how. Reaching a
// limit counts as exceeding it: once the cap is hit there is nothing left to
// spend.
func (l Limit) exceededBy(u Usage) (string, bool) {
	if l.CostUSD > 0 && u.CostUSD >= l.CostUSD {
		return fmt.Sprintf("$%.2f spent of a $%.2f limit", u.CostUSD, l.CostUSD), true
	}
	if tokens := u.InputTokens + u.OutputTokens; l.Tokens > 0 && tokens >= l.Tokens {
		return fmt.Sprintf("%d tokens used of a %d limit", tokens, l.Tokens), true
	}
	return "", false
}

// or returns l, or fallback when l limits nothing.
func (l Limit) or(fallback Limit) Limit {
	if l.IsZero() {
		return fallback
	}
	return l
}

// ProjectBudget is a project's own limits. A zero Limit means the project
// uses the fleet-wide default for that scope.
type ProjectBudget struct {
	// Task caps what any one task of the project may use in total.
	Task Limit `json:"task,omitempty"`
	// Daily caps what the project's tasks may use together in one day.
	Daily Limit `json:"daily,omitempty"`
}

// Budget is the fleet's spending policy. A task may run only while it is
// within all three scopes.
//
// Usage is known when an agent's turn ends, not while it runs, so a limit is
// enforced at the next boundary: no new run starts once a limit is reached,
// and when a daily limit is reached the runs already going are interrupted.
type Budget struct {
	// Task is the default cap on one task's total usage, across all its
	// runs. A project's own ProjectBudget.Task overrides it.
	Task Limit
	// ProjectDaily is the default cap on one project's usage per day. A
	// project's own ProjectBudget.Daily overrides it.
	ProjectDaily Limit
	// Daily caps the whole fleet's usage per day.
	Daily Limit
	// Location decides where a day starts. Defaults to time.Local.
	Location *time.Location
}

// BudgetScope names which limit was reached.
type BudgetScope string

const (
	BudgetScopeTask    BudgetScope = "task"
	BudgetScopeProject BudgetScope = "project"
	BudgetScopeFleet   BudgetScope = "fleet"
)

// BudgetError reports a limit that has been reached. It matches
// ErrOverBudget with errors.Is.
type BudgetError struct {
	Scope     BudgetScope
	ProjectID string
	TaskID    string
	// Detail says what was used against what limit.
	Detail string
}

func (e *BudgetError) Error() string {
	switch e.Scope {
	case BudgetScopeTask:
		return fmt.Sprintf("fleet: task %s is over budget: %s", e.TaskID, e.Detail)
	case BudgetScopeProject:
		return fmt.Sprintf("fleet: project %s is over its daily budget: %s", e.ProjectID, e.Detail)
	default:
		return fmt.Sprintf("fleet: the daily budget is used up: %s", e.Detail)
	}
}

// Reason is the short form used as a task's status reason.
func (e *BudgetError) Reason() string {
	switch e.Scope {
	case BudgetScopeTask:
		return "over the task budget: " + e.Detail
	case BudgetScopeProject:
		return "over the project's daily budget: " + e.Detail
	default:
		return "over the fleet's daily budget: " + e.Detail
	}
}

// Is makes errors.Is(err, ErrOverBudget) match.
func (e *BudgetError) Is(target error) bool { return target == ErrOverBudget }

// ErrOverBudget matches any *BudgetError.
var ErrOverBudget = errors.New("fleet: over budget")

// dayStart returns the beginning of the day containing now.
func (b *Budget) dayStart(now time.Time) time.Time {
	loc := time.Local
	if b != nil && b.Location != nil {
		loc = b.Location
	}
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
}

// budgetView is one consistent reading of today's usage, shared by every
// check in a scheduling or watching pass so they agree with each other.
type budgetView struct {
	budget   *Budget
	store    *Store
	since    time.Time
	fleet    *Usage
	projects map[string]Usage
}

func (b *Budget) view(store *Store) *budgetView {
	if b == nil {
		return nil
	}
	return &budgetView{budget: b, store: store, since: b.dayStart(store.now()), projects: map[string]Usage{}}
}

// day identifies the day being measured, for wake de-duplication.
func (v *budgetView) day() string { return v.since.Format("2006-01-02") }

// fleetExceeded checks the whole fleet's daily limit.
func (v *budgetView) fleetExceeded(ctx context.Context) (*BudgetError, error) {
	if v == nil || v.budget.Daily.IsZero() {
		return nil, nil
	}
	if v.fleet == nil {
		used, err := v.store.UsageSince(ctx, "", v.since)
		if err != nil {
			return nil, err
		}
		v.fleet = &used
	}
	if detail, over := v.budget.Daily.exceededBy(*v.fleet); over {
		return &BudgetError{Scope: BudgetScopeFleet, Detail: detail}, nil
	}
	return nil, nil
}

// projectExceeded checks one project's daily limit.
func (v *budgetView) projectExceeded(ctx context.Context, project Project) (*BudgetError, error) {
	if v == nil {
		return nil, nil
	}
	limit := project.Budget.Daily.or(v.budget.ProjectDaily)
	if limit.IsZero() {
		return nil, nil
	}
	used, ok := v.projects[project.ID]
	if !ok {
		var err error
		if used, err = v.store.UsageSince(ctx, project.ID, v.since); err != nil {
			return nil, err
		}
		v.projects[project.ID] = used
	}
	if detail, over := limit.exceededBy(used); over {
		return &BudgetError{Scope: BudgetScopeProject, ProjectID: project.ID, Detail: detail}, nil
	}
	return nil, nil
}

// taskExceeded checks one task's own limit against its lifetime usage.
func (v *budgetView) taskExceeded(task Task, project Project) *BudgetError {
	if v == nil {
		return nil
	}
	limit := project.Budget.Task.or(v.budget.Task)
	used := Usage{InputTokens: task.InputTokens, OutputTokens: task.OutputTokens, CostUSD: task.CostUSD}
	if detail, over := limit.exceededBy(used); over {
		return &BudgetError{Scope: BudgetScopeTask, ProjectID: task.ProjectID, TaskID: task.ID, Detail: detail}
	}
	return nil
}

// admit reports the first limit that stops the task from running, checking
// the narrowest scope first, or nil when the task may run.
func (v *budgetView) admit(ctx context.Context, task Task, project Project) (*BudgetError, error) {
	if v == nil {
		return nil, nil
	}
	if over := v.taskExceeded(task, project); over != nil {
		return over, nil
	}
	if over, err := v.projectExceeded(ctx, project); over != nil || err != nil {
		if over != nil {
			over.TaskID = task.ID
		}
		return over, err
	}
	over, err := v.fleetExceeded(ctx)
	if over != nil {
		over.TaskID, over.ProjectID = task.ID, task.ProjectID
	}
	return over, err
}
