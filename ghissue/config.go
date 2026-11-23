package ghissue

// Config holds GitHub integration settings: which lifecycle events post
// comments, whether checklists are synced, and which labels are applied.
type Config struct {
	Enabled         bool
	CommentOnStart  bool
	CommentOnDone   bool
	CommentOnFail   bool
	CommentOnBudget bool
	SyncChecklist   bool
	AutoCloseLabel  string
	FailureLabel    string
	TriggerTaskSync bool
}

// ShouldComment returns true when GitHub commenting is enabled for the given event.
func (g *Config) ShouldComment(event string) bool {
	if !g.Enabled {
		return false
	}
	switch event {
	case "start":
		return g.CommentOnStart
	case "complete":
		return g.CommentOnDone
	case "fail":
		return g.CommentOnFail
	case "budget_exceeded":
		return g.CommentOnBudget
	}
	return false
}
