package fleet

import (
	"context"
	"strings"
)

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
	case strings.HasPrefix(task.StatusReason, quotaWaitPrefix):
		backend := strings.TrimPrefix(task.StatusReason, quotaWaitPrefix)
		if backend == "codex" && (r.CodexScoutBinary == "" || r.scoutUSDLimited(task, project)) {
			return "claude", &scheduledScoutRoute{backend: "claude", model: task.Model, reason: "quota_eligibility_changed"}
		}
		if task.Shape == ShapeScout && r.scoutScheduleRoute != nil && task.quotaBackend == "" {
			_, model, _ := r.scoutScheduleRoute(ctx, task, r.scoutUSDLimited(task, project))
			return backend, &scheduledScoutRoute{backend: backend, model: model, reason: "quota_recovered"}
		}
		return backend, nil
	case task.Shape != ShapeScout:
		return "claude", nil
	case task.quotaBackend != "":
		if task.quotaBackend == "codex" && (r.CodexScoutBinary == "" || r.scoutUSDLimited(task, project)) {
			return "claude", &scheduledScoutRoute{backend: "claude", model: task.Model, reason: "quota_eligibility_changed"}
		}
		return task.quotaBackend, nil
	case r.scoutScheduleRoute != nil:
		backend, model, reason := r.scoutScheduleRoute(ctx, task, r.scoutUSDLimited(task, project))
		return backend, &scheduledScoutRoute{backend: backend, model: model, reason: reason}
	case r.ScoutRoute != nil:
		// Existing embedders may have a side-effecting route callback.
		return "", nil
	case r.CodexScoutBinary != "" && !r.scoutUSDLimited(task, project):
		return "codex", nil
	default:
		return "claude", nil
	}
}
