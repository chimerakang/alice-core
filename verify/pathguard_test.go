package verify

import (
	"reflect"
	"testing"
)

func TestPathGuard_DefaultPatterns(t *testing.T) {
	g := NewPathGuard()
	cases := []struct {
		path        string
		wantBlocked bool
		wantPattern string
	}{
		{"config.json", true, "config.json"},
		{"/srv/app/config.json", true, "config.json"},
		{"./config.json", true, "config.json"},
		{"config.json.example", false, ""},
		{"my-config.json", false, ""},

		{".env", true, ".env"},
		{"deploy/.env.production", true, ".env.production"},
		{".env.example", false, ""},
		{"environment.go", false, ""},

		{".git", true, ".git/"},
		{".git/config", true, ".git/"},
		{"/repo/.git/hooks/pre-commit", true, ".git/"},
		{"sub/.git", true, ".git/"},
		{".github/workflows/ci.yml", false, ""},
		{".gitignore", false, ""},
		{"docs/git/notes.md", false, ""},

		{"certs/server.pem", true, "*.pem"},
		{"tls.key", true, "*.key"},
		{"keys.go", false, ""},
		{"/home/u/.ssh/id_rsa", true, "id_rsa"},
		{"id_rsa.pub", false, ""},
		{"id_ed25519", true, "id_ed25519"},

		{"internal/app/main.go", false, ""},
		{"", false, ""},
	}
	for _, tc := range cases {
		pattern, blocked := g.Check(tc.path)
		if blocked != tc.wantBlocked || pattern != tc.wantPattern {
			t.Errorf("Check(%q) = (%q, %v), want (%q, %v)", tc.path, pattern, blocked, tc.wantPattern, tc.wantBlocked)
		}
	}
}

func TestPathGuard_ExtraPatterns(t *testing.T) {
	g := NewPathGuard("secrets/", "*.sqlite", "deploy/prod.yaml")
	for _, blocked := range []string{"secrets/api.txt", "a/secrets/b", "data/alice.sqlite", "deploy/prod.yaml", ".env"} {
		if _, ok := g.Check(blocked); !ok {
			t.Errorf("%q must be blocked", blocked)
		}
	}
	for _, allowed := range []string{"secrets.go", "other/prod.yaml", "data/alice.sqlite.bak"} {
		if pattern, ok := g.Check(allowed); ok {
			t.Errorf("%q must be allowed, blocked by %q", allowed, pattern)
		}
	}
}

func TestPathGuard_ExtraPatternsDoNotLeakIntoDefaults(t *testing.T) {
	before := append([]string{}, DefaultDenyPatterns...)
	_ = NewPathGuard("one")
	_ = NewPathGuard("two")
	if !reflect.DeepEqual(DefaultDenyPatterns, before) {
		t.Fatalf("DefaultDenyPatterns was mutated: %q", DefaultDenyPatterns)
	}
	if _, ok := NewPathGuard().Check("one"); ok {
		t.Fatal("a pattern from one guard leaked into another")
	}
}

func TestPathGuard_With(t *testing.T) {
	g := NewPathGuardWith("only.txt")
	if _, ok := g.Check(".env"); ok {
		t.Error("NewPathGuardWith must not include the defaults")
	}
	if _, ok := g.Check("dir/only.txt"); !ok {
		t.Error("explicit pattern must be honoured")
	}
	patterns := g.Patterns()
	patterns[0] = "changed"
	if _, ok := g.Check("only.txt"); !ok {
		t.Error("Patterns() must return a copy")
	}
}

func TestPathGuard_CheckAll(t *testing.T) {
	g := NewPathGuard()
	path, pattern, blocked := g.CheckAll("main.go", "certs/a.pem", ".env")
	if !blocked || path != "certs/a.pem" || pattern != "*.pem" {
		t.Fatalf("CheckAll = (%q, %q, %v), want the first blocked path", path, pattern, blocked)
	}
	if _, _, blocked := g.CheckAll("main.go", "README.md"); blocked {
		t.Fatal("no path should be blocked")
	}
	if _, _, blocked := g.CheckAll(); blocked {
		t.Fatal("empty input is never blocked")
	}
}
