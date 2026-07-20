package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type localWorkspaceFixture struct {
	manager *localEpicWorkerWorkspaceManager
	root    string
	mirror  string
	baseSHA string
	st      *store.Store
}

func newLocalWorkspaceFixture(t *testing.T) localWorkspaceFixture {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "repo", Owner: "sam", Repo: "repo",
		DefaultBranch: "main", Active: true}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-a", ProjectID: "default", Repo: "repo",
		DeliveryRepo: "repo", Branch: "epic/a"}, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-b", ProjectID: "default", Repo: "repo",
		DeliveryRepo: "repo", Branch: "epic/b"}, 1, now); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, source, "init", "-b", "main")
	runGitFixture(t, source, "config", "user.email", "flowbee@example.invalid")
	runGitFixture(t, source, "config", "user.name", "Flowbee")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, source, "add", "README.md")
	runGitFixture(t, source, "commit", "-m", "base")
	baseSHA := workspaceGitOutput(t, source, "rev-parse", "HEAD")
	mirror := filepath.Join(base, "mirror.git")
	runGitFixture(t, "", "clone", "--bare", source, mirror)
	resolvedMirror, err := filepath.EvalSymlinks(mirror)
	if err != nil {
		t.Fatal(err)
	}
	mirror = resolvedMirror
	root := filepath.Join(base, "workspaces")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &localEpicWorkerWorkspaceManager{DB: st.DB, Roots: map[string]string{"local": root},
		LocalHostIDs: map[string]bool{"host-local": true}, MirrorPath: func(store.Repo) string { return mirror }}
	return localWorkspaceFixture{manager: manager, root: root, mirror: mirror, baseSHA: baseSHA, st: st}
}

func workspaceAction(epicID, role, rel, source string) driver.Action {
	a := driver.Action{ActionID: "ensure-" + epicID + "-" + role, ProjectID: "default", EpicID: epicID,
		Kind: "builder_launch", TargetRole: role, TargetHostID: "host-local", LifecycleKey: epicID + "-" + role,
		WorkspaceRootID: "local", WorkspaceRelativePath: rel, BaseSHA: source}
	if role == store.DriverReviewerRole {
		a.Kind, a.HeadSHA, a.BaseSHA = "reviewer_launch", source, ""
	}
	return a
}

func TestLocalEpicWorkspaceTwoSimultaneousEpicsNeverShareAPath(t *testing.T) {
	f := newLocalWorkspaceFixture(t)
	a := workspaceAction("epic-a", store.DriverBuilderRole, "projects/default/epic-a/builder", f.baseSHA)
	b := workspaceAction("epic-b", store.DriverBuilderRole, "projects/default/epic-b/builder", f.baseSHA)
	errCh := make(chan error, 2)
	go func() { errCh <- f.manager.PrepareLifecycleWorkspace(context.Background(), a, time.Now()) }()
	go func() { errCh <- f.manager.PrepareLifecycleWorkspace(context.Background(), b, time.Now()) }()
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{a.WorkspaceRelativePath, b.WorkspaceRelativePath} {
		if _, err := os.Stat(filepath.Join(f.root, rel, "README.md")); err != nil {
			t.Fatalf("isolated workspace %s: %v", rel, err)
		}
	}
}

func TestLocalEpicWorkspacePrepareCrashRetryAndMovingHeadUsesCommittedSHA(t *testing.T) {
	f := newLocalWorkspaceFixture(t)
	a := workspaceAction("epic-a", store.DriverBuilderRole, "projects/default/epic-a/builder", f.baseSHA)
	if err := f.manager.PrepareLifecycleWorkspace(context.Background(), a, time.Now()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(f.root, a.WorkspaceRelativePath) + ".flowbee-workspace.json"
	if err := os.Remove(marker); err != nil { // crash after git registration, before marker persistence
		t.Fatal(err)
	}
	if err := f.manager.PrepareLifecycleWorkspace(context.Background(), a, time.Now()); err != nil {
		t.Fatalf("crash retry: %v", err)
	}
	// Advance the source repository and mirror's main after the action commit.
	sourceClone := filepath.Join(t.TempDir(), "moving")
	runGitFixture(t, "", "clone", f.mirror, sourceClone)
	runGitFixture(t, sourceClone, "config", "user.email", "flowbee@example.invalid")
	runGitFixture(t, sourceClone, "config", "user.name", "Flowbee")
	if err := os.WriteFile(filepath.Join(sourceClone, "README.md"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitFixture(t, sourceClone, "commit", "-am", "move")
	runGitFixture(t, sourceClone, "push", "origin", "main")
	got := workspaceGitOutput(t, filepath.Join(f.root, a.WorkspaceRelativePath), "rev-parse", "HEAD")
	if got != f.baseSHA {
		t.Fatalf("moving HEAD changed committed workspace source: got %s want %s", got, f.baseSHA)
	}
}

func TestLocalEpicWorkspaceReviewerUsesExactHeadAndStopCleansBeforeProjection(t *testing.T) {
	f := newLocalWorkspaceFixture(t)
	a := workspaceAction("epic-a", store.DriverReviewerRole, "projects/default/epic-a/reviewer", f.baseSHA)
	if err := f.manager.PrepareLifecycleWorkspace(context.Background(), a, time.Now()); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(f.root, a.WorkspaceRelativePath)
	if got := workspaceGitOutput(t, workspace, "rev-parse", "HEAD"); got != a.HeadSHA {
		t.Fatalf("reviewer workspace=%s want exact head=%s", got, a.HeadSHA)
	}
	stop := a
	stop.Kind = "worker_stop"
	receipt := driver.LifecycleReceipt{Operation: "stop", Status: "target_absent",
		AbsenceObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := f.manager.FinalizeLifecycleWorkspace(context.Background(), stop, receipt, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remained after positive absence: %v", err)
	}
	if err := f.manager.FinalizeLifecycleWorkspace(context.Background(), stop, receipt, time.Now()); err != nil {
		t.Fatalf("cleanup replay not idempotent: %v", err)
	}
}

func TestLocalEpicWorkspacePreEffectCleanupDeletesOnlyExactPreparedWorkspace(t *testing.T) {
	f := newLocalWorkspaceFixture(t)
	a := workspaceAction("epic-a", store.DriverBuilderRole, "projects/default/epic-a/builder", f.baseSHA)
	if err := f.manager.PrepareLifecycleWorkspace(context.Background(), a, time.Now()); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(f.root, a.WorkspaceRelativePath)
	cleanup := a
	cleanup.ActionID = "workspace-cleanup-epic-a"
	cleanup.Kind = "worker_workspace_cleanup"
	if err := f.manager.FinalizePreEffectLifecycleWorkspace(context.Background(), cleanup, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(workspace); !os.IsNotExist(err) {
		t.Fatalf("pre-effect workspace remained after exact cleanup: %v", err)
	}
	if err := f.manager.FinalizePreEffectLifecycleWorkspace(context.Background(), cleanup, time.Now()); err != nil {
		t.Fatalf("pre-effect cleanup replay was not idempotent: %v", err)
	}
	if err := f.manager.FinalizePreEffectLifecycleWorkspace(context.Background(), a, time.Now()); err == nil {
		t.Fatal("ordinary Ensure action was accepted as a pre-effect cleanup")
	}
}

func workspaceGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestLocalEpicWorkspaceRejectsRemoteEscapeSymlinkAndAmbiguousCleanup(t *testing.T) {
	f := newLocalWorkspaceFixture(t)
	ctx := context.Background()
	base := workspaceAction("epic-a", store.DriverBuilderRole, "projects/default/epic-a/builder", f.baseSHA)
	cases := []struct {
		name string
		edit func(*driver.Action)
	}{
		{"remote", func(a *driver.Action) { a.TargetHostID = "host-remote" }},
		{"escape", func(a *driver.Action) { a.WorkspaceRelativePath = "../escape" }},
		{"unknown root", func(a *driver.Action) { a.WorkspaceRootID = "missing" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base
			tc.edit(&a)
			if err := f.manager.PrepareLifecycleWorkspace(ctx, a, time.Now()); err == nil {
				t.Fatal("unsafe workspace authority accepted")
			}
		})
	}
	parent := filepath.Join(f.root, "projects")
	if err := os.Symlink(t.TempDir(), parent); err != nil {
		t.Fatal(err)
	}
	if err := f.manager.PrepareLifecycleWorkspace(ctx, base, time.Now()); err == nil {
		t.Fatal("symlink workspace parent accepted")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(f.root, base.WorkspaceRelativePath)
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	stop := base
	stop.Kind = "worker_stop"
	receipt := driver.LifecycleReceipt{Operation: "stop", Status: "stopped",
		AbsenceObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := f.manager.FinalizeLifecycleWorkspace(ctx, stop, receipt, time.Now()); err == nil {
		t.Fatal("ambiguous unmarked workspace was deleted")
	}
	if _, err := os.Stat(workspace); err != nil {
		t.Fatalf("ambiguous workspace was mutated: %v", err)
	}
}
