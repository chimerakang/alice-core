package engine

import (
	"context"
	"testing"
	"time"

	"github.com/chimerakang/alice-core/chat"
	legacyerrorclass "github.com/chimerakang/alice-core/engine/errorclass"
	"github.com/chimerakang/alice-core/errorclass"
)

// The engine aliases must be the chat types themselves, so values cross the
// package boundary without conversion.
func TestChatAliasesAreIdentical(t *testing.T) {
	var cc *ChatContext = chat.NewChatContext(1, 2, "/p")
	if cc.ProjectDir != "/p" || NewChatContext(1, 2, "/p").ThreadID != 2 {
		t.Fatal("ChatContext alias must be chat.ChatContext")
	}
	var snap StateSnapshot = cc.State()
	if snap.Agent != "chat" {
		t.Fatalf("snapshot = %+v", snap)
	}
	var _ RuntimeAgent = runtimeAgentStub{cc}

	if ChatStateBusy != chat.ChatStateBusy || BackendCodex != chat.BackendCodex ||
		ExecutionStateStreaming != chat.ExecutionStateStreaming || SessionPolicyFresh != chat.SessionPolicyFresh ||
		RecoveryActionRetry != chat.RecoveryActionRetry {
		t.Fatal("re-exported constants must equal the chat constants")
	}
	var life *ExecutionLifecycle = chat.NewExecutionLifecycle()
	if !ValidExecutionStateTransition(life.Snapshot().State, ExecutionStateStarting) {
		t.Fatal("wrapper must delegate")
	}
	if got := DecideSessionPolicy(SessionPolicyRequest{HasIssueScope: true}); got.Policy != chat.SessionPolicyHybridIssueOnly {
		t.Fatalf("DecideSessionPolicy = %+v", got)
	}
}

type runtimeAgentStub struct{ *ChatContext }

func (runtimeAgentStub) Handle(context.Context, Event) ([]Command, error) {
	return nil, nil
}

func TestErrorclassLegacyPathIsIdentical(t *testing.T) {
	var c legacyerrorclass.Class = errorclass.ClassRateLimit
	if c != legacyerrorclass.ClassRateLimit || !c.IsTransient() {
		t.Fatal("legacy errorclass path must alias the top-level package")
	}
	if legacyerrorclass.ClassifyText("HTTP 429") != errorclass.ClassifyText("HTTP 429") {
		t.Fatal("legacy ClassifyText must delegate")
	}
}

// With an error present every mode uses the shared error policy; without one
// the plan-execute modes keep their own rules.
func TestDecideRecovery_ErrorPathMatchesChat(t *testing.T) {
	for _, mode := range []string{"direct_stream", "task_review", "sub_task_retry", "planner_retry", ""} {
		req := RecoveryRequest{Mode: mode, Attempt: 1, MaxAttempts: 3, ErrorText: "HTTP 429 rate limit", Fallback: "x"}
		want := chat.DecideRecovery(chat.RecoveryRequest{Mode: mode, Attempt: 1, MaxAttempts: 3, ErrorText: req.ErrorText, Fallback: "x"})
		if got := DecideRecovery(req); got != want {
			t.Errorf("mode %q: %+v, want %+v", mode, got, want)
		}
	}
}

func TestDecideRecovery_PlanExecuteModes(t *testing.T) {
	cases := []struct {
		name string
		req  RecoveryRequest
		want RecoveryDecision
	}{
		{"unknown mode without an error", RecoveryRequest{Mode: "direct_stream"},
			RecoveryDecision{Action: RecoveryActionNone, Reason: "no_error"}},
		{"sub-task retry within budget", RecoveryRequest{Mode: "sub_task_retry", Attempt: 1, MaxAttempts: 2},
			RecoveryDecision{Action: RecoveryActionRetry, Retryable: true, Reason: "sub_task_retry_allowed", NextAttempt: 2}},
		{"sub-task retry exhausted", RecoveryRequest{Mode: "sub_task_retry", Attempt: 2, MaxAttempts: 2},
			RecoveryDecision{Action: RecoveryActionNone, Terminal: true, Reason: "max_sub_task_retries_reached"}},
		{"sub-task retry disabled", RecoveryRequest{Mode: "sub_task_retry"},
			RecoveryDecision{Action: RecoveryActionNone, Terminal: true, Reason: "sub_task_retry_disabled"}},
		{"planner retry within budget", RecoveryRequest{Mode: "planner_retry", Attempt: 1, MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionRetry, Retryable: true, Reason: "planner_retry", NextAttempt: 2}},
		{"planner retry exhausted", RecoveryRequest{Mode: "planner_retry", Attempt: 3, MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionFail, Terminal: true, Reason: "max_planner_retries_reached"}},
		{"watchdog always cancels", RecoveryRequest{Mode: "watchdog_cancel"},
			RecoveryDecision{Action: RecoveryActionCancel, Terminal: true, Reason: "watchdog_context_done"}},
		{"task review with retry disabled", RecoveryRequest{Mode: "task_review"},
			RecoveryDecision{Action: RecoveryActionNone, Reason: "task_retry_disabled"}},
		{"strict review that did not block", RecoveryRequest{Mode: "strict_review", MaxAttempts: 2},
			RecoveryDecision{Action: RecoveryActionNone, Reason: "strict_review_allowed"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRecovery(tc.req); got != tc.want {
				t.Fatalf("decision =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestRecoveryTraceEvent(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	req := RecoveryRequest{Mode: "direct_stream", Attempt: 0, MaxAttempts: 1, ErrorText: "HTTP 429"}
	decision := DecideRecovery(req)
	event := RecoveryTraceEvent(req, decision, at)
	if event.Type != "RecoveryDecision" || !event.Timestamp.Equal(at) {
		t.Fatalf("event = %+v", event)
	}
	payload, ok := event.Payload.(RecoveryTracePayload)
	if !ok || payload.Action != RecoveryActionRetry || payload.Mode != "direct_stream" || payload.NextAttempt != 1 {
		t.Fatalf("payload = %#v", event.Payload)
	}
	if RecoveryTraceEvent(req, decision, time.Time{}).Timestamp.IsZero() {
		t.Fatal("a zero time must default to now")
	}
	LogRecoveryDecision(req, decision)
}
