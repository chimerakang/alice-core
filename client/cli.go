package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/chimerakang/alice-core/process"
)

const defaultAgentProcessTimeout = 15 * time.Minute

// CLIClient calls Claude Code CLI as a subprocess.
type CLIClient struct {
	Model    string
	MaxTurns int // max conversation turns per CLI invocation (default 50)
}

// NewCLIClient returns a CLIClient using the given model.
func NewCLIClient(model string) *CLIClient {
	return &CLIClient{Model: model, MaxTurns: 50}
}

func (c *CLIClient) maxTurnsStr() string {
	if c.MaxTurns <= 0 {
		return "50"
	}
	return fmt.Sprintf("%d", c.MaxTurns)
}

// GetModel implements Client.
func (c *CLIClient) GetModel() string { return c.Model }

// Call invokes the Claude Code CLI in print mode.
func (c *CLIClient) Call(ctx context.Context, message, projectDir, sessionID, modelOverride string) (*CLIResponse, error) {
	model := c.Model
	if modelOverride != "" {
		model = modelOverride
	}

	args := []string{
		"-p",
		"--output-format", "json",
		"--model", model,
		"--dangerously-skip-permissions",
		"--max-turns", c.maxTurnsStr(),
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, message)

	output, err := process.RunOutput(ctx, process.Options{
		Dir:     projectDir,
		Env:     cleanEnvForCLI(),
		Timeout: defaultAgentProcessTimeout,
	}, "claude", args...)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("agent aborted by user")
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			if len(output) > 0 {
				var resp CLIResponse
				if parseErr := json.Unmarshal(output, &resp); parseErr == nil {
					if !resp.IsError {
						resp.IsError = true
					}
					return &resp, fmt.Errorf("CLI exited with error: %s", resp.Result)
				}
			}
			return nil, fmt.Errorf("claude CLI error: %s", string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("claude CLI exec: %w", err)
	}

	var resp CLIResponse
	if err := json.Unmarshal(output, &resp); err != nil {
		return nil, fmt.Errorf("parse CLI output: %w\nraw: %s", err, string(output))
	}
	if resp.IsError {
		return &resp, fmt.Errorf("CLI returned error: %s", resp.Result)
	}
	return &resp, nil
}

// CallStream invokes Claude Code CLI with stream-json output.
func (c *CLIClient) CallStream(ctx context.Context, message, projectDir, sessionID, modelOverride string, onToolUse func(string, map[string]interface{}), onContent func(string, string)) (*CLIResponse, error) {
	model := c.Model
	if modelOverride != "" {
		model = modelOverride
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--dangerously-skip-permissions",
		"--max-turns", c.maxTurnsStr(),
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, message)

	cmd, cancel := process.Command(ctx, process.Options{
		Dir:     projectDir,
		Env:     cleanEnvForCLI(),
		Timeout: defaultAgentProcessTimeout,
	}, "claude", args...)
	defer cancel()

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude CLI start: %w", err)
	}

	var finalResp *CLIResponse
	var thinkingBlocks, textBlocks []string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		switch event.Type {
		case "assistant":
			if event.Message != nil {
				for _, blk := range event.Message.Content {
					switch blk.Type {
					case "tool_use":
						if onToolUse != nil {
							onToolUse(blk.Name, blk.Input)
						}
					case "thinking":
						if blk.Thinking != "" {
							thinkingBlocks = append(thinkingBlocks, blk.Thinking)
							if onContent != nil {
								onContent("thinking", blk.Thinking)
							}
						}
					case "text":
						if blk.Text != "" {
							textBlocks = append(textBlocks, blk.Text)
							if onContent != nil {
								onContent("text", blk.Text)
							}
						}
					}
				}
			}
		case "result":
			finalResp = eventToResponse(&event)
		}
	}

	if finalResp != nil {
		finalResp.ThinkingContent = strings.Join(thinkingBlocks, "\n\n---\n\n")
		finalResp.TextContent = strings.Join(textBlocks, "\n\n")
	}

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("agent aborted by user")
		}
		if finalResp != nil {
			if !finalResp.IsError {
				finalResp.IsError = true
			}
		} else {
			if _, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("claude CLI error: %s", stderrBuf.String())
			}
			return nil, fmt.Errorf("claude CLI exec: %w", err)
		}
	}

	if finalResp == nil {
		return nil, fmt.Errorf("no result event in stream output")
	}
	if finalResp.IsError {
		return finalResp, fmt.Errorf("CLI returned error: %s", formatCLIStreamError(finalResp, c.MaxTurns))
	}
	return finalResp, nil
}

// CallPlan invokes Claude Code CLI in planning mode using the emit_plan MCP tool.
func (c *CLIClient) CallPlan(ctx context.Context, message, projectDir, sessionID, modelOverride string, onContent func(string, string)) (*CLIResponse, error) {
	model := c.Model
	if modelOverride != "" {
		model = modelOverride
	}

	mcpConfigPath, cleanupMCP, err := writePlannerEmitPlanMCPConfig()
	if err != nil {
		return nil, fmt.Errorf("planner emit_plan mcp config: %w", err)
	}
	defer cleanupMCP()

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--model", model,
		"--dangerously-skip-permissions",
		"--strict-mcp-config",
		"--mcp-config", mcpConfigPath,
		"--tools", "",
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}

	log.Printf("[cli] CallPlan: model=%s projectDir=%s len=%d", model, projectDir, len(message))
	_ = os.WriteFile("/tmp/alice_callplan_last.txt", []byte(message), 0644)

	cmd, cancel := process.Command(ctx, process.Options{
		Dir:     projectDir,
		Env:     cleanEnvForCLI(),
		Timeout: defaultAgentProcessTimeout,
	}, "claude", args...)
	defer cancel()

	cmd.Stdin = strings.NewReader(message)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude CLI start: %w", err)
	}

	var finalResp *CLIResponse
	var thinkingBlocks, textBlocks []string
	var emitPlanJSON string
	var sawEmitPlan bool
	var latestSessionID string

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal(line, &event); err != nil {
			continue
		}
		if event.SessionID != "" {
			latestSessionID = event.SessionID
		}
		switch event.Type {
		case "assistant":
			if event.Message != nil {
				for _, blk := range event.Message.Content {
					switch blk.Type {
					case "thinking":
						if blk.Thinking != "" {
							thinkingBlocks = append(thinkingBlocks, blk.Thinking)
							if onContent != nil {
								onContent("thinking", blk.Thinking)
							}
						}
					case "text":
						if blk.Text != "" {
							textBlocks = append(textBlocks, blk.Text)
							if onContent != nil {
								onContent("text", blk.Text)
							}
						}
					case "tool_use":
						if isPlannerEmitPlanTool(blk.Name) {
							if payload, ok := marshalPlannerEmitPlanPayload(blk.Input); ok {
								emitPlanJSON = payload
								sawEmitPlan = true
							}
						}
					}
				}
			}
		case "result":
			finalResp = eventToResponse(&event)
			if finalResp.SessionID == "" && latestSessionID != "" {
				finalResp.SessionID = latestSessionID
			}
		}
	}

	if finalResp != nil {
		finalResp.ThinkingContent = strings.Join(thinkingBlocks, "\n\n---\n\n")
		if sawEmitPlan && emitPlanJSON != "" {
			finalResp.TextContent = emitPlanJSON
			if finalResp.Result == "" {
				finalResp.Result = "emit_plan"
			}
		} else {
			finalResp.TextContent = strings.Join(textBlocks, "\n\n")
		}
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		if ctx.Err() == context.Canceled {
			return nil, fmt.Errorf("agent aborted by user")
		}
		if finalResp != nil {
			log.Printf("[cli] CallPlan CLI exited with error (is_error=%v, exit=%v, stderr=%q)", finalResp.IsError, waitErr, truncStderr(stderrBuf.String()))
			if !finalResp.IsError {
				finalResp.IsError = true
			}
		} else {
			if _, ok := waitErr.(*exec.ExitError); ok {
				return nil, fmt.Errorf("claude CLI error (exit=%v): %s", waitErr, stderrBuf.String())
			}
			return nil, fmt.Errorf("claude CLI exec: %w", waitErr)
		}
	}

	if finalResp == nil {
		return nil, fmt.Errorf("no result event in stream output (stderr=%q)", truncStderr(stderrBuf.String()))
	}
	if finalResp.IsError {
		detail := strings.TrimSpace(finalResp.Result)
		if detail == "" {
			detail = strings.TrimSpace(stderrBuf.String())
		}
		if detail == "" && waitErr != nil {
			detail = waitErr.Error()
		}
		if detail == "" {
			detail = "(empty CLI error — check claude CLI auth, network, or prompt size)"
		}
		return finalResp, fmt.Errorf("CLI returned error: %s", detail)
	}
	return finalResp, nil
}

// ── stream event helpers ──────────────────────────────────────────────────────

type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message *struct {
		Content []struct {
			Type     string                 `json:"type"`
			Name     string                 `json:"name"`
			Input    map[string]interface{} `json:"input"`
			Text     string                 `json:"text"`
			Thinking string                 `json:"thinking"`
		} `json:"content"`
	} `json:"message"`
	SessionID    string  `json:"session_id"`
	IsError      bool    `json:"is_error"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMs   int     `json:"duration_ms"`
	Usage        struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func eventToResponse(e *streamEvent) *CLIResponse {
	r := &CLIResponse{
		Type:         e.Type,
		Subtype:      e.Subtype,
		SessionID:    e.SessionID,
		IsError:      e.IsError,
		NumTurns:     e.NumTurns,
		Result:       e.Result,
		TotalCostUSD: e.TotalCostUSD,
		DurationMs:   e.DurationMs,
	}
	r.Usage.InputTokens = e.Usage.InputTokens
	r.Usage.OutputTokens = e.Usage.OutputTokens
	r.Usage.CacheReadInputTokens = e.Usage.CacheReadInputTokens
	r.Usage.CacheCreationInputTokens = e.Usage.CacheCreationInputTokens
	return r
}

// ── emit_plan MCP helpers ─────────────────────────────────────────────────────

const plannerEmitPlanServerName = "planner_emit_plan"
const plannerEmitPlanToolName = "emit_plan"

func isPlannerEmitPlanTool(name string) bool {
	name = strings.TrimSpace(name)
	return name == plannerEmitPlanToolName || strings.HasSuffix(name, "__"+plannerEmitPlanToolName)
}

func marshalPlannerEmitPlanPayload(input map[string]interface{}) (string, bool) {
	if len(input) == 0 {
		return "", false
	}
	payload, ok := input["sub_tasks"]
	if !ok {
		payload, ok = input["subTasks"]
	}
	if !ok {
		payload, ok = input["plan"]
	}
	if !ok {
		return "", false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func writePlannerEmitPlanMCPConfig() (string, func(), error) {
	dir, err := os.MkdirTemp("", "alice-core-planner-emit-plan-*")
	if err != nil {
		return "", nil, err
	}
	scriptPath := filepath.Join(dir, "emit_plan_mcp.py")
	if err := os.WriteFile(scriptPath, []byte(plannerEmitPlanMCPScript), 0755); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	configPath := filepath.Join(dir, "mcp.json")
	config := map[string]any{
		"mcpServers": map[string]any{
			plannerEmitPlanServerName: map[string]any{
				"command": "python3",
				"args":    []string{"-u", scriptPath},
			},
		},
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	if err := os.WriteFile(configPath, configBytes, 0644); err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return configPath, func() { _ = os.RemoveAll(dir) }, nil
}

const plannerEmitPlanMCPScript = `#!/usr/bin/env python3
import json
import sys

TOOL_NAME = "emit_plan"
TOOL_SCHEMA = {
    "type": "object",
    "additionalProperties": False,
    "properties": {
        "sub_tasks": {
            "type": "array",
            "items": {
                "type": "object",
                "additionalProperties": True,
                "properties": {
                    "id": {"type": "string"},
                    "description": {"type": "string"},
                    "tool_hints": {
                        "type": "array",
                        "items": {"type": "string"},
                    },
                },
                "required": ["id", "description", "tool_hints"],
            },
        }
    },
    "required": ["sub_tasks"],
}

def send(obj):
    sys.stdout.write(json.dumps(obj, ensure_ascii=False) + "\n")
    sys.stdout.flush()

def tool_response(arguments):
    sub_tasks = arguments.get("sub_tasks") or arguments.get("subTasks") or arguments.get("plan")
    if sub_tasks is None:
        return {"content": [{"type": "text", "text": "emit_plan tool received no sub_tasks array"}], "isError": True}
    return {
        "content": [{"type": "text", "text": json.dumps(sub_tasks, ensure_ascii=False)}],
        "structuredContent": {"sub_tasks": sub_tasks},
        "isError": False,
    }

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except json.JSONDecodeError:
        continue
    method = msg.get("method")
    req_id = msg.get("id")
    params = msg.get("params") or {}
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": req_id, "result": {
            "protocolVersion": params.get("protocolVersion", "2025-03-26"),
            "capabilities": {"tools": {"listChanged": False}},
            "serverInfo": {"name": "alice-core-planner-emit-plan", "version": "1.0.0"},
        }})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": req_id, "result": {
            "tools": [{"name": TOOL_NAME, "description": "Emit the final Hermes sub-task array and stop planning.", "inputSchema": TOOL_SCHEMA}],
        }})
    elif method == "tools/call":
        if params.get("name") != TOOL_NAME:
            send({"jsonrpc": "2.0", "id": req_id, "error": {"code": -32602, "message": "Unknown tool"}})
            continue
        send({"jsonrpc": "2.0", "id": req_id, "result": tool_response(params.get("arguments") or {})})
    elif method == "notifications/initialized":
        continue
    else:
        if req_id is not None:
            send({"jsonrpc": "2.0", "id": req_id, "error": {"code": -32601, "message": "Method not found"}})
`
