// Package worktree manages git worktrees: one isolated checkout per task, so
// agents working in parallel on the same repository never share a working
// directory.
//
// A worktree's branch is never deleted here. Removing a worktree discards only
// the checkout; committed work stays on its branch.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/process"
)

var (
	// ErrDirty is returned by Remove when the worktree has uncommitted or
	// untracked changes and force was not requested.
	ErrDirty = errors.New("worktree: uncommitted changes")
	// ErrMainWorktree is returned by Remove for a repository's main working
	// tree, which must never be removed.
	ErrMainWorktree = errors.New("worktree: refusing to remove the main working tree")
	// ErrNotWorktree is returned when a path is not a git worktree.
	ErrNotWorktree = errors.New("worktree: not a git worktree")
	// ErrBranchExists is returned by Create when the branch already exists.
	ErrBranchExists = errors.New("worktree: branch already exists")
	// ErrPathExists is returned when the target path exists and is not empty.
	ErrPathExists = errors.New("worktree: target path already exists")
	// ErrInvalidName is returned by Layout.Path for an ID that cannot be used
	// as a single path component.
	ErrInvalidName = errors.New("worktree: invalid path component")
)

const gitTimeout = 60 * time.Second

// Worktree describes one entry of `git worktree list`.
type Worktree struct {
	// Path is the absolute path of the working tree.
	Path string
	// Head is the commit checked out.
	Head string
	// Branch is the short branch name, or "" when HEAD is detached.
	Branch string
	// Main is true for the repository's main working tree.
	Main bool
	// Detached reports a detached HEAD.
	Detached bool
	// Locked reports that the worktree is locked against pruning.
	Locked bool
	// Prunable reports that git considers the entry stale, typically because
	// its directory no longer exists.
	Prunable bool
}

// Layout places worktrees under a single root as <root>/<projectID>/<taskID>.
type Layout struct {
	Root string
}

// Path returns the worktree directory for a task. Both IDs must be plain
// names: no path separators, no "." or "..", and not empty.
func (l Layout) Path(projectID, taskID string) (string, error) {
	if strings.TrimSpace(l.Root) == "" {
		return "", fmt.Errorf("%w: empty root", ErrInvalidName)
	}
	for _, name := range []string{projectID, taskID} {
		if !validComponent(name) {
			return "", fmt.Errorf("%w: %q", ErrInvalidName, name)
		}
	}
	return filepath.Join(l.Root, projectID, taskID), nil
}

func validComponent(name string) bool {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name {
		return false
	}
	return !strings.ContainsAny(name, `/\`+"\x00")
}

// Create adds a worktree at path on a new branch that starts at base (a
// branch, tag or commit; "" means the repository's current HEAD).
//
// It fails with ErrBranchExists if the branch is already present, and with
// ErrPathExists if path exists and is not an empty directory. Use Attach to
// check out an existing branch.
func Create(ctx context.Context, repo, path, branch, base string) (Worktree, error) {
	if strings.TrimSpace(branch) == "" {
		return Worktree{}, errors.New("worktree: branch is required")
	}
	if err := checkTarget(path); err != nil {
		return Worktree{}, err
	}
	if branchExists(ctx, repo, branch) {
		return Worktree{}, fmt.Errorf("%w: %s", ErrBranchExists, branch)
	}
	args := []string{"worktree", "add", "-b", branch, path}
	if strings.TrimSpace(base) != "" {
		args = append(args, base)
	}
	if out, err := git(ctx, repo, args...); err != nil {
		return Worktree{}, gitError("worktree add", out, err)
	}
	return find(ctx, repo, path)
}

// Attach adds a worktree at path for a branch that already exists, for
// example to resume a task whose checkout was removed.
func Attach(ctx context.Context, repo, path, branch string) (Worktree, error) {
	if strings.TrimSpace(branch) == "" {
		return Worktree{}, errors.New("worktree: branch is required")
	}
	if err := checkTarget(path); err != nil {
		return Worktree{}, err
	}
	if out, err := git(ctx, repo, "worktree", "add", path, branch); err != nil {
		return Worktree{}, gitError("worktree add", out, err)
	}
	return find(ctx, repo, path)
}

// Remove deletes the worktree at path. The branch is kept.
//
// A worktree with uncommitted or untracked changes is refused with ErrDirty
// unless force is true. The main working tree is always refused.
func Remove(ctx context.Context, path string, force bool) error {
	main, err := mainWorktree(ctx, path)
	if err != nil {
		return err
	}
	top, err := toplevel(ctx, path)
	if err != nil {
		return err
	}
	if samePath(top, main) {
		return fmt.Errorf("%w: %s", ErrMainWorktree, path)
	}
	if !samePath(top, path) {
		// path is a subdirectory of a worktree, not the worktree itself.
		return fmt.Errorf("%w: %s", ErrNotWorktree, path)
	}
	if !force {
		dirty, err := IsDirty(ctx, path)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("%w: %s", ErrDirty, path)
		}
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, top)
	if out, err := git(ctx, main, args...); err != nil {
		return gitError("worktree remove", out, err)
	}
	return nil
}

// IsDirty reports whether the working tree at path has uncommitted changes or
// untracked files. Ignored files do not count.
func IsDirty(ctx context.Context, path string) (bool, error) {
	changes, err := Changes(ctx, path)
	return len(changes) > 0, err
}

// Changes returns the `git status --porcelain` lines for the working tree at
// path: one entry per modified, staged or untracked file.
func Changes(ctx context.Context, path string) ([]string, error) {
	if _, err := toplevel(ctx, path); err != nil {
		return nil, err
	}
	out, err := git(ctx, path, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return nil, gitError("status", out, err)
	}
	var changes []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) != "" {
			changes = append(changes, line)
		}
	}
	return changes, nil
}

// List returns every worktree of the repository, main working tree first.
func List(ctx context.Context, repo string) ([]Worktree, error) {
	out, err := git(ctx, repo, "worktree", "list", "--porcelain")
	if err != nil {
		if isNotRepo(out) {
			return nil, fmt.Errorf("%w: %s", ErrNotWorktree, repo)
		}
		return nil, gitError("worktree list", out, err)
	}
	return parseList(string(out)), nil
}

// Prune drops git's records of worktrees whose directories no longer exist.
func Prune(ctx context.Context, repo string) error {
	if out, err := git(ctx, repo, "worktree", "prune"); err != nil {
		return gitError("worktree prune", out, err)
	}
	return nil
}

// parseList parses `git worktree list --porcelain`. Entries are separated by
// blank lines and the first entry is the main working tree.
func parseList(out string) []Worktree {
	var list []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil {
			cur.Main = len(list) == 0
			list = append(list, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		key, value, _ := strings.Cut(line, " ")
		if key == "worktree" {
			flush()
			cur = &Worktree{Path: value}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "HEAD":
			cur.Head = value
		case "branch":
			cur.Branch = strings.TrimPrefix(value, "refs/heads/")
		case "detached":
			cur.Detached = true
		case "locked":
			cur.Locked = true
		case "prunable":
			cur.Prunable = true
		}
	}
	flush()
	return list
}

func find(ctx context.Context, repo, path string) (Worktree, error) {
	list, err := List(ctx, repo)
	if err != nil {
		return Worktree{}, err
	}
	for _, wt := range list {
		if samePath(wt.Path, path) {
			return wt, nil
		}
	}
	return Worktree{}, fmt.Errorf("%w: %s was not registered", ErrNotWorktree, path)
}

// checkTarget allows a missing path or an empty directory.
func checkTarget(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("worktree: path is required")
	}
	entries, err := os.ReadDir(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		// Exists but is not a readable directory (for example a file).
		return fmt.Errorf("%w: %s", ErrPathExists, path)
	case len(entries) > 0:
		return fmt.Errorf("%w: %s", ErrPathExists, path)
	}
	return nil
}

func branchExists(ctx context.Context, repo, branch string) bool {
	_, err := git(ctx, repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// toplevel returns the root of the working tree containing path.
func toplevel(ctx context.Context, path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotWorktree, path)
	}
	out, err := git(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		if isNotRepo(out) {
			return "", fmt.Errorf("%w: %s", ErrNotWorktree, path)
		}
		return "", gitError("rev-parse", out, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// mainWorktree returns the main working tree of the repository that the
// worktree at path belongs to.
func mainWorktree(ctx context.Context, path string) (string, error) {
	if _, err := toplevel(ctx, path); err != nil {
		return "", err
	}
	list, err := List(ctx, path)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "", fmt.Errorf("%w: %s", ErrNotWorktree, path)
	}
	return list[0].Path, nil
}

func samePath(a, b string) bool {
	return canonical(a) == canonical(b)
}

func canonical(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

func isNotRepo(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "not a git repository")
}

func gitError(op string, out []byte, err error) error {
	msg := strings.TrimSpace(string(out))
	var exitErr *exec.ExitError
	if msg == "" || !errors.As(err, &exitErr) {
		return fmt.Errorf("git %s: %w", op, err)
	}
	return fmt.Errorf("git %s: %s", op, msg)
}

func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return process.RunCombinedOutput(ctx, process.Options{
		Dir:       dir,
		Timeout:   gitTimeout,
		Env:       append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"),
		LogPrefix: "worktree",
	}, "git", args...)
}
