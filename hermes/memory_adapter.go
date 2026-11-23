package hermes

import (
	"errors"

	"github.com/chimerakang/alice-core/memory"
)

// ChatTaskReader is the read side of a task store that memory draws on.
type ChatTaskReader interface {
	// GetActiveForChat returns the chat's current task, or ErrNoTask.
	GetActiveForChat(chatID int64) (TaskState, error)
	// ListForChat returns up to limit recent tasks for the chat.
	ListForChat(chatID int64, limit int) ([]TaskState, error)
}

// MemoryTaskSource adapts a hermes task reader to memory.TaskSource, so the
// memory resolver can render hermes task history without depending on hermes.
// A nil reader yields a nil source, which memory treats as "no task history".
func MemoryTaskSource(reader ChatTaskReader) memory.TaskSource {
	if reader == nil {
		return nil
	}
	return memoryTaskSource{reader}
}

type memoryTaskSource struct{ reader ChatTaskReader }

func (s memoryTaskSource) GetActiveForChat(chatID int64) (memory.TaskSummary, error) {
	state, err := s.reader.GetActiveForChat(chatID)
	if err != nil {
		if errors.Is(err, ErrNoTask) {
			return memory.TaskSummary{}, memory.ErrNoTask
		}
		return memory.TaskSummary{}, err
	}
	return MemoryTaskSummary(state), nil
}

func (s memoryTaskSource) ListForChat(chatID int64, limit int) ([]memory.TaskSummary, error) {
	states, err := s.reader.ListForChat(chatID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]memory.TaskSummary, 0, len(states))
	for _, state := range states {
		out = append(out, MemoryTaskSummary(state))
	}
	return out, nil
}

// MemoryTaskSummary reduces a task state to what memory renders into context.
func MemoryTaskSummary(state TaskState) memory.TaskSummary {
	summary := memory.TaskSummary{
		ID:          state.ID,
		Status:      string(state.Status),
		IssueNumber: state.GithubIssueNumber,
		Goal:        state.Goal,
		UpdatedAt:   state.UpdatedAt,
		Progress:    state.Accumulated,
	}
	for _, sub := range state.Plan {
		summary.Steps = append(summary.Steps, memory.TaskStep{
			Description: sub.Description,
			Status:      string(sub.Status),
			Result:      sub.Result,
		})
	}
	return summary
}
