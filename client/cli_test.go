package client

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeClaudeScript stands in for the real `claude` binary. It records how it
// was invoked and replays a canned response, all driven by FAKE_CLAUDE_* env.
const fakeClaudeScript = `#!/bin/sh
: > "$FAKE_CLAUDE_DIR/args"
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_CLAUDE_DIR/args"; done
pwd -P > "$FAKE_CLAUDE_DIR/cwd"
env > "$FAKE_CLAUDE_DIR/env"
if [ -n "$FAKE_CLAUDE_SLEEP" ]; then sleep "$FAKE_CLAUDE_SLEEP"; fi
if [ -f "$FAKE_CLAUDE_DIR/stdout" ]; then cat "$FAKE_CLAUDE_DIR/stdout"; fi
if [ -f "$FAKE_CLAUDE_DIR/stderr" ]; then cat "$FAKE_CLAUDE_DIR/stderr" >&2; fi
exit "${FAKE_CLAUDE_EXIT:-0}"
`

type fakeClaude struct {
	t   *testing.T
	dir string
}

// installFakeClaude puts a fake `claude` first on PATH for the current test.
func installFakeClaude(t *testing.T) *fakeClaude {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte(fakeClaudeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_DIR", dir)
	return &fakeClaude{t: t, dir: dir}
}

func (f *fakeClaude) stdout(s string) *fakeClaude { f.write("stdout", s); return f }
func (f *fakeClaude) stderr(s string) *fakeClaude { f.write("stderr", s); return f }
func (f *fakeClaude) exit(code string) *fakeClaude {
	f.t.Setenv("FAKE_CLAUDE_EXIT", code)
	return f
}

func (f *fakeClaude) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeClaude) read(name string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		f.t.Fatalf("fake claude did not record %s (was it invoked?): %v", name, err)
	}
	return string(b)
}

func (f *fakeClaude) args() []string {
	return strings.Split(strings.TrimSuffix(f.read("args"), "\n"), "\n")
}

func (f *fakeClaude) env() map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(f.read("env"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			env[k] = v
		}
	}
	return env
}

const okJSON = `{"type":"result","subtype":"success","session_id":"sess-1","is_error":false,"num_turns":3,` +
	`"result":"done","total_cost_usd":0.42,"duration_ms":1500,` +
	`"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":300,"cache_creation_input_tokens":4000}}`

func TestCall_BuildsArgs(t *testing.T) {
	cases := []struct {
		name          string
		model         string
		maxTurns      int
		sessionID     string
		modelOverride string
		want          []string
	}{
		{
			name: "fresh session", model: "model-a", maxTurns: 50,
			want: []string{"-p", "--output-format", "json", "--model", "model-a",
				"--dangerously-skip-permissions", "--max-turns", "50", "the prompt"},
		},
		{
			name: "resume places session before the prompt", model: "model-a", maxTurns: 50, sessionID: "sess-9",
			want: []string{"-p", "--output-format", "json", "--model", "model-a",
				"--dangerously-skip-permissions", "--max-turns", "50", "--resume", "sess-9", "the prompt"},
		},
		{
			name: "model override wins", model: "model-a", maxTurns: 50, modelOverride: "model-b",
			want: []string{"-p", "--output-format", "json", "--model", "model-b",
				"--dangerously-skip-permissions", "--max-turns", "50", "the prompt"},
		},
		{
			name: "custom max turns", model: "model-a", maxTurns: 7,
			want: []string{"-p", "--output-format", "json", "--model", "model-a",
				"--dangerously-skip-permissions", "--max-turns", "7", "the prompt"},
		},
		{
			name: "non-positive max turns falls back to 50", model: "model-a", maxTurns: 0,
			want: []string{"-p", "--output-format", "json", "--model", "model-a",
				"--dangerously-skip-permissions", "--max-turns", "50", "the prompt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := installFakeClaude(t).stdout(okJSON)
			c := &CLIClient{Model: tc.model, MaxTurns: tc.maxTurns}
			if _, err := c.Call(context.Background(), "the prompt", t.TempDir(), tc.sessionID, tc.modelOverride); err != nil {
				t.Fatalf("Call: %v", err)
			}
			if got := fake.args(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("args =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestCall_PromptIsPassedVerbatim(t *testing.T) {
	fake := installFakeClaude(t).stdout(okJSON)
	prompt := "重構 `auth.go`\n  --resume $(rm -rf /) \"quoted\""
	if _, err := NewCLIClient("m").Call(context.Background(), prompt, t.TempDir(), "", ""); err != nil {
		t.Fatalf("Call: %v", err)
	}
	// The fake records one argument per line, so a multi-line prompt spans lines.
	if got := fake.read("args"); !strings.HasSuffix(got, prompt+"\n") {
		t.Fatalf("recorded args do not end with the verbatim prompt:\n%q", got)
	}
}

func TestCall_ParsesResponse(t *testing.T) {
	installFakeClaude(t).stdout(okJSON)
	resp, err := NewCLIClient("m").Call(context.Background(), "hi", t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if resp.SessionID != "sess-1" || resp.Result != "done" || resp.NumTurns != 3 ||
		resp.TotalCostUSD != 0.42 || resp.DurationMs != 1500 || resp.IsError {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 20 {
		t.Fatalf("unexpected usage: %+v", resp.Usage)
	}
	if got := resp.TotalInputTokensWithCache(); got != 4310 {
		t.Fatalf("TotalInputTokensWithCache = %d, want 4310", got)
	}
}

func TestCall_RunsInProjectDirWithCleanEnv(t *testing.T) {
	fake := installFakeClaude(t).stdout(okJSON)
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("CLAUDE_AGENT_SDK_VERSION", "9.9.9")
	t.Setenv("ALICE_TEST_PASSTHROUGH", "kept")

	dir := t.TempDir()
	if _, err := NewCLIClient("m").Call(context.Background(), "hi", dir, "", ""); err != nil {
		t.Fatalf("Call: %v", err)
	}

	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(fake.read("cwd")); got != resolved {
		t.Errorf("cwd = %q, want %q", got, resolved)
	}

	env := fake.env()
	for _, blocked := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_AGENT_SDK_VERSION"} {
		if _, ok := env[blocked]; ok {
			t.Errorf("%s leaked into the CLI environment", blocked)
		}
	}
	if env["ALICE_SKIP_HOOKS"] != "1" {
		t.Errorf("ALICE_SKIP_HOOKS = %q, want 1", env["ALICE_SKIP_HOOKS"])
	}
	if env["ALICE_TEST_PASSTHROUGH"] != "kept" {
		t.Errorf("unrelated env var was dropped")
	}
	if _, ok := env["FORCE_PROMPT_CACHING_5M"]; ok {
		t.Errorf("FORCE_PROMPT_CACHING_5M set without opting in")
	}
}

func TestCall_ForcePromptCaching5m(t *testing.T) {
	fake := installFakeClaude(t).stdout(okJSON)
	SetForcePromptCaching5m(true)
	t.Cleanup(func() { SetForcePromptCaching5m(false) })

	if _, err := NewCLIClient("m").Call(context.Background(), "hi", t.TempDir(), "", ""); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := fake.env()["FORCE_PROMPT_CACHING_5M"]; got != "1" {
		t.Fatalf("FORCE_PROMPT_CACHING_5M = %q, want 1", got)
	}
}

func TestCall_Errors(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		exit     string
		wantResp bool
		wantErr  string
	}{
		{
			name:     "is_error in a zero-exit response",
			stdout:   `{"type":"result","is_error":true,"result":"rate limited","session_id":"s"}`,
			wantResp: true, wantErr: "CLI returned error: rate limited",
		},
		{
			name:     "non-zero exit with a parseable response keeps the response",
			stdout:   `{"type":"result","is_error":false,"result":"partial","session_id":"s"}`,
			exit:     "1",
			wantResp: true, wantErr: "CLI exited with error: partial",
		},
		{
			name:   "non-zero exit without output reports stderr",
			stderr: "auth required\n", exit: "2",
			wantErr: "claude CLI error: auth required",
		},
		{
			name:    "unparseable output",
			stdout:  "not json at all",
			wantErr: "parse CLI output",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := installFakeClaude(t)
			if tc.stdout != "" {
				fake.stdout(tc.stdout)
			}
			if tc.stderr != "" {
				fake.stderr(tc.stderr)
			}
			if tc.exit != "" {
				fake.exit(tc.exit)
			}
			resp, err := NewCLIClient("m").Call(context.Background(), "hi", t.TempDir(), "", "")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if (resp != nil) != tc.wantResp {
				t.Fatalf("resp = %+v, wantResp = %v", resp, tc.wantResp)
			}
			if resp != nil && !resp.IsError {
				t.Fatalf("a response returned alongside an error must have IsError set")
			}
		})
	}
}

func TestCall_Cancelled(t *testing.T) {
	installFakeClaude(t).stdout(okJSON)
	t.Setenv("FAKE_CLAUDE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	_, err := NewCLIClient("m").Call(ctx, "hi", t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "agent aborted by user") {
		t.Fatalf("err = %v, want an abort error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}

func TestCall_MissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := NewCLIClient("m").Call(context.Background(), "hi", t.TempDir(), "", "")
	if err == nil || !strings.Contains(err.Error(), "claude CLI exec") {
		t.Fatalf("err = %v, want a claude CLI exec error", err)
	}
}

const streamOK = `{"type":"system","subtype":"init","session_id":"sess-2"}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"plan A"},{"type":"text","text":"Reading the file."}]}}

this line is not json and must be skipped
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/a.go","limit":10}}]}}
{"type":"user","message":{"content":[{"type":"tool_result"}]}}
{"type":"assistant","message":{"content":[{"type":"thinking","thinking":""},{"type":"text","text":""},{"type":"thinking","thinking":"plan B"},{"type":"text","text":"All done."}]}}
{"type":"result","subtype":"success","session_id":"sess-2","is_error":false,"num_turns":4,"result":"All done.","total_cost_usd":1.5,"duration_ms":9000,"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}
`

func TestCallStream_BuildsArgs(t *testing.T) {
	fake := installFakeClaude(t).stdout(streamOK)
	c := &CLIClient{Model: "model-a", MaxTurns: 12}
	if _, err := c.CallStream(context.Background(), "the prompt", t.TempDir(), "sess-9", "model-b", nil, nil); err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--model", "model-b",
		"--dangerously-skip-permissions", "--max-turns", "12", "--resume", "sess-9", "the prompt"}
	if got := fake.args(); !reflect.DeepEqual(got, want) {
		t.Fatalf("args =\n  %q\nwant\n  %q", got, want)
	}
}

func TestCallStream_ParsesEvents(t *testing.T) {
	installFakeClaude(t).stdout(streamOK)

	type toolCall struct {
		name  string
		input map[string]interface{}
	}
	var tools []toolCall
	var content []string
	resp, err := NewCLIClient("m").CallStream(context.Background(), "hi", t.TempDir(), "", "",
		func(name string, input map[string]interface{}) { tools = append(tools, toolCall{name, input}) },
		func(kind, text string) { content = append(content, kind+":"+text) },
	)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}

	if len(tools) != 1 || tools[0].name != "Read" || tools[0].input["file_path"] != "/tmp/a.go" {
		t.Errorf("tool calls = %+v", tools)
	}
	wantContent := []string{"thinking:plan A", "text:Reading the file.", "thinking:plan B", "text:All done."}
	if !reflect.DeepEqual(content, wantContent) {
		t.Errorf("content callbacks = %q, want %q (empty blocks must be skipped)", content, wantContent)
	}

	if resp.SessionID != "sess-2" || resp.Result != "All done." || resp.NumTurns != 4 ||
		resp.TotalCostUSD != 1.5 || resp.DurationMs != 9000 || resp.Subtype != "success" {
		t.Errorf("unexpected response: %+v", resp)
	}
	if got := resp.TotalInputTokensWithCache(); got != 8 {
		t.Errorf("TotalInputTokensWithCache = %d, want 8", got)
	}
	if resp.Usage.OutputTokens != 2 {
		t.Errorf("OutputTokens = %d, want 2", resp.Usage.OutputTokens)
	}
	if want := "plan A\n\n---\n\nplan B"; resp.ThinkingContent != want {
		t.Errorf("ThinkingContent = %q, want %q", resp.ThinkingContent, want)
	}
	if want := "Reading the file.\n\nAll done."; resp.TextContent != want {
		t.Errorf("TextContent = %q, want %q", resp.TextContent, want)
	}
}

func TestCallStream_NilCallbacks(t *testing.T) {
	installFakeClaude(t).stdout(streamOK)
	resp, err := NewCLIClient("m").CallStream(context.Background(), "hi", t.TempDir(), "", "", nil, nil)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	if resp.TextContent == "" {
		t.Fatal("content must still be accumulated without callbacks")
	}
}

func TestCallStream_LongLine(t *testing.T) {
	big := strings.Repeat("x", 1024*1024) // well past bufio.Scanner's 64 KiB default
	installFakeClaude(t).stdout(
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + big + `"}]}}` + "\n" +
			`{"type":"result","is_error":false,"result":"ok","session_id":"s"}` + "\n")
	resp, err := NewCLIClient("m").CallStream(context.Background(), "hi", t.TempDir(), "", "", nil, nil)
	if err != nil {
		t.Fatalf("CallStream: %v", err)
	}
	if len(resp.TextContent) != len(big) {
		t.Fatalf("TextContent length = %d, want %d", len(resp.TextContent), len(big))
	}
}

func TestCallStream_Errors(t *testing.T) {
	cases := []struct {
		name     string
		stdout   string
		stderr   string
		exit     string
		maxTurns int
		wantResp bool
		wantErr  []string
	}{
		{
			name:    "no result event",
			stdout:  `{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n",
			wantErr: []string{"no result event in stream output"},
		},
		{
			name: "is_error result at the turn limit",
			stdout: `{"type":"assistant","message":{"content":[{"type":"text","text":"working on it"}]}}` + "\n" +
				`{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":5,"result":"","session_id":"s"}` + "\n",
			maxTurns: 5,
			wantResp: true,
			wantErr: []string{"CLI returned error", "no result text", "turns=5 max_turns=5",
				"likely exceeded --max-turns", "subtype=error_max_turns", "partial_text=working on it"},
		},
		{
			name:     "non-zero exit after a successful result marks the response as an error",
			stdout:   `{"type":"result","is_error":false,"result":"looked fine","session_id":"s"}` + "\n",
			exit:     "1",
			wantResp: true,
			wantErr:  []string{"CLI returned error", "looked fine"},
		},
		{
			name:    "non-zero exit without a result reports stderr",
			stderr:  "boom\n",
			exit:    "3",
			wantErr: []string{"claude CLI error: boom"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := installFakeClaude(t)
			if tc.stdout != "" {
				fake.stdout(tc.stdout)
			}
			if tc.stderr != "" {
				fake.stderr(tc.stderr)
			}
			if tc.exit != "" {
				fake.exit(tc.exit)
			}
			c := NewCLIClient("m")
			if tc.maxTurns > 0 {
				c.MaxTurns = tc.maxTurns
			}
			resp, err := c.CallStream(context.Background(), "hi", t.TempDir(), "", "", nil, nil)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %q, want it to contain %q", err, want)
				}
			}
			if (resp != nil) != tc.wantResp {
				t.Fatalf("resp = %+v, wantResp = %v", resp, tc.wantResp)
			}
			if resp != nil && !resp.IsError {
				t.Fatalf("a response returned alongside an error must have IsError set")
			}
		})
	}
}

func TestCallStream_Cancelled(t *testing.T) {
	installFakeClaude(t).stdout(streamOK)
	t.Setenv("FAKE_CLAUDE_SLEEP", "30")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	_, err := NewCLIClient("m").CallStream(ctx, "hi", t.TempDir(), "", "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "agent aborted by user") {
		t.Fatalf("err = %v, want an abort error", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("cancel took %s", elapsed)
	}
}

func TestCLIClient_GetModel(t *testing.T) {
	c := NewCLIClient("model-a")
	if c.GetModel() != "model-a" || c.MaxTurns != 50 {
		t.Fatalf("NewCLIClient = %+v", c)
	}
	var _ Client = c
}

func TestExtractModelShortName(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-6":         "sonnet",
		"claude-haiku-4-5-20251001": "haiku",
		"claude-opus-4-7":           "opus",
		"CLAUDE-OPUS-4-7":           "opus",
		"gpt-5":                     "gpt",
		"some-other-model":          "some-other-model",
		"":                          "",
	}
	for in, want := range cases {
		if got := ExtractModelShortName(in); got != want {
			t.Errorf("ExtractModelShortName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatCLIStreamError(t *testing.T) {
	if got := formatCLIStreamError(nil, 50); got != "unknown stream error" {
		t.Errorf("nil response = %q", got)
	}

	under := &CLIResponse{Result: "  tool failed  ", NumTurns: 3}
	got := formatCLIStreamError(under, 50)
	if !strings.Contains(got, "tool failed") || !strings.Contains(got, "turns=3 max_turns=50") {
		t.Errorf("under limit = %q", got)
	}
	if strings.Contains(got, "likely exceeded") {
		t.Errorf("under-limit error must not blame --max-turns: %q", got)
	}

	long := &CLIResponse{TextContent: strings.Repeat("a", 600)}
	got = formatCLIStreamError(long, 0)
	if !strings.Contains(got, "partial_text="+strings.Repeat("a", 500)+"…") {
		t.Errorf("partial text must be truncated to 500 chars: %q", got)
	}
	if strings.Contains(got, "turns=") {
		t.Errorf("turn info must be omitted when unknown: %q", got)
	}
}
