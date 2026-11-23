package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var ctx = context.Background()

// newRepo creates a repository with one commit on main and returns its path.
func newRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n")
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.log\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "initial")
	return repo
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestCreate(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "proj", "task-1")

	wt, err := Create(ctx, repo, path, "alice/task-1", "main")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !samePath(wt.Path, path) || wt.Branch != "alice/task-1" || wt.Main || wt.Detached {
		t.Fatalf("worktree = %+v", wt)
	}
	if wt.Head != gitT(t, repo, "rev-parse", "main") {
		t.Errorf("Head = %s, want the tip of main", wt.Head)
	}
	if !exists(filepath.Join(path, "README.md")) {
		t.Error("the checkout must contain the repository's files")
	}
	if got := gitT(t, path, "rev-parse", "--abbrev-ref", "HEAD"); got != "alice/task-1" {
		t.Errorf("checked-out branch = %q", got)
	}
	if got := gitT(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("the main working tree must stay on main, got %q", got)
	}
}

func TestCreate_FromSpecificBase(t *testing.T) {
	repo := newRepo(t)
	first := gitT(t, repo, "rev-parse", "HEAD")
	writeFile(t, filepath.Join(repo, "second.txt"), "2\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-q", "-m", "second")

	old, err := Create(ctx, repo, filepath.Join(t.TempDir(), "old"), "from-first", first)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if old.Head != first || exists(filepath.Join(old.Path, "second.txt")) {
		t.Fatalf("worktree must start at the given base: %+v", old)
	}

	head, err := Create(ctx, repo, filepath.Join(t.TempDir(), "head"), "from-head", "")
	if err != nil {
		t.Fatalf("Create with empty base: %v", err)
	}
	if head.Head != gitT(t, repo, "rev-parse", "HEAD") {
		t.Fatalf("an empty base must mean the current HEAD: %+v", head)
	}
}

func TestCreate_Rejections(t *testing.T) {
	repo := newRepo(t)

	t.Run("existing branch", func(t *testing.T) {
		_, err := Create(ctx, repo, filepath.Join(t.TempDir(), "a"), "main", "main")
		if !errors.Is(err, ErrBranchExists) {
			t.Fatalf("err = %v, want ErrBranchExists", err)
		}
	})

	t.Run("non-empty target directory", func(t *testing.T) {
		path := t.TempDir()
		writeFile(t, filepath.Join(path, "occupied.txt"), "x")
		_, err := Create(ctx, repo, path, "alice/occupied", "main")
		if !errors.Is(err, ErrPathExists) {
			t.Fatalf("err = %v, want ErrPathExists", err)
		}
		if exists(filepath.Join(path, "README.md")) || branchExists(ctx, repo, "alice/occupied") {
			t.Fatal("a rejected create must leave no checkout and no branch behind")
		}
	})

	t.Run("target is a file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		writeFile(t, path, "x")
		if _, err := Create(ctx, repo, path, "alice/file", "main"); !errors.Is(err, ErrPathExists) {
			t.Fatalf("err = %v, want ErrPathExists", err)
		}
	})

	t.Run("empty target directory is allowed", func(t *testing.T) {
		if _, err := Create(ctx, repo, t.TempDir(), "alice/empty-dir", "main"); err != nil {
			t.Fatalf("Create into an empty directory: %v", err)
		}
	})

	t.Run("unknown base", func(t *testing.T) {
		_, err := Create(ctx, repo, filepath.Join(t.TempDir(), "b"), "alice/nobase", "no-such-ref")
		if err == nil {
			t.Fatal("expected an error for an unknown base")
		}
		if branchExists(ctx, repo, "alice/nobase") {
			t.Fatal("a failed create must not leave the branch behind")
		}
	})

	t.Run("missing branch name", func(t *testing.T) {
		if _, err := Create(ctx, repo, filepath.Join(t.TempDir(), "c"), "  ", "main"); err == nil {
			t.Fatal("expected an error for an empty branch")
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		if _, err := Create(ctx, t.TempDir(), filepath.Join(t.TempDir(), "d"), "x", ""); err == nil {
			t.Fatal("expected an error outside a repository")
		}
	})
}

func TestIsDirtyAndChanges(t *testing.T) {
	repo := newRepo(t)
	wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "alice/dirty", "main")
	if err != nil {
		t.Fatal(err)
	}

	assertDirty := func(want bool, why string) {
		t.Helper()
		got, err := IsDirty(ctx, wt.Path)
		if err != nil {
			t.Fatalf("IsDirty: %v", err)
		}
		if got != want {
			t.Fatalf("IsDirty = %v, want %v (%s)", got, want, why)
		}
	}

	assertDirty(false, "fresh checkout")

	writeFile(t, filepath.Join(wt.Path, "ignored.log"), "noise")
	assertDirty(false, "ignored files do not count")

	writeFile(t, filepath.Join(wt.Path, "new.txt"), "x")
	assertDirty(true, "untracked file")
	changes, err := Changes(ctx, wt.Path)
	if err != nil || !reflect.DeepEqual(changes, []string{"?? new.txt"}) {
		t.Fatalf("Changes = %q, %v", changes, err)
	}

	gitT(t, wt.Path, "add", "new.txt")
	assertDirty(true, "staged file")

	gitT(t, wt.Path, "commit", "-q", "-m", "add new")
	assertDirty(false, "committed work is not dirty")

	writeFile(t, filepath.Join(wt.Path, "README.md"), "changed\n")
	assertDirty(true, "modified tracked file")

	if dirty, _ := IsDirty(ctx, repo); dirty {
		t.Fatal("changes in a worktree must not make the main working tree dirty")
	}
}

func TestIsDirty_NotAWorktree(t *testing.T) {
	if _, err := IsDirty(ctx, t.TempDir()); !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("plain directory: err = %v, want ErrNotWorktree", err)
	}
	if _, err := IsDirty(ctx, filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("missing directory: err = %v, want ErrNotWorktree", err)
	}
}

func TestRemove_Clean(t *testing.T) {
	repo := newRepo(t)
	wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "alice/clean", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "work.txt"), "done")
	gitT(t, wt.Path, "add", ".")
	gitT(t, wt.Path, "commit", "-q", "-m", "work")

	if err := Remove(ctx, wt.Path, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if exists(wt.Path) {
		t.Fatal("the checkout must be gone")
	}
	if !branchExists(ctx, repo, "alice/clean") {
		t.Fatal("the branch must survive removal")
	}
	if got := gitT(t, repo, "show", "alice/clean:work.txt"); got != "done" {
		t.Fatalf("committed work must stay on the branch, got %q", got)
	}
	list, _ := List(ctx, repo)
	if len(list) != 1 {
		t.Fatalf("worktree list = %+v, want only the main working tree", list)
	}
}

func TestRemove_DirtyIsRefusedUnlessForced(t *testing.T) {
	cases := map[string]func(t *testing.T, path string){
		"untracked file": func(t *testing.T, path string) { writeFile(t, filepath.Join(path, "new.txt"), "x") },
		"modified file":  func(t *testing.T, path string) { writeFile(t, filepath.Join(path, "README.md"), "edit\n") },
		"staged file": func(t *testing.T, path string) {
			writeFile(t, filepath.Join(path, "staged.txt"), "x")
			gitT(t, path, "add", "staged.txt")
		},
	}
	for name, makeDirty := range cases {
		t.Run(name, func(t *testing.T) {
			repo := newRepo(t)
			wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "alice/dirty", "main")
			if err != nil {
				t.Fatal(err)
			}
			makeDirty(t, wt.Path)

			if err := Remove(ctx, wt.Path, false); !errors.Is(err, ErrDirty) {
				t.Fatalf("err = %v, want ErrDirty", err)
			}
			if !exists(wt.Path) {
				t.Fatal("a refused removal must leave the checkout intact")
			}

			if err := Remove(ctx, wt.Path, true); err != nil {
				t.Fatalf("forced Remove: %v", err)
			}
			if exists(wt.Path) {
				t.Fatal("a forced removal must delete the checkout")
			}
		})
	}
}

func TestRemove_RefusesMainWorktree(t *testing.T) {
	repo := newRepo(t)
	wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "alice/x", "main")
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if err := Remove(ctx, repo, force); !errors.Is(err, ErrMainWorktree) {
			t.Fatalf("force=%v: err = %v, want ErrMainWorktree", force, err)
		}
	}
	if !exists(filepath.Join(repo, "README.md")) || !exists(wt.Path) {
		t.Fatal("refusing must not touch anything")
	}
}

func TestRemove_NotAWorktree(t *testing.T) {
	repo := newRepo(t)
	wt, err := Create(ctx, repo, filepath.Join(t.TempDir(), "wt"), "alice/x", "main")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(wt.Path, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "keep.txt"), "x")
	gitT(t, wt.Path, "add", ".")
	gitT(t, wt.Path, "commit", "-q", "-m", "sub")

	for name, path := range map[string]string{
		"plain directory":            t.TempDir(),
		"missing path":               filepath.Join(t.TempDir(), "missing"),
		"subdirectory of a worktree": sub,
	} {
		if err := Remove(ctx, path, true); !errors.Is(err, ErrNotWorktree) {
			t.Errorf("%s: err = %v, want ErrNotWorktree", name, err)
		}
	}
	if !exists(filepath.Join(sub, "keep.txt")) {
		t.Fatal("removing a subdirectory path must not delete the worktree")
	}
}

func TestAttach(t *testing.T) {
	repo := newRepo(t)
	path := filepath.Join(t.TempDir(), "wt")
	wt, err := Create(ctx, repo, path, "alice/resume", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt.Path, "progress.txt"), "half")
	gitT(t, wt.Path, "add", ".")
	gitT(t, wt.Path, "commit", "-q", "-m", "progress")
	if err := Remove(ctx, wt.Path, false); err != nil {
		t.Fatal(err)
	}

	again, err := Attach(ctx, repo, path, "alice/resume")
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if again.Branch != "alice/resume" || !exists(filepath.Join(path, "progress.txt")) {
		t.Fatalf("attached worktree must carry the branch's work: %+v", again)
	}

	if _, err := Attach(ctx, repo, filepath.Join(t.TempDir(), "other"), "alice/resume"); err == nil {
		t.Fatal("a branch checked out in one worktree must not attach to a second")
	}
	if _, err := Attach(ctx, repo, filepath.Join(t.TempDir(), "none"), "no-such-branch"); err == nil {
		t.Fatal("expected an error for an unknown branch")
	}
}

func TestList(t *testing.T) {
	repo := newRepo(t)
	a, err := Create(ctx, repo, filepath.Join(t.TempDir(), "a"), "alice/a", "main")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(ctx, repo, filepath.Join(t.TempDir(), "b"), "alice/b", "main")
	if err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, repo)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %+v, want main plus two", list)
	}
	if !list[0].Main || !samePath(list[0].Path, repo) || list[0].Branch != "main" {
		t.Errorf("first entry must be the main working tree: %+v", list[0])
	}
	branches := map[string]bool{}
	for _, wt := range list[1:] {
		if wt.Main {
			t.Errorf("linked worktree marked as main: %+v", wt)
		}
		branches[wt.Branch] = true
	}
	if !branches["alice/a"] || !branches["alice/b"] {
		t.Errorf("branches = %v", branches)
	}

	// Listing from inside a linked worktree sees the same set.
	fromLinked, err := List(ctx, a.Path)
	if err != nil || len(fromLinked) != 3 || !fromLinked[0].Main {
		t.Fatalf("List from a linked worktree = %+v, %v", fromLinked, err)
	}

	// A worktree whose directory vanished is reported as prunable, then pruned.
	if err := os.RemoveAll(b.Path); err != nil {
		t.Fatal(err)
	}
	list, _ = List(ctx, repo)
	var orphan *Worktree
	for i := range list {
		if list[i].Branch == "alice/b" {
			orphan = &list[i]
		}
	}
	if orphan == nil || !orphan.Prunable {
		t.Fatalf("a vanished worktree must be listed as prunable: %+v", list)
	}
	if err := Prune(ctx, repo); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if list, _ = List(ctx, repo); len(list) != 2 {
		t.Fatalf("after prune list = %+v", list)
	}

	if _, err := List(ctx, t.TempDir()); !errors.Is(err, ErrNotWorktree) {
		t.Fatalf("List outside a repository: err = %v, want ErrNotWorktree", err)
	}
}

func TestParseList(t *testing.T) {
	out := "worktree /srv/repo\nHEAD 1111111111111111111111111111111111111111\nbranch refs/heads/main\n\n" +
		"worktree /srv/wt/detached\nHEAD 2222222222222222222222222222222222222222\ndetached\n\n" +
		"worktree /srv/wt/with space\nHEAD 3333333333333333333333333333333333333333\nbranch refs/heads/alice/t-1\nlocked reason here\n\n" +
		"worktree /srv/wt/gone\nHEAD 4444444444444444444444444444444444444444\nbranch refs/heads/alice/t-2\nprunable gitdir file points to non-existent location\n"
	want := []Worktree{
		{Path: "/srv/repo", Head: "1111111111111111111111111111111111111111", Branch: "main", Main: true},
		{Path: "/srv/wt/detached", Head: "2222222222222222222222222222222222222222", Detached: true},
		{Path: "/srv/wt/with space", Head: "3333333333333333333333333333333333333333", Branch: "alice/t-1", Locked: true},
		{Path: "/srv/wt/gone", Head: "4444444444444444444444444444444444444444", Branch: "alice/t-2", Prunable: true},
	}
	if got := parseList(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseList =\n  %+v\nwant\n  %+v", got, want)
	}
	if got := parseList(""); len(got) != 0 {
		t.Fatalf("empty output = %+v", got)
	}
}

func TestLayoutPath(t *testing.T) {
	l := Layout{Root: "/var/lib/alice/worktrees"}
	got, err := l.Path("alice-core", "t-3f9a")
	if err != nil || got != "/var/lib/alice/worktrees/alice-core/t-3f9a" {
		t.Fatalf("Path = %q, %v", got, err)
	}

	bad := [][2]string{
		{"", "t"}, {"p", ""}, {"..", "t"}, {"p", ".."}, {".", "t"},
		{"a/b", "t"}, {"p", "a/b"}, {`a\b`, "t"}, {"p", "../../etc"}, {" p", "t"}, {"p", "t "},
	}
	for _, ids := range bad {
		if _, err := l.Path(ids[0], ids[1]); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Path(%q, %q): err = %v, want ErrInvalidName", ids[0], ids[1], err)
		}
	}
	if _, err := (Layout{}).Path("p", "t"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("empty root: err = %v, want ErrInvalidName", err)
	}
}

// Two tasks on the same repository must not see each other's files.
func TestWorktreesAreIsolated(t *testing.T) {
	repo := newRepo(t)
	layout := Layout{Root: t.TempDir()}
	pathA, _ := layout.Path("proj", "task-a")
	pathB, _ := layout.Path("proj", "task-b")

	a, err := Create(ctx, repo, pathA, "alice/task-a", "main")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Create(ctx, repo, pathB, "alice/task-b", "main")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(a.Path, "README.md"), "from a\n")
	writeFile(t, filepath.Join(b.Path, "README.md"), "from b\n")
	gitT(t, a.Path, "commit", "-q", "-am", "a")
	gitT(t, b.Path, "commit", "-q", "-am", "b")

	if got := gitT(t, repo, "show", "alice/task-a:README.md"); got != "from a" {
		t.Errorf("branch a = %q", got)
	}
	if got := gitT(t, repo, "show", "alice/task-b:README.md"); got != "from b" {
		t.Errorf("branch b = %q", got)
	}
	if got := gitT(t, repo, "show", "main:README.md"); got != "hello" {
		t.Errorf("main must be untouched, got %q", got)
	}
}
