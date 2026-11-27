package fleet

import "context"

type scheduledScoutRoute struct {
	backend string
	model   string
	reason  string
}

// ConfigureScoutQuotaAdmission sets a side-effect-free routing decision for
// queued Scouts. Direct runs continue to use ScoutRoute.
func (r *Runner) ConfigureScoutQuotaAdmission(choose func(context.Context, Task, bool) (backend, model, reason string)) {
	r.scoutScheduleRoute = choose
}

func (r *Runner) queuedBackend(ctx context.Context, task Task, project Project) (string, *scheduledScoutRoute) {
	switch {
	case task.Shape != ShapeScout:
		return "claude", nil
	case task.quotaBackend != "":
		return task.quotaBackend, nil
	case r.scoutScheduleRoute != nil:
		backend, model, reason := r.scoutScheduleRoute(ctx, task, r.scoutUSDLimited(task, project))
		return backend, &scheduledScoutRoute{backend: backend, model: model, reason: reason}
	case r.ScoutRoute != nil:
		// Existing embedders may have a side-effecting route callback.
		return "", nil
	case r.CodexScoutBinary != "":
		return "codex", nil
	default:
		return "claude", nil
	}
}
