package issueops

import (
	"context"
	"fmt"
	"strings"

	"github.com/chimerakang/alice-core/ghissue"
)

// IssueOps describes the high-level GitHub issue workflow surface.
type IssueOps interface {
	LoadIssue(ctx context.Context, projectDir string, issueNumber int) (*ghissue.IssueContext, error)
	LoadIssueChecklistMapping(ctx context.Context, projectDir string, issueNumber int, subtasks []WorkItem) (ChecklistMappingResult, error)
	CommentDone(ctx context.Context, projectDir string, issueNumber int, summary ghissue.CompletionSummary, notes string) error
	CommentBudgetExceeded(ctx context.Context, projectDir string, issueNumber int, used, max int) error
	ApplyLabel(ctx context.Context, projectDir string, issueNumber int, label string) error
	PlanIssue(ctx context.Context, req PlanIssueRequest) error
	RecordEvidence(ctx context.Context, req RecordEvidenceRequest) error
	SyncChecklist(ctx context.Context, req SyncChecklistRequest) (SyncChecklistResult, error)
	AssessCloseReadiness(ctx context.Context, req AssessCloseReadinessRequest) (CloseReadinessResult, error)
	CloseIssue(ctx context.Context, req CloseIssueRequest) (CloseReadinessResult, error)
}

// Service is the default IssueOps implementation backed by the ghissue package.
type Service struct{}

// New returns a stateless IssueOps service.
func New() *Service {
	return &Service{}
}

// EvidenceKind classifies a piece of close-readiness evidence.
type EvidenceKind string

const (
	EvidenceKindSubTask    EvidenceKind = "subtask"
	EvidenceKindReview     EvidenceKind = "review"
	EvidenceKindValidation EvidenceKind = "validation"
)

// EvidenceRecord captures traceable evidence for an issue operation.
type EvidenceRecord struct {
	Kind      EvidenceKind
	Summary   string
	Detail    string
	Reference string
}

// ReviewEvidence captures review output that should be traceable in issue evidence.
type ReviewEvidence struct {
	Verdict   string
	Score     int
	Summary   string
	Detail    string
	Reference string
	SubTaskID string
}

// ValidationEvidence captures a validation command/result pair that should be traceable in issue evidence.
type ValidationEvidence struct {
	Command   string
	Passed    bool
	ExitCode  int
	Output    string
	Reference string
	SubTaskID string
}

// PlanIssueRequest describes the inputs needed to seed or update issue plan state.
type PlanIssueRequest struct {
	ProjectDir    string
	IssueNumber   int
	PlannerModel  string
	ExecutorModel string
	Goal          string
	Tasks         []WorkItem
	CommentStart  bool
	WritePlan     bool
}

// RecordEvidenceRequest describes one issue evidence write.
type RecordEvidenceRequest struct {
	ProjectDir       string
	IssueNumber      int
	Index            int
	Total            int
	SubTask          WorkItem
	Result           string
	Tokens           int
	Completed        int
	Evidence         []EvidenceRecord
	ChecklistMapping *ChecklistMapping
	Review           *ReviewEvidence
	Validation       *ValidationEvidence
	Comment          bool
}

// SyncChecklistRequest describes a checklist synchronization request.
type SyncChecklistRequest struct {
	ProjectDir           string
	IssueNumber          int
	SubTasks             []WorkItem
	DryRun               bool
	ChecklistMapping     *ChecklistMappingResult
	RequireHumanDecision bool
}

// ChecklistSyncGuard records the guard state used before mutating the issue body.
type ChecklistSyncGuard struct {
	IssueState             ghissue.IssueState
	HasBlockingLabel       bool
	HasCompletedItems      bool
	HasBodyChange          bool
	NeedsHumanConfirmation bool
}

// CanWrite reports whether the issue body may be updated safely.
func (g ChecklistSyncGuard) CanWrite() bool {
	return g.IssueState != ghissue.IssueStateClosed &&
		g.IssueState != ghissue.IssueStateBlocked &&
		!g.HasBlockingLabel &&
		g.HasCompletedItems &&
		g.HasBodyChange &&
		!g.NeedsHumanConfirmation
}

// SyncOutcome classifies the terminal outcome of a checklist sync attempt so
// callers can emit consistent FSM events without re-deriving the reason.
type SyncOutcome string

const (
	SyncOutcomeUnknown    SyncOutcome = ""
	SyncOutcomeWrote      SyncOutcome = "wrote"
	SyncOutcomeDryRun     SyncOutcome = "dry_run"
	SyncOutcomeNoMatch    SyncOutcome = "no_match"
	SyncOutcomeNoChange   SyncOutcome = "no_change"
	SyncOutcomeNeedsHuman SyncOutcome = "needs_human"
	SyncOutcomeIssueState SyncOutcome = "issue_state"
	SyncOutcomeFailed     SyncOutcome = "failed"
	SyncOutcomeLoadFailed SyncOutcome = "load_failed"
)

// SyncChecklistResult describes the checklist synchronization preview and outcome.
type SyncChecklistResult struct {
	Issue      *ghissue.IssueContext
	State      ghissue.IssueState
	DryRun     bool
	Guard      ChecklistSyncGuard
	Preview    ghissue.ChecklistSyncPreview
	WouldWrite bool
	Wrote      bool
	Outcome    SyncOutcome
	Reason     string
	Notes      []string
	Recovery   *IssueOperationRecovery
}

// AssessCloseReadinessRequest describes the data needed to evaluate issue close readiness.
type AssessCloseReadinessRequest struct {
	ProjectDir         string
	IssueNumber        int
	RequiredCloseLabel string
	ReviewAccepted     bool
	ValidationPassed   bool
}

// CloseIssueRequest extends readiness assessment with the actual close intent.
type CloseIssueRequest struct {
	AssessCloseReadinessRequest
	DryRun bool
}

// CloseReadinessResult summarizes the current lifecycle and close guard state.
type CloseReadinessResult struct {
	Issue              *ghissue.IssueContext
	Reconciliation     ghissue.IssueReconciliation
	State              ghissue.IssueState
	Guard              ghissue.IssueCloseReadiness
	RequiredCloseLabel string
	HasRequiredLabel   bool
	Notes              []string
	CanAutoClose       bool
	Closed             bool
	Recovery           *IssueOperationRecovery
}

// IssueOperationRecovery captures the retry metadata for a blocked GitHub
// issue operation.
type IssueOperationRecovery struct {
	State       ghissue.IssueState `json:"state"`
	Operation   string             `json:"operation"`
	RetryAction string             `json:"retry_action"`
	Retryable   bool               `json:"retryable"`
	Message     string             `json:"message,omitempty"`
	Error       string             `json:"error,omitempty"`
}

func blockedRecovery(operation, retryAction string, err error) *IssueOperationRecovery {
	if err == nil {
		return nil
	}
	return &IssueOperationRecovery{
		State:       ghissue.IssueStateBlocked,
		Operation:   strings.TrimSpace(operation),
		RetryAction: strings.TrimSpace(retryAction),
		Retryable:   true,
		Message:     fmt.Sprintf("%s failed; issue moved to blocked", strings.TrimSpace(operation)),
		Error:       err.Error(),
	}
}

// LoadIssue fetches the latest GitHub issue snapshot.
func (s *Service) LoadIssue(ctx context.Context, projectDir string, issueNumber int) (*ghissue.IssueContext, error) {
	return ghissue.FetchIssue(ctx, projectDir, issueNumber)
}

// LoadIssueChecklistMapping fetches the issue and builds a sub-task to checklist mapping.
func (s *Service) LoadIssueChecklistMapping(ctx context.Context, projectDir string, issueNumber int, subtasks []WorkItem) (ChecklistMappingResult, error) {
	issue, err := s.LoadIssue(ctx, projectDir, issueNumber)
	if err != nil {
		return ChecklistMappingResult{}, err
	}
	return s.BuildChecklistMapping(issue, subtasks), nil
}

// BuildChecklistMapping builds a deterministic mapping from Hermes subtasks to GitHub checklist items.
func (s *Service) BuildChecklistMapping(issue *ghissue.IssueContext, subtasks []WorkItem) ChecklistMappingResult {
	return buildChecklistMapping(issue, subtasks)
}

// CommentDone posts the terminal Hermes completion comment.
func (s *Service) CommentDone(ctx context.Context, projectDir string, issueNumber int, summary ghissue.CompletionSummary, notes string) error {
	body := ghissue.CommentDone(summary, notes)
	return ghissue.PostComment(ctx, projectDir, issueNumber, body)
}

// CommentBudgetExceeded posts the budget exceeded comment.
func (s *Service) CommentBudgetExceeded(ctx context.Context, projectDir string, issueNumber int, used, max int) error {
	body := ghissue.CommentBudgetExceeded(used, max)
	return ghissue.PostComment(ctx, projectDir, issueNumber, body)
}

// ApplyLabel attaches a label to the issue.
func (s *Service) ApplyLabel(ctx context.Context, projectDir string, issueNumber int, label string) error {
	return ghissue.ApplyLabel(ctx, projectDir, issueNumber, label)
}

// PlanIssue writes the plan scaffolding into GitHub issue state.
func (s *Service) PlanIssue(ctx context.Context, req PlanIssueRequest) error {
	if req.IssueNumber <= 0 {
		return nil
	}
	if req.CommentStart {
		body := ghissue.CommentStarted(req.PlannerModel, req.ExecutorModel)
		if err := ghissue.PostComment(ctx, req.ProjectDir, req.IssueNumber, body); err != nil {
			return err
		}
	}
	if req.WritePlan {
		if err := ghissue.WritePlan(ctx, req.ProjectDir, req.IssueNumber, planEntries(req.Tasks)); err != nil {
			return err
		}
	}
	return nil
}

// RecordEvidence posts traceable issue evidence.
func (s *Service) RecordEvidence(ctx context.Context, req RecordEvidenceRequest) error {
	if req.IssueNumber <= 0 || !req.Comment {
		return nil
	}
	body := buildEvidenceComment(req)
	if strings.TrimSpace(body) == "" {
		return nil
	}
	return ghissue.PostComment(ctx, req.ProjectDir, req.IssueNumber, body)
}

// SyncChecklist syncs completed Hermes sub-tasks back into the issue body.
func (s *Service) SyncChecklist(ctx context.Context, req SyncChecklistRequest) (SyncChecklistResult, error) {
	if req.IssueNumber <= 0 {
		return SyncChecklistResult{DryRun: req.DryRun}, nil
	}
	issue, err := s.LoadIssue(ctx, req.ProjectDir, req.IssueNumber)
	if err != nil {
		return SyncChecklistResult{
			DryRun:   req.DryRun,
			State:    ghissue.IssueStateBlocked,
			Outcome:  SyncOutcomeLoadFailed,
			Reason:   fmt.Sprintf("failed to load issue: %v", err),
			Recovery: blockedRecovery("sync-checklist", "retry-sync-checklist", err),
			Notes:    []string{fmt.Sprintf("failed to load issue: %v", err)},
		}, err
	}

	preview := ghissue.BuildChecklistSyncPreview(issue.Body, completedWork(req.SubTasks))
	guard := ChecklistSyncGuard{
		IssueState:        ghissue.IssueStateFromGitHub(issue.State),
		HasBlockingLabel:  ghissue.HasLabel(issue, "blocked"),
		HasCompletedItems: len(preview.CompletedDescriptions) > 0,
		HasBodyChange:     preview.Changed,
	}
	// RequireHumanDecision must hold even without a mapping: callers set it
	// precisely when the mapping could not be loaded.
	guard.NeedsHumanConfirmation = req.RequireHumanDecision ||
		(req.ChecklistMapping != nil && req.ChecklistMapping.NeedsHumanConfirmation)

	result := SyncChecklistResult{
		Issue:      issue,
		State:      guard.IssueState,
		DryRun:     req.DryRun,
		Guard:      guard,
		Preview:    preview,
		WouldWrite: guard.CanWrite(),
		Notes:      make([]string, 0, 4),
	}
	if len(preview.CompletedDescriptions) == 0 {
		result.Outcome = SyncOutcomeNoMatch
		result.Reason = "no completed checklist items match sub-tasks"
		result.Notes = append(result.Notes, "no completed checklist items to sync")
		return result, nil
	}
	if len(preview.UpdatedChecklistItems) > 0 {
		result.Notes = append(result.Notes, fmt.Sprintf("will update checklist items: %s", strings.Join(preview.UpdatedChecklistItems, ", ")))
	}
	if result.Guard.NeedsHumanConfirmation {
		result.Notes = append(result.Notes, "checklist mapping requires human confirmation")
	}
	if !result.Guard.CanWrite() {
		switch {
		case result.Guard.NeedsHumanConfirmation:
			result.State = ghissue.IssueStateBlocked
			result.Outcome = SyncOutcomeNeedsHuman
			result.Reason = "checklist mapping requires human confirmation"
		case result.Guard.IssueState == ghissue.IssueStateClosed:
			result.Outcome = SyncOutcomeIssueState
			result.Reason = "issue is closed"
			result.Notes = append(result.Notes, "issue is closed")
		case result.Guard.IssueState == ghissue.IssueStateBlocked || result.Guard.HasBlockingLabel:
			result.State = ghissue.IssueStateBlocked
			result.Outcome = SyncOutcomeIssueState
			result.Reason = "issue is blocked"
			result.Notes = append(result.Notes, "issue is blocked")
		case !result.Guard.HasBodyChange:
			result.Outcome = SyncOutcomeNoChange
			result.Reason = "checklist body already matches completed sub-tasks"
			result.Notes = append(result.Notes, "checklist body already matches completed sub-tasks")
		}
		return result, nil
	}
	if req.DryRun {
		result.State = ghissue.IssueStateChecklistSynced
		result.Outcome = SyncOutcomeDryRun
		result.Reason = "dry-run: no GitHub write performed"
		result.Notes = append(result.Notes, "dry-run: no GitHub write performed")
		return result, nil
	}
	if err := ghissue.UpdateIssueBody(ctx, req.ProjectDir, req.IssueNumber, preview.BodyAfter); err != nil {
		result.State = ghissue.IssueStateBlocked
		result.Outcome = SyncOutcomeFailed
		result.Reason = fmt.Sprintf("GitHub checklist sync failed: %v", err)
		result.Recovery = blockedRecovery("sync-checklist", "retry-sync-checklist", err)
		result.Notes = append(result.Notes, fmt.Sprintf("GitHub checklist sync failed: %v", err))
		return result, err
	}
	result.State = ghissue.IssueStateChecklistSynced
	result.Outcome = SyncOutcomeWrote
	result.Wrote = true
	return result, nil
}

// AssessCloseReadiness reads the issue, maps lifecycle state, and computes auto-close readiness.
func (s *Service) AssessCloseReadiness(ctx context.Context, req AssessCloseReadinessRequest) (CloseReadinessResult, error) {
	issue, err := s.LoadIssue(ctx, req.ProjectDir, req.IssueNumber)
	if err != nil {
		return CloseReadinessResult{
			State:    ghissue.IssueStateBlocked,
			Recovery: blockedRecovery("close", "retry-close", err),
			Notes:    []string{fmt.Sprintf("failed to load issue: %v", err)},
		}, err
	}
	return buildCloseReadiness(issue, req), nil
}

// CloseIssue assesses readiness and closes the issue only when guards pass.
func (s *Service) CloseIssue(ctx context.Context, req CloseIssueRequest) (CloseReadinessResult, error) {
	readiness, err := s.AssessCloseReadiness(ctx, req.AssessCloseReadinessRequest)
	if err != nil {
		return readiness, err
	}
	if req.DryRun || !readiness.CanAutoClose {
		return readiness, nil
	}
	if err := ghissue.CloseIssue(ctx, req.ProjectDir, req.IssueNumber); err != nil {
		readiness.State = ghissue.IssueStateBlocked
		readiness.CanAutoClose = false
		readiness.Recovery = blockedRecovery("close", "retry-close", err)
		readiness.Notes = append(readiness.Notes, fmt.Sprintf("GitHub close failed: %v", err))
		return readiness, err
	}
	readiness.State = ghissue.IssueStateClosed
	readiness.Closed = true
	return readiness, nil
}

func buildEvidenceComment(req RecordEvidenceRequest) string {
	var sb strings.Builder
	records := buildEvidenceRecords(req)
	if req.ChecklistMapping != nil {
		if summary := buildEvidenceMappingSummary(req.ChecklistMapping, records); summary != "" {
			sb.WriteString(summary)
			sb.WriteString("\n\n")
		}
	}
	if len(records) == 0 {
		if req.SubTask.Description == "" && strings.TrimSpace(req.Result) == "" {
			return ""
		}
		sb.WriteString(fmt.Sprintf("Issue evidence: sub-task %d/%d\n\n", req.Index+1, req.Total))
		sb.WriteString(fmt.Sprintf("- SubTask: %s\n", req.SubTask.Description))
		if strings.TrimSpace(req.Result) != "" {
			sb.WriteString(fmt.Sprintf("- Result: %s\n", strings.TrimSpace(req.Result)))
		}
		if req.Tokens > 0 {
			sb.WriteString(fmt.Sprintf("- Tokens: %d\n", req.Tokens))
		}
		if req.Completed > 0 {
			sb.WriteString(fmt.Sprintf("- Completed: %d\n", req.Completed))
		}
		return strings.TrimSpace(sb.String())
	}

	sb.WriteString("Issue evidence:\n")
	for _, ev := range records {
		sb.WriteString("- ")
		sb.WriteString(string(ev.Kind))
		if strings.TrimSpace(ev.Summary) != "" {
			sb.WriteString(": ")
			sb.WriteString(strings.TrimSpace(ev.Summary))
		}
		if strings.TrimSpace(ev.Reference) != "" {
			sb.WriteString(" [")
			sb.WriteString(strings.TrimSpace(ev.Reference))
			sb.WriteString("]")
		}
		sb.WriteString("\n")
		if strings.TrimSpace(ev.Detail) != "" {
			sb.WriteString("  ")
			sb.WriteString(strings.TrimSpace(ev.Detail))
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String())
}

func buildCloseReadiness(issue *ghissue.IssueContext, req AssessCloseReadinessRequest) CloseReadinessResult {
	rec := ghissue.ReconcileIssueCompletion(issue)
	result := CloseReadinessResult{
		Issue:              issue,
		Reconciliation:     rec,
		RequiredCloseLabel: strings.TrimSpace(req.RequiredCloseLabel),
		HasRequiredLabel:   true,
	}
	if issue == nil {
		result.State = ghissue.IssueStateBlocked
		result.Notes = append(result.Notes, "issue unavailable")
		return result
	}

	switch strings.ToLower(strings.TrimSpace(issue.State)) {
	case "closed":
		result.State = ghissue.IssueStateClosed
	case "blocked":
		result.State = ghissue.IssueStateBlocked
	default:
		switch {
		case ghissue.HasLabel(issue, "blocked"):
			result.State = ghissue.IssueStateBlocked
		case rec.HasUnchecked():
			if _, ok := ghissue.RecentSuccessfulHermesCompletion(issue); ok {
				result.State = ghissue.IssueStateChecklistUnsynced
				result.Notes = append(result.Notes, "issue evidence indicates checklist is unsynced")
			} else {
				result.State = ghissue.IssueStateEvidenceCollected
			}
		case !req.ReviewAccepted || !req.ValidationPassed:
			result.State = ghissue.IssueStateBlocked
		case req.RequiredCloseLabel != "":
			if ghissue.HasLabel(issue, req.RequiredCloseLabel) {
				result.State = ghissue.IssueStateReadyToClose
			} else {
				result.State = ghissue.IssueStateChecklistSynced
				result.HasRequiredLabel = false
				result.Notes = append(result.Notes, fmt.Sprintf("missing required label %q", req.RequiredCloseLabel))
			}
		default:
			result.State = ghissue.IssueStateReadyToClose
		}
	}

	result.Guard = ghissue.IssueCloseReadiness{
		State:                     result.State,
		HasBlockingLabel:          ghissue.HasLabel(issue, "blocked"),
		ChecklistSynced:           !rec.HasUnchecked(),
		ReviewAccepted:            req.ReviewAccepted,
		ValidationPassed:          req.ValidationPassed,
		HasUncheckedRequiredItems: rec.HasUnchecked(),
	}
	result.CanAutoClose = result.Guard.CanAutoClose() && result.HasRequiredLabel
	if rec.HasUnchecked() {
		result.Notes = append(result.Notes, rec.CommentNote())
	}
	if result.State == ghissue.IssueStateClosed {
		result.CanAutoClose = false
	}
	return result
}
