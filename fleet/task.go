package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Shape says what a task is allowed to produce.
type Shape string

const (
	// ShapeShip delivers changes to the repository.
	ShapeShip Shape = "ship"
	// ShapeScout investigates and leaves a report. It must not modify the
	// repository or push anything.
	ShapeScout Shape = "scout"
)

// Valid reports whether s is a known shape.
func (s Shape) Valid() bool { return s == ShapeShip || s == ShapeScout }

// SourceKind says where a task came from.
type SourceKind string

const (
	// SourceManual is a task typed in by the owner.
	SourceManual SourceKind = "manual"
	// SourceIssue is a task dispatched from a GitHub issue.
	SourceIssue SourceKind = "issue"
)

// Source records a task's origin.
type Source struct {
	Kind SourceKind
	// IssueNumber is the GitHub issue, for SourceIssue.
	IssueNumber int
}

// Status is a task's position in its lifecycle.
type Status string

const (
	// StatusQueued: accepted, waiting for a slot.
	StatusQueued Status = "queued"
	// StatusRunning: an agent is working on it.
	StatusRunning Status = "running"
	// StatusVerifying: the agent finished and acceptance checks are running.
	StatusVerifying Status = "verifying"
	// StatusReviewReady: checks passed and the result awaits the owner.
	StatusReviewReady Status = "review_ready"
	// StatusNeedsDecision: blocked until the owner decides something.
	StatusNeedsDecision Status = "needs_decision"
	// StatusDone: finished and accepted.
	StatusDone Status = "done"
	// StatusFailed: gave up. May be retried.
	StatusFailed Status = "failed"
	// StatusCancelled: stopped or discarded by the owner.
	StatusCancelled Status = "cancelled"
)

// Liveness is what is known about a task's agent process, separately from
// the task's status. A missing or stale signal reads as unknown, never as
// idle: "we do not know" must not be mistaken for "it has stopped".
type Liveness string

const (
	// LivenessUnknown: nothing is known. The default.
	LivenessUnknown Liveness = "unknown"
	// LivenessBusy: an agent process is running for the task.
	LivenessBusy Liveness = "busy"
	// LivenessIdle: the task's last run ended normally and nothing is running.
	LivenessIdle Liveness = "idle"
	// LivenessDead: the task should have a process and does not.
	LivenessDead Liveness = "dead"
)

// Valid reports whether l is a known liveness.
func (l Liveness) Valid() bool {
	switch l {
	case LivenessUnknown, LivenessBusy, LivenessIdle, LivenessDead:
		return true
	}
	return false
}

// AllStatuses lists every status, in lifecycle order.
var AllStatuses = []Status{
	StatusQueued, StatusRunning, StatusVerifying, StatusReviewReady,
	StatusNeedsDecision, StatusDone, StatusFailed, StatusCancelled,
}

// transitions is the complete set of legal status changes. Anything not
// listed is rejected.
var transitions = map[Status][]Status{
	// A slot opens, or the owner withdraws the task before it starts.
	StatusQueued: {StatusRunning, StatusCancelled},
	// The agent finishes, dies, asks a question, or is stopped.
	StatusRunning: {StatusVerifying, StatusNeedsDecision, StatusFailed, StatusCancelled},
	// Checks pass (review_ready, or done when nothing needs review), fail
	// and go back to the agent (running), exhaust their retries
	// (needs_decision), or cannot run at all (failed).
	StatusVerifying: {StatusReviewReady, StatusDone, StatusRunning, StatusNeedsDecision, StatusFailed, StatusCancelled},
	// The owner accepts, sends it back with more instructions, or discards
	// it; or delivering the result fails and the owner has to step in.
	StatusReviewReady: {StatusDone, StatusRunning, StatusNeedsDecision, StatusCancelled},
	// The owner answers (running), restarts it (queued), waves it through
	// (review_ready), gives up (failed) or discards it (cancelled).
	StatusNeedsDecision: {StatusRunning, StatusQueued, StatusReviewReady, StatusFailed, StatusCancelled},
	// A failed task may be retried from the start.
	StatusFailed: {StatusQueued},
	// done and cancelled are final.
	StatusDone:      {},
	StatusCancelled: {},
}

// Valid reports whether s is a known status.
func (s Status) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// IsTerminal reports whether the task has stopped: done, failed or cancelled.
// A failed task is terminal but can still be retried.
func (s Status) IsTerminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// IsActive reports whether the task currently occupies a concurrency slot.
func (s Status) IsActive() bool {
	return s == StatusRunning || s == StatusVerifying
}

// NeedsOwner reports whether the task is waiting on the owner.
func (s Status) NeedsOwner() bool {
	return s == StatusReviewReady || s == StatusNeedsDecision
}

// CanTransition reports whether a task may move from one status to another.
func CanTransition(from, to Status) bool {
	for _, next := range transitions[from] {
		if next == to {
			return true
		}
	}
	return false
}

// NextStatuses returns the statuses reachable from s in one step.
func NextStatuses(s Status) []Status {
	return append([]Status(nil), transitions[s]...)
}

// TransitionError is returned for a status change the lifecycle forbids.
type TransitionError struct {
	TaskID string
	From   Status
	To     Status
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("fleet: task %s cannot go from %s to %s", e.TaskID, e.From, e.To)
}

// Is makes errors.Is(err, ErrInvalidTransition) match.
func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

var (
	// ErrTaskNotFound is returned when no task has the given ID.
	ErrTaskNotFound = errors.New("fleet: task not found")
	// ErrInvalidTask wraps every task validation failure.
	ErrInvalidTask = errors.New("fleet: invalid task")
	// ErrInvalidTransition matches any *TransitionError.
	ErrInvalidTransition = errors.New("fleet: invalid status transition")
	// ErrConflict is returned when a task changed underneath an update.
	// Reload the task and decide again.
	ErrConflict = errors.New("fleet: task was modified concurrently")
)

// Task is one agent run against one project, from dispatch to a final state.
type Task struct {
	// ID is a short random identifier, unique across all projects.
	ID        string
	ProjectID string
	Shape     Shape
	// Goal is the instruction given to the agent.
	Goal   string
	Source Source
	// TrustMode is the project's mode at dispatch. It does not change.
	TrustMode TrustMode
	// Model is the model requested for this task, or "" for the default.
	Model string
	// Priority orders the queue: higher runs first.
	Priority int

	Status Status
	// StatusReason says why the task is in its current status.
	StatusReason string
	// Liveness is what is known about the task's agent process.
	Liveness Liveness

	// Branch and WorktreePath locate the task's isolated checkout, once one
	// has been created.
	Branch       string
	WorktreePath string
	// SessionID is the agent's native session, used to resume it.
	SessionID string
	// PRURL and PRNumber identify the pull request opened for the task, in
	// the trust modes that open one.
	PRURL    string
	PRNumber int

	InputTokens  int64
	OutputTokens int64
	CostUSD      float64

	CreatedAt time.Time
	UpdatedAt time.Time
	// StartedAt is when the current run began; nil while queued.
	StartedAt *time.Time
	// FinishedAt is when the task reached a terminal status; nil otherwise.
	FinishedAt *time.Time
	// LastActivityAt is when the agent last produced output; nil before the
	// first run. A running task whose activity has gone stale may be stuck.
	LastActivityAt *time.Time
}

// NewTask is the input to CreateTask.
type NewTask struct {
	ProjectID string
	// Shape defaults to ShapeShip.
	Shape Shape
	// Goal is required unless the task comes from an issue.
	Goal string
	// Source defaults to SourceManual.
	Source   Source
	Model    string
	Priority int
}

// Usage is an increment of token and cost consumption.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
}

// EventStatus is the event type recorded for every status transition.
const EventStatus = "status"

// Event is one entry in a task's append-only history.
type Event struct {
	// ID increases monotonically across all tasks, so it doubles as a
	// resume cursor for readers.
	ID     int64
	TaskID string
	Type   string
	// Payload is the event's JSON body, or nil.
	Payload   json.RawMessage
	CreatedAt time.Time
}

// StatusChange is the payload of an EventStatus event.
type StatusChange struct {
	From   Status `json:"from"`
	To     Status `json:"to"`
	Reason string `json:"reason,omitempty"`
}

// TaskOrder selects how ListTasks sorts.
type TaskOrder int

const (
	// NewestFirst sorts by creation time, newest first. The default.
	NewestFirst TaskOrder = iota
	// QueueOrder sorts by priority, highest first, then oldest first: the
	// order in which queued tasks should start.
	QueueOrder
)

// TaskFilter narrows ListTasks. The zero value lists every task.
type TaskFilter struct {
	// ProjectID restricts to one project when not empty.
	ProjectID string
	// Statuses restricts to the given statuses when not empty.
	Statuses []Status
	Order    TaskOrder
	// Limit caps the result when positive.
	Limit int
}
