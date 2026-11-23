// Package hermes provides tool execution hooks for the Hermes Brain-Executor architecture.
// Hooks run at tool boundaries to guard paths and validate results without reimplementing
// the Claude Code CLI toolset.
package hermes

import (
	"context"
	"fmt"
	"strings"

	"github.com/chimerakang/alice-core/verify"
)

// PreHook runs before a tool executes. Return an error to block execution.
type PreHook func(ctx context.Context, tool string, args map[string]any) error

// PostHook runs after a tool succeeds. A returned error is attached to the result
// as "validation_error" but does not fail the tool call itself.
type PostHook func(ctx context.Context, tool string, args map[string]any, result map[string]any) error

// HookRegistry holds registered pre/post hooks and executes them around tool calls.
type HookRegistry struct {
	preHooks  []PreHook
	postHooks []PostHook
}

// NewHookRegistry returns an empty registry.
func NewHookRegistry() *HookRegistry {
	return &HookRegistry{}
}

// RegisterPre adds a pre-hook that runs before every tool execution.
func (r *HookRegistry) RegisterPre(h PreHook) {
	r.preHooks = append(r.preHooks, h)
}

// RegisterPost adds a post-hook that runs after a tool succeeds.
func (r *HookRegistry) RegisterPost(h PostHook) {
	r.postHooks = append(r.postHooks, h)
}

// RunPre executes all pre-hooks in registration order. First error stops the chain.
func (r *HookRegistry) RunPre(ctx context.Context, tool string, args map[string]any) error {
	for _, h := range r.preHooks {
		if err := h(ctx, tool, args); err != nil {
			return err
		}
	}
	return nil
}

// RunPost executes all post-hooks and attaches any validation errors to result.
func (r *HookRegistry) RunPost(ctx context.Context, tool string, args map[string]any, result map[string]any) {
	var errs []string
	for _, h := range r.postHooks {
		if err := h(ctx, tool, args, result); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		result["validation_error"] = strings.Join(errs, "; ")
	}
}

// factError formats an error as a plain fact with no blame language.
func factError(format string, args ...any) error {
	return fmt.Errorf("Error: "+format, args...)
}

// ─── PathGuard ────────────────────────────────────────────────────────────────

// PathGuard returns a PreHook that blocks write operations to protected paths.
// It checks the "path" and "file_path" arguments used by Edit, Write, and Bash tools.
// The matching rules and default deny list live in verify.PathGuard.
func PathGuard(extraDenyPatterns ...string) PreHook {
	guard := verify.NewPathGuard(extraDenyPatterns...)
	writeTools := map[string]bool{
		"Edit":  true,
		"Write": true,
		"Bash":  true, // checked via heuristic, not guaranteed
	}

	return func(ctx context.Context, tool string, args map[string]any) error {
		if !writeTools[tool] {
			return nil
		}

		// Collect candidate paths from known argument keys
		var candidates []string
		for _, key := range []string{"path", "file_path", "command"} {
			if v, ok := args[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					candidates = append(candidates, s)
				}
			}
		}

		if candidate, pattern, blocked := guard.CheckAll(candidates...); blocked {
			return factError("path %q matches denylist pattern %q", candidate, pattern)
		}
		return nil
	}
}

// ─── Post-write validators ────────────────────────────────────────────────────
//
// Each validator is a verify step that runs only after a matching file write.

const validatorMaxOutputLines = 50

// GoBuild returns a PostHook that runs "go build ./..." after any .go file is written.
// workDir is the module root to run the build from.
func GoBuild(workDir string) PostHook {
	return func(ctx context.Context, tool string, args map[string]any, result map[string]any) error {
		if !isGoFileWrite(tool, args) {
			return nil
		}
		if res := verify.GoBuild().Run(ctx, workDir); !res.Passed {
			return factError("go build ./... failed: %s", validatorOutput(res))
		}
		return nil
	}
}

// isGoFileWrite returns true when the tool is writing a .go file.
func isGoFileWrite(tool string, args map[string]any) bool {
	return isFileWrite(tool) && anyPathHasSuffix(args, ".go")
}

// TscCheck returns a PostHook that runs "tsc --noEmit" after any .ts/.tsx file is written.
// workDir is the directory containing tsconfig.json.
func TscCheck(workDir string) PostHook {
	return func(ctx context.Context, tool string, args map[string]any, result map[string]any) error {
		if !isTsFileWrite(tool, args) {
			return nil
		}
		if res := verify.TscCheck().Run(ctx, workDir); !res.Passed {
			return factError("tsc --noEmit failed: %s", validatorOutput(res))
		}
		return nil
	}
}

// isTsFileWrite returns true when the tool is writing a .ts or .tsx file.
func isTsFileWrite(tool string, args map[string]any) bool {
	return isFileWrite(tool) && anyPathHasSuffix(args, ".ts", ".tsx")
}

// JsonLint returns a PostHook that runs "jq empty" after any .json file is written.
func JsonLint(workDir string) PostHook {
	return func(ctx context.Context, tool string, args map[string]any, result map[string]any) error {
		if !isJsonFileWrite(tool, args) {
			return nil
		}
		path := extractFilePath(args)
		if res := verify.JSONLint(path).Run(ctx, workDir); !res.Passed {
			return factError("jq empty %s failed: %s", path, strings.TrimSpace(validatorOutput(res)))
		}
		return nil
	}
}

// isJsonFileWrite returns true when the tool is writing a .json file.
func isJsonFileWrite(tool string, args map[string]any) bool {
	return isFileWrite(tool) && strings.HasSuffix(extractFilePath(args), ".json")
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func isFileWrite(tool string) bool {
	return tool == "Edit" || tool == "Write"
}

// anyPathHasSuffix reports whether any path argument ends in one of suffixes.
func anyPathHasSuffix(args map[string]any, suffixes ...string) bool {
	for _, key := range []string{"path", "file_path"} {
		s, _ := args[key].(string)
		for _, suffix := range suffixes {
			if strings.HasSuffix(s, suffix) {
				return true
			}
		}
	}
	return false
}

// validatorOutput returns what to show for a failed validator: its output,
// trimmed to a readable length, or the reason it produced none.
func validatorOutput(res verify.Result) string {
	if strings.TrimSpace(res.Output) == "" && res.Err != "" {
		return res.Err
	}
	return verify.TrimLines(res.Output, validatorMaxOutputLines)
}

// extractFilePath returns the first non-empty path argument from known keys.
func extractFilePath(args map[string]any) string {
	for _, key := range []string{"path", "file_path"} {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// ─── Builder ──────────────────────────────────────────────────────────────────

// HooksConfig controls which hooks are enabled.
type HooksConfig struct {
	PathGuard      bool     `json:"path_guard"`
	ExtraDenyPaths []string `json:"extra_deny_paths"`
	PostValidators []string `json:"post_validators"` // "go_build", "tsc_check", "json_lint"
	WorkDir        string   `json:"work_dir"`        // root for build validators; defaults to project dir
}

// BuildRegistry constructs a HookRegistry from config.
// projectDir is used as the default WorkDir when config.WorkDir is empty.
func BuildRegistry(cfg HooksConfig, projectDir string) *HookRegistry {
	reg := NewHookRegistry()
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = projectDir
	}

	if cfg.PathGuard {
		reg.RegisterPre(PathGuard(cfg.ExtraDenyPaths...))
	}

	validatorSet := make(map[string]bool, len(cfg.PostValidators))
	for _, v := range cfg.PostValidators {
		validatorSet[v] = true
	}

	if validatorSet["go_build"] {
		reg.RegisterPost(GoBuild(workDir))
	}
	if validatorSet["tsc_check"] {
		reg.RegisterPost(TscCheck(workDir))
	}
	if validatorSet["json_lint"] {
		reg.RegisterPost(JsonLint(workDir))
	}

	return reg
}
