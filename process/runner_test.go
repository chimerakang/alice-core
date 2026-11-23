package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunOutput_ReturnsStdoutOnly(t *testing.T) {
	out, err := RunOutput(context.Background(), Options{}, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(out); got != "out\n" {
		t.Fatalf("stdout = %q, want %q", got, "out\n")
	}
}

func TestRunOutput_AttachesStderrToExitError(t *testing.T) {
	out, err := RunOutput(context.Background(), Options{}, "sh", "-c", "echo partial; echo boom >&2; exit 3")
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("err = %v, want *exec.ExitError", err)
	}
	if exitErr.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", exitErr.ExitCode())
	}
	if got := string(exitErr.Stderr); got != "boom\n" {
		t.Errorf("stderr = %q, want %q", got, "boom\n")
	}
	if got := string(out); got != "partial\n" {
		t.Errorf("stdout = %q, want %q", got, "partial\n")
	}
}

func TestRunCombinedOutput_MergesStreams(t *testing.T) {
	out, err := RunCombinedOutput(context.Background(), Options{}, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "out\n") || !strings.Contains(got, "err\n") {
		t.Fatalf("combined output = %q, want both streams", got)
	}
}

func TestRunOutput_DirAndEnv(t *testing.T) {
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	out, err := RunOutput(context.Background(), Options{
		Dir: dir,
		Env: append(os.Environ(), "ALICE_TEST_VALUE=hello"),
	}, "sh", "-c", "pwd -P; echo $ALICE_TEST_VALUE")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] != resolved || lines[1] != "hello" {
		t.Fatalf("output = %q, want cwd %q and env value", out, resolved)
	}
}

func TestRunOutput_TimeoutKillsProcess(t *testing.T) {
	start := time.Now()
	_, err := RunOutput(context.Background(), Options{Timeout: 200 * time.Millisecond}, "sleep", "30")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a timed-out command")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("command took %s, timeout was not enforced", elapsed)
	}
}

// A child that outlives its parent shell keeps the stdout pipe open. The
// runner must kill the whole process group, or Run blocks until the child exits.
func TestRunOutput_TimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	_, err := RunCombinedOutput(context.Background(), Options{Timeout: 200 * time.Millisecond},
		"sh", "-c", "sleep 30 & wait")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a timed-out command")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("command took %s, background child was not killed", elapsed)
	}
}

func TestRunOutput_ContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)

	start := time.Now()
	_, err := RunOutput(ctx, Options{Timeout: time.Minute}, "sleep", "30")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error from a cancelled command")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v, want context.Canceled", ctx.Err())
	}
	if elapsed > 5*time.Second {
		t.Fatalf("command took %s, cancellation was not enforced", elapsed)
	}
}

func TestRunOutput_NilContext(t *testing.T) {
	//nolint:staticcheck // nil context is an explicitly supported input.
	out, err := DefaultRunner.RunOutput(nil, Options{}, "echo", "ok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "ok\n" {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunOutput_OutputLimitTruncates(t *testing.T) {
	const marker = "\n[process output truncated]\n"

	out, err := RunOutput(context.Background(), Options{OutputLimit: 10}, "sh", "-c", "printf '%s' 0123456789ABCDEFGHIJ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := string(out), "0123456789"+marker; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}

	out, err = RunOutput(context.Background(), Options{OutputLimit: 10}, "sh", "-c", "printf '%s' 0123456789")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(out); got != "0123456789" {
		t.Fatalf("output at exactly the limit = %q, want no truncation marker", got)
	}
}

func TestRunOutput_MissingBinary(t *testing.T) {
	_, err := RunOutput(context.Background(), Options{}, "alice-core-no-such-binary")
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		t.Fatalf("err = %v, a missing binary must not look like an exit error", err)
	}
}

func TestCommand_StartWaitRun(t *testing.T) {
	cmd, cancel := Command(context.Background(), Options{CorrelationID: "abc", LogPrefix: "test"}, "sh", "-c", "exit 0")
	defer cancel()
	if err := cmd.Run(); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("command must run in its own process group")
	}
}

func TestResolveTimeout(t *testing.T) {
	cases := []struct {
		name    string
		timeout time.Duration
		binary  string
		want    time.Duration
	}{
		{"explicit wins over binary default", 5 * time.Second, "claude", 5 * time.Second},
		{"claude default", 0, "claude", 15 * time.Minute},
		{"codex default", 0, "codex", 10 * time.Minute},
		{"gh default", 0, "gh", 60 * time.Second},
		{"git default", 0, "git", 30 * time.Second},
		{"unknown binary falls back", 0, "sh", fallbackTimeout},
		{"negative treated as unset", -time.Second, "gh", 60 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveTimeout(tc.timeout, tc.binary); got != tc.want {
				t.Fatalf("resolveTimeout(%s, %q) = %s, want %s", tc.timeout, tc.binary, got, tc.want)
			}
		})
	}
}

func TestLimitedBuffer(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		writes    []string
		want      string
		truncated bool
	}{
		{"no limit", 0, []string{"abc", "def"}, "abcdef", false},
		{"under limit", 10, []string{"abc"}, "abc", false},
		{"exactly at limit", 3, []string{"abc"}, "abc", false},
		{"single write over limit", 3, []string{"abcdef"}, "abc", true},
		{"second write crosses limit", 4, []string{"abc", "def"}, "abcd", true},
		{"write after full", 3, []string{"abc", "d"}, "abc", true},
		{"empty write after full", 3, []string{"abc", ""}, "abc", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := limitedBuffer{limit: tc.limit}
			for _, w := range tc.writes {
				n, err := b.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v; must report the full length", w, n, err)
				}
			}
			if got := string(b.Bytes()); got != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if b.Truncated() != tc.truncated {
				t.Errorf("Truncated() = %v, want %v", b.Truncated(), tc.truncated)
			}
		})
	}
}

func TestLogHelpers(t *testing.T) {
	if got := logPrefix(""); got != "process" {
		t.Errorf("logPrefix(\"\") = %q", got)
	}
	if got := logPrefix("hermes.process"); got != "hermes.process" {
		t.Errorf("logPrefix = %q", got)
	}
	if got := logName("git", nil); got != "git" {
		t.Errorf("logName without args = %q", got)
	}
	if got := logName("git", []string{"status", "--short"}); got != "git status --short" {
		t.Errorf("logName = %q", got)
	}
	if got := correlationID("fixed"); got != "fixed" {
		t.Errorf("correlationID = %q", got)
	}
	a, b := correlationID(""), correlationID("")
	if len(a) != 8 || a == b {
		t.Errorf("generated correlation IDs = %q, %q; want distinct 8-char values", a, b)
	}
}
