package chat

import (
	"log"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/errorclass"
)

// RecoveryAction is what a caller should do after a failed attempt.
type RecoveryAction string

const (
	RecoveryActionNone     RecoveryAction = "none"
	RecoveryActionRetry    RecoveryAction = "retry"
	RecoveryActionFallback RecoveryAction = "fallback"
	RecoveryActionCancel   RecoveryAction = "cancel"
	RecoveryActionFail     RecoveryAction = "fail"
)

// RecoveryRequest describes a failed attempt of a single agent run.
type RecoveryRequest struct {
	// Mode labels the call site in traces, e.g. "direct_stream".
	Mode string
	// Attempt is the number of retries already made.
	Attempt int
	// MaxAttempts is the retry budget.
	MaxAttempts int
	// ErrorText is the failure message. Empty means there is nothing to recover.
	ErrorText string
	// Fallback names an alternative to switch to when retrying is not possible.
	Fallback string
}

// RecoveryDecision is the outcome of DecideRecovery.
type RecoveryDecision struct {
	Action      RecoveryAction
	RetryAfter  time.Duration
	Retryable   bool
	Terminal    bool
	Reason      string
	NextAttempt int
}

// RecoveryTracePayload is the structured trace record of one recovery decision.
type RecoveryTracePayload struct {
	Mode        string         `json:"mode"`
	Action      RecoveryAction `json:"action"`
	Reason      string         `json:"reason"`
	Attempt     int            `json:"attempt"`
	MaxAttempts int            `json:"max_attempts"`
	NextAttempt int            `json:"next_attempt,omitempty"`
	RetryAfter  time.Duration  `json:"retry_after,omitempty"`
	Retryable   bool           `json:"retryable"`
	Terminal    bool           `json:"terminal"`
	Fallback    string         `json:"fallback,omitempty"`
}

// DecideRecovery maps a failed attempt onto an action: retry transient errors
// with linear backoff while budget remains, otherwise fall back when a
// fallback is available, otherwise fail.
func DecideRecovery(req RecoveryRequest) RecoveryDecision {
	if strings.TrimSpace(req.ErrorText) == "" {
		return RecoveryDecision{Action: RecoveryActionNone, Reason: "no_error"}
	}
	if req.MaxAttempts < 0 {
		req.MaxAttempts = 0
	}
	if isTransientRecoveryError(req.ErrorText) && req.Attempt < req.MaxAttempts {
		nextAttempt := req.Attempt + 1
		return RecoveryDecision{
			Action:      RecoveryActionRetry,
			RetryAfter:  time.Duration(nextAttempt*15) * time.Second,
			Retryable:   true,
			Reason:      "transient_error",
			NextAttempt: nextAttempt,
		}
	}
	if strings.TrimSpace(req.Fallback) != "" {
		return RecoveryDecision{
			Action:   RecoveryActionFallback,
			Terminal: false,
			Reason:   "fallback_available",
		}
	}
	return RecoveryDecision{
		Action:   RecoveryActionFail,
		Terminal: true,
		Reason:   "terminal_error",
	}
}

// RecoveryTrace builds the trace record for a decision.
func RecoveryTrace(req RecoveryRequest, decision RecoveryDecision) RecoveryTracePayload {
	return RecoveryTracePayload{
		Mode:        strings.TrimSpace(req.Mode),
		Action:      decision.Action,
		Reason:      decision.Reason,
		Attempt:     req.Attempt,
		MaxAttempts: req.MaxAttempts,
		NextAttempt: decision.NextAttempt,
		RetryAfter:  decision.RetryAfter,
		Retryable:   decision.Retryable,
		Terminal:    decision.Terminal,
		Fallback:    strings.TrimSpace(req.Fallback),
	}
}

// LogRecoveryDecision writes the decision to the process log.
func LogRecoveryDecision(req RecoveryRequest, decision RecoveryDecision) {
	LogRecoveryTrace(RecoveryTrace(req, decision))
}

// LogRecoveryTrace writes a trace record to the process log.
func LogRecoveryTrace(payload RecoveryTracePayload) {
	log.Printf(
		"[recovery] event=%s mode=%s action=%s reason=%s attempt=%d max_attempts=%d next_attempt=%d retry_after=%s retryable=%t terminal=%t fallback=%q",
		"RecoveryDecision",
		payload.Mode,
		payload.Action,
		payload.Reason,
		payload.Attempt,
		payload.MaxAttempts,
		payload.NextAttempt,
		payload.RetryAfter,
		payload.Retryable,
		payload.Terminal,
		payload.Fallback,
	)
}

// IsRetryableRecoveryError reports whether err is a transient failure that a
// retry may clear (rate limit, overload, network reset, timeout, EOF).
func IsRetryableRecoveryError(err error) bool {
	if err == nil {
		return false
	}
	return isTransientRecoveryError(err.Error())
}

func isTransientRecoveryError(text string) bool {
	return errorclass.ClassifyText(text).IsTransient()
}
