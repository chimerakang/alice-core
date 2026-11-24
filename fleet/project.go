// Package fleet is the command layer that runs agents across many projects:
// it records which repositories are under management and every task dispatched
// to them. One task is one agent run from start to finish; fleet does not
// decompose work, it only tracks, isolates and supervises it.
//
// All state lives in SQLite, so a restarted process picks up where it left off.
package fleet

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TrustMode fixes how far a task may go without the owner. It is chosen per
// project and copied onto each task at dispatch, so changing a project's mode
// never alters a task that is already running.
type TrustMode string

const (
	// TrustLocalOnly keeps work on a local branch. Nothing is pushed.
	TrustLocalOnly TrustMode = "local-only"
	// TrustDirectPR pushes the branch and opens a pull request once
	// verification passes. Merging is left to the owner.
	TrustDirectPR TrustMode = "direct-pr"
	// TrustNoMistakes opens a pull request and merges it only after the
	// owner approves.
	TrustNoMistakes TrustMode = "no-mistakes"
)

// Valid reports whether m is a known trust mode.
func (m TrustMode) Valid() bool {
	switch m {
	case TrustLocalOnly, TrustDirectPR, TrustNoMistakes:
		return true
	}
	return false
}

// NeedsRemote reports whether the mode pushes to a remote.
func (m TrustMode) NeedsRemote() bool {
	return m == TrustDirectPR || m == TrustNoMistakes
}

// ParseTrustMode converts user input into a TrustMode.
func ParseTrustMode(s string) (TrustMode, error) {
	m := TrustMode(strings.ToLower(strings.TrimSpace(s)))
	if !m.Valid() {
		return "", fmt.Errorf("%w: unknown trust mode %q (want local-only, direct-pr or no-mistakes)", ErrInvalidProject, s)
	}
	return m, nil
}

// Defaults applied by RegisterProject to fields left empty.
const (
	DefaultTrustMode     = TrustLocalOnly
	DefaultBranchPrefix  = "alice/"
	DefaultMaxConcurrent = 1
)

// Project is a repository under fleet management.
type Project struct {
	// ID is a short stable slug, unique across projects. It is used in
	// worktree paths, so it is restricted to [a-z0-9._-]. Derived from Name
	// or the repository directory when empty at registration. Immutable.
	ID string
	// Name is the display name. Defaults to the repository directory name.
	Name string
	// RepoPath is the absolute path of the repository's main working tree.
	RepoPath string
	// Remote is the git remote tasks push to, e.g. "origin". Empty for a
	// repository with no remote, which is only allowed in local-only mode.
	Remote string
	// DefaultBranch is the branch new task branches start from.
	DefaultBranch string
	// TrustMode is copied onto each task at dispatch.
	TrustMode TrustMode
	// VerifyCommands are shell commands run in order, in the task's
	// worktree, to accept or reject its result. Exit status decides.
	VerifyCommands []string
	// MaxConcurrent caps how many of this project's tasks run at once.
	MaxConcurrent int
	// BranchPrefix is prepended to task IDs to form branch names.
	BranchPrefix string
	// Budget holds the project's own spending limits. A zero limit means
	// the fleet-wide default applies.
	Budget ProjectBudget
	// Bindings are free-form key/value attachments, e.g. the Telegram topic
	// a project is bound to. Keys are unique per project.
	Bindings map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}

var (
	// ErrProjectNotFound is returned when no project has the given ID.
	ErrProjectNotFound = errors.New("fleet: project not found")
	// ErrProjectExists is returned when the ID or repository path is
	// already registered.
	ErrProjectExists = errors.New("fleet: project already registered")
	// ErrProjectHasTasks is returned by DeleteProject while the project
	// still has task history.
	ErrProjectHasTasks = errors.New("fleet: project has tasks")
	// ErrInvalidProject wraps every project validation failure.
	ErrInvalidProject = errors.New("fleet: invalid project")
	// ErrNotGitRepo is returned when a project path is not a git repository.
	ErrNotGitRepo = errors.New("fleet: not a git repository")
	// ErrBranchNotFound is returned when the default branch does not exist.
	ErrBranchNotFound = errors.New("fleet: branch not found")
	// ErrRemoteNotFound is returned when the named remote does not exist.
	ErrRemoteNotFound = errors.New("fleet: remote not found")
)

var projectIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Slug reduces s to a valid project ID, or "" when nothing usable remains.
func Slug(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-._")
	if len(slug) > 63 {
		slug = strings.TrimRight(slug[:63], "-._")
	}
	return slug
}

// normalize fills defaults and validates the fields that need no git access.
// RepoPath must already be resolved to the repository's top level.
func (p *Project) normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" {
		p.Name = filepath.Base(p.RepoPath)
	}
	p.ID = strings.TrimSpace(p.ID)
	if p.ID == "" {
		p.ID = Slug(p.Name)
		if p.ID == "" {
			p.ID = Slug(filepath.Base(p.RepoPath))
		}
	}
	if !projectIDRe.MatchString(p.ID) {
		return fmt.Errorf("%w: ID %q must match %s", ErrInvalidProject, p.ID, projectIDRe)
	}

	if p.TrustMode == "" {
		p.TrustMode = DefaultTrustMode
	}
	if !p.TrustMode.Valid() {
		return fmt.Errorf("%w: unknown trust mode %q", ErrInvalidProject, p.TrustMode)
	}

	if p.MaxConcurrent == 0 {
		p.MaxConcurrent = DefaultMaxConcurrent
	}
	if p.MaxConcurrent < 0 {
		return fmt.Errorf("%w: MaxConcurrent must be positive, got %d", ErrInvalidProject, p.MaxConcurrent)
	}

	if p.BranchPrefix == "" {
		p.BranchPrefix = DefaultBranchPrefix
	}

	for name, l := range map[string]Limit{"task": p.Budget.Task, "daily": p.Budget.Daily} {
		if l.CostUSD < 0 || l.Tokens < 0 {
			return fmt.Errorf("%w: the %s budget cannot be negative", ErrInvalidProject, name)
		}
	}

	commands := make([]string, 0, len(p.VerifyCommands))
	for _, c := range p.VerifyCommands {
		if c = strings.TrimSpace(c); c != "" {
			commands = append(commands, c)
		}
	}
	p.VerifyCommands = commands

	bindings := make(map[string]string, len(p.Bindings))
	for k, v := range p.Bindings {
		k = strings.TrimSpace(k)
		if k == "" {
			return fmt.Errorf("%w: binding key must not be empty", ErrInvalidProject)
		}
		bindings[k] = v
	}
	p.Bindings = bindings
	return nil
}

// BranchName returns the branch a task works on in this project.
func (p Project) BranchName(taskID string) string {
	return p.BranchPrefix + taskID
}
