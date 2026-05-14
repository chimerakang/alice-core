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

	"github.com/chimerakang/alice-core/process"
)

// EnhancedCLIClient wraps CLIClient with support for file attachments.
type EnhancedCLIClient struct {
	*CLIClient
}

// NewEnhancedCLIClient returns an EnhancedCLIClient using the given model.
func NewEnhancedCLIClient(model string) *EnhancedCLIClient {
	return &EnhancedCLIClient{CLIClient: NewCLIClient(model)}
}

// CallWithFiles invokes Claude Code CLI and attaches files via --file.
func (c *EnhancedCLIClient) CallWithFiles(ctx context.Context, message string, filePaths []string, projectDir, sessionID string) (*CLIResponse, error) {
	args := []string{
		"-p",
		"--output-format", "json",
		"--model", c.Model,
		"--dangerously-skip-permissions",
		"--max-turns", c.maxTurnsStr(),
	}
	for i, fp := range filePaths {
		rel, err := c.getRelativePath(fp, projectDir)
		if err != nil {
			log.Printf("[enhanced-cli] warning: relative path for %s: %v", fp, err)
			rel = fp
		}
		args = append(args, "--file", fmt.Sprintf("img_%d:%s", i, rel))
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

// CallStreamWithFiles invokes the CLI with streaming and file attachments.
func (c *EnhancedCLIClient) CallStreamWithFiles(ctx context.Context, message string, filePaths []string, projectDir, sessionID string, onToolUse func(string, map[string]interface{}), onContent func(string, string)) (*CLIResponse, error) {
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--model", c.Model,
		"--dangerously-skip-permissions",
		"--max-turns", c.maxTurnsStr(),
		"--include-partial-messages",
	}
	for i, fp := range filePaths {
		rel, err := c.getRelativePath(fp, projectDir)
		if err != nil {
			rel = fp
		}
		args = append(args, "--file", fmt.Sprintf("img_%d:%s", i, rel))
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	args = append(args, message)

	cmd, cancel := process.Command(ctx, process.Options{
		Dir:     projectDir,
		Env:     append(os.Environ(), "ALICE_SKIP_HOOKS=1"),
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
			return finalResp, fmt.Errorf("CLI returned error: %s", formatCLIStreamError(finalResp, c.MaxTurns))
		}
		if stderrBuf.Len() > 0 {
			return nil, fmt.Errorf("claude CLI error: %s", stderrBuf.String())
		}
		return nil, fmt.Errorf("claude CLI wait: %w", err)
	}

	if finalResp == nil {
		return nil, fmt.Errorf("no final result received from claude CLI")
	}
	return finalResp, nil
}

func (c *EnhancedCLIClient) getRelativePath(filePath, projectDir string) (string, error) {
	if !filepath.IsAbs(filePath) {
		return filePath, nil
	}
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absProject, filePath)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}
