package fleet

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

var ctx = context.Background()

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// newRepo creates a repository named name with one commit on main.
func newRepo(t *testing.T, name string) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "initial")
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// newRepoWithRemote creates a repository cloned from a bare remote, so it has
// an "origin" whose HEAD points at main.
func newRepoWithRemote(t *testing.T, name string) string {
	t.Helper()
	seed := newRepo(t, "seed")
	bare := filepath.Join(t.TempDir(), "remote.git")
	gitT(t, seed, "clone", "-q", "--bare", seed, bare)
	clone := filepath.Join(t.TempDir(), name)
	gitT(t, seed, "clone", "-q", bare, clone)
	resolved, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestRegisterProject_Defaults(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "My Project")

	p, err := store.RegisterProject(ctx, Project{RepoPath: repo})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	want := Project{
		ID: "my-project", Name: "My Project", RepoPath: repo, Remote: "", DefaultBranch: "main",
		TrustMode: TrustLocalOnly, VerifyCommands: []string{}, MaxConcurrent: 1, BranchPrefix: "alice/",
		Bindings: map[string]string{}, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("project =\n  %+v\nwant\n  %+v", p, want)
	}
	if p.CreatedAt.IsZero() || !p.CreatedAt.Equal(p.UpdatedAt) {
		t.Errorf("timestamps = %v / %v", p.CreatedAt, p.UpdatedAt)
	}

	stored, err := store.GetProject(ctx, "my-project")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	// Timestamps round to milliseconds in storage.
	stored.CreatedAt, stored.UpdatedAt = p.CreatedAt, p.UpdatedAt
	if !reflect.DeepEqual(stored, p) {
		t.Fatalf("stored project =\n  %+v\nwant\n  %+v", stored, p)
	}
	if p.BranchName("3f9a1c2b7d4e") != "alice/3f9a1c2b7d4e" {
		t.Errorf("BranchName = %q", p.BranchName("3f9a1c2b7d4e"))
	}
}

func TestRegisterProject_ExplicitFields(t *testing.T) {
	store := newStore(t)
	repo := newRepoWithRemote(t, "app")
	gitT(t, repo, "branch", "develop")

	p, err := store.RegisterProject(ctx, Project{
		ID: "web", Name: "Web App", RepoPath: repo, Remote: "origin", DefaultBranch: "develop",
		TrustMode: TrustNoMistakes, MaxConcurrent: 3, BranchPrefix: "bot/",
		VerifyCommands: []string{"  go build ./...  ", "", "go test ./..."},
		Bindings:       map[string]string{"telegram.topic": "-100123:7"},
	})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if p.ID != "web" || p.Name != "Web App" || p.Remote != "origin" || p.DefaultBranch != "develop" ||
		p.TrustMode != TrustNoMistakes || p.MaxConcurrent != 3 || p.BranchPrefix != "bot/" {
		t.Fatalf("project = %+v", p)
	}
	if !reflect.DeepEqual(p.VerifyCommands, []string{"go build ./...", "go test ./..."}) {
		t.Errorf("VerifyCommands = %q, want trimmed with blanks dropped", p.VerifyCommands)
	}
	stored, _ := store.GetProject(ctx, "web")
	if stored.Bindings["telegram.topic"] != "-100123:7" || len(stored.VerifyCommands) != 2 {
		t.Errorf("stored = %+v", stored)
	}
}

func TestRegisterProject_DetectsRemoteAndRemoteHead(t *testing.T) {
	store := newStore(t)
	repo := newRepoWithRemote(t, "app")
	// Check out another branch locally: the default must still follow origin/HEAD.
	gitT(t, repo, "checkout", "-q", "-b", "scratch")

	p, err := store.RegisterProject(ctx, Project{RepoPath: repo, TrustMode: TrustDirectPR})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if p.Remote != "origin" || p.DefaultBranch != "main" {
		t.Fatalf("remote=%q default=%q, want origin/main", p.Remote, p.DefaultBranch)
	}
}

func TestRegisterProject_ResolvesSubdirectoryToRepoRoot(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "mono")
	sub := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := store.RegisterProject(ctx, Project{RepoPath: sub})
	if err != nil {
		t.Fatalf("RegisterProject: %v", err)
	}
	if p.RepoPath != repo || p.ID != "mono" {
		t.Fatalf("project = %+v, want the repository root", p)
	}
	if _, err := store.RegisterProject(ctx, Project{ID: "other", RepoPath: repo}); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("the same repository under another ID: err = %v, want ErrProjectExists", err)
	}
}

func TestRegisterProject_Rejections(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "app")
	plain := t.TempDir()
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		project Project
		want    error
	}{
		{"empty path", Project{}, ErrInvalidProject},
		{"missing directory", Project{RepoPath: filepath.Join(plain, "nope")}, ErrNotGitRepo},
		{"path is a file", Project{RepoPath: file}, ErrNotGitRepo},
		{"directory without a repository", Project{RepoPath: plain}, ErrNotGitRepo},
		{"default branch does not exist", Project{RepoPath: repo, DefaultBranch: "release"}, ErrBranchNotFound},
		{"remote does not exist", Project{RepoPath: repo, Remote: "upstream"}, ErrRemoteNotFound},
		{"pushing mode without a remote", Project{RepoPath: repo, TrustMode: TrustDirectPR}, ErrInvalidProject},
		{"unknown trust mode", Project{RepoPath: repo, TrustMode: "yolo"}, ErrInvalidProject},
		{"negative concurrency", Project{RepoPath: repo, MaxConcurrent: -1}, ErrInvalidProject},
		{"ID with a path separator", Project{RepoPath: repo, ID: "a/b"}, ErrInvalidProject},
		{"ID with uppercase", Project{RepoPath: repo, ID: "App"}, ErrInvalidProject},
		{"ID that escapes the worktree root", Project{RepoPath: repo, ID: ".."}, ErrInvalidProject},
		{"branch prefix git rejects", Project{RepoPath: repo, BranchPrefix: "bad prefix/"}, ErrInvalidProject},
		{"branch prefix with a parent reference", Project{RepoPath: repo, BranchPrefix: "a..b/"}, ErrInvalidProject},
		{"empty binding key", Project{RepoPath: repo, Bindings: map[string]string{" ": "x"}}, ErrInvalidProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.RegisterProject(ctx, tc.project); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if projects, _ := store.ListProjects(ctx); len(projects) != 0 {
		t.Fatalf("a rejected registration must store nothing: %+v", projects)
	}
}

func TestRegisterProject_DetachedHeadNeedsExplicitBranch(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "app")
	gitT(t, repo, "checkout", "-q", "--detach")

	if _, err := store.RegisterProject(ctx, Project{RepoPath: repo}); !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("err = %v, want ErrInvalidProject", err)
	}
	p, err := store.RegisterProject(ctx, Project{RepoPath: repo, DefaultBranch: "main"})
	if err != nil || p.DefaultBranch != "main" {
		t.Fatalf("explicit branch on a detached HEAD: %+v, %v", p, err)
	}
}

func TestRegisterProject_DuplicateID(t *testing.T) {
	store := newStore(t)
	if _, err := store.RegisterProject(ctx, Project{ID: "app", RepoPath: newRepo(t, "one")}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RegisterProject(ctx, Project{ID: "app", RepoPath: newRepo(t, "two")}); !errors.Is(err, ErrProjectExists) {
		t.Fatalf("err = %v, want ErrProjectExists", err)
	}
}

func TestListAndGetProjects(t *testing.T) {
	store := newStore(t)
	repoB := newRepo(t, "bravo")
	for _, name := range []string{"charlie", "alpha"} {
		if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, name)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: repoB}); err != nil {
		t.Fatal(err)
	}

	projects, err := store.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var ids []string
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	if !reflect.DeepEqual(ids, []string{"alpha", "bravo", "charlie"}) {
		t.Fatalf("ids = %v, want them sorted", ids)
	}

	byPath, err := store.GetProjectByRepoPath(ctx, repoB)
	if err != nil || byPath.ID != "bravo" {
		t.Fatalf("GetProjectByRepoPath = %+v, %v", byPath, err)
	}
	if _, err := store.GetProject(ctx, "missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if _, err := store.GetProjectByRepoPath(ctx, "/nowhere"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestUpdateProject(t *testing.T) {
	store := newStore(t)
	repo := newRepoWithRemote(t, "app")
	created, err := store.RegisterProject(ctx, Project{RepoPath: repo, Bindings: map[string]string{"a": "1", "b": "2"}})
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return created.CreatedAt.Add(time.Hour) }

	updated, err := store.UpdateProject(ctx, "app", func(p *Project) error {
		p.Name = "Renamed"
		p.TrustMode = TrustDirectPR
		p.MaxConcurrent = 4
		p.VerifyCommands = append(p.VerifyCommands, "make check")
		delete(p.Bindings, "a")
		p.Bindings["c"] = "3"
		return nil
	})
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	stored, _ := store.GetProject(ctx, "app")
	if stored.Name != "Renamed" || stored.TrustMode != TrustDirectPR || stored.MaxConcurrent != 4 ||
		!reflect.DeepEqual(stored.VerifyCommands, []string{"make check"}) {
		t.Fatalf("stored = %+v", stored)
	}
	if !reflect.DeepEqual(stored.Bindings, map[string]string{"b": "2", "c": "3"}) {
		t.Errorf("Bindings = %v", stored.Bindings)
	}
	if !stored.CreatedAt.Equal(created.CreatedAt.Truncate(time.Millisecond)) || !stored.UpdatedAt.After(stored.CreatedAt) {
		t.Errorf("timestamps: created=%v updated=%v", stored.CreatedAt, stored.UpdatedAt)
	}
	if updated.Name != "Renamed" {
		t.Errorf("returned project = %+v", updated)
	}
}

func TestUpdateProject_RejectedChangesAreNotSaved(t *testing.T) {
	store := newStore(t)
	if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, "app")}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("changed my mind")
	cases := map[string]struct {
		change func(*Project) error
		want   error
	}{
		"callback error":           {func(p *Project) error { p.Name = "x"; return boom }, boom},
		"ID change":                {func(p *Project) error { p.ID = "other"; return nil }, ErrInvalidProject},
		"missing default branch":   {func(p *Project) error { p.DefaultBranch = "gone"; return nil }, ErrBranchNotFound},
		"pushing mode, no remote":  {func(p *Project) error { p.TrustMode = TrustNoMistakes; return nil }, ErrInvalidProject},
		"path that is not a repo":  {func(p *Project) error { p.RepoPath = t.TempDir(); return nil }, ErrNotGitRepo},
		"negative max concurrency": {func(p *Project) error { p.MaxConcurrent = -2; return nil }, ErrInvalidProject},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := store.UpdateProject(ctx, "app", tc.change); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			stored, _ := store.GetProject(ctx, "app")
			if stored.Name != "app" || stored.TrustMode != TrustLocalOnly || stored.DefaultBranch != "main" || stored.MaxConcurrent != 1 {
				t.Fatalf("a rejected update changed the project: %+v", stored)
			}
		})
	}
	if _, err := store.UpdateProject(ctx, "missing", func(*Project) error { return nil }); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestBindings(t *testing.T) {
	store := newStore(t)
	for _, name := range []string{"alpha", "bravo"} {
		if _, err := store.RegisterProject(ctx, Project{RepoPath: newRepo(t, name)}); err != nil {
			t.Fatal(err)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(store.SetBinding(ctx, "alpha", "telegram.topic", "-100:1"))
	must(store.SetBinding(ctx, "bravo", "telegram.topic", "-100:2"))
	must(store.SetBinding(ctx, "bravo", "owner", "me"))
	must(store.SetBinding(ctx, "alpha", "owner", "me"))

	found, err := store.ProjectsByBinding(ctx, "telegram.topic", "-100:2")
	if err != nil || len(found) != 1 || found[0].ID != "bravo" || found[0].Bindings["owner"] != "me" {
		t.Fatalf("ProjectsByBinding = %+v, %v", found, err)
	}
	shared, _ := store.ProjectsByBinding(ctx, "owner", "me")
	if len(shared) != 2 || shared[0].ID != "alpha" || shared[1].ID != "bravo" {
		t.Fatalf("a value shared by two projects must return both, sorted: %+v", shared)
	}
	if none, _ := store.ProjectsByBinding(ctx, "telegram.topic", "-100:9"); len(none) != 0 {
		t.Fatalf("unknown binding = %+v", none)
	}

	// Setting an existing key replaces its value.
	must(store.SetBinding(ctx, "alpha", "telegram.topic", "-100:5"))
	if old, _ := store.ProjectsByBinding(ctx, "telegram.topic", "-100:1"); len(old) != 0 {
		t.Fatal("the old value must no longer match")
	}
	p, _ := store.GetProject(ctx, "alpha")
	if !reflect.DeepEqual(p.Bindings, map[string]string{"telegram.topic": "-100:5", "owner": "me"}) {
		t.Fatalf("Bindings = %v", p.Bindings)
	}

	must(store.DeleteBinding(ctx, "alpha", "owner"))
	must(store.DeleteBinding(ctx, "alpha", "owner")) // deleting twice is fine
	if p, _ = store.GetProject(ctx, "alpha"); len(p.Bindings) != 1 {
		t.Fatalf("Bindings after delete = %v", p.Bindings)
	}

	if err := store.SetBinding(ctx, "missing", "k", "v"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("SetBinding on a missing project: %v", err)
	}
	if err := store.DeleteBinding(ctx, "missing", "k"); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("DeleteBinding on a missing project: %v", err)
	}
	if err := store.SetBinding(ctx, "alpha", "  ", "v"); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("empty key: %v", err)
	}
}

func TestDeleteProject(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "app")
	if _, err := store.RegisterProject(ctx, Project{RepoPath: repo, Bindings: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	task, err := store.CreateTask(ctx, NewTask{ProjectID: "app", Goal: "do it"})
	if err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteProject(ctx, "app"); !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("err = %v, want ErrProjectHasTasks", err)
	}
	// Finished tasks still count: history must not be orphaned.
	if _, err := store.Transition(ctx, task.ID, StatusCancelled, "withdrawn"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, "app"); !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("with only finished tasks: err = %v, want ErrProjectHasTasks", err)
	}
	if _, err := store.GetProject(ctx, "app"); err != nil {
		t.Fatalf("a refused delete must keep the project: %v", err)
	}

	// A project without tasks can be removed, and its repository re-registered.
	other := newRepo(t, "empty")
	if _, err := store.RegisterProject(ctx, Project{RepoPath: other, Bindings: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteProject(ctx, "empty"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := store.GetProject(ctx, "empty"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
	if found, _ := store.ProjectsByBinding(ctx, "k", "v"); len(found) != 1 || found[0].ID != "app" {
		t.Fatalf("bindings of a deleted project must be gone: %+v", found)
	}
	if _, err := store.RegisterProject(ctx, Project{RepoPath: other}); err != nil {
		t.Fatalf("re-registering after delete: %v", err)
	}
	if err := store.DeleteProject(ctx, "missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("err = %v, want ErrProjectNotFound", err)
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"My Project":            "my-project",
		"alice-core":            "alice-core",
		"  Web_App v2.1  ":      "web_app-v2.1",
		"遊戲 Server":             "server",
		"純中文":                   "",
		"--weird--name--":       "weird-name",
		"a/b\\c":                "a-b-c",
		"..":                    "",
		strings.Repeat("x", 80): strings.Repeat("x", 63),
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
		if got := Slug(in); got != "" && !projectIDRe.MatchString(got) {
			t.Errorf("Slug(%q) = %q is not a valid project ID", in, got)
		}
	}
}

func TestParseTrustMode(t *testing.T) {
	for in, want := range map[string]TrustMode{
		"local-only": TrustLocalOnly, " Direct-PR ": TrustDirectPR, "NO-MISTAKES": TrustNoMistakes,
	} {
		if got, err := ParseTrustMode(in); err != nil || got != want {
			t.Errorf("ParseTrustMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "strict", "direct_pr"} {
		if _, err := ParseTrustMode(in); !errors.Is(err, ErrInvalidProject) {
			t.Errorf("ParseTrustMode(%q): err = %v, want ErrInvalidProject", in, err)
		}
	}
	if TrustLocalOnly.NeedsRemote() || !TrustDirectPR.NeedsRemote() || !TrustNoMistakes.NeedsRemote() {
		t.Error("only the pushing modes need a remote")
	}
}

// A non-ASCII directory name cannot form an ID, so one must be supplied.
func TestRegisterProject_NonASCIIName(t *testing.T) {
	store := newStore(t)
	repo := newRepo(t, "遊戲專案")
	if _, err := store.RegisterProject(ctx, Project{RepoPath: repo}); !errors.Is(err, ErrInvalidProject) {
		t.Fatalf("err = %v, want ErrInvalidProject", err)
	}
	p, err := store.RegisterProject(ctx, Project{ID: "game", RepoPath: repo})
	if err != nil || p.Name != "遊戲專案" {
		t.Fatalf("with an explicit ID: %+v, %v", p, err)
	}
}

func TestStore_PersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.db")
	repo := newRepo(t, "app")

	first, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RegisterProject(ctx, Project{RepoPath: repo, Bindings: map[string]string{"k": "v"}}); err != nil {
		t.Fatal(err)
	}
	task, err := first.CreateTask(ctx, NewTask{ProjectID: "app", Goal: "survive a restart"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Transition(ctx, task.ID, StatusRunning, "slot free"); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	p, err := second.GetProject(ctx, "app")
	if err != nil || p.Bindings["k"] != "v" {
		t.Fatalf("project after reopen = %+v, %v", p, err)
	}
	got, err := second.GetTask(ctx, task.ID)
	if err != nil || got.Status != StatusRunning || got.Goal != "survive a restart" || got.StartedAt == nil {
		t.Fatalf("task after reopen = %+v, %v", got, err)
	}
	if events, _ := second.ListEvents(ctx, task.ID, 0, 0); len(events) != 2 {
		t.Fatalf("events after reopen = %+v", events)
	}
}

func TestOpenStore_Errors(t *testing.T) {
	if _, err := OpenStore("  "); err == nil {
		t.Error("an empty path must be rejected")
	}
	if _, err := NewStore(nil); err == nil {
		t.Error("a nil database must be rejected")
	}
	if _, err := OpenStore(filepath.Join(t.TempDir(), "no", "such", "dir", "fleet.db")); err == nil {
		t.Error("an unreachable path must be rejected")
	}
}

// gitCmd builds a git command without failing the test, for checks whose
// exit status is the answer.
func gitCmd(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd
}

func gitOut(dir string, args ...string) (string, error) {
	out, err := gitCmd(dir, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
