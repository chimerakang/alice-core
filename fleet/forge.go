package fleet

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/process"
)

// PullRequest describes a pull request to open.
type PullRequest struct {
	// Branch is the head branch, already pushed.
	Branch string
	// Base is the branch to merge into.
	Base  string
	Title string
	Body  string
}

// Forge is the code host: where pull requests are opened and merged.
type Forge interface {
	// OpenPullRequest opens a pull request for a pushed branch. dir is a
	// checkout of the repository, used to identify it.
	OpenPullRequest(ctx context.Context, dir string, pr PullRequest) (url string, number int, err error)
	// MergePullRequest merges a pull request with a merge commit, leaving
	// the branch's history as it is. Merging one that is already merged
	// must succeed.
	MergePullRequest(ctx context.Context, dir string, number int) error
}

// GitHub is a Forge backed by the gh CLI.
type GitHub struct{}

const forgeTimeout = 90 * time.Second

var prNumberRe = regexp.MustCompile(`/pull/(\d+)`)

// OpenPullRequest implements Forge with `gh pr create`.
func (GitHub) OpenPullRequest(ctx context.Context, dir string, pr PullRequest) (string, int, error) {
	out, err := gh(ctx, dir, "pr", "create", "--head", pr.Branch, "--base", pr.Base, "--title", pr.Title, "--body", pr.Body)
	if err != nil {
		return "", 0, fmt.Errorf("gh pr create: %w: %s", err, strings.TrimSpace(out))
	}
	// gh prints the pull request's URL on its last line.
	lines := strings.Fields(out)
	for i := len(lines) - 1; i >= 0; i-- {
		if m := prNumberRe.FindStringSubmatch(lines[i]); m != nil {
			number, _ := strconv.Atoi(m[1])
			return lines[i], number, nil
		}
	}
	return "", 0, fmt.Errorf("gh pr create: no pull request URL in output: %s", strings.TrimSpace(out))
}

// MergePullRequest implements Forge with `gh pr merge --merge`.
func (GitHub) MergePullRequest(ctx context.Context, dir string, number int) error {
	out, err := gh(ctx, dir, "pr", "merge", strconv.Itoa(number), "--merge")
	if err != nil {
		if strings.Contains(strings.ToLower(out), "already merged") {
			return nil
		}
		return fmt.Errorf("gh pr merge %d: %w: %s", number, err, strings.TrimSpace(out))
	}
	return nil
}

func gh(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := process.RunCombinedOutput(ctx, process.Options{
		Dir: dir, Timeout: forgeTimeout, OutputLimit: 64 * 1024, LogPrefix: "fleet.forge",
		// A pull request body can be long; keep it out of the process log.
		LogLabel: "gh " + strings.Join(args[:min(len(args), 3)], " "),
	}, "gh", args...)
	return string(out), err
}
