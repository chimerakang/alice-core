package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/chimerakang/alice-core/agent"
)

// Bus delivers the fleet's events to subscribers as they happen.
//
// It is a tail on the durable event log, not a second channel beside it:
// every event has the ID it was stored with, IDs only grow, and a subscriber
// that reconnects with the last ID it saw gets exactly what it missed. There
// is nothing to lose in a restart because nothing lives only in memory.
type Bus struct {
	// Store holds the event log. Required.
	Store *Store
	// Interval is how often the log is checked for new events. Zero means
	// DefaultBusInterval.
	Interval time.Duration
}

// DefaultBusInterval is the Bus's default polling interval.
const DefaultBusInterval = 250 * time.Millisecond

// LifecycleEvents are the event types that describe how tasks are doing, as
// opposed to what their agents are saying: status changes, liveness changes,
// verification results, and wakes.
var LifecycleEvents = []string{EventStatus, EventLiveness, EventVerification, EventWake, EventStartupResumed, EventStartupResumeCapped}

// BusEvent is an event from the log together with the project it belongs to.
type BusEvent struct {
	Event
	// ProjectID is the task's project, or the project a project-wide wake
	// concerns. Empty for a fleet-wide wake.
	ProjectID string
}

// EventFilter selects what a subscriber receives and where it starts. The
// zero value means every event from now on.
type EventFilter struct {
	// ProjectID restricts to one project when not empty.
	ProjectID string
	// TaskID restricts to one task when not empty.
	TaskID string
	// Types restricts to the listed event types when not empty. Use
	// LifecycleEvents to follow tasks without their agents' output.
	Types []string
	// AfterID, when positive, starts just after that event: pass the ID of
	// the last event received to resume without a gap or a repeat.
	AfterID int64
	// FromStart replays the whole log before going live. AfterID wins when
	// both are set.
	FromStart bool
}

// Subscribe returns a channel of events matching filter, in the order they
// were stored. The channel is closed when ctx is cancelled.
//
// Delivery waits for the subscriber: a slow reader is not skipped, it simply
// falls behind and catches up, because the log is still there.
func (b *Bus) Subscribe(ctx context.Context, filter EventFilter) (<-chan BusEvent, error) {
	if b.Store == nil {
		return nil, errors.New("fleet: Bus.Store is required")
	}
	cursor := filter.AfterID
	if cursor <= 0 && !filter.FromStart {
		var err error
		if cursor, err = b.Store.lastEventID(ctx); err != nil {
			return nil, err
		}
	}
	if cursor < 0 {
		cursor = 0
	}
	wanted := map[string]bool{}
	for _, t := range filter.Types {
		wanted[t] = true
	}

	out := make(chan BusEvent)
	go func() {
		defer close(out)
		interval := b.Interval
		if interval <= 0 {
			interval = DefaultBusInterval
		}
		projectOf := map[string]string{}
		const page = 500
		for {
			events, err := b.Store.ListEvents(ctx, filter.TaskID, cursor, page)
			if err == nil {
				for _, ev := range events {
					cursor = ev.ID
					if len(wanted) > 0 && !wanted[ev.Type] {
						continue
					}
					be := BusEvent{Event: ev, ProjectID: b.projectOf(ctx, ev, projectOf)}
					if filter.ProjectID != "" && be.ProjectID != filter.ProjectID {
						continue
					}
					select {
					case out <- be:
					case <-ctx.Done():
						return
					}
				}
				if len(events) == page {
					continue // more is waiting; do not pause
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
	return out, nil
}

// projectOf resolves the project an event belongs to, remembering the answer
// per task.
func (b *Bus) projectOf(ctx context.Context, ev Event, cache map[string]string) string {
	if ev.TaskID == "" {
		// Only wakes are written without a task; they name their project.
		var raised WakeRaised
		_ = json.Unmarshal(ev.Payload, &raised)
		return raised.ProjectID
	}
	if id, ok := cache[ev.TaskID]; ok {
		return id
	}
	task, err := b.Store.GetTask(ctx, ev.TaskID)
	if err != nil {
		return ""
	}
	cache[ev.TaskID] = task.ProjectID
	return task.ProjectID
}

// Tool event names passed to an agent.EventSink.
const (
	SinkToolStart    = "tool_execution_start"
	SinkToolComplete = "tool_execution_complete"
)

// ForwardTools feeds a fleet's tool activity to an agent.EventSink, the
// interface the single-agent path already broadcasts through. A dashboard
// wired to that sink shows fleet agents' tool calls with no changes.
//
// It blocks until ctx is cancelled. filter chooses which tasks to follow;
// its Types are ignored.
func (b *Bus) ForwardTools(ctx context.Context, sink agent.EventSink, filter EventFilter) error {
	if sink == nil {
		return errors.New("fleet: an EventSink is required")
	}
	filter.Types = []string{EventAgentToolUse, EventAgentToolResult}
	events, err := b.Subscribe(ctx, filter)
	if err != nil {
		return err
	}

	type started struct {
		exec agent.ToolExecution
	}
	open := map[string]started{} // tool-use ID -> the call it began
	repoOf := map[string]string{}
	for ev := range events {
		switch ev.Type {
		case EventAgentToolUse:
			var use struct {
				ID    string          `json:"id"`
				Tool  string          `json:"tool"`
				Input json.RawMessage `json:"input"`
			}
			if json.Unmarshal(ev.Payload, &use) != nil {
				continue
			}
			exec := agent.ToolExecution{
				Timestamp:   ev.CreatedAt,
				ToolName:    use.Tool,
				Status:      "running",
				ProjectPath: b.repoPath(ctx, ev.ProjectID, repoOf),
			}
			_ = json.Unmarshal(use.Input, &exec.Input)
			if use.ID != "" {
				open[use.ID] = started{exec: exec}
			}
			sink.OnToolEvent(SinkToolStart, exec)
		case EventAgentToolResult:
			var res struct {
				ToolUseID string `json:"tool_use_id"`
				IsError   bool   `json:"is_error"`
			}
			if json.Unmarshal(ev.Payload, &res) != nil {
				continue
			}
			begun, ok := open[res.ToolUseID]
			if !ok {
				continue // its start was before this subscription began
			}
			delete(open, res.ToolUseID)
			exec := begun.exec
			exec.Duration = ev.CreatedAt.Sub(exec.Timestamp)
			exec.Status = "success"
			if res.IsError {
				exec.Status, exec.Error = "error", "tool reported an error"
			}
			sink.OnToolEvent(SinkToolComplete, exec)
		}
	}
	return nil
}

func (b *Bus) repoPath(ctx context.Context, projectID string, cache map[string]string) string {
	if path, ok := cache[projectID]; ok {
		return path
	}
	project, err := b.Store.GetProject(ctx, projectID)
	if err != nil {
		return ""
	}
	cache[projectID] = project.RepoPath
	return project.RepoPath
}
