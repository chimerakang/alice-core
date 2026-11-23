package ghissue

import "testing"

func TestValidIssueTransition_HappyPath(t *testing.T) {
	path := []struct {
		from  IssueState
		event IssueEvent
		to    IssueState
	}{
		{"", IssueEventIssueLoaded, IssueStateDrafted},
		{IssueStateDrafted, IssueEventIssuePlanned, IssueStatePlanned},
		{IssueStatePlanned, IssueEventHermesTaskStarted, IssueStateInProgress},
		{IssueStateInProgress, IssueEventEvidenceAttached, IssueStateEvidenceCollected},
		{IssueStateEvidenceCollected, IssueEventChecklistSyncRequested, IssueStateChecklistUnsynced},
		{IssueStateChecklistUnsynced, IssueEventChecklistSynced, IssueStateChecklistSynced},
		{IssueStateChecklistSynced, IssueEventCloseRequested, IssueStateReadyToClose},
		{IssueStateReadyToClose, IssueEventIssueClosed, IssueStateClosed},
	}
	for _, step := range path {
		if !ValidIssueTransition(step.from, step.event, step.to) {
			t.Errorf("%q --%s--> %q must be allowed", step.from, step.event, step.to)
		}
	}
}

func TestValidIssueTransition_Rejections(t *testing.T) {
	cases := []struct {
		name  string
		from  IssueState
		event IssueEvent
		to    IssueState
	}{
		{"skipping checklist sync", IssueStateEvidenceCollected, IssueEventCloseRequested, IssueStateReadyToClose},
		{"closing straight from in-progress", IssueStateInProgress, IssueEventIssueClosed, IssueStateClosed},
		{"right event, wrong target", IssueStateDrafted, IssueEventIssuePlanned, IssueStateInProgress},
		{"closed is terminal", IssueStateClosed, IssueEventIssuePlanned, IssueStatePlanned},
		{"closed cannot be blocked", IssueStateClosed, IssueEventHumanDecisionRequired, IssueStateBlocked},
		{"blocked only resumes through planning", IssueStateBlocked, IssueEventHermesTaskStarted, IssueStateInProgress},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ValidIssueTransition(tc.from, tc.event, tc.to) {
				t.Fatalf("%q --%s--> %q must be rejected", tc.from, tc.event, tc.to)
			}
		})
	}
}

func TestValidIssueTransition_EveryOpenStateCanBlock(t *testing.T) {
	open := []IssueState{
		IssueStateDrafted, IssueStatePlanned, IssueStateInProgress, IssueStateEvidenceCollected,
		IssueStateChecklistUnsynced, IssueStateChecklistSynced, IssueStateReadyToClose,
	}
	blockers := []IssueEvent{IssueEventHumanDecisionRequired, IssueEventReviewPartialOrFailed, IssueEventSyncFailed}
	for _, s := range open {
		for _, e := range blockers {
			if !ValidIssueTransition(s, e, IssueStateBlocked) {
				t.Errorf("%q --%s--> blocked must be allowed", s, e)
			}
		}
	}
	if !ValidIssueTransition(IssueStateBlocked, IssueEventIssuePlanned, IssueStatePlanned) {
		t.Error("blocked --IssuePlanned--> planned must be allowed")
	}
}

func TestIssueStatePredicates(t *testing.T) {
	if !IssueStateClosed.IsTerminal() || IssueStateReadyToClose.IsTerminal() || IssueStateBlocked.IsTerminal() {
		t.Error("only closed is terminal")
	}
	if !IssueStateBlocked.NeedsHumanDecision() || IssueStateInProgress.NeedsHumanDecision() {
		t.Error("only blocked needs a human decision")
	}
}

func TestIssueStateFromGitHub(t *testing.T) {
	cases := map[string]IssueState{
		"closed":           IssueStateClosed,
		"CLOSED":           IssueStateClosed,
		"  Blocked ":       IssueStateBlocked,
		"in_progress":      IssueStateInProgress,
		"checklist_synced": IssueStateChecklistSynced,
		"ready_to_close":   IssueStateReadyToClose,
		"open":             IssueStateDrafted,
		"OPEN":             IssueStateDrafted,
		"":                 IssueStateDrafted,
		"something else":   IssueStateDrafted,
	}
	for raw, want := range cases {
		if got := IssueStateFromGitHub(raw); got != want {
			t.Errorf("IssueStateFromGitHub(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Arriving at a state via its representative event must itself be a legal
// transition from at least one predecessor.
func TestIssueEventForState_IsReachable(t *testing.T) {
	all := []IssueState{
		IssueStateDrafted, IssueStatePlanned, IssueStateInProgress, IssueStateEvidenceCollected,
		IssueStateChecklistUnsynced, IssueStateChecklistSynced, IssueStateReadyToClose,
		IssueStateClosed, IssueStateBlocked,
	}
	for _, to := range all {
		event := IssueEventForState(to)
		reachable := false
		for _, from := range append([]IssueState{""}, all...) {
			if ValidIssueTransition(from, event, to) {
				reachable = true
				break
			}
		}
		if !reachable {
			t.Errorf("no transition reaches %q via %s", to, event)
		}
	}
}

func TestIssueCloseReadiness_CanAutoClose(t *testing.T) {
	ready := IssueCloseReadiness{
		State:            IssueStateReadyToClose,
		ChecklistSynced:  true,
		ReviewAccepted:   true,
		ValidationPassed: true,
	}
	if !ready.CanAutoClose() {
		t.Fatal("a fully satisfied readiness must allow auto-close")
	}
	breakers := map[string]func(*IssueCloseReadiness){
		"wrong state":         func(r *IssueCloseReadiness) { r.State = IssueStateChecklistSynced },
		"blocking label":      func(r *IssueCloseReadiness) { r.HasBlockingLabel = true },
		"checklist unsynced":  func(r *IssueCloseReadiness) { r.ChecklistSynced = false },
		"review not accepted": func(r *IssueCloseReadiness) { r.ReviewAccepted = false },
		"validation failed":   func(r *IssueCloseReadiness) { r.ValidationPassed = false },
		"unchecked required":  func(r *IssueCloseReadiness) { r.HasUncheckedRequiredItems = true },
	}
	for name, breakIt := range breakers {
		r := ready
		breakIt(&r)
		if r.CanAutoClose() {
			t.Errorf("%s: auto-close must be refused", name)
		}
	}
}

func TestConfig_ShouldComment(t *testing.T) {
	all := &Config{Enabled: true, CommentOnStart: true, CommentOnDone: true, CommentOnFail: true, CommentOnBudget: true}
	for _, event := range []string{"start", "complete", "fail", "budget_exceeded"} {
		if !all.ShouldComment(event) {
			t.Errorf("event %q must comment when enabled", event)
		}
	}
	if all.ShouldComment("unknown") {
		t.Error("unknown events never comment")
	}

	disabled := *all
	disabled.Enabled = false
	if disabled.ShouldComment("start") {
		t.Error("a disabled config never comments")
	}

	onlyDone := &Config{Enabled: true, CommentOnDone: true}
	if onlyDone.ShouldComment("start") || !onlyDone.ShouldComment("complete") {
		t.Error("per-event switches must be honoured")
	}
}
