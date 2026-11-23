package verify

import (
	"path/filepath"
	"strings"
)

// DefaultDenyPatterns are the path patterns a PathGuard blocks by default:
// runtime secrets, credentials, and git internals.
var DefaultDenyPatterns = []string{
	"config.json",
	".git/",
	".git",
	".env",
	".env.local",
	".env.production",
	"*.pem",
	"*.key",
	"id_rsa",
	"id_ed25519",
}

// PathGuard decides whether a path is off-limits for writes.
//
// A pattern matches when it equals the path's base name, glob-matches the base
// name, or equals the whole cleaned path. A pattern ending in "/" matches the
// named directory as a path component anywhere in the path.
type PathGuard struct {
	patterns []string
}

// NewPathGuard returns a guard using DefaultDenyPatterns plus extra.
func NewPathGuard(extra ...string) *PathGuard {
	patterns := make([]string, 0, len(DefaultDenyPatterns)+len(extra))
	patterns = append(patterns, DefaultDenyPatterns...)
	patterns = append(patterns, extra...)
	return &PathGuard{patterns: patterns}
}

// NewPathGuardWith returns a guard using exactly the given patterns.
func NewPathGuardWith(patterns ...string) *PathGuard {
	return &PathGuard{patterns: append([]string{}, patterns...)}
}

// Patterns returns a copy of the guard's deny patterns.
func (g *PathGuard) Patterns() []string {
	return append([]string{}, g.patterns...)
}

// Check reports whether path is blocked and, if so, by which pattern.
func (g *PathGuard) Check(path string) (pattern string, blocked bool) {
	clean := filepath.Clean(path)
	base := filepath.Base(clean)

	for _, pattern := range g.patterns {
		// Exact base name match
		if base == pattern {
			return pattern, true
		}
		// Directory pattern (e.g. ".git/"): match as a path component, not a prefix
		if strings.HasSuffix(pattern, "/") {
			dir := strings.TrimSuffix(pattern, "/")
			if clean == dir ||
				strings.HasPrefix(clean, dir+"/") ||
				strings.Contains(clean, "/"+dir+"/") ||
				strings.HasSuffix(clean, "/"+dir) {
				return pattern, true
			}
			continue
		}
		// Glob match on base name
		if matched, _ := filepath.Match(pattern, base); matched {
			return pattern, true
		}
		// Exact full path match
		if clean == pattern {
			return pattern, true
		}
	}
	return "", false
}

// CheckAll returns the first blocked path among paths, with its pattern.
func (g *PathGuard) CheckAll(paths ...string) (path, pattern string, blocked bool) {
	for _, p := range paths {
		if pat, ok := g.Check(p); ok {
			return p, pat, true
		}
	}
	return "", "", false
}
