package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandStep_Pass(t *testing.T) {
	res := Command("greet", "sh", "-c", "echo hello; echo warn >&2").Run(context.Background(), t.TempDir())
	if !res.Passed || res.ExitCode != 0 || res.Err != "" || res.TimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Name != "greet" {
		t.Errorf("Name = %q", res.Name)
	}
	if !strings.Contains(res.Output, "hello") || !strings.Contains(res.Output, "warn") {
		t.Errorf("Output = %q, want stdout and stderr combined", res.Output)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %s, want it measured", res.Duration)
	}
}

func TestCommandStep_FailReportsExitCodeAndOutput(t *testing.T) {
	res := Shell("echo broken; exit 3").Run(context.Background(), t.TempDir())
	if res.Passed || res.ExitCode != 3 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Err != "" || res.TimedOut {
		t.Errorf("an ordinary non-zero exit must not set Err/TimedOut: %+v", res)
	}
	if strings.TrimSpace(res.Output) != "broken" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestCommandStep_RunsInDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := Shell("test -f marker.txt").Run(context.Background(), dir); !res.Passed {
		t.Fatalf("step did not run in dir: %+v", res)
	}
	if res := Shell("test -f marker.txt").Run(context.Background(), t.TempDir()); res.Passed {
		t.Fatalf("step passed in a directory without the marker")
	}
}

func TestCommandStep_Env(t *testing.T) {
	step := Shell(`test "$VERIFY_TEST_VALUE" = yes`)
	if res := step.Run(context.Background(), t.TempDir()); res.Passed {
		t.Fatal("variable must not be set by default")
	}
	step.Env = append(os.Environ(), "VERIFY_TEST_VALUE=yes")
	if res := step.Run(context.Background(), t.TempDir()); !res.Passed {
		t.Fatalf("Env was not applied: %+v", res)
	}
}

func TestCommandStep_Timeout(t *testing.T) {
	start := time.Now()
	res := Command("slow", "sleep", "30").WithTimeout(200*time.Millisecond).Run(context.Background(), t.TempDir())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced, took %s", elapsed)
	}
	if res.Passed || !res.TimedOut || res.ExitCode != -1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.Err, "timed out after 200ms") {
		t.Errorf("Err = %q", res.Err)
	}
}

func TestCommandStep_Cancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	res := Command("slow", "sleep", "30").Run(ctx, t.TempDir())
	if res.Passed || res.TimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.Contains(res.Err, "cancelled") {
		t.Errorf("Err = %q, want a cancellation reason", res.Err)
	}
}

func TestCommandStep_MissingBinary(t *testing.T) {
	res := Command("ghost", "alice-core-no-such-binary").Run(context.Background(), t.TempDir())
	if res.Passed || res.ExitCode != -1 || res.Err == "" || res.TimedOut {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestCommandStep_OutputLimit(t *testing.T) {
	step := Shell("printf '%s' 0123456789ABCDEFGHIJ")
	step.OutputLimit = 10
	res := step.Run(context.Background(), t.TempDir())
	if !res.Passed || !strings.HasPrefix(res.Output, "0123456789") || strings.Contains(res.Output, "ABCDEFGHIJ") {
		t.Fatalf("Output = %q, want it capped at 10 bytes", res.Output)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("Output = %q, want a truncation marker", res.Output)
	}
}

func TestCommandStep_NameAndCommandLine(t *testing.T) {
	cases := []struct {
		step     *CommandStep
		wantName string
		wantCmd  string
	}{
		{Command("go_build", "go", "build", "./..."), "go_build", "go build ./..."},
		{Command("", "make"), "make", "make"},
		{Shell("npm test && npm run lint"), "npm test && npm run lint", "npm test && npm run lint"},
		{Shell("make check").WithLabel("check"), "check", "make check"},
		{GoBuild(), "go_build", "go build ./..."},
		{GoVet(), "go_vet", "go vet ./..."},
		{GoTest(), "go_test", "go test ./..."},
		{TscCheck(), "tsc_check", "tsc --noEmit"},
		{JSONLint("a.json"), "json_lint", "jq empty a.json"},
	}
	for _, tc := range cases {
		if got := tc.step.Name(); got != tc.wantName {
			t.Errorf("Name() = %q, want %q", got, tc.wantName)
		}
		if got := tc.step.CommandLine(); got != tc.wantCmd {
			t.Errorf("CommandLine() = %q, want %q", got, tc.wantCmd)
		}
	}
}

func TestRun_StopsAtFirstFailure(t *testing.T) {
	dir := t.TempDir()
	report := Run(context.Background(), dir,
		Shell("true").WithLabel("first"),
		Shell("exit 2").WithLabel("second"),
		Shell("touch should-not-exist").WithLabel("third"),
	)
	if report.Passed {
		t.Fatal("report must fail")
	}
	if len(report.Results) != 2 {
		t.Fatalf("ran %d steps, want 2 (stop at the failure)", len(report.Results))
	}
	if failed := report.Failed(); failed == nil || failed.Name != "second" || failed.ExitCode != 2 {
		t.Fatalf("Failed() = %+v", failed)
	}
	if _, err := os.Stat(filepath.Join(dir, "should-not-exist")); err == nil {
		t.Fatal("a step after the failure ran")
	}
}

func TestRunAll_ContinuesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	report := RunAll(context.Background(), dir,
		Shell("exit 1").WithLabel("first"),
		Shell("touch ran").WithLabel("second"),
	)
	if report.Passed || len(report.Results) != 2 {
		t.Fatalf("unexpected report: %+v", report)
	}
	if !report.Results[1].Passed {
		t.Errorf("second step must still run and pass")
	}
	if failed := report.Failed(); failed == nil || failed.Name != "first" {
		t.Errorf("Failed() = %+v, want the first failure", failed)
	}
}

func TestRun_AllPassAndEmpty(t *testing.T) {
	report := Run(context.Background(), t.TempDir(), Shell("true"), Shell("true"))
	if !report.Passed || len(report.Results) != 2 || report.Failed() != nil {
		t.Fatalf("unexpected report: %+v", report)
	}
	empty := Run(context.Background(), t.TempDir())
	if !empty.Passed || len(empty.Results) != 0 || empty.Failed() != nil {
		t.Fatalf("an empty sequence must pass: %+v", empty)
	}
}

func TestGoBuild_AgainstRealModule(t *testing.T) {
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no env")
	}
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/verifytest\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc main() {}\n")
	if res := GoBuild().Run(context.Background(), dir); !res.Passed {
		t.Fatalf("a valid module must build: %+v", res)
	}

	write("main.go", "package main\n\nfunc main() { undefinedSymbol() }\n")
	res := GoBuild().Run(context.Background(), dir)
	if res.Passed || res.ExitCode == 0 {
		t.Fatalf("a broken module must fail: %+v", res)
	}
	if !strings.Contains(res.Output, "undefinedSymbol") {
		t.Errorf("Output = %q, want the compiler error", res.Output)
	}
}

func TestTrimLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"under the limit is returned unchanged", "a\nb\n", 5, "a\nb\n"},
		{"exactly at the limit", "a\nb", 2, "a\nb"},
		{"over the limit", "a\nb\nc\nd", 2, "a\nb\n... (2 more lines omitted)"},
		{"trailing newlines do not count as lines", "a\nb\n\n\n", 2, "a\nb\n\n\n"},
		{"non-positive limit disables trimming", "a\nb\nc", 0, "a\nb\nc"},
		{"empty input", "", 3, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TrimLines(tc.in, tc.n); got != tc.want {
				t.Fatalf("TrimLines = %q, want %q", got, tc.want)
			}
		})
	}
}
