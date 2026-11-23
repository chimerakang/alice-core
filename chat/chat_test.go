package chat

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewChatContext(t *testing.T) {
	c := NewChatContext(100, 7, "/srv/project")
	if c.ChatID != 100 || c.ThreadID != 7 || c.ProjectDir != "/srv/project" {
		t.Fatalf("unexpected context: %+v", c)
	}
	if c.CurrentState != ChatStateIdle || c.Sessions == nil || c.CreatedAt.IsZero() {
		t.Fatalf("context not initialised: %+v", c)
	}
	if c.Name() != "chat" {
		t.Errorf("Name() = %q", c.Name())
	}
}

func TestChatContext_TransitionState(t *testing.T) {
	c := NewChatContext(1, 0, "")
	steps := []ChatState{ChatStateReceiving, ChatStateRouting, ChatStateDispatching, ChatStateBusy,
		ChatStateInterrupting, ChatStateBusy, ChatStateAwaitingInput, ChatStateIdle}
	for _, to := range steps {
		if err := c.TransitionState(to, "step"); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
		if c.CurrentState != to {
			t.Fatalf("CurrentState = %s, want %s", c.CurrentState, to)
		}
	}

	c = NewChatContext(1, 0, "")
	if err := c.TransitionState(ChatStateInterrupting, "nope"); err == nil {
		t.Fatal("idle -> interrupting must be rejected")
	}
	if c.CurrentState != ChatStateIdle {
		t.Fatalf("a rejected transition must not change state, got %s", c.CurrentState)
	}
	if err := c.TransitionState("", "nope"); err == nil {
		t.Fatal("a transition to the empty state must be rejected")
	}
}

func TestChatContext_SameStateTransitionKeepsSinceButUpdatesReason(t *testing.T) {
	c := NewChatContext(1, 0, "")
	if err := c.TransitionState(ChatStateBusy, "first"); err != nil {
		t.Fatal(err)
	}
	since := c.StateSince
	time.Sleep(5 * time.Millisecond)
	if err := c.TransitionState(ChatStateBusy, "second"); err != nil {
		t.Fatal(err)
	}
	if !c.StateSince.Equal(since) {
		t.Error("re-entering the same state must not reset StateSince")
	}
	if snap := c.State(); snap.Reason != "second" || snap.State != "busy" || snap.Agent != "chat" || snap.Terminal {
		t.Errorf("snapshot = %+v", snap)
	}
}

func TestChatContext_NilSafe(t *testing.T) {
	var c *ChatContext
	if snap := c.StateSnapshot(); snap.State != "idle" || !snap.Terminal || snap.Reason != "nil_context" {
		t.Errorf("nil snapshot = %+v", snap)
	}
	if c.Session(BackendClaude) != "" || c.RecentMessagesSnapshot() != nil {
		t.Error("nil context must read as empty")
	}
	c.SetSession(BackendClaude, "s")
	c.ClearSession(BackendClaude)
	c.AddRecentMessage("u", "a")
}

func TestValidChatStateTransition(t *testing.T) {
	cases := []struct {
		from, to ChatState
		want     bool
	}{
		{"", ChatStateBusy, true},
		{ChatStateBusy, ChatStateBusy, true},
		{ChatStateIdle, ChatStateBusy, true},
		{ChatStateIdle, ChatStateInterrupting, false},
		{ChatStateReceiving, ChatStateBusy, false},
		{ChatStateBusy, ChatStateInterrupting, true},
		{ChatStateBusy, ChatStateRouting, false},
		{ChatStateInterrupting, ChatStateAwaitingInput, false},
		{ChatStateAwaitingInput, ChatStateBusy, true},
		{ChatStateIdle, "", false},
		{"bogus", ChatStateIdle, false},
	}
	for _, tc := range cases {
		if got := ValidChatStateTransition(tc.from, tc.to); got != tc.want {
			t.Errorf("ValidChatStateTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestChatContext_Sessions(t *testing.T) {
	c := NewChatContext(1, 0, "")
	c.SetSession(BackendClaude, "claude-1")
	c.SetSession(BackendCodex, "codex-1")
	if c.Session(BackendClaude) != "claude-1" || c.Session(BackendCodex) != "codex-1" {
		t.Fatalf("sessions = %+v", c.Sessions)
	}
	if c.LastBackend != BackendCodex {
		t.Errorf("LastBackend = %v, want the most recently set backend", c.LastBackend)
	}
	c.SetSession(BackendClaude, "")
	if c.Session(BackendClaude) != "claude-1" || c.LastBackend != BackendCodex {
		t.Error("an empty session ID must be ignored")
	}
	c.ClearSession(BackendClaude)
	if c.Session(BackendClaude) != "" || c.Session(BackendCodex) != "codex-1" {
		t.Errorf("ClearSession removed the wrong session: %+v", c.Sessions)
	}

	bare := &ChatContext{}
	bare.SetSession(BackendClaude, "s")
	if bare.Session(BackendClaude) != "s" {
		t.Error("SetSession must initialise a nil map")
	}
}

func TestChatContext_RecentMessages(t *testing.T) {
	c := NewChatContext(1, 0, "")
	for i := 0; i < 7; i++ {
		c.AddRecentMessage("u"+string(rune('0'+i)), "a"+string(rune('0'+i)))
	}
	if len(c.RecentMsgs) != 10 {
		t.Fatalf("kept %d messages, want the last 10", len(c.RecentMsgs))
	}
	if c.RecentMsgs[0].Role != "user" || c.RecentMsgs[0].Content != "u2" || c.RecentMsgs[9].Content != "a6" {
		t.Errorf("window = %+v", c.RecentMsgs)
	}

	snap := c.RecentMessagesSnapshot()
	snap[0].Content = "mutated"
	if c.RecentMsgs[0].Content != "u2" {
		t.Error("RecentMessagesSnapshot must return a copy")
	}
}

func TestChatContext_RecentMessageTruncation(t *testing.T) {
	c := NewChatContext(1, 0, "")
	long := strings.Repeat("頭", 500) + strings.Repeat("中", 500) + strings.Repeat("尾", 500)
	c.AddRecentMessage(long, strings.Repeat("x", 900))

	user := []rune(c.RecentMsgs[0].Content)
	if !strings.HasPrefix(string(user), strings.Repeat("頭", 430)) || !strings.HasSuffix(string(user), strings.Repeat("尾", 430)) {
		t.Error("truncation must keep 430 runes from each end")
	}
	if !strings.Contains(string(user), "[middle omitted for continuity]") || strings.Contains(string(user), "中") {
		t.Error("the middle must be replaced by a marker")
	}
	if got := c.RecentMsgs[1].Content; len([]rune(got)) != 900 {
		t.Errorf("a message at exactly the limit must not be truncated, got %d runes", len([]rune(got)))
	}
}

func TestAssistantAwaitsInput(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"完成了，所有測試通過。", false},
		{"有兩個方案：(A) 重寫 (B) 修補。你想先做哪個？", true},
		{"Option 1 or option 2 — which one do you prefer?", true},
		{"方案已經實作完成。", false},
		{"已修好第一個問題。要我繼續嗎？", true},
		{"Fixed the first bug. Should I continue?", true},
		{"Should I continue? No — I went ahead and finished it.", false},
		{"  Want me to continue?  ", true},
	}
	for _, tc := range cases {
		if got := AssistantAwaitsInput(tc.text); got != tc.want {
			t.Errorf("AssistantAwaitsInput(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestBackendKindForModel(t *testing.T) {
	cases := map[string]BackendKind{
		"claude-sonnet-4-6": BackendClaude,
		"gpt-5":             BackendCodex,
		"GPT-5-Codex":       BackendCodex,
		"codex-mini":        BackendCodex,
		"":                  BackendClaude,
		"unknown":           BackendClaude,
	}
	for model, want := range cases {
		if got := BackendKindForModel(model); got != want {
			t.Errorf("BackendKindForModel(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestDecideSessionRun_ModelPriority(t *testing.T) {
	full := SessionRunRequest{
		OverrideModel: "override", StickyModel: "sticky", ContinuationModel: "cont",
		RoutedModel: "routed", RoutedReason: "triage", RoutedLatencyMS: 42, DefaultModel: "default",
	}
	cases := []struct {
		name       string
		strip      func(*SessionRunRequest)
		wantModel  string
		wantReason string
		wantMS     int
	}{
		{"override wins", func(*SessionRunRequest) {}, "override", "user_command", 0},
		{"then sticky", func(r *SessionRunRequest) { r.OverrideModel = "" }, "sticky", "sticky_session", 0},
		{"then continuation", func(r *SessionRunRequest) { r.OverrideModel, r.StickyModel = "", "" }, "cont", "follow_up", 0},
		{"then routed, with its reason and latency",
			func(r *SessionRunRequest) { r.OverrideModel, r.StickyModel, r.ContinuationModel = "", "", "" },
			"routed", "triage", 42},
		{"routed without a reason reports default",
			func(r *SessionRunRequest) {
				r.OverrideModel, r.StickyModel, r.ContinuationModel, r.RoutedReason = "", "", "", ""
			}, "routed", "default", 42},
		{"finally the default",
			func(r *SessionRunRequest) {
				r.OverrideModel, r.StickyModel, r.ContinuationModel, r.RoutedModel = "", "", "", ""
			}, "default", "default", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := full
			tc.strip(&req)
			d := DecideSessionRun(req)
			if d.Model != tc.wantModel || d.RoutingReason != tc.wantReason || d.LatencyMS != tc.wantMS {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

func TestDecideSessionRun_NativeSessionLookup(t *testing.T) {
	sessions := map[BackendKind]string{BackendClaude: "claude-sess"}

	d := DecideSessionRun(SessionRunRequest{DefaultModel: "claude-sonnet-4-6", BackendSessions: sessions, PreviousBackend: BackendCodex})
	if d.Backend != BackendClaude || d.SessionID != "claude-sess" || !d.HasNativeSession || d.PreviousBackend != BackendCodex {
		t.Fatalf("decision = %+v", d)
	}

	d = DecideSessionRun(SessionRunRequest{OverrideModel: "gpt-5", BackendSessions: sessions})
	if d.Backend != BackendCodex || d.SessionID != "" || d.HasNativeSession {
		t.Fatalf("a backend without a session must not borrow another's: %+v", d)
	}

	d = DecideSessionRun(SessionRunRequest{DefaultModel: "claude-sonnet-4-6"})
	if d.HasNativeSession {
		t.Fatalf("nil session map must mean no native session: %+v", d)
	}
}

func TestDecideSessionPolicy(t *testing.T) {
	cases := []struct {
		name string
		req  SessionPolicyRequest
		want SessionPolicyDecision
	}{
		{"resume fallback trusts only recent messages",
			SessionPolicyRequest{Mode: "direct_resume_fallback", HasNativeSession: true},
			SessionPolicyDecision{Policy: SessionPolicyMemoryOnly, AllowRecentMemory: true, Reason: "native_resume_unavailable"}},
		{"resume fallback inside an issue scope drops recent messages too",
			SessionPolicyRequest{Mode: "direct_resume_fallback", HasIssueScope: true},
			SessionPolicyDecision{Policy: SessionPolicyMemoryOnly, Reason: "native_resume_unavailable"}},
		{"model switch bridges with recent messages",
			SessionPolicyRequest{Mode: "direct_model_switch"},
			SessionPolicyDecision{Policy: SessionPolicyMemoryOnly, AllowRecentMemory: true, Reason: "trusted_recent_bridge"}},
		{"continuation behaves like a model switch",
			SessionPolicyRequest{Mode: "direct_continuation"},
			SessionPolicyDecision{Policy: SessionPolicyMemoryOnly, AllowRecentMemory: true, Reason: "trusted_recent_bridge"}},
		{"issue scope keeps the native session and excludes chat history",
			SessionPolicyRequest{HasIssueScope: true, HasNativeSession: true},
			SessionPolicyDecision{Policy: SessionPolicyHybridIssueOnly, UseNativeSession: true,
				AllowGeneralMemory: true, AllowStaticHints: true, Reason: "explicit_issue_scope"}},
		{"issue scope without a native session",
			SessionPolicyRequest{HasIssueScope: true},
			SessionPolicyDecision{Policy: SessionPolicyHybridIssueOnly,
				AllowGeneralMemory: true, AllowStaticHints: true, Reason: "explicit_issue_scope"}},
		{"default bundles every memory source",
			SessionPolicyRequest{Mode: "anything", HasNativeSession: true},
			SessionPolicyDecision{Policy: SessionPolicyMemoryOnly, AllowRecentMemory: true,
				AllowGeneralMemory: true, AllowStaticHints: true, Reason: "memory_bundle"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideSessionPolicy(tc.req); got != tc.want {
				t.Fatalf("decision =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestExecutionLifecycle(t *testing.T) {
	l := NewExecutionLifecycle()
	if snap := l.Snapshot(); snap.State != ExecutionStateIdle || snap.Terminal || l.IsProcessing() {
		t.Fatalf("new lifecycle = %+v", snap)
	}

	for _, to := range []ExecutionState{ExecutionStateStarting, ExecutionStateStreaming, ExecutionStateRetrying, ExecutionStateStreaming} {
		if err := l.Transition(to, "run"); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
		if !l.IsProcessing() {
			t.Fatalf("%s must count as processing", to)
		}
	}
	if err := l.Transition(ExecutionStateSucceeded, "done"); err != nil {
		t.Fatal(err)
	}
	if snap := l.Snapshot(); !snap.Terminal || snap.Reason != "done" || l.IsProcessing() {
		t.Fatalf("finished lifecycle = %+v", snap)
	}
	if err := l.Transition(ExecutionStateStarting, "next run"); err != nil {
		t.Fatalf("a finished lifecycle must be reusable: %v", err)
	}

	if err := l.Transition(ExecutionStateSucceeded, "skip"); err == nil {
		t.Fatal("starting -> succeeded must be rejected")
	}
	if l.Snapshot().State != ExecutionStateStarting {
		t.Fatal("a rejected transition must not change state")
	}
}

func TestExecutionLifecycle_CancelPath(t *testing.T) {
	l := NewExecutionLifecycle()
	for _, to := range []ExecutionState{ExecutionStateStarting, ExecutionStateCancelling, ExecutionStateCancelled} {
		if err := l.Transition(to, "abort"); err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	if !l.Snapshot().Terminal {
		t.Fatal("cancelled must be terminal")
	}
	l2 := NewExecutionLifecycle()
	_ = l2.Transition(ExecutionStateStarting, "")
	_ = l2.Transition(ExecutionStateCancelling, "")
	if err := l2.Transition(ExecutionStateStreaming, ""); err == nil {
		t.Fatal("cancelling -> streaming must be rejected")
	}
}

func TestExecutionLifecycle_NilSafe(t *testing.T) {
	var l *ExecutionLifecycle
	if err := l.Transition(ExecutionStateStarting, ""); err != nil {
		t.Fatalf("nil lifecycle must ignore transitions: %v", err)
	}
	if snap := l.Snapshot(); snap.State != ExecutionStateIdle || !snap.Terminal || l.IsProcessing() {
		t.Fatalf("nil snapshot = %+v", snap)
	}
}

func TestExecutionLifecycle_ConcurrentUse(t *testing.T) {
	l := NewExecutionLifecycle()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = l.Transition(ExecutionStateStarting, "a")
			_ = l.Transition(ExecutionStateFailed, "b")
		}
	}()
	for i := 0; i < 200; i++ {
		_ = l.Snapshot()
		_ = l.IsProcessing()
	}
	<-done
}

func TestDecideRecovery(t *testing.T) {
	cases := []struct {
		name string
		req  RecoveryRequest
		want RecoveryDecision
	}{
		{"no error", RecoveryRequest{Mode: "direct_stream", MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionNone, Reason: "no_error"}},
		{"whitespace-only error is no error", RecoveryRequest{ErrorText: "  \n", MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionNone, Reason: "no_error"}},
		{"transient error with budget retries with linear backoff",
			RecoveryRequest{ErrorText: "HTTP 429 rate limit exceeded", Attempt: 1, MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionRetry, RetryAfter: 30 * time.Second, Retryable: true,
				Reason: "transient_error", NextAttempt: 2}},
		{"first retry waits 15s",
			RecoveryRequest{ErrorText: "API overloaded", Attempt: 0, MaxAttempts: 1},
			RecoveryDecision{Action: RecoveryActionRetry, RetryAfter: 15 * time.Second, Retryable: true,
				Reason: "transient_error", NextAttempt: 1}},
		{"transient error out of budget fails",
			RecoveryRequest{ErrorText: "connection reset by peer", Attempt: 3, MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionFail, Terminal: true, Reason: "terminal_error"}},
		{"transient error out of budget falls back when possible",
			RecoveryRequest{ErrorText: "connection reset by peer", Attempt: 3, MaxAttempts: 3, Fallback: "gpt-5"},
			RecoveryDecision{Action: RecoveryActionFallback, Reason: "fallback_available"}},
		{"non-transient error fails without retrying",
			RecoveryRequest{ErrorText: "syntax error in main.go", MaxAttempts: 3},
			RecoveryDecision{Action: RecoveryActionFail, Terminal: true, Reason: "terminal_error"}},
		{"negative budget is treated as zero",
			RecoveryRequest{ErrorText: "HTTP 429 rate limit exceeded", MaxAttempts: -1},
			RecoveryDecision{Action: RecoveryActionFail, Terminal: true, Reason: "terminal_error"}},
		{"blank fallback is not a fallback",
			RecoveryRequest{ErrorText: "boom", Fallback: "   "},
			RecoveryDecision{Action: RecoveryActionFail, Terminal: true, Reason: "terminal_error"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DecideRecovery(tc.req); got != tc.want {
				t.Fatalf("decision =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

func TestRecoveryTrace(t *testing.T) {
	req := RecoveryRequest{Mode: " direct_stream ", Attempt: 1, MaxAttempts: 3, ErrorText: "429", Fallback: " gpt-5 "}
	decision := DecideRecovery(req)
	got := RecoveryTrace(req, decision)
	want := RecoveryTracePayload{
		Mode: "direct_stream", Action: RecoveryActionRetry, Reason: "transient_error",
		Attempt: 1, MaxAttempts: 3, NextAttempt: 2, RetryAfter: 30 * time.Second, Retryable: true, Fallback: "gpt-5",
	}
	if got != want {
		t.Fatalf("trace =\n  %+v\nwant\n  %+v", got, want)
	}
	LogRecoveryDecision(req, decision) // must not panic
}

func TestIsRetryableRecoveryError(t *testing.T) {
	if IsRetryableRecoveryError(nil) {
		t.Error("nil is not retryable")
	}
	if !IsRetryableRecoveryError(errors.New("upstream connect error: 503 Service Unavailable")) {
		t.Error("a 503 must be retryable")
	}
	if IsRetryableRecoveryError(errors.New("prompt is too long for the context window")) {
		t.Error("context overflow is not retryable: the same prompt fails again")
	}
}
