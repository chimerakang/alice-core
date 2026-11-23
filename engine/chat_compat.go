package engine

import (
	"github.com/chimerakang/alice-core/chat"
)

// The chat context, session policy, and execution lifecycle live in the chat
// package. These aliases and wrappers keep the previous engine API working
// for existing callers.

type (
	BackendKind           = chat.BackendKind
	ChatContext           = chat.ChatContext
	ChatState             = chat.ChatState
	ContextMessage        = chat.ContextMessage
	ExecutionLifecycle    = chat.ExecutionLifecycle
	ExecutionSnapshot     = chat.ExecutionSnapshot
	ExecutionState        = chat.ExecutionState
	ModelPreference       = chat.ModelPreference
	SessionPolicy         = chat.SessionPolicy
	SessionPolicyDecision = chat.SessionPolicyDecision
	SessionPolicyRequest  = chat.SessionPolicyRequest
	SessionRunDecision    = chat.SessionRunDecision
	SessionRunRequest     = chat.SessionRunRequest
	StateSnapshot         = chat.StateSnapshot
)

const (
	BackendClaude                = chat.BackendClaude
	BackendCodex                 = chat.BackendCodex
	BackendAuto                  = chat.BackendAuto
	ChatStateIdle                = chat.ChatStateIdle
	ChatStateReceiving           = chat.ChatStateReceiving
	ChatStateRouting             = chat.ChatStateRouting
	ChatStateDispatching         = chat.ChatStateDispatching
	ChatStateBusy                = chat.ChatStateBusy
	ChatStateAwaitingInput       = chat.ChatStateAwaitingInput
	ChatStateInterrupting        = chat.ChatStateInterrupting
	SessionPolicyNativeOnly      = chat.SessionPolicyNativeOnly
	SessionPolicyMemoryOnly      = chat.SessionPolicyMemoryOnly
	SessionPolicyHybridIssueOnly = chat.SessionPolicyHybridIssueOnly
	SessionPolicyFresh           = chat.SessionPolicyFresh
	ExecutionStateIdle           = chat.ExecutionStateIdle
	ExecutionStateStarting       = chat.ExecutionStateStarting
	ExecutionStateStreaming      = chat.ExecutionStateStreaming
	ExecutionStateRetrying       = chat.ExecutionStateRetrying
	ExecutionStateCancelling     = chat.ExecutionStateCancelling
	ExecutionStateSucceeded      = chat.ExecutionStateSucceeded
	ExecutionStateFailed         = chat.ExecutionStateFailed
	ExecutionStateCancelled      = chat.ExecutionStateCancelled
)

// NewChatContext is chat.NewChatContext.
func NewChatContext(chatID int64, threadID int, projectDir string) *ChatContext {
	return chat.NewChatContext(chatID, threadID, projectDir)
}

// ValidChatStateTransition is chat.ValidChatStateTransition.
func ValidChatStateTransition(from, to ChatState) bool {
	return chat.ValidChatStateTransition(from, to)
}

// AssistantAwaitsInput is chat.AssistantAwaitsInput.
func AssistantAwaitsInput(text string) bool {
	return chat.AssistantAwaitsInput(text)
}

// BackendKindForModel is chat.BackendKindForModel.
func BackendKindForModel(model string) BackendKind {
	return chat.BackendKindForModel(model)
}

// DecideSessionRun is chat.DecideSessionRun.
func DecideSessionRun(req SessionRunRequest) SessionRunDecision {
	return chat.DecideSessionRun(req)
}

// DecideSessionPolicy is chat.DecideSessionPolicy.
func DecideSessionPolicy(req SessionPolicyRequest) SessionPolicyDecision {
	return chat.DecideSessionPolicy(req)
}

// NewExecutionLifecycle is chat.NewExecutionLifecycle.
func NewExecutionLifecycle() *ExecutionLifecycle {
	return chat.NewExecutionLifecycle()
}

// ValidExecutionStateTransition is chat.ValidExecutionStateTransition.
func ValidExecutionStateTransition(from, to ExecutionState) bool {
	return chat.ValidExecutionStateTransition(from, to)
}

// IsTerminalExecutionState is chat.IsTerminalExecutionState.
func IsTerminalExecutionState(state ExecutionState) bool {
	return chat.IsTerminalExecutionState(state)
}
