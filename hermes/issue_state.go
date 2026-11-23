package hermes

import "github.com/chimerakang/alice-core/ghissue"

// The issue lifecycle FSM lives in the ghissue package. These aliases keep the
// previous hermes API working for existing callers.

type (
	IssueState          = ghissue.IssueState
	IssueEvent          = ghissue.IssueEvent
	IssueTransition     = ghissue.IssueTransition
	IssueCloseReadiness = ghissue.IssueCloseReadiness
)

const (
	IssueStateDrafted           = ghissue.IssueStateDrafted
	IssueStatePlanned           = ghissue.IssueStatePlanned
	IssueStateInProgress        = ghissue.IssueStateInProgress
	IssueStateEvidenceCollected = ghissue.IssueStateEvidenceCollected
	IssueStateChecklistUnsynced = ghissue.IssueStateChecklistUnsynced
	IssueStateChecklistSynced   = ghissue.IssueStateChecklistSynced
	IssueStateReadyToClose      = ghissue.IssueStateReadyToClose
	IssueStateClosed            = ghissue.IssueStateClosed
	IssueStateBlocked           = ghissue.IssueStateBlocked
)

const (
	IssueEventIssueLoaded               = ghissue.IssueEventIssueLoaded
	IssueEventIssuePlanned              = ghissue.IssueEventIssuePlanned
	IssueEventHermesTaskStarted         = ghissue.IssueEventHermesTaskStarted
	IssueEventSubTaskCompleted          = ghissue.IssueEventSubTaskCompleted
	IssueEventReviewPassed              = ghissue.IssueEventReviewPassed
	IssueEventReviewPartialOrFailed     = ghissue.IssueEventReviewPartialOrFailed
	IssueEventEvidenceAttached          = ghissue.IssueEventEvidenceAttached
	IssueEventChecklistMismatchDetected = ghissue.IssueEventChecklistMismatchDetected
	IssueEventChecklistSyncRequested    = ghissue.IssueEventChecklistSyncRequested
	IssueEventChecklistSynced           = ghissue.IssueEventChecklistSynced
	IssueEventCloseRequested            = ghissue.IssueEventCloseRequested
	IssueEventIssueClosed               = ghissue.IssueEventIssueClosed
	IssueEventHumanDecisionRequired     = ghissue.IssueEventHumanDecisionRequired
	IssueEventSyncFailed                = ghissue.IssueEventSyncFailed
)

// ValidIssueTransition is ghissue.ValidIssueTransition.
func ValidIssueTransition(from IssueState, event IssueEvent, to IssueState) bool {
	return ghissue.ValidIssueTransition(from, event, to)
}

// IssueStateFromGitHub is ghissue.IssueStateFromGitHub.
func IssueStateFromGitHub(raw string) IssueState { return ghissue.IssueStateFromGitHub(raw) }

// IssueEventForState is ghissue.IssueEventForState.
func IssueEventForState(state IssueState) IssueEvent { return ghissue.IssueEventForState(state) }
