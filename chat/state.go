// Package chat holds the conversation-level state shared by every way of
// running an agent: the per-chat context and its lifecycle, native session
// policy, the execution lifecycle of a single run, and error recovery policy.
//
// It has no dependency on any orchestrator package.
package chat

import "time"

// StateSnapshot is the shared, lightweight runtime-state shape exposed by
// agent-like components. Components reuse this vocabulary even when their
// internal states differ.
type StateSnapshot struct {
	Agent    string
	State    string
	Since    time.Time
	Terminal bool
	Reason   string
}
