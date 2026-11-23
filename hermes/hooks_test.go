package hermes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathGuardHook(t *testing.T) {
	hook := PathGuard("secrets/")
	ctx := context.Background()
	cases := []struct {
		name    string
		tool    string
		args    map[string]any
		wantErr string
	}{
		{"edit of a protected file", "Edit", map[string]any{"file_path": "/repo/.env"},
			`Error: path "/repo/.env" matches denylist pattern ".env"`},
		{"write under an extra denied directory", "Write", map[string]any{"path": "secrets/key.txt"},
			`matches denylist pattern "secrets/"`},
		{"bash command ending in a protected path", "Bash", map[string]any{"command": "cat /repo/.env"},
			`matches denylist pattern ".env"`},
		{"edit of an ordinary file", "Edit", map[string]any{"file_path": "main.go"}, ""},
		{"read tools are never blocked", "Read", map[string]any{"file_path": ".env"}, ""},
		{"non-string and empty arguments are ignored", "Write", map[string]any{"file_path": 42, "path": ""}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := hook(ctx, tc.tool, tc.args)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected block: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestGoBuildHook(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/hooktest\n\ngo 1.21\n")
	write("main.go", "package main\n\nfunc main() { undefinedSymbol() }\n")
	hook := GoBuild(dir)
	ctx := context.Background()

	if err := hook(ctx, "Edit", map[string]any{"file_path": "README.md"}, map[string]any{}); err != nil {
		t.Fatalf("a non-Go write must not trigger the build: %v", err)
	}
	if err := hook(ctx, "Read", map[string]any{"file_path": "main.go"}, map[string]any{}); err != nil {
		t.Fatalf("a read must not trigger the build: %v", err)
	}

	err := hook(ctx, "Edit", map[string]any{"file_path": "main.go"}, map[string]any{})
	if err == nil || !strings.HasPrefix(err.Error(), "Error: go build ./... failed: ") ||
		!strings.Contains(err.Error(), "undefinedSymbol") {
		t.Fatalf("err = %v, want a build failure quoting the compiler", err)
	}

	write("main.go", "package main\n\nfunc main() {}\n")
	if err := hook(ctx, "Write", map[string]any{"path": "main.go"}, map[string]any{}); err != nil {
		t.Fatalf("a valid module must pass: %v", err)
	}
}

func TestFileWriteDetection(t *testing.T) {
	cases := []struct {
		name               string
		tool               string
		args               map[string]any
		wantGo, wantTs, js bool
	}{
		{"go via file_path", "Edit", map[string]any{"file_path": "a.go"}, true, false, false},
		{"go via path", "Write", map[string]any{"path": "a.go"}, true, false, false},
		{"ts", "Edit", map[string]any{"file_path": "a.ts"}, false, true, false},
		{"tsx", "Write", map[string]any{"file_path": "a.tsx"}, false, true, false},
		{"json", "Write", map[string]any{"file_path": "a.json"}, false, false, true},
		{"either path key can match", "Edit", map[string]any{"path": "notes.md", "file_path": "a.go"}, true, false, false},
		{"bash is not a file write", "Bash", map[string]any{"file_path": "a.go"}, false, false, false},
		{"unrelated extension", "Edit", map[string]any{"file_path": "a.md"}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isGoFileWrite(tc.tool, tc.args); got != tc.wantGo {
				t.Errorf("isGoFileWrite = %v, want %v", got, tc.wantGo)
			}
			if got := isTsFileWrite(tc.tool, tc.args); got != tc.wantTs {
				t.Errorf("isTsFileWrite = %v, want %v", got, tc.wantTs)
			}
			if got := isJsonFileWrite(tc.tool, tc.args); got != tc.js {
				t.Errorf("isJsonFileWrite = %v, want %v", got, tc.js)
			}
		})
	}
}

func TestHookRegistry(t *testing.T) {
	reg := BuildRegistry(HooksConfig{PathGuard: true, ExtraDenyPaths: []string{"locked.txt"}}, t.TempDir())
	ctx := context.Background()
	if err := reg.RunPre(ctx, "Write", map[string]any{"file_path": "locked.txt"}); err == nil {
		t.Fatal("the configured path guard must block")
	}
	if err := reg.RunPre(ctx, "Write", map[string]any{"file_path": "free.txt"}); err != nil {
		t.Fatalf("unexpected block: %v", err)
	}

	reg.RegisterPost(func(context.Context, string, map[string]any, map[string]any) error { return factError("first") })
	reg.RegisterPost(func(context.Context, string, map[string]any, map[string]any) error { return nil })
	reg.RegisterPost(func(context.Context, string, map[string]any, map[string]any) error { return factError("second") })
	result := map[string]any{}
	reg.RunPost(ctx, "Write", nil, result)
	if got := result["validation_error"]; got != "Error: first; Error: second" {
		t.Fatalf("validation_error = %q", got)
	}

	clean := map[string]any{}
	NewHookRegistry().RunPost(ctx, "Write", nil, clean)
	if _, ok := clean["validation_error"]; ok {
		t.Fatal("no hooks means no validation_error key")
	}
}
