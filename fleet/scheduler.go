package fleet

import (
	"context"
	"errors"
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
	// MaxConcurrent caps active tasks across all projects. Zero means
	// DefaultSchedulerConcurrency. Each project's own MaxConcurrent applies
	// on top of it.
	MaxConcurrent int
	// PollInterval is how often the queue is re-examined when nothing wakes
	// the scheduler sooner. Zero means DefaultPollInterval.
	PollInterval time.Duration

	mu       sync.Mutex
	inflight map[string]inflightTask // tasks this scheduler started and has not finished
	wg       sync.WaitGroup
	kick     chan struct{}
	kickOnce sync.Once
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

// Run schedules until ctx is cancelled, then waits for the tasks it started
// to wind down. Cancelling ctx stops their agents; each such task is left
// needing a decision with its session intact.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Store == nil || s.Runner == nil {
		return errors.New("fleet: Scheduler needs a Store and a Runner")
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
	var started []string
	for _, task := range queued {
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

		s.wg.Add(1)
		go func(id string) {
			defer s.wg.Done()
			_, _ = s.RunTask(ctx, id, RunOptions{})
			s.mu.Lock()
			delete(s.inflight, id)
			s.mu.Unlock()
			s.Kick() // a slot just opened
		}(task.ID)
	}
	return started, nil
}

// RunTask runs a task's agent and then verifies the result, sending the task
// back to its agent with the failure for as long as the Verifier asks for a
// retry. It returns the result of the last run.
//
// The Scheduler calls it for every task it starts. Call it directly to push
// a task that is waiting on the owner through the same run-and-verify cycle,
// for example with an answer or further instructions in opts.
func (s *Scheduler) RunTask(ctx context.Context, taskID string, opts RunOptions) (RunResult, error) {
	verifier := s.verifier()
	res, err := s.Runner.Run(ctx, taskID, opts)
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
	}
	return res, err
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
