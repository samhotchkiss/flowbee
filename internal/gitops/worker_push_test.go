package gitops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkerPushStacksNodeCommits proves the worker-push model: a build node commits
// (authored as itself) and pushes the issue branch; a later node (a reviewer's EMPTY
// findings-commit) starts from the branch tip and STACKS on top, authored as the
// reviewer. The branch history is then the node-by-node story.
func TestWorkerPushStacksNodeCommits(t *testing.T) {
	m, base := newFixture(t)
	remote := m.Path // the bare mirror doubles as the GitHub origin in-test

	// ── build node: change, commit AS builder, push to flowbee/issue-7 ──
	ws1 := WorktreeBase(t.TempDir(), "job-b", 1)
	wt1, err := m.AddWorktree(ws1, base)
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	mustWrite(t, filepath.Join(ws1, "feature.txt"), "built by the agent\n")
	sha1, err := wt1.CommitAuthored("builder-claude", "build(builder-claude): add feature\n\nImplements the thing.", false)
	if err != nil {
		t.Fatalf("commit authored: %v", err)
	}
	if err := wt1.PushTo(remote, "flowbee/issue-7", false); err != nil {
		t.Fatalf("push build: %v", err)
	}
	wt1.Destroy()

	// the branch now exists on the remote at the build commit.
	tip, exists, err := m.RemoteBranchTip(remote, "flowbee/issue-7")
	if err != nil || !exists || tip != sha1 {
		t.Fatalf("RemoteBranchTip = %s,%v,%v want %s,true,nil", tip, exists, err, sha1)
	}
	// a branch that does not exist reports exists=false (the first-build start signal).
	if _, ex, _ := m.RemoteBranchTip(remote, "flowbee/issue-999"); ex {
		t.Fatal("absent branch must report exists=false")
	}

	// ── reviewer node: start from the branch tip, EMPTY findings-commit AS reviewer ──
	ws2 := WorktreeBase(t.TempDir(), "job-b", 2)
	wt2, err := m.AddWorktree(ws2, tip)
	if err != nil {
		t.Fatalf("reviewer worktree: %v", err)
	}
	sha2, err := wt2.CommitAuthored("reviewer-opus", "review(reviewer-opus): APPROVED\n\nMeets the spec; tests pass.", true)
	if err != nil {
		t.Fatalf("empty commit: %v", err)
	}
	if sha2 == sha1 {
		t.Fatal("the empty findings-commit must advance HEAD")
	}
	if err := wt2.PushTo(remote, "flowbee/issue-7", false); err != nil {
		t.Fatalf("fast-forward push of the empty commit: %v", err)
	}
	wt2.Destroy()

	// the reviewer's empty commit STACKED on the build commit (parent == build sha).
	parent, _ := run("", "git", "--git-dir", m.Path, "rev-parse", sha2+"^")
	if strings.TrimSpace(parent) != sha1 {
		t.Fatalf("empty commit parent = %s, want %s (must stack on the build)", strings.TrimSpace(parent), sha1)
	}
	// and it is authored AS the reviewer identity (git log attributes the verdict).
	author, _ := run("", "git", "--git-dir", m.Path, "log", "-1", "--format=%an", sha2)
	if strings.TrimSpace(author) != "reviewer-opus" {
		t.Fatalf("empty commit author = %s, want reviewer-opus", strings.TrimSpace(author))
	}
	// the empty commit introduced NO file changes (it is purely the verdict record).
	stat, _ := run("", "git", "--git-dir", m.Path, "diff", "--name-only", sha1, sha2)
	if strings.TrimSpace(stat) != "" {
		t.Fatalf("empty findings-commit must touch no files, got: %q", stat)
	}
}

func TestWorkerPushRebasesIssueBranchOntoGrantedBase(t *testing.T) {
	m, oldBase := newFixture(t)
	remote := m.Path

	// Existing issue branch was built from the old integration base.
	ws1 := WorktreeBase(t.TempDir(), "job-b", 1)
	wt1, err := m.AddWorktree(ws1, oldBase)
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	mustWrite(t, filepath.Join(ws1, "feature.txt"), "built on old base\n")
	oldTip, err := wt1.CommitAuthored("builder-claude", "build: old base", false)
	if err != nil {
		t.Fatalf("commit authored: %v", err)
	}
	if err := wt1.PushTo(remote, "flowbee/issue-7", false); err != nil {
		t.Fatalf("push build: %v", err)
	}
	wt1.Destroy()

	// Main advances independently. A rebuild lease now grants this new base SHA.
	wsMain := WorktreeBase(t.TempDir(), "main", 1)
	mainWT, err := m.AddWorktree(wsMain, oldBase)
	if err != nil {
		t.Fatalf("main worktree: %v", err)
	}
	mustWrite(t, filepath.Join(wsMain, "main.txt"), "new main\n")
	newBase, err := mainWT.CommitAuthored("integrator", "main advances", false)
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}
	if err := mainWT.PushTo(remote, "main", false); err != nil {
		t.Fatalf("push main: %v", err)
	}
	mainWT.Destroy()

	contains, err := m.IsAncestor(newBase, oldTip)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if contains {
		t.Fatal("old issue branch must not already contain the new base")
	}

	ws2 := WorktreeBase(t.TempDir(), "job-b", 2)
	wt2, err := m.AddWorktree(ws2, oldTip)
	if err != nil {
		t.Fatalf("rebuild worktree: %v", err)
	}
	if err := wt2.RebaseOnto(newBase); err != nil {
		t.Fatalf("rebase onto granted base: %v", err)
	}
	rebasedTip, err := wt2.HeadSHA()
	if err != nil {
		t.Fatalf("rebased head: %v", err)
	}
	if rebasedTip == oldTip {
		t.Fatal("rebase must create a new issue-branch head")
	}
	if err := wt2.PushTo(remote, "flowbee/issue-7", true); err != nil {
		t.Fatalf("force-push rebased issue branch: %v", err)
	}
	wt2.Destroy()

	tip, exists, err := m.RemoteBranchTip(remote, "flowbee/issue-7")
	if err != nil || !exists || tip != rebasedTip {
		t.Fatalf("RemoteBranchTip = %s,%v,%v want %s,true,nil", tip, exists, err, rebasedTip)
	}
	contains, err = m.IsAncestor(newBase, tip)
	if err != nil {
		t.Fatalf("IsAncestor after rebase: %v", err)
	}
	if !contains {
		t.Fatalf("rebased branch %s must contain granted base %s", tip, newBase)
	}
}

// TestRebaseOntoReturnsConflictErrorWithBranchDiff: when the branch's change overlaps
// work that landed on the new base (same file, divergent edits), RebaseOnto's 3-way apply
// conflicts and it returns a *RebaseConflictError carrying the branch's own diff — the
// signal + payload the worker uses to divert the job to a conflict_resolver instead of
// burning attempts to needs_human.
func TestRebaseOntoReturnsConflictErrorWithBranchDiff(t *testing.T) {
	m, oldBase := newFixture(t)
	remote := m.Path

	// branch edits shared.txt one way, from the old base.
	ws1 := WorktreeBase(t.TempDir(), "job-c", 1)
	wt1, err := m.AddWorktree(ws1, oldBase)
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	mustWrite(t, filepath.Join(ws1, "shared.txt"), "branch version of the line\n")
	oldTip, err := wt1.CommitAuthored("builder", "build: edit shared.txt", false)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := wt1.PushTo(remote, "flowbee/issue-9", false); err != nil {
		t.Fatalf("push: %v", err)
	}
	wt1.Destroy()

	// main edits the SAME line differently, so a rebase cannot auto-merge.
	wsMain := WorktreeBase(t.TempDir(), "main", 1)
	mainWT, err := m.AddWorktree(wsMain, oldBase)
	if err != nil {
		t.Fatalf("main worktree: %v", err)
	}
	mustWrite(t, filepath.Join(wsMain, "shared.txt"), "main version of the line\n")
	newBase, err := mainWT.CommitAuthored("integrator", "main edits shared.txt", false)
	if err != nil {
		t.Fatalf("main commit: %v", err)
	}
	mainWT.Destroy()

	ws2 := WorktreeBase(t.TempDir(), "job-c", 2)
	wt2, err := m.AddWorktree(ws2, oldTip)
	if err != nil {
		t.Fatalf("rebuild worktree: %v", err)
	}
	err = wt2.RebaseOnto(newBase)
	if err == nil {
		t.Fatal("rebase onto a divergent same-file base must conflict")
	}
	var conflict *RebaseConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err=%T (%v), want *RebaseConflictError", err, err)
	}
	if !strings.Contains(conflict.BranchDiff, "shared.txt") || !strings.Contains(conflict.BranchDiff, "branch version") {
		t.Fatalf("conflict.BranchDiff must carry the branch's own change, got:\n%s", conflict.BranchDiff)
	}
}

func TestWorkerPushFallsBackToFreshBaseForUnrelatedIssueBranch(t *testing.T) {
	m, base := newFixture(t)

	// Simulate a stale remote issue branch whose history is unrelated to the current
	// repo mirror. Flowbee cannot safely infer its net patch, so it should cut a fresh
	// builder worktree from the granted base instead of recycling the lease forever.
	work := filepath.Join(t.TempDir(), "unrelated")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("mkdir unrelated repo: %v", err)
	}
	mustGit(t, work, "git", "init")
	mustWrite(t, filepath.Join(work, "feature.txt"), "unrelated branch work\n")
	mustGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "add", "-A")
	mustGit(t, work, "git", "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "unrelated issue branch")
	mustGit(t, work, "git", "remote", "add", "origin", m.Path)
	mustGit(t, work, "git", "push", "origin", "HEAD:refs/heads/flowbee/issue-8")

	unrelatedTip, err := m.HeadSHA("refs/heads/flowbee/issue-8")
	if err != nil {
		t.Fatalf("unrelated branch tip: %v", err)
	}
	ws := WorktreeBase(t.TempDir(), "job-unrelated", 1)
	wt, err := m.AddWorktree(ws, unrelatedTip)
	if err != nil {
		t.Fatalf("unrelated worktree: %v", err)
	}
	defer wt.Destroy()

	if err := wt.RebaseOnto(base); err != nil {
		t.Fatalf("fresh-base fallback: %v", err)
	}
	head, err := wt.HeadSHA()
	if err != nil {
		t.Fatalf("head after fallback: %v", err)
	}
	if head != base {
		t.Fatalf("fallback head = %s, want granted base %s", head, base)
	}
	changed, err := wt.HasChanges()
	if err != nil {
		t.Fatalf("status after fallback: %v", err)
	}
	if changed {
		t.Fatal("fresh-base fallback must not carry unrelated branch changes")
	}
}
