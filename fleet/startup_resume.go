package fleet

import (
	"context"
	"encoding/json"
)

const (
	runnerStoppedPrefix           = "runner stopped: "
	verificationInterruptedPrefix = "verification interrupted: "
	startupLostAgent              = "agent process is gone: it died or the host restarted while it was running"
	startupLostVerification       = "verification was cut off by a host restart"
	startupLeftoverAgent          = "agent was left running without supervision by a host restart and has been stopped"

	EventStartupResumed      = "startup.resumed"
	EventStartupResumeCapped = "startup.resume_capped"
)

type startupResume struct {
	task          Task
	attempt       int
	statusEventID int64
}

func stoppedDuringServiceShutdown(reason string) bool {
	switch reason {
	case runnerStoppedPrefix + "context canceled", verificationInterruptedPrefix + "context canceled", startupLostAgent, startupLostVerification, startupLeftoverAgent:
		return true
	}
	return false
}

func (s *Scheduler) automaticResumePaused(ctx context.Context, taskID string) bool {
	return s != nil && s.PauseAutomaticResume != nil && s.PauseAutomaticResume(ctx, taskID)
}

func (s *Scheduler) findStartupResumes(ctx context.Context) (map[string]startupResume, error) {
	tasks, err := s.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusNeedsDecision}})
	if err != nil {
		return nil, err
	}
	resumes := make(map[string]startupResume)
	for _, task := range tasks {
		if task.SessionID == "" || !stoppedDuringServiceShutdown(task.StatusReason) || s.automaticResumePaused(ctx, task.ID) {
			continue
		}

		// The Codex Scout runner starts a new one-shot process; it cannot
		// continue the recorded session after a restart.
		if task.Shape == ShapeScout && s.Runner.CodexScoutBinary != "" {
			continue
		}

		stopped, ok, err := s.Store.LastEvent(ctx, task.ID, EventStatus)
		if err != nil {
			return nil, err
		}
		var change StatusChange
		if !ok || json.Unmarshal(stopped.Payload, &change) != nil || change.Requested ||
			change.To != StatusNeedsDecision || change.Reason != task.StatusReason {
			continue
		}

		var attempts int
		if err := s.Store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM fleet_task_events WHERE task_id = ? AND type = ?`,
			task.ID, EventStartupResumed).Scan(&attempts); err != nil {
			return nil, err
		}
		if attempts < s.StartupResumeLimit {
			resumes[task.ID] = startupResume{task: task, attempt: attempts + 1, statusEventID: stopped.ID}
			continue
		}
		capped, ok, err := s.Store.LastEvent(ctx, task.ID, EventStartupResumeCapped)
		if err != nil {
			return nil, err
		}
		if !ok || capped.ID < stopped.ID {
			if _, err := s.Store.AppendEvent(ctx, task.ID, EventStartupResumeCapped,
				map[string]any{"attempt": attempts, "limit": s.StartupResumeLimit}); err != nil {
				return nil, err
			}
		}
	}
	return resumes, nil
}

func (s *Store) transitionStartupResume(ctx context.Context, taskID string, resume *startupResume) (Task, error) {
	return s.transitionWithStartupResume(ctx, taskID, StatusRunning, "resumed after service restart", false, resume)
}

func startupResumeAudit(task Task, err error) AuditEntry {
	entry := AuditEntry{Actor: "system", Via: "startup", Command: "startup_resume", ProjectID: task.ProjectID, TaskID: task.ID, OK: err == nil}
	if err != nil {
		entry.Error = err.Error()
	}
	return entry
}
