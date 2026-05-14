package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
)

// ErrSessionUnavailable is returned when a native CLI session ID is rejected by
// the subprocess (e.g. the session has expired or was evicted).
var ErrSessionUnavailable = errors.New("session unavailable")

// Client is the unified interface for all AI backend implementations.
type Client interface {
	Call(ctx context.Context, message, projectDir, sessionID, modelOverride string) (*CLIResponse, error)
	CallStream(ctx context.Context, message, projectDir, sessionID, modelOverride string, onToolUse func(toolName string, toolInput map[string]interface{}), onContent func(contentType, text string)) (*CLIResponse, error)
	// CallPlan invokes the CLI in planning mode. If sessionID is non-empty,
	// implementations that support native resume should continue that session.
	CallPlan(ctx context.Context, message, projectDir, sessionID, modelOverride string, onContent func(contentType, text string)) (*CLIResponse, error)
	GetModel() string
}

// CLIResponse is the JSON output from `claude -p --output-format json`.
type CLIResponse struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	IsError         bool    `json:"is_error"`
	NumTurns        int     `json:"num_turns"`
	Result          string  `json:"result"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	DurationMs      int     `json:"duration_ms"`
	ThinkingContent string  `json:"thinking_content"`
	TextContent     string  `json:"text_content"`
	// Usage mirrors Anthropic's message.usage. input_tokens carries only the
	// uncached portion; cache_read_input_tokens (0.1x rate) and
	// cache_creation_input_tokens (1.25x rate) account for prompt-cache activity.
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// TotalInputTokensWithCache returns the full Anthropic input volume:
// uncached input + cache reads + cache creations.
func (u CLIResponse) TotalInputTokensWithCache() int {
	return u.Usage.InputTokens + u.Usage.CacheReadInputTokens + u.Usage.CacheCreationInputTokens
}

// forcePromptCaching5m, when true, makes cleanEnvForCLI inject
// FORCE_PROMPT_CACHING_5M=1 into every claude-spawn env. Claude Code v2.1.108+
// honours this and forces 5m cache TTL (1.25x rate) instead of its default 1h
// (2x rate), which is the right default for short-lived Hermes workflows.
var forcePromptCaching5m atomic.Bool

// SetForcePromptCaching5m toggles process-wide injection of
// FORCE_PROMPT_CACHING_5M=1 into the claude CLI env. Used by the Hermes
// walking-agent path; safe to call multiple times.
func SetForcePromptCaching5m(v bool) {
	forcePromptCaching5m.Store(v)
}

// cleanEnvForCLI returns the current env without Claude Code nesting-detection vars.
func cleanEnvForCLI() []string {
	blocked := map[string]bool{
		"CLAUDECODE":             true,
		"CLAUDE_CODE_ENTRYPOINT": true,
		"CLAUDE_CODE_ENABLE_SDK_FILE_CHECKPOINTING": true,
		"CLAUDE_AGENT_SDK_VERSION":                  true,
	}
	var env []string
	for _, e := range os.Environ() {
		key := strings.SplitN(e, "=", 2)[0]
		if !blocked[key] {
			env = append(env, e)
		}
	}
	env = append(env, "ALICE_SKIP_HOOKS=1")
	if forcePromptCaching5m.Load() {
		env = append(env, "FORCE_PROMPT_CACHING_5M=1")
	}
	return env
}

// ExtractModelShortName converts a full model ID to a short label.
// e.g. "claude-sonnet-4-6" → "sonnet", "claude-haiku-4-5-20251001" → "haiku".
func ExtractModelShortName(fullModelID string) string {
	id := strings.ToLower(fullModelID)
	switch {
	case strings.Contains(id, "haiku"):
		return "haiku"
	case strings.Contains(id, "opus"):
		return "opus"
	case strings.Contains(id, "sonnet"):
		return "sonnet"
	case strings.Contains(id, "gpt"):
		return "gpt"
	default:
		return fullModelID
	}
}

func truncStderr(s string) string {
	const max = 500
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func formatCLIStreamError(resp *CLIResponse, maxTurns int) string {
	if resp == nil {
		return "unknown stream error"
	}
	var parts []string
	if detail := strings.TrimSpace(resp.Result); detail != "" {
		parts = append(parts, detail)
	} else {
		parts = append(parts, "stream ended with is_error=true but no result text")
	}
	if resp.NumTurns > 0 && maxTurns > 0 {
		parts = append(parts, fmt.Sprintf("turns=%d max_turns=%d", resp.NumTurns, maxTurns))
		if resp.NumTurns >= maxTurns {
			parts = append(parts, "likely exceeded --max-turns")
		}
	}
	if resp.Subtype != "" {
		parts = append(parts, "subtype="+resp.Subtype)
	}
	if partial := truncStderr(resp.TextContent); partial != "" {
		parts = append(parts, "partial_text="+partial)
	}
	return strings.Join(parts, "; ")
}
