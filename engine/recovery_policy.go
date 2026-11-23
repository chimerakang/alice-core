package engine

import (
	"strings"
	"time"

	"github.com/chimerakang/alice-core/chat"
)

// The error-recovery policy for a single agent run lives in the chat package.
// This file layers the plan-execute modes (task review, sub-task retry, strict
// review, watchdog, planner retry) on top of it.

type (
	RecoveryAction       = chat.RecoveryAction
	RecoveryDecision     = chat.RecoveryDecision
	RecoveryTracePayload = chat.RecoveryTracePayload
)

const (
	RecoveryActionNone     = chat.RecoveryActionNone
	RecoveryActionRetry    = chat.RecoveryActionRetry
	RecoveryActionFallback = chat.RecoveryActionFallback
	RecoveryActionCancel   = chat.RecoveryActionCancel
	RecoveryActionFail     = chat.RecoveryActionFail
)

type RecoveryRequest struct {
	Mode        string
	Attempt     int
	MaxAttempts int
	ErrorText   string
	Fallback    string
	Review      ReviewResult
	Strict      StrictReviewDecision
	TaskRetry   TaskRetryConfig
}

func (req RecoveryRequest) base() chat.RecoveryRequest {
	return chat.RecoveryRequest{
		Mode:        req.Mode,
		Attempt:     req.Attempt,
		MaxAttempts: req.MaxAttempts,
		ErrorText:   req.ErrorText,
		Fallback:    req.Fallback,
	}
}

func DecideRecovery(req RecoveryRequest) RecoveryDecision {
	if strings.TrimSpace(req.ErrorText) == "" {
		switch req.Mode {
		case "task_review":
			return decideTaskReviewRecovery(req)
		case "sub_task_retry":
			return decideSubTaskRetryRecovery(req)
		case "strict_review":
			return decideStrictReviewRecovery(req)
		case "watchdog_cancel":
			return decideWatchdogCancelRecovery(req)
		case "planner_retry":
			return decidePlannerRetryRecovery(req)
		}
	}
	return chat.DecideRecovery(req.base())
}

func RecoveryTraceEvent(req RecoveryRequest, decision RecoveryDecision, at time.Time) Event {
	if at.IsZero() {
		at = time.Now()
	}
	return Event{
		Type:      "RecoveryDecision",
		Timestamp: at,
		Payload:   chat.RecoveryTrace(req.base(), decision),
	}
}

func LogRecoveryDecision(req RecoveryRequest, decision RecoveryDecision) {
	chat.LogRecoveryDecision(req.base(), decision)
}

func decideTaskReviewRecovery(req RecoveryRequest) RecoveryDecision {
	if !req.TaskRetry.Enabled {
		return RecoveryDecision{Action: RecoveryActionNone, Reason: "task_retry_disabled"}
	}
	cfg := req.TaskRetry.WithDefaults()
	if req.Attempt >= cfg.MaxTaskRetries {
		return RecoveryDecision{Action: RecoveryActionNone, Reason: "max_task_retries_reached"}
	}
	if req.Review.Verdict == "" {
		return RecoveryDecision{Action: RecoveryActionNone, Reason: "review_skipped"}
	}
	if req.Review.Verdict == VerdictFail {
		return RecoveryDecision{
			Action:      RecoveryActionRetry,
			Retryable:   true,
			Reason:      "review_failed",
			NextAttempt: req.Attempt + 1,
		}
	}
	if req.Review.OverallScore < cfg.ScoreThreshold {
		return RecoveryDecision{
			Action:      RecoveryActionRetry,
			Retryable:   true,
			Reason:      "review_score_below_threshold",
			NextAttempt: req.Attempt + 1,
		}
	}
	return RecoveryDecision{Action: RecoveryActionNone, Reason: "review_passed"}
}

func decideSubTaskRetryRecovery(req RecoveryRequest) RecoveryDecision {
	if req.MaxAttempts <= 0 {
		return RecoveryDecision{
			Action:   RecoveryActionNone,
			Terminal: true,
			Reason:   "sub_task_retry_disabled",
		}
	}
	if req.Attempt >= req.MaxAttempts {
		return RecoveryDecision{
			Action:   RecoveryActionNone,
			Terminal: true,
			Reason:   "max_sub_task_retries_reached",
		}
	}
	return RecoveryDecision{
		Action:      RecoveryActionRetry,
		Retryable:   true,
		Reason:      "sub_task_retry_allowed",
		NextAttempt: req.Attempt + 1,
	}
}

func decideStrictReviewRecovery(req RecoveryRequest) RecoveryDecision {
	if req.Strict.Verdict != VerdictBlock {
		return RecoveryDecision{Action: RecoveryActionNone, Reason: "strict_review_allowed"}
	}
	if req.Attempt < req.MaxAttempts {
		return RecoveryDecision{
			Action:      RecoveryActionRetry,
			Retryable:   true,
			Reason:      "strict_review_blocked",
			NextAttempt: req.Attempt + 1,
		}
	}
	return RecoveryDecision{
		Action:   RecoveryActionNone,
		Terminal: true,
		Reason:   "max_strict_review_retries_reached",
	}
}

func decideWatchdogCancelRecovery(req RecoveryRequest) RecoveryDecision {
	return RecoveryDecision{
		Action:   RecoveryActionCancel,
		Terminal: true,
		Reason:   "watchdog_context_done",
	}
}

func decidePlannerRetryRecovery(req RecoveryRequest) RecoveryDecision {
	if req.MaxAttempts <= 0 {
		return RecoveryDecision{
			Action:   RecoveryActionFail,
			Terminal: true,
			Reason:   "planner_retry_disabled",
		}
	}
	if req.Attempt < req.MaxAttempts {
		return RecoveryDecision{
			Action:      RecoveryActionRetry,
			Retryable:   true,
			Reason:      "planner_retry",
			NextAttempt: req.Attempt + 1,
		}
	}
	return RecoveryDecision{
		Action:   RecoveryActionFail,
		Terminal: true,
		Reason:   "max_planner_retries_reached",
	}
}

// IsRetryableRecoveryError is chat.IsRetryableRecoveryError.
func IsRetryableRecoveryError(err error) bool { return chat.IsRetryableRecoveryError(err) }
