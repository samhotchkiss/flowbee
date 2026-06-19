package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newFixture builds a local BARE repo with one commit on main (no network, no
// GitHub) and returns the mirror plus the base SHA of main. This is the §6/§7.4
// bare-repo fixture used everywhere git ops are tested.
func newFixture(t *testing.T) (*Mirror, string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "mirror.git")
	m, err := InitBare(bare)
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	// seed a commit via a throwaway working clone, then push to the bare main.
	work := filepath.Join(root, "seed")
	mustGit(t, "", "git", "clone", bare, work)
	mustWrite(t, filepath.Join(work, "README.md"), "hello\n")
	mustGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	// default branch may be 'main' or 'master' depending on git config; force main.
	mustGit(t, work, "git", "branch", "-M", "main")
	mustGit(t, work, "git", "push", "origin", "main")
	sha, err := m.HeadSHA("refs/heads/main")
	if err != nil {
		t.Fatalf("head sha: %v", err)
	}
	return m, sha
}

func TestWorktreeDiffCommitPushPromote(t *testing.T) {
	m, base := newFixture(t)

	wsRoot := t.TempDir()
	ws := WorktreeBase(wsRoot, "job-1", 3)
	wt, err := m.AddWorktree(ws, base)
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	defer wt.Destroy()

	// the "agent" makes a change in the worktree.
	mustWrite(t, filepath.Join(ws, "feature.txt"), "built by agent\n")

	changed, err := wt.HasChanges()
	if err != nil || !changed {
		t.Fatalf("HasChanges=%v err=%v want true", changed, err)
	}
	diff, err := wt.Diff()
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "built by agent") {
		t.Fatalf("diff missing change:\n%s", diff)
	}

	sha, ref, err := wt.CommitAndPushEpoch("job-1", 3, "msg")
	if err != nil {
		t.Fatalf("commit+push: %v", err)
	}
	if ref != EpochRef("job-1", 3) {
		t.Fatalf("ref=%s want %s", ref, EpochRef("job-1", 3))
	}
	// the epoch ref now exists in the mirror at the pushed SHA.
	got, ok := m.RefSHA(ref)
	if !ok || got != sha {
		t.Fatalf("epoch ref sha=%q ok=%v want %s", got, ok, sha)
	}

	// promotion fast-forwards a real branch from the epoch ref (Flowbee's step).
	if err := m.PromoteEpochRef(ref, "refs/heads/job-1-branch"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	promoted, ok := m.RefSHA("refs/heads/job-1-branch")
	if !ok || promoted != sha {
		t.Fatalf("promoted branch sha=%q want %s", promoted, sha)
	}
}

// TestSoftResetToNormalizesSelfCommittingAgent proves the fix for an agentic CLI (codex)
// that runs `git commit` on its own. The harness owns the commit (§3.5), so a self-
// committed worktree is CLEAN — HasChanges would read false and the build be wrongly
// rejected as "no changes", and CommitAndPushEpoch would fail on the empty tree. After
// SoftResetTo(base) the agent's change is pending again, so the normal collect path
// (HasChanges → Diff → CommitAndPushEpoch) works exactly as for a non-committing agent.
func TestSoftResetToNormalizesSelfCommittingAgent(t *testing.T) {
	m, base := newFixture(t)

	ws := WorktreeBase(t.TempDir(), "job-cx", 1)
	wt, err := m.AddWorktree(ws, base)
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	defer wt.Destroy()

	// the "agent" writes a file AND commits it itself (what codex does).
	mustWrite(t, filepath.Join(ws, "feature.txt"), "built + self-committed by agent\n")
	if _, err := wt.Run("git", "add", "-A"); err != nil {
		t.Fatalf("agent add: %v", err)
	}
	if _, err := wt.Run("git", "-c", "user.email=a@b.c", "-c", "user.name=agent", "commit", "-m", "agent's own commit"); err != nil {
		t.Fatalf("agent commit: %v", err)
	}
	// the worktree is now CLEAN — the pre-fix failure mode.
	if changed, _ := wt.HasChanges(); changed {
		t.Fatal("precondition: a self-committed worktree should be clean")
	}

	// normalize: undo the agent's commit, keep its change pending.
	if err := wt.SoftResetTo(base); err != nil {
		t.Fatalf("soft reset: %v", err)
	}
	changed, err := wt.HasChanges()
	if err != nil || !changed {
		t.Fatalf("after SoftResetTo HasChanges=%v err=%v want true", changed, err)
	}
	diff, err := wt.Diff()
	if err != nil || !strings.Contains(diff, "feature.txt") || !strings.Contains(diff, "self-committed by agent") {
		t.Fatalf("diff must capture the agent's change after reset; err=%v diff:\n%s", err, diff)
	}
	// the harness's own commit now succeeds (it would have failed on the clean tree).
	sha, ref, err := wt.CommitAndPushEpoch("job-cx", 1, "harness commit")
	if err != nil {
		t.Fatalf("commit+push after reset: %v", err)
	}
	if got, ok := m.RefSHA(ref); !ok || got != sha {
		t.Fatalf("epoch ref sha=%q ok=%v want %s", got, ok, sha)
	}
}

// TestSoftResetToIsNoOpForNonCommittingAgent: a non-committing agent (claude) leaves HEAD
// at base, so SoftResetTo(base) must be a harmless no-op that preserves pending changes.
func TestSoftResetToIsNoOpForNonCommittingAgent(t *testing.T) {
	m, base := newFixture(t)
	ws := WorktreeBase(t.TempDir(), "job-cl", 1)
	wt, err := m.AddWorktree(ws, base)
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	defer wt.Destroy()
	mustWrite(t, filepath.Join(ws, "feature.txt"), "built, not committed\n")
	if err := wt.SoftResetTo(base); err != nil {
		t.Fatalf("soft reset (no-op): %v", err)
	}
	changed, err := wt.HasChanges()
	if err != nil || !changed {
		t.Fatalf("pending change must survive the no-op reset: HasChanges=%v err=%v", changed, err)
	}
}

// TestDropRefOrphansEpochRef proves the M11 compensation primitive (§6.5.4, I-12):
// dropping a dead epoch's ref orphans the zombie's work so it can never be promoted.
// Dropping a missing ref is a no-op (idempotent compensation).
func TestDropRefOrphansEpochRef(t *testing.T) {
	m, base := newFixture(t)

	ws := WorktreeBase(t.TempDir(), "job-z", 1)
	wt, err := m.AddWorktree(ws, base)
	if err != nil {
		t.Fatalf("add worktree: %v", err)
	}
	defer wt.Destroy()
	mustWrite(t, filepath.Join(ws, "zombie.txt"), "by zombie\n")
	_, ref, err := wt.CommitAndPushEpoch("job-z", 1, "zombie build")
	if err != nil {
		t.Fatalf("commit+push: %v", err)
	}
	if _, ok := m.RefSHA(ref); !ok {
		t.Fatalf("epoch ref must exist before drop")
	}

	// drop the dead epoch ref: it is orphaned, gone from the mirror.
	if err := m.DropRef(ref); err != nil {
		t.Fatalf("drop ref: %v", err)
	}
	if _, ok := m.RefSHA(ref); ok {
		t.Fatalf("the dead epoch ref must be gone after DropRef")
	}
	// a promotion of the orphaned ref now fails (there is nothing to promote).
	if err := m.PromoteEpochRef(ref, "refs/heads/never"); err == nil {
		t.Fatalf("promoting an orphaned ref must fail (nothing to fast-forward)")
	}
	if _, ok := m.RefSHA("refs/heads/never"); ok {
		t.Fatalf("the real branch must never advance from an orphaned ref")
	}
	// dropping again (or dropping a never-existed ref) is a no-op (idempotent).
	if err := m.DropRef(ref); err != nil {
		t.Fatalf("re-drop must be a no-op: %v", err)
	}
	if err := m.DropRef(EpochRef("job-z", 99)); err != nil {
		t.Fatalf("dropping a missing ref must be a no-op: %v", err)
	}
}

// TestBundleCloneApplyPushEpoch proves the F3 credential-less round-trip at the
// gitops layer: the control plane bundles base_sha; a worker clones a working tree
// from the bundle bytes alone (no mirror, no creds), edits it, and returns a diff;
// the control plane applies that diff onto base and pushes the epoch ref ITSELF;
// the pushed ref carries exactly the worker's change and promotes onto a branch.
func TestBundleCloneApplyPushEpoch(t *testing.T) {
	m, base := newFixture(t)

	// (1) control plane serves a read-only bundle of base_sha.
	bundle, err := m.Bundle(base)
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if len(bundle) == 0 {
		t.Fatalf("bundle must be non-empty")
	}

	// (2) the CREDENTIAL-LESS worker clones a working tree from the bundle bytes —
	// no mirror, no network, no token — edits it, and computes a diff against base.
	wsDir := filepath.Join(t.TempDir(), "ws")
	ws, err := CloneFromBundle(wsDir, bundle, base)
	if err != nil {
		t.Fatalf("clone from bundle: %v", err)
	}
	defer ws.Destroy()
	mustWrite(t, filepath.Join(wsDir, "feature.go"), "package x // by bundle worker\n")
	changed, err := ws.HasChanges()
	if err != nil || !changed {
		t.Fatalf("HasChanges=%v err=%v want true", changed, err)
	}
	diff, err := ws.Diff()
	if err != nil {
		t.Fatalf("ws diff: %v", err)
	}
	if !strings.Contains(diff, "feature.go") || !strings.Contains(diff, "by bundle worker") {
		t.Fatalf("worker diff missing change:\n%s", diff)
	}

	// the worker has NO push path on its workspace: BundleWorkspace exposes no
	// CommitAndPushEpoch — it can only return the diff (compile-time guarantee).

	// (3) the CONTROL PLANE applies the untrusted patch onto base and pushes the
	// epoch ref itself (Flowbee does the git write — R4/§8).
	sha, ref, err := m.ApplyPatchAndPushEpoch("job-bundle", 1, base, diff, "flowbee applies bundle patch")
	if err != nil {
		t.Fatalf("apply+push: %v", err)
	}
	if ref != EpochRef("job-bundle", 1) {
		t.Fatalf("ref=%s want %s", ref, EpochRef("job-bundle", 1))
	}
	got, ok := m.RefSHA(ref)
	if !ok || got != sha {
		t.Fatalf("epoch ref sha=%q ok=%v want %s", got, ok, sha)
	}

	// (4) the pushed epoch ref carries the worker's change and promotes onto a branch.
	if err := m.PromoteEpochRef(ref, "refs/heads/job-bundle-branch"); err != nil {
		t.Fatalf("promote: %v", err)
	}
	tree, err := run("", "git", "--git-dir", m.Path, "show", "refs/heads/job-bundle-branch:feature.go")
	if err != nil {
		t.Fatalf("show promoted file: %v", err)
	}
	if !strings.Contains(tree, "by bundle worker") {
		t.Fatalf("promoted branch missing worker change:\n%s", tree)
	}
}

// TestApplyPatchRejectsMalformedDiff proves a hostile/malformed patch cannot
// corrupt the mirror: it fails to apply in the disposable worktree and the epoch
// ref is never created.
func TestApplyPatchRejectsMalformedDiff(t *testing.T) {
	m, base := newFixture(t)
	_, _, err := m.ApplyPatchAndPushEpoch("job-bad", 1, base, "this is not a valid diff\n", "msg")
	if err == nil {
		t.Fatalf("a malformed diff must fail to apply")
	}
	if _, ok := m.RefSHA(EpochRef("job-bad", 1)); ok {
		t.Fatalf("a failed apply must NOT create the epoch ref")
	}
}

func mustGit(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestGCAutoIsSafeAndAddWorktreeStillWorks: GCAuto runs on a real bare mirror without
// error, and a worktree add right after it (the per-build hot path that now calls
// GCAuto) still succeeds — so the maintenance hook never breaks a legitimate build.
func TestGCAutoIsSafeAndAddWorktreeStillWorks(t *testing.T) {
	m, base := newFixture(t)
	if err := m.GCAuto(); err != nil {
		t.Fatalf("GCAuto on a fresh mirror: %v", err)
	}
	// AddWorktree now calls GCAuto internally; prove it still provisions a worktree.
	wsDir := filepath.Join(t.TempDir(), "ws")
	wt, err := m.AddWorktree(wsDir, base)
	if err != nil {
		t.Fatalf("AddWorktree after GCAuto: %v", err)
	}
	defer wt.Destroy()
	if _, err := os.Stat(filepath.Join(wsDir, "README.md")); err != nil {
		t.Fatalf("worktree not checked out: %v", err)
	}
	// idempotent: a second GC is fine too.
	if err := m.GCAuto(); err != nil {
		t.Fatalf("second GCAuto: %v", err)
	}
}
