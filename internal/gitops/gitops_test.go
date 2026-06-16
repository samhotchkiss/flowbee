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
