package fleet

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fakeForge struct {
	opened   []PullRequest
	merged   []int
	openErr  error
	mergeErr error
}

func (f *fakeForge) OpenPullRequest(_ context.Context, _ string, pr PullRequest) (string, int, error) {
	if f.openErr != nil {
		return "", 0, f.openErr
	}
	f.opened = append(f.opened, pr)
	return "https://example.test/acme/web/pull/7", 7, nil
}

func (f *fakeForge) MergePullRequest(_ context.Context, _ string, number int) error {
	if f.mergeErr != nil {
		return f.mergeErr
	}
	f.merged = append(f.merged, number)
	return nil
}

type deliveryEnv struct {
	*runnerEnv
	deliverer *Deliverer
	forge     *fakeForge
	repo      string // the "web" project's main working tree
	remote    string // its bare origin
}

// newDeliveryEnv adds a project "web" that has a remote and the given mode.
func newDeliveryEnv(t *testing.T, mode TrustMode, verifyCommands ...string) *deliveryEnv {
	t.Helper()
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "Fleet Test"}, {"GIT_AUTHOR_EMAIL", "fleet@example.test"},
		{"GIT_COMMITTER_NAME", "Fleet Test"}, {"GIT_COMMITTER_EMAIL", "fleet@example.test"},
	} {
		t.Setenv(kv[0], kv[1])
	}
	e := newRunnerEnv(t)
	repo := newRepoWithRemote(t, "web")
	if _, err := e.store.RegisterProject(ctx, Project{ID: "web", RepoPath: repo, TrustMode: mode, VerifyCommands: verifyCommands}); err != nil {
		t.Fatal(err)
	}
	forge := &fakeForge{}
	return &deliveryEnv{
		runnerEnv: e, forge: forge, repo: repo,
		remote:    gitT(t, repo, "remote", "get-url", "origin"),
		deliverer: &Deliverer{Store: e.store, Forge: forge},
	}
}

// reviewReady runs the fake agent on a new "web" task and verifies it.
func (e *deliveryEnv) reviewReady(goal string, script ...string) Task {
	e.t.Helper()
	if len(script) == 0 {
		script = []string{lineInit, "#touch feature.txt", lineResultOK}
	}
	e.script(script...)
	task := e.task(NewTask{ProjectID: "web", Goal: goal})
	e.run(task.ID, RunOptions{})
	res, err := (&Verifier{Store: e.store}).Verify(ctx, task.ID)
	if err != nil {
		e.t.Fatalf("Verify: %v", err)
	}
	if res.Task.Status != StatusReviewReady {
		e.t.Fatalf("setup: task is %s (%s), want review_ready", res.Task.Status, res.Task.StatusReason)
	}
	return res.Task
}

func (e *deliveryEnv) deliver(taskID string) Delivery {
	e.t.Helper()
	d, err := e.deliverer.Deliver(ctx, taskID)
	if err != nil {
		e.t.Fatalf("Deliver: %v", err)
	}
	return d
}

func (e *deliveryEnv) remoteHas(branch string) bool {
	cmd := gitCmd(e.remote, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return cmd.Run() == nil
}

func TestDeliver_LocalOnlyCommitsAndWaits(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	task := e.reviewReady("Add the feature flag\n\nLonger explanation that is not the subject.")

	d := e.deliver(task.ID)

	if !d.Committed || d.Pushed || d.PRURL != "" {
		t.Fatalf("delivery = %+v, want committed only", d)
	}
	if d.Task.Status != StatusReviewReady {
		t.Fatalf("a local-only task must wait for the owner, got %s", d.Task.Status)
	}
	if changes, _ := gitOut(task.WorktreePath, "status", "--porcelain"); changes != "" {
		t.Errorf("the worktree must be clean after delivery, got %q", changes)
	}
	if subject := gitT(t, task.WorktreePath, "log", "-1", "--format=%s"); subject != "Add the feature flag" {
		t.Errorf("commit subject = %q", subject)
	}
	if body := gitT(t, task.WorktreePath, "log", "-1", "--format=%b"); body != "Task: "+task.ID {
		t.Errorf("commit body = %q", body)
	}
	if e.remoteHas(task.Branch) {
		t.Error("local-only must not push")
	}
	if len(e.forge.opened) != 0 {
		t.Error("local-only must not open a pull request")
	}
	if got := gitT(t, e.repo, "rev-parse", "main"); got == gitT(t, task.WorktreePath, "rev-parse", "HEAD") {
		t.Error("the default branch must not move before approval")
	}
}

func TestApprove_LocalOnlyFastForwards(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	task := e.reviewReady("Add the feature")
	e.deliver(task.ID)
	before := gitT(t, e.repo, "rev-parse", "main")
	head := gitT(t, task.WorktreePath, "rev-parse", "HEAD")

	// An existing database may have a task already awaiting approval.
	if _, err := e.store.db.ExecContext(ctx, "UPDATE fleet_tasks SET target_branch = '' WHERE id = ?", task.ID); err != nil {
		t.Fatal(err)
	}
	gitT(t, e.repo, "branch", "develop")
	if _, err := e.store.UpdateProject(ctx, "web", func(p *Project) error {
		p.DefaultBranch = "develop"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	done, err := e.deliverer.Approve(ctx, task.ID, "ck")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if done.Status != StatusDone || done.StatusReason != "approved by ck: fast-forwarded main" {
		t.Fatalf("task = %s (%q)", done.Status, done.StatusReason)
	}
	if got := gitT(t, e.repo, "rev-parse", "main"); got != head {
		t.Fatalf("main = %s, want the task's head %s", got, head)
	}
	if got := gitT(t, e.repo, "rev-parse", "develop"); got != before {
		t.Fatalf("develop = %s, want unchanged tip %s", got, before)
	}
	// main is checked out in the main working tree, so its files moved too.
	if _, err := os.Stat(filepath.Join(e.repo, "feature.txt")); err != nil {
		t.Errorf("the main working tree must show the landed work: %v", err)
	}
	// A fast-forward: the old tip is still an ancestor and no merge commit exists.
	if gitCmd(e.repo, "merge-base", "--is-ancestor", before, "main").Run() != nil {
		t.Error("history was rewritten: the old tip is no longer an ancestor of main")
	}
	if merges := gitT(t, e.repo, "rev-list", "--merges", "--count", before+"..main"); merges != "0" {
		t.Errorf("a fast-forward must not create merge commits, found %s", merges)
	}
	approved := e.eventPayloads(task.ID, EventDeliveryApproved)
	if len(approved) != 1 || approved[0]["approver"] != "ck" {
		t.Errorf("delivery.approved = %v", approved)
	}
	if e.remoteHas(task.Branch) || len(e.forge.merged) != 0 {
		t.Error("local-only approval must stay local")
	}
}

// When the default branch is not checked out, only the branch pointer moves.
func TestApprove_LocalOnlyWhenDefaultBranchIsNotCheckedOut(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	task := e.reviewReady("Add the feature")
	e.deliver(task.ID)
	gitT(t, e.repo, "checkout", "-q", "-b", "owner-is-working-here")
	if err := os.WriteFile(filepath.Join(e.repo, "owner-notes.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := e.deliverer.Approve(ctx, task.ID, ""); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got, want := gitT(t, e.repo, "rev-parse", "main"), gitT(t, task.WorktreePath, "rev-parse", "HEAD"); got != want {
		t.Fatalf("main = %s, want %s", got, want)
	}
	if got := gitT(t, e.repo, "rev-parse", "--abbrev-ref", "HEAD"); got != "owner-is-working-here" {
		t.Errorf("the owner's checkout was switched to %q", got)
	}
	if _, err := os.Stat(filepath.Join(e.repo, "owner-notes.txt")); err != nil {
		t.Errorf("the owner's uncommitted work was disturbed: %v", err)
	}
}

func TestApprove_RefusesWhenNotFastForward(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	task := e.reviewReady("Add the feature")
	e.deliver(task.ID)
	// main moves on while the task waits.
	if err := os.WriteFile(filepath.Join(e.repo, "meanwhile.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, e.repo, "add", ".")
	gitT(t, e.repo, "commit", "-q", "-m", "meanwhile on main")
	mainBefore := gitT(t, e.repo, "rev-parse", "main")

	_, err := e.deliverer.Approve(ctx, task.ID, "ck")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("err = %v, want ErrNotFastForward", err)
	}
	if got := gitT(t, e.repo, "rev-parse", "main"); got != mainBefore {
		t.Fatalf("main moved from %s to %s on a refused approval", mainBefore, got)
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusReviewReady {
		t.Fatalf("a refused approval changed the task to %s", got.Status)
	}
	if len(e.eventPayloads(task.ID, EventDeliveryApproved)) != 0 {
		t.Error("a refused approval must not be recorded as an approval")
	}
}

func TestDeliver_DirectPROpensAPullRequestAndFinishes(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR, "test -f feature.txt")
	task := e.reviewReady("Add dark mode\n\nUsers asked for it.")

	d := e.deliver(task.ID)

	if !d.Committed || !d.Pushed || d.PRURL != "https://example.test/acme/web/pull/7" {
		t.Fatalf("delivery = %+v", d)
	}
	if d.Task.Status != StatusDone || d.Task.StatusReason != "pull request opened: https://example.test/acme/web/pull/7" {
		t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
	}
	if d.Task.PRURL != d.PRURL || d.Task.PRNumber != 7 {
		t.Errorf("stored pull request = %q #%d", d.Task.PRURL, d.Task.PRNumber)
	}
	// The remote has exactly what the worktree has.
	local := gitT(t, task.WorktreePath, "rev-parse", "HEAD")
	if remote := gitT(t, e.remote, "rev-parse", "refs/heads/"+task.Branch); remote != local {
		t.Errorf("remote branch = %s, want %s", remote, local)
	}
	if remoteMain := gitT(t, e.remote, "rev-parse", "refs/heads/main"); remoteMain == local {
		t.Error("delivery must push the task branch, never the default branch")
	}

	if len(e.forge.opened) != 1 {
		t.Fatalf("pull requests opened = %+v", e.forge.opened)
	}
	pr := e.forge.opened[0]
	if pr.Branch != task.Branch || pr.Base != "main" || pr.Title != "Add dark mode" {
		t.Errorf("pull request = %+v", pr)
	}
	for _, want := range []string{"Users asked for it.", "- `test -f feature.txt`", "Task: " + task.ID} {
		if !strings.Contains(pr.Body, want) {
			t.Errorf("pull request body is missing %q:\n%s", want, pr.Body)
		}
	}
	if len(e.forge.merged) != 0 {
		t.Error("direct-pr must leave merging to the owner")
	}
	if _, err := e.deliverer.Approve(ctx, task.ID, "ck"); !errors.Is(err, ErrNothingToApprove) {
		t.Errorf("Approve on a finished direct-pr task: %v", err)
	}
}

func TestDeliverAndApprove_NoMistakes(t *testing.T) {
	e := newDeliveryEnv(t, TrustNoMistakes, "test -f feature.txt")
	task := e.reviewReady("Rework billing")

	d := e.deliver(task.ID)
	if !d.Pushed || d.PRURL == "" {
		t.Fatalf("delivery = %+v", d)
	}
	if d.Task.Status != StatusReviewReady {
		t.Fatalf("a no-mistakes task must wait for approval, got %s", d.Task.Status)
	}
	if len(e.forge.merged) != 0 {
		t.Fatal("nothing may be merged before approval")
	}

	done, err := e.deliverer.Approve(ctx, task.ID, "ck")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if !reflect.DeepEqual(e.forge.merged, []int{7}) {
		t.Fatalf("merged = %v, want pull request 7", e.forge.merged)
	}
	if done.Status != StatusDone || done.StatusReason != "approved by ck: merged https://example.test/acme/web/pull/7" {
		t.Fatalf("task = %s (%q)", done.Status, done.StatusReason)
	}
	// The local default branch is untouched: the merge happened on the forge.
	if got := gitT(t, e.repo, "rev-parse", "main"); got == gitT(t, task.WorktreePath, "rev-parse", "HEAD") {
		t.Error("no-mistakes approval must not move the local default branch")
	}
}

func TestApprove_NoMistakesFailures(t *testing.T) {
	e := newDeliveryEnv(t, TrustNoMistakes, "true")
	task := e.reviewReady("Rework billing")

	// Approved before a pull request exists.
	if _, err := e.deliverer.Approve(ctx, task.ID, "ck"); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("Approve without a pull request: %v", err)
	}
	e.deliver(task.ID)

	// The forge refuses the merge: the task must stay where it is.
	e.forge.mergeErr = errors.New("required status check is failing")
	if _, err := e.deliverer.Approve(ctx, task.ID, "ck"); err == nil || !strings.Contains(err.Error(), "required status check") {
		t.Fatalf("Approve with a failing merge: %v", err)
	}
	if got, _ := e.store.GetTask(ctx, task.ID); got.Status != StatusReviewReady {
		t.Fatalf("a failed merge changed the task to %s", got.Status)
	}
	e.forge.mergeErr = nil
	if done, err := e.deliverer.Approve(ctx, task.ID, "ck"); err != nil || done.Status != StatusDone {
		t.Fatalf("Approve after the forge recovered: %+v, %v", done, err)
	}
}

func TestDeliver_NoMistakesRequiresVerifyCommands(t *testing.T) {
	e := newDeliveryEnv(t, TrustNoMistakes)
	task := e.reviewReady("Rework billing")
	d := e.deliver(task.ID)
	if d.Task.Status != StatusNeedsDecision || !strings.Contains(d.Task.StatusReason, "requires verification commands") {
		t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
	}
	if d.Committed || d.Pushed || e.remoteHas(task.Branch) || len(e.forge.opened) != 0 {
		t.Fatalf("nothing may be delivered: %+v", d)
	}
}

func TestDeliver_WorkTheAgentAlreadyCommitted(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR)
	task := e.reviewReady("Add the feature")
	gitT(t, task.WorktreePath, "add", "-A")
	gitT(t, task.WorktreePath, "commit", "-q", "-m", "agent's own commit")
	head := gitT(t, task.WorktreePath, "rev-parse", "HEAD")

	d := e.deliver(task.ID)
	if d.Committed {
		t.Error("nothing was left to commit")
	}
	if !d.Pushed || d.Task.Status != StatusDone {
		t.Fatalf("delivery = %+v (%s)", d, d.Task.Status)
	}
	if got := gitT(t, task.WorktreePath, "rev-parse", "HEAD"); got != head {
		t.Error("delivery must not add a commit when the tree is clean")
	}
}

// "Done" with nothing to show for it is not done.
func TestDeliver_NothingChanged(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR)
	task := e.reviewReady("Add the feature", lineInit, lineResultOK)
	d := e.deliver(task.ID)
	if d.Task.Status != StatusNeedsDecision || d.Task.StatusReason != "nothing to deliver: the task changed nothing" {
		t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
	}
	if e.remoteHas(task.Branch) || len(e.forge.opened) != 0 {
		t.Error("an empty branch must not be pushed or get a pull request")
	}
}

func TestDeliver_RefusesToCommitSecrets(t *testing.T) {
	for _, name := range []string{".env", ".env.production", "deploy/server.pem", "id_rsa"} {
		t.Run(name, func(t *testing.T) {
			e := newDeliveryEnv(t, TrustDirectPR)
			task := e.reviewReady("Add the feature")
			secret := filepath.Join(task.WorktreePath, name)
			if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(secret, []byte("SECRET=1"), 0o600); err != nil {
				t.Fatal(err)
			}

			d := e.deliver(task.ID)
			if d.Task.Status != StatusNeedsDecision || !strings.Contains(d.Task.StatusReason, "refusing to commit "+name) {
				t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
			}
			if d.Committed || d.Pushed || e.remoteHas(task.Branch) {
				t.Fatalf("a secret reached a commit or the remote: %+v", d)
			}
			if tracked, _ := gitOut(task.WorktreePath, "ls-files", name); tracked != "" {
				t.Errorf("%s was staged", name)
			}
			if _, err := os.Stat(secret); err != nil {
				t.Errorf("the file itself must be left alone: %v", err)
			}
		})
	}
}

func TestDeliver_PushFailure(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR)
	task := e.reviewReady("Add the feature")
	gitT(t, e.repo, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git"))

	d := e.deliver(task.ID)
	if d.Task.Status != StatusNeedsDecision || !strings.Contains(d.Task.StatusReason, "delivery failed: git push") {
		t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
	}
	if d.Pushed || len(e.forge.opened) != 0 {
		t.Fatalf("delivery = %+v", d)
	}
	// The work is still safe on its branch.
	if !d.Committed || gitT(t, task.WorktreePath, "log", "-1", "--format=%s") != "Add the feature" {
		t.Error("the commit must survive a failed push")
	}
}

func TestDeliver_PullRequestFailureCanBeRetried(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR)
	task := e.reviewReady("Add the feature")
	e.forge.openErr = errors.New("HTTP 502 from the forge")

	d := e.deliver(task.ID)
	if d.Task.Status != StatusNeedsDecision || !strings.Contains(d.Task.StatusReason, "open pull request: HTTP 502") {
		t.Fatalf("task = %s (%q)", d.Task.Status, d.Task.StatusReason)
	}
	if !e.remoteHas(task.Branch) {
		t.Fatal("the push happened before the pull request and must stand")
	}

	// The owner sends it round again once the forge is back.
	e.forge.openErr = nil
	walk(t, e.store, task.ID, StatusReviewReady)
	again := e.deliver(task.ID)
	if again.Task.Status != StatusDone || again.PRURL == "" || len(e.forge.opened) != 1 {
		t.Fatalf("retry = %+v (%s)", again, again.Task.Status)
	}
	if again.Committed {
		t.Error("the retry had nothing new to commit")
	}
}

// The mode a task was dispatched with is the one that governs its delivery.
func TestDeliver_UsesTheModeFixedAtDispatch(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)
	task := e.reviewReady("Add the feature")
	if _, err := e.store.UpdateProject(ctx, "web", func(p *Project) error { p.TrustMode = TrustDirectPR; return nil }); err != nil {
		t.Fatal(err)
	}

	d := e.deliver(task.ID)
	if d.Pushed || e.remoteHas(task.Branch) || len(e.forge.opened) != 0 {
		t.Fatalf("a task dispatched as local-only was pushed after the project changed: %+v", d)
	}
	if d.Task.Status != StatusReviewReady || d.Task.TrustMode != TrustLocalOnly {
		t.Fatalf("task = %s, mode %s", d.Task.Status, d.Task.TrustMode)
	}

	// A task dispatched after the change follows the new mode.
	later := e.reviewReady("Another feature")
	if d := e.deliver(later.ID); !d.Pushed || d.Task.Status != StatusDone {
		t.Fatalf("a task dispatched under direct-pr = %+v (%s)", d, d.Task.Status)
	}
}

func TestDeliverAndApprove_Refusals(t *testing.T) {
	e := newDeliveryEnv(t, TrustLocalOnly)

	queued := e.task(NewTask{ProjectID: "web"})
	if _, err := e.deliverer.Deliver(ctx, queued.ID); err == nil || !strings.Contains(err.Error(), "not review_ready") {
		t.Errorf("Deliver on a queued task: %v", err)
	}
	if _, err := e.deliverer.Approve(ctx, queued.ID, "ck"); !errors.Is(err, ErrNothingToApprove) {
		t.Errorf("Approve on a queued task: %v", err)
	}

	scout := e.finishedTask(NewTask{ProjectID: "web", Shape: ShapeScout, Goal: "look"})
	if _, err := e.deliverer.Deliver(ctx, scout.ID); err == nil || !strings.Contains(err.Error(), "only ship tasks") {
		t.Errorf("Deliver on a scout: %v", err)
	}

	if _, err := e.deliverer.Deliver(ctx, "missing"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("Deliver on a missing task: %v", err)
	}
	if _, err := e.deliverer.Approve(ctx, "missing", "ck"); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("Approve on a missing task: %v", err)
	}
	if _, err := (&Deliverer{}).Deliver(ctx, queued.ID); err == nil {
		t.Error("a Deliverer without a Store must refuse")
	}
	if got, _ := e.store.GetTask(ctx, queued.ID); got.Status != StatusQueued {
		t.Errorf("refusals changed the queued task to %s", got.Status)
	}
}

// The scheduler carries a task all the way: run, verify, deliver.
func TestRunTask_DeliversAfterVerification(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR, "test -f feature.txt")
	e.script(lineInit, "#touch feature.txt", lineResultOK)
	task := e.task(NewTask{ProjectID: "web", Goal: "Add the feature"})
	s := e.scheduler(1)
	s.Deliverer = e.deliverer

	res, err := s.RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if res.Task.Status != StatusDone || res.Task.PRNumber != 7 || !e.remoteHas(res.Task.Branch) {
		t.Fatalf("task = %s (%q), PR #%d", res.Task.Status, res.Task.StatusReason, res.Task.PRNumber)
	}

	// The whole journey is in the event log, in order.
	var journey []string
	for _, typ := range e.eventTypes(task.ID) {
		switch typ {
		case EventRunStarted, EventVerification, EventDeliveryCommitted, EventDeliveryPushed, EventDeliveryPROpened:
			journey = append(journey, typ)
		}
	}
	want := []string{EventRunStarted, EventVerification, EventDeliveryCommitted, EventDeliveryPushed, EventDeliveryPROpened}
	if !reflect.DeepEqual(journey, want) {
		t.Fatalf("journey = %v, want %v", journey, want)
	}
}

// A task that fails verification is never delivered.
func TestRunTask_DoesNotDeliverUnverifiedWork(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR, "exit 1")
	e.script(lineInit, "#touch feature.txt", lineResultOK)
	task := e.task(NewTask{ProjectID: "web", Goal: "Add the feature"})
	s := e.scheduler(1)
	s.Deliverer = e.deliverer
	s.Verifier = &Verifier{Store: e.store, MaxRetries: -1}

	res, err := s.RunTask(ctx, task.ID, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Task.Status != StatusNeedsDecision {
		t.Fatalf("task = %s", res.Task.Status)
	}
	if e.remoteHas(res.Task.Branch) || len(e.forge.opened) != 0 {
		t.Fatal("work that failed verification reached the remote")
	}
}

func TestChangedPaths(t *testing.T) {
	got := changedPaths([]string{
		" M README.md",
		"?? new file.txt",
		"A  src/app.go",
		`R  old.go -> "new name.go"`,
		"??",
	})
	want := []string{"README.md", "new file.txt", "src/app.go", "old.go", "new name.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changedPaths = %q, want %q", got, want)
	}
}

func TestTaskSubject(t *testing.T) {
	long := strings.Repeat("字", 100)
	cases := []struct {
		task Task
		want string
	}{
		{Task{ID: "t1", Goal: "  Fix the bug  \n\ndetails"}, "Fix the bug"},
		{Task{ID: "t1", Goal: long}, strings.Repeat("字", 71) + "…"},
		{Task{ID: "t1", Source: Source{Kind: SourceIssue, IssueNumber: 42}}, "Work on issue #42"},
		{Task{ID: "t1"}, "Task t1"},
	}
	for _, tc := range cases {
		if got := taskSubject(tc.task); got != tc.want {
			t.Errorf("taskSubject(%q) = %q, want %q", tc.task.Goal, got, tc.want)
		}
	}
	body := prBody(Task{ID: "t1", Goal: "Do it", Source: Source{Kind: SourceIssue, IssueNumber: 42}}, Project{})
	if !strings.Contains(body, "Refs #42") || strings.Contains(body, "Verified before") {
		t.Errorf("pull request body = %q", body)
	}
}

func TestGitHubForge(t *testing.T) {
	bin, dir := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
: > "$FAKE_GH_DIR/args"
for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_GH_DIR/args"; done
if [ -f "$FAKE_GH_DIR/stdout" ]; then cat "$FAKE_GH_DIR/stdout"; fi
exit "${FAKE_GH_EXIT:-0}"
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", dir)
	reply := func(s string) {
		if err := os.WriteFile(filepath.Join(dir, "stdout"), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := func() string {
		b, _ := os.ReadFile(filepath.Join(dir, "args"))
		return string(b)
	}
	forge := GitHub{}

	reply("Creating pull request for alice/abc into main in acme/web\n\nhttps://github.com/acme/web/pull/123\n")
	url, number, err := forge.OpenPullRequest(ctx, t.TempDir(), PullRequest{Branch: "alice/abc", Base: "main", Title: "Add it", Body: "line one\nline two"})
	if err != nil || url != "https://github.com/acme/web/pull/123" || number != 123 {
		t.Fatalf("OpenPullRequest = %q, %d, %v", url, number, err)
	}
	if want := "pr\ncreate\n--head\nalice/abc\n--base\nmain\n--title\nAdd it\n--body\nline one\nline two\n"; args() != want {
		t.Errorf("gh args = %q, want %q", args(), want)
	}

	reply("something unexpected\n")
	if _, _, err := forge.OpenPullRequest(ctx, t.TempDir(), PullRequest{Branch: "b", Base: "main"}); err == nil {
		t.Error("output without a pull request URL must be an error")
	}

	reply("")
	if err := forge.MergePullRequest(ctx, t.TempDir(), 123); err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if want := "pr\nmerge\n123\n--merge\n"; args() != want {
		t.Errorf("gh args = %q, want %q: a merge commit, never squash or rebase", args(), want)
	}

	t.Setenv("FAKE_GH_EXIT", "1")
	reply("! Pull request #123 was already merged\n")
	if err := forge.MergePullRequest(ctx, t.TempDir(), 123); err != nil {
		t.Errorf("merging an already-merged pull request must succeed: %v", err)
	}
	reply("GraphQL: Pull request is not mergeable\n")
	if err := forge.MergePullRequest(ctx, t.TempDir(), 123); err == nil || !strings.Contains(err.Error(), "not mergeable") {
		t.Errorf("a refused merge must surface gh's reason: %v", err)
	}
	if _, _, err := forge.OpenPullRequest(ctx, t.TempDir(), PullRequest{Branch: "b", Base: "main"}); err == nil {
		t.Error("a failing gh must be an error")
	}
}

func TestShipKeepsTargetWhenProjectChanges(t *testing.T) {
	e := newDeliveryEnv(t, TrustDirectPR)
	old := e.reviewReady("Old ship")
	if old.TargetBranch != "main" {
		t.Fatalf("old target = %q, want main", old.TargetBranch)
	}

	gitT(t, e.repo, "switch", "-c", "develop")
	if err := os.WriteFile(filepath.Join(e.repo, "develop-only.txt"), []byte("new base"), 0600); err != nil {
		t.Fatal(err)
	}
	gitT(t, e.repo, "add", "develop-only.txt")
	gitT(t, e.repo, "commit", "-m", "Advance develop")
	// Make develop contain the workspace commit as well. Using develop for
	// the old diff or delivery would now hide the task's change.
	gitT(t, old.WorktreePath, "add", "feature.txt")
	gitT(t, old.WorktreePath, "commit", "-m", "Old task change")
	gitT(t, e.repo, "merge", "--no-ff", "-m", "Integrate old change on develop", old.Branch)

	if _, err := e.store.UpdateProject(ctx, "web", func(p *Project) error {
		p.DefaultBranch = "develop"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	diff, err := (&Commander{Store: e.store}).TaskDiff(ctx, old.ID)
	if err != nil || !strings.Contains(diff.Patch, "feature.txt") {
		t.Fatalf("old diff = %+v, err = %v", diff, err)
	}
	e.deliver(old.ID)
	if len(e.forge.opened) != 1 || e.forge.opened[0].Base != "main" {
		t.Fatalf("old pull request = %+v, want base main", e.forge.opened)
	}

	fresh := e.reviewReady("New ship")
	if fresh.TargetBranch != "develop" {
		t.Fatalf("new target = %q, want develop", fresh.TargetBranch)
	}
	if head := gitT(t, fresh.WorktreePath, "rev-parse", "HEAD"); head != gitT(t, e.repo, "rev-parse", "develop") {
		t.Fatalf("new worktree head = %s, want develop", head)
	}
}
