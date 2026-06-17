package gitops

import (
	"os"
	"strings"
	"testing"
)

// countWorktrees returns how many worktrees git tracks for the mirror (excludes the
// bare repo's own entry).
func countWorktrees(t *testing.T, m *Mirror) int {
	t.Helper()
	out, err := run("", "git", "--git-dir", m.Path, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatalf("worktree list: %v", err)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") && !strings.HasSuffix(line, m.Path) {
			n++
		}
	}
	return n
}

// TestAddWorktreePrunesLeakedEntries: a worker that crashes mid-build leaves its
// worktree directory deleted but its .git/worktrees/ metadata behind (Destroy is
// defer-only, so a kill skips it). Without pruning these accumulate forever on a
// long-running fleet. AddWorktree prunes first, so a leaked entry is reaped by the
// next build instead of leaking — the metadata count stays bounded.
func TestAddWorktreePrunesLeakedEntries(t *testing.T) {
	m, base := newFixture(t)
	wsRoot := t.TempDir()

	// build #1: add a worktree, then simulate a CRASH — delete the working dir WITHOUT
	// calling Destroy, exactly what a killed worker leaves behind.
	ws1 := WorktreeBase(wsRoot, "job-crash", 1)
	if _, err := m.AddWorktree(ws1, base); err != nil {
		t.Fatalf("add #1: %v", err)
	}
	if err := os.RemoveAll(ws1); err != nil {
		t.Fatal(err)
	}
	// the stale entry is still tracked (the leak).
	if got := countWorktrees(t, m); got != 1 {
		t.Fatalf("after simulated crash, tracked worktrees=%d, want 1 (the leak)", got)
	}

	// build #2: AddWorktree must prune the leak before adding, leaving exactly ONE
	// live worktree (its own), not two.
	ws2 := WorktreeBase(wsRoot, "job-next", 1)
	wt2, err := m.AddWorktree(ws2, base)
	if err != nil {
		t.Fatalf("add #2: %v", err)
	}
	defer wt2.Destroy()
	if got := countWorktrees(t, m); got != 1 {
		t.Fatalf("after a self-healing add, tracked worktrees=%d, want 1 (leak reaped)", got)
	}
}

// TestPruneWorktreesLeavesLiveWorktreeAlone: prune must NEVER reap a worktree whose
// working directory still exists — only the dead ones.
func TestPruneWorktreesLeavesLiveWorktreeAlone(t *testing.T) {
	m, base := newFixture(t)
	ws := WorktreeBase(t.TempDir(), "job-live", 1)
	wt, err := m.AddWorktree(ws, base)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	defer wt.Destroy()
	if err := m.PruneWorktrees(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := countWorktrees(t, m); got != 1 {
		t.Fatalf("prune reaped a LIVE worktree: tracked=%d, want 1", got)
	}
}
