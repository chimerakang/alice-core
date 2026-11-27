package engine

import (
	"time"

	"github.com/chimerakang/alice-core/chat"
)

// The error-recovery policy for a single agent run lives in the chat package.
// These aliases and wrappers keep the previous engine API working for existing
// callers.

type (
	RecoveryAction       = chat.RecoveryAction
	RecoveryDecision     = chat.RecoveryDecision
	RecoveryRequest      = chat.RecoveryRequest
	RecoveryTracePayload = chat.RecoveryTracePayload
)

const (
	RecoveryActionNone     = chat.RecoveryActionNone
	RecoveryActionRetry    = chat.RecoveryActionRetry
	RecoveryActionFallback = chat.RecoveryActionFallback
	RecoveryActionCancel   = chat.RecoveryActionCancel
	RecoveryActionFail     = chat.RecoveryActionFail
)

// DecideRecovery is chat.DecideRecovery.
func DecideRecovery(req RecoveryRequest) RecoveryDecision { return chat.DecideRecovery(req) }

// RecoveryTraceEvent wraps the trace record of a decision as a runtime event.
func RecoveryTraceEvent(req RecoveryRequest, decision RecoveryDecision, at time.Time) Event {
	if at.IsZero() {
		at = time.Now()
	}
	return Event{
		Type:      "RecoveryDecision",
		Timestamp: at,
		Payload:   chat.RecoveryTrace(req, decision),
	}
}

// LogRecoveryDecision is chat.LogRecoveryDecision.
func LogRecoveryDecision(req RecoveryRequest, decision RecoveryDecision) {
	chat.LogRecoveryDecision(req, decision)
}

// IsRetryableRecoveryError is chat.IsRetryableRecoveryError.
func IsRetryableRecoveryError(err error) bool { return chat.IsRetryableRecoveryError(err) }
