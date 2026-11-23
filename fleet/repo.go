package fleet

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

const gitTimeout = 30 * time.Second

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := process.RunCombinedOutput(ctx, process.Options{
		Dir:       dir,
		Timeout:   gitTimeout,
		Env:       append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"),
		LogPrefix: "fleet",
	}, "git", args...)
	return strings.TrimSpace(string(out)), err
}

// isExitError reports whether git ran and exited non-zero, as opposed to
// failing to start or being killed.
func isExitError(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() > 0
}

// resolveRepo checks the project's repository and fills in what can be read
// from it: the top-level path, the remote, and the default branch.
func resolveRepo(ctx context.Context, p *Project) error {
	path := strings.TrimSpace(p.RepoPath)
	if path == "" {
		return fmt.Errorf("%w: RepoPath is required", ErrInvalidProject)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidProject, err)
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrNotGitRepo, abs)
	}

	top, err := runGit(ctx, abs, "rev-parse", "--show-toplevel")
	if err != nil {
		if isExitError(err) {
			return fmt.Errorf("%w: %s", ErrNotGitRepo, abs)
		}
		return fmt.Errorf("fleet: inspect repository: %w", err)
	}
	p.RepoPath = top

	if p.Remote, err = resolveRemote(ctx, top, strings.TrimSpace(p.Remote)); err != nil {
		return err
	}
	if p.DefaultBranch, err = resolveDefaultBranch(ctx, top, p.Remote, strings.TrimSpace(p.DefaultBranch)); err != nil {
		return err
	}
	return nil
}

// resolveRemote validates want, or picks "origin" when it exists. A
// repository without remotes resolves to "".
func resolveRemote(ctx context.Context, repo, want string) (string, error) {
	out, err := runGit(ctx, repo, "remote")
	if err != nil {
		return "", fmt.Errorf("fleet: list remotes: %w", err)
	}
	remotes := strings.Fields(out)
	has := func(name string) bool {
		for _, r := range remotes {
			if r == name {
				return true
			}
		}
		return false
	}
	switch {
	case want != "":
		if !has(want) {
			return "", fmt.Errorf("%w: %s", ErrRemoteNotFound, want)
		}
		return want, nil
	case has("origin"):
		return "origin", nil
	case len(remotes) == 1:
		return remotes[0], nil
	}
	return "", nil
}

// resolveDefaultBranch validates want, or detects the branch: the remote's
// HEAD when known, otherwise the branch currently checked out.
func resolveDefaultBranch(ctx context.Context, repo, remote, want string) (string, error) {
	if want == "" {
		if remote != "" {
			if ref, err := runGit(ctx, repo, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
				want = strings.TrimPrefix(ref, remote+"/")
			}
		}
		if want == "" {
			ref, err := runGit(ctx, repo, "symbolic-ref", "--quiet", "--short", "HEAD")
			if err != nil || ref == "" {
				return "", fmt.Errorf("%w: HEAD is detached, so DefaultBranch must be given", ErrInvalidProject)
			}
			want = ref
		}
	}

	refs := []string{"refs/heads/" + want}
	if remote != "" {
		refs = append(refs, "refs/remotes/"+remote+"/"+want)
	}
	for _, ref := range refs {
		if _, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err == nil {
			return want, nil
		}
	}
	return "", fmt.Errorf("%w: %s", ErrBranchNotFound, want)
}

// checkBranchPrefix verifies that prefix forms valid branch names.
func checkBranchPrefix(ctx context.Context, repo, prefix string) error {
	if _, err := runGit(ctx, repo, "check-ref-format", "--branch", prefix+"probe"); err != nil {
		return fmt.Errorf("%w: branch prefix %q does not form a valid branch name", ErrInvalidProject, prefix)
	}
	return nil
}
