package engine

import (
	"context"
	"time"

	"github.com/chimerakang/alice-core/hermes"
)

// DirectRunner is the legacy Agent.Run shape wrapped by DirectEngine.
type DirectRunner interface {
	Run(userMessage string, onUpdate func(string, bool)) (string, error)
}

// DirectEngine delegates execution to the existing direct Agent.Run path.
type DirectEngine struct {
	runner DirectRunner
}

type DirectOption func(*DirectEngine)

func NewDirectEngine(runner DirectRunner, opts ...DirectOption) *DirectEngine {
	engine := &DirectEngine{runner: runner}
	for _, opt := range opts {
		if opt != nil {
			opt(engine)
		}
	}
	return engine
}

func (e *DirectEngine) Name() string {
	return "direct"
}

// BindSubTask forwards the current sub-task to the underlying runner if it
// implements SubTaskBindable. It lets a caller switch the runner's executor models (e.g. Haiku → Sonnet) per sub-task. Safe no-op when the
// runner does not implement the interface.
func (e *DirectEngine) BindSubTask(subTask hermes.SubTask) {
	if binder, ok := e.runner.(SubTaskBindable); ok {
		binder.BindSubTask(subTask)
	}
}

// SetWalkingEnabled forwards walking-mode toggle to the underlying runner if
// it implements WalkingToggleable. Issue #149.
func (e *DirectEngine) SetWalkingEnabled(enabled bool) {
	if w, ok := e.runner.(WalkingToggleable); ok {
		w.SetWalkingEnabled(enabled)
	}
}

// ForceFreshSession asks the runner to clear the target model session before
// the next Run call. This is used by walking-agent mode when the engine needs a
// cold prompt without disabling walking for the rest of the task.
func (e *DirectEngine) ForceFreshSession() {
	if w, ok := e.runner.(WalkingSessionResettable); ok {
		w.ForceFreshSession()
	}
}

func (e *DirectEngine) Run(ctx context.Context, goal string, cc *ChatContext, prog ProgressSink) (Result, error) {
	start := time.Now()
	if err := ctx.Err(); err != nil {
		return Result{Duration: time.Since(start)}, err
	}

	if prog != nil {
		prog.OnSubTaskStart(1, 1, goal)
	}

	text, err := e.runner.Run(goal, func(update string, silent bool) {
		if prog == nil || update == "" {
			return
		}
		if silent {
			prog.OnToolUse(update, nil)
			return
		}
		prog.OnContent("status", update)
	})

	result := Result{
		Text: text,
	}
	if metrics, ok := e.runner.(DirectRunnerMetrics); ok {
		result.Model, result.InputTokens, result.OutputTokens, result.Cost = metrics.LastCallMetrics()
	}
	if cacheMetrics, ok := e.runner.(DirectRunnerCacheMetrics); ok {
		result.CacheReadInputTokens, result.CacheCreationInputTokens = cacheMetrics.LastCacheMetrics()
	}
	if err != nil {
		result.Duration = time.Since(start)
		return result, err
	}

	if prog != nil {
		prog.OnSubTaskDone(1, text)
		prog.OnComplete(text)
	}
	result.Duration = time.Since(start)
	return result, nil
}
