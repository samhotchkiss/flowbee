package gitops

import (
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
