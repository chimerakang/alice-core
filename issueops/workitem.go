package issueops

import "github.com/chimerakang/alice-core/ghissue"

// WorkItem is one unit of work an orchestrator performed for an issue: a
// planned sub-task, or a whole task when the orchestrator does not decompose.
// It is the orchestrator-neutral shape IssueOps reasons about, so this package
// does not depend on any particular planner.
type WorkItem struct {
	// ID identifies the item in evidence references. May be empty.
	ID string
	// Description is matched against checklist item text when the item
	// declares no ChecklistItemIDs.
	Description string
	// Done reports that the item finished successfully. Only done items tick
	// checklist entries.
	Done bool
	// ChecklistItemIDs lists the ghissue.ChecklistItem.ID values this item
	// is responsible for completing.
	ChecklistItemIDs []string
}

// completedWork returns the done items in the shape checklist sync expects.
func completedWork(items []WorkItem) []ghissue.CompletedWork {
	done := make([]ghissue.CompletedWork, 0, len(items))
	for _, item := range items {
		if !item.Done {
			continue
		}
		done = append(done, ghissue.CompletedWork{
			Description:      item.Description,
			ChecklistItemIDs: item.ChecklistItemIDs,
		})
	}
	return done
}

// planEntries renders work items as plan-section lines.
func planEntries(items []WorkItem) []ghissue.PlanEntry {
	entries := make([]ghissue.PlanEntry, 0, len(items))
	for _, item := range items {
		entries = append(entries, ghissue.PlanEntry{
			Description:      item.Description,
			ChecklistItemIDs: item.ChecklistItemIDs,
		})
	}
	return entries
}
