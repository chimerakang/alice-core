package fleet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Scheduler starts queued tasks as capacity allows and carries each one
// through running and verification.
//
// It decides only when a task starts. Capacity is counted from stored task
// statuses, so runs started some other way (a follow-up sent straight to the
// Runner, say) take up slots too.
type Scheduler struct {
	// Store holds the tasks. Required.
	Store *Store
	// Runner executes tasks. Required.
	Runner *Runner
	// Verifier checks finished tasks. When nil, one is built from the
	// Runner's settings: a task must never be left verifying with nothing
	// checking it, or its slot would be held forever.
	Verifier *Verifier
	// Deliverer, when set, carries each verified ship task as far as its
	// trust mode allows: committing, pushing, opening a pull request. When
	// nil, verified tasks simply wait at review_ready.
	Deliverer *Deliverer
	// Issues, when set, keeps the GitHub issue behind an issue-driven task
	// informed: a comment when work starts and when it ends, and checklist
	// items ticked once the work is verified.
	Issues *IssueTracker
	// MaxConcurrent caps active tasks across all projects. Zero means
	// DefaultSchedulerConcurrency. Each project's own MaxConcurrent applies
	// on top of it.
	MaxConcurrent int
	// StartupResumeLimit caps automatic recovery attempts per task. Zero disables them.
	StartupResumeLimit int
	// PollInterval is how often the queue is re-examined when nothing wakes
	// the scheduler sooner. Zero means DefaultPollInterval.
	PollInterval time.Duration
	// OnRecovery, when set, receives what startup recovery found. Run calls
	// it once, before the first scheduling pass, when there is anything to
	// report.
	OnRecovery func(RecoveryReport)

	mu             sync.Mutex
	base           context.Context         // the context Run was given; background work lives as long as it does
	inflight       map[string]inflightTask // tasks this scheduler started and has not finished
	wg             sync.WaitGroup
	kick           chan struct{}
	kickOnce       sync.Once
	startupResumes map[string]startupResume
}

// Scheduler defaults.
const (
	DefaultSchedulerConcurrency = 2
	DefaultPollInterval         = 2 * time.Second
)

type inflightTask struct {
	projectID string
	branch    string
}

// Run first recovers tasks cut off by an earlier crash or restart (see
// Runner.Recover), then schedules until ctx is cancelled, then waits for the
// tasks it started to wind down. Cancelling ctx stops their agents; each such task is left
// needing a decision with its session intact.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Store == nil || s.Runner == nil {
		return errors.New("fleet: Scheduler needs a Store and a Runner")
	}
	// Tasks left active by a previous process would hold their slots for
	// ever, so they are reconciled before anything is scheduled.
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()

	report, err := s.Runner.Recover(ctx)
	if ctx.Err() != nil {
		return nil // asked to stop before scheduling began
	}
	if err != nil {
		return err
	}
	if s.OnRecovery != nil && !report.Empty() {
		s.OnRecovery(report)
	}

	s.mu.Lock()
	s.startupResumes = nil
	if s.StartupResumeLimit > 0 {
		s.startupResumes, err = s.findStartupResumes(ctx)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(s.pollInterval())
	defer ticker.Stop()
	for {
		// A failed pass is simply retried on the next tick: the queue is durable.
		_, _ = s.Tick(ctx)
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return nil
		case <-ticker.C:
		case <-s.kickChan():
		}
	}
}

// Kick asks a running scheduler to look at the queue now rather than at the
// next poll. Call it after queueing a task.
func (s *Scheduler) Kick() {
	select {
	case s.kickChan() <- struct{}{}:
	default:
	}
}

func (s *Scheduler) kickChan() chan struct{} {
	s.kickOnce.Do(func() { s.kick = make(chan struct{}, 1) })
	return s.kick
}

// Wait blocks until every task this scheduler started has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Tick makes one scheduling pass: it starts every queued task that fits, in
// queue order (highest priority first, then oldest), and returns the IDs it
// started. Tasks run in the background; Tick does not wait for them.
//
// A queued task is passed over, without blocking the tasks behind it, when
// its project is at its own limit or when another active task is using the
// same branch.
func (s *Scheduler) Tick(ctx context.Context) ([]string, error) {
	if s.Store == nil || s.Runner == nil {
		return nil, errors.New("fleet: Scheduler needs a Store and a Runner")
	}
	active, err := s.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusRunning, StatusVerifying}})
	if err != nil {
		return nil, err
	}
	queued, err := s.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusQueued}, Order: QueueOrder})
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight == nil {
		s.inflight = make(map[string]inflightTask)
	}

	// Occupancy is what the store says is active, plus what this scheduler
	// has launched that the store does not show as active yet.
	occupied := make(map[string]inflightTask, len(active)+len(s.inflight))
	for _, t := range active {
		occupied[t.ID] = inflightTask{projectID: t.ProjectID, branch: t.Branch}
	}
	for id, t := range s.inflight {
		if _, ok := occupied[id]; !ok {
			occupied[id] = t
		}
	}
	perProject := make(map[string]int)
	branches := make(map[string]bool)
	for _, t := range occupied {
		perProject[t.projectID]++
		if t.branch != "" {
			branches[t.projectID+"\x00"+t.branch] = true
		}
	}

	limit := s.maxConcurrent()
	projects := make(map[string]Project)
	budget := s.Runner.Budget.view(s.Store)
	var started []string
	var blocked []newWake
	defer func() {
		if len(blocked) > 0 {
			_, _ = s.Store.raiseWakes(context.WithoutCancel(ctx), blocked...)
		}
	}()
	candidates := make([]Task, 0, len(s.startupResumes)+len(queued))
	if len(s.startupResumes) > 0 {
		waiting, err := s.Store.ListTasks(ctx, TaskFilter{Statuses: []Status{StatusNeedsDecision}, Order: QueueOrder})
		if err != nil {
			return nil, err
		}
		remaining := make(map[string]startupResume, len(s.startupResumes))
		for _, task := range waiting {
			if resume, ok := s.startupResumes[task.ID]; ok && task.UpdatedAt.Equal(resume.task.UpdatedAt) {
				remaining[task.ID] = resume
				candidates = append(candidates, task)
			}
		}
		s.startupResumes = remaining
	}
	candidates = append(candidates, queued...)
	for _, task := range candidates {
		if len(occupied) >= limit {
			break
		}
		if _, busy := occupied[task.ID]; busy {
			continue
		}
		project, ok := projects[task.ProjectID]
		if !ok {
			if project, err = s.Store.GetProject(ctx, task.ProjectID); err != nil {
				continue // the project vanished; leave the task queued
			}
			projects[task.ProjectID] = project
		}
		if perProject[task.ProjectID] >= project.MaxConcurrent {
			continue
		}
		// A task past a spending limit waits in the queue; the owner is
		// told once per limit per day.
		if over, err := budget.admit(ctx, task, project); err != nil {
			continue
		} else if over != nil {
			blocked = append(blocked, budgetWake(over, budget.day()))
			continue
		}
		if task.quotaBackend != "" {
			if blocked, _, err := s.Store.BackendQuotaBlocked(ctx, task.quotaBackend); err != nil || blocked {
				continue
			}
		}
		branch := task.Branch
		if branch == "" {
			branch = project.BranchName(task.ID)
		}
		key := task.ProjectID + "\x00" + branch
		if branches[key] {
			continue
		}

		slot := inflightTask{projectID: task.ProjectID, branch: branch}
		occupied[task.ID], s.inflight[task.ID] = slot, slot
		perProject[task.ProjectID]++
		branches[key] = true
		started = append(started, task.ID)

		var opts RunOptions
		if resumed, ok := s.startupResumes[task.ID]; ok && task.Status == StatusNeedsDecision {
			delete(s.startupResumes, task.ID)
			opts.startupResume = &resumed
		}
		s.wg.Add(1)
		go func(id string, opts RunOptions) {
			defer s.wg.Done()
			_, _ = s.RunTask(ctx, id, opts)
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
			s.Kick() // a slot just opened
		}(task.ID, opts)
	}
	return started, nil
}

// RunTask runs a task's agent and then verifies the result, sending the task
// back to its agent with the failure for as long as the Verifier asks for a
// retry. A ship task that passes is then delivered, if a Deliverer is set.
// It returns the result of the last run, with the task as finally stored.
//
// The Scheduler calls it for every task it starts. Call it directly to push
// a task that is waiting on the owner through the same run-and-verify cycle,
// for example with an answer or further instructions in opts.
func (s *Scheduler) RunTask(ctx context.Context, taskID string, opts RunOptions) (RunResult, error) {
	before, lookupErr := s.Store.GetTask(ctx, taskID)
	tracked := lookupErr == nil && s.Issues.isTracked(before)
	if tracked && before.Status == StatusQueued && opts.OnStart == nil {
		opts.OnStart = func(t Task) { s.Issues.Started(ctx, t) }
	}
	note := func(res RunResult) {
		if tracked && res.Outcome != OutcomeRequeued {
			s.Issues.NoteResult(ctx, res.Task, res.Text)
		}
	}

	res, err := s.cycle(ctx, taskID, opts, note)
	if err == nil && res.Outcome == OutcomeRequeued {
		s.Kick()
	}
	if err == nil && tracked && res.Outcome != OutcomeRequeued {
		s.Issues.Finished(ctx, res.Task)
	}
	return res, err
}

// cycle is the run, verify, retry, deliver sequence. afterRun sees the
// result of every agent run in it.
func (s *Scheduler) cycle(ctx context.Context, taskID string, opts RunOptions, afterRun func(RunResult)) (RunResult, error) {
	verifier := s.verifier()
	res, err := s.Runner.Run(ctx, taskID, opts)
	if err == nil {
		afterRun(res)
	}
	for err == nil && res.Outcome == OutcomeFinished {
		check, verr := verifier.Verify(ctx, taskID)
		if verr != nil {
			return res, verr
		}
		res.Task = check.Task
		if !check.Retry {
			break
		}
		res, err = s.Runner.Run(ctx, taskID, RunOptions{Instruction: check.Feedback})
		var over *BudgetError
		if errors.As(err, &over) {
			// The budget ran out between the check and the retry. The task
			// must not be left verifying with nothing coming to check it.
			return s.stopForBudget(ctx, taskID, res, over)
		}
		if err == nil {
			afterRun(res)
		}
	}
	if err == nil && s.Deliverer != nil && res.Task.Shape == ShapeShip && res.Task.Status == StatusReviewReady {
		delivery, derr := s.Deliverer.Deliver(ctx, taskID)
		if derr != nil {
			return res, derr
		}
		res.Task = delivery.Task
	}
	return res, err
}

// Go starts RunTask for a task in the background and returns at once. It is
// how a task that is waiting on the owner gets going again: an answer, more
// instructions, or simply "carry on".
//
// The run lives as long as the context given to Run, not the caller's, so a
// request handler can return while the agent works. Problems that can be
// seen up front are returned: a task that does not exist, one that cannot
// run from its current status, one that already has a live run, or a
// reached budget limit. Run waits for runs started here when it shuts down.
func (s *Scheduler) Go(ctx context.Context, taskID string, opts RunOptions) error {
	if s.Store == nil || s.Runner == nil {
		return errors.New("fleet: Scheduler needs a Store and a Runner")
	}
	task, err := s.Store.GetTask(ctx, taskID)
	if err != nil {
		return err
	}
	if !CanTransition(task.Status, StatusRunning) {
		return &TransitionError{TaskID: taskID, From: task.Status, To: StatusRunning}
	}
	if s.Runner.Running(taskID) {
		return fmt.Errorf("%w: task %s already has a live run", ErrConflict, taskID)
	}
	if s.Runner.Budget != nil && !opts.IgnoreBudget {
		project, err := s.Store.GetProject(ctx, task.ProjectID)
		if err != nil {
			return err
		}
		over, err := s.Runner.Budget.view(s.Store).admit(ctx, task, project)
		if err != nil {
			return err
		}
		if over != nil {
			return over
		}
	}

	s.mu.Lock()
	base := s.base
	if base == nil {
		base = context.WithoutCancel(ctx)
	}
	if s.inflight == nil {
		s.inflight = make(map[string]inflightTask)
	}
	if _, busy := s.inflight[taskID]; busy {
		s.mu.Unlock()
		return fmt.Errorf("%w: task %s is already being started", ErrConflict, taskID)
	}
	s.inflight[taskID] = inflightTask{projectID: task.ProjectID, branch: task.Branch}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		_, _ = s.RunTask(base, taskID, opts)
		s.mu.Lock()
		delete(s.inflight, taskID)
		s.mu.Unlock()
		s.Kick()
	}()
	return nil
}

// stopForBudget hands a task that cannot continue for lack of budget to the
// owner, and tells them why.
func (s *Scheduler) stopForBudget(ctx context.Context, taskID string, res RunResult, over *BudgetError) (RunResult, error) {
	recCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	task, err := s.Store.Transition(recCtx, taskID, StatusNeedsDecision, over.Reason())
	if err != nil {
		return res, err
	}
	_, _ = s.Store.raiseWakes(recCtx, budgetWake(over, s.Runner.Budget.view(s.Store).day()))
	res.Task, res.Outcome, res.Reason = task, OutcomeNeedsDecision, over.Reason()
	return res, nil
}

func (s *Scheduler) verifier() *Verifier {
	if s.Verifier != nil {
		return s.Verifier
	}
	return &Verifier{Store: s.Store, ReportRoot: s.Runner.ReportRoot}
}

func (s *Scheduler) maxConcurrent() int {
	if s.MaxConcurrent > 0 {
		return s.MaxConcurrent
	}
	return DefaultSchedulerConcurrency
}

func (s *Scheduler) pollInterval() time.Duration {
	if s.PollInterval > 0 {
		return s.PollInterval
	}
	return DefaultPollInterval
}
