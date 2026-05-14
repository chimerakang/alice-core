package git

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chimerakang/alice-core/process"
)

// GitInfo holds the current state of a git repository.
type GitInfo struct {
	Branch        string
	CommitHash    string // short hash
	RemoteURL     string
	IsDirty       bool
	ModifiedFiles []string
}

type cacheEntry struct {
	info    *GitInfo
	fetchAt time.Time
}

var (
	cacheMu sync.RWMutex
	cache   = make(map[string]cacheEntry)
	cacheTTL = 30 * time.Second
)

// Get returns git info for the given directory. Returns an error if
// the directory is not a git repo. Results are cached for 30 seconds.
func Get(projectDir string) (*GitInfo, error) {
	cacheMu.RLock()
	entry, ok := cache[projectDir]
	cacheMu.RUnlock()
	if ok && time.Since(entry.fetchAt) < cacheTTL {
		return entry.info, nil
	}

	info, err := fetch(projectDir)
	if err != nil {
		return nil, err
	}

	cacheMu.Lock()
	cache[projectDir] = cacheEntry{info: info, fetchAt: time.Now()}
	cacheMu.Unlock()

	return info, nil
}

// InvalidateCache removes the cached entry for a directory.
func InvalidateCache(projectDir string) {
	cacheMu.Lock()
	delete(cache, projectDir)
	cacheMu.Unlock()
}

// IsRepo reports whether a directory is a git repository.
func IsRepo(projectDir string) bool {
	ctx := context.Background()
	opts := process.Options{Dir: projectDir}
	out, err := process.RunOutput(ctx, opts, "git", "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "true"
}

func fetch(projectDir string) (*GitInfo, error) {
	ctx := context.Background()
	opts := process.Options{Dir: projectDir}

	info := &GitInfo{}

	// Branch
	branchOut, err := process.RunOutput(ctx, opts, "git", "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	info.Branch = strings.TrimSpace(string(branchOut))

	// Short commit hash
	hashOut, err := process.RunOutput(ctx, opts, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, err
	}
	info.CommitHash = strings.TrimSpace(string(hashOut))

	// Remote URL (best-effort)
	remoteOut, _ := process.RunOutput(ctx, opts, "git", "remote", "get-url", "origin")
	info.RemoteURL = strings.TrimSpace(string(remoteOut))

	// Dirty state via git status --porcelain
	statusOut, err := process.RunOutput(ctx, opts, "git", "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(statusOut), "\n")
	var modified []string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if len(line) < 3 {
			continue
		}
		// Porcelain format: XY filename
		filename := strings.TrimSpace(line[2:])
		if filename != "" {
			modified = append(modified, filename)
		}
	}
	info.ModifiedFiles = modified
	info.IsDirty = len(modified) > 0

	return info, nil
}
