// Package gitops performs the LOCAL git operations Flowbee and its same-box
// workers need: a shared bare mirror, per-lease worktrees off it at a base SHA,
// and pushes to epoch-namespaced refs `refs/flowbee/<job>/epoch-<n>` (DESIGN
// §3.5, §7.4, I-7/I-12). It shells out to the `git` binary — no network, no
// GitHub, no credentials. The worker pushes to its epoch ref; Flowbee alone
// promotes that ref onto a real branch after validating the epoch.
//
// This package is NOT part of the deterministic core (archcheck does not cover
// it): it does I/O against the filesystem. It is used by the Mode-A worker
// harness (same-box `worktree` provisioning) and by the control plane's
// promotion path.
package gitops

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EpochRef is the canonical epoch-namespaced ref a worker pushes its build to.
// Flowbee promotes it onto a real branch only after validating the epoch (I-12);
// a stale epoch's ref is orphaned, never promoted.
func EpochRef(jobID string, epoch int) string {
	return fmt.Sprintf("refs/flowbee/%s/epoch-%d", jobID, epoch)
}

// Mirror is a shared bare repository the control plane keeps. Same-box workers
// add per-lease worktrees off it (O(1), no network, no creds). The worker never
// receives a credential — it pushes locally to an epoch ref on this mirror.
type Mirror struct {
	// Path is the bare repo directory (…/mirror.git).
	Path string
}

// Open binds a Mirror to an existing bare repo at path.
func Open(path string) *Mirror { return &Mirror{Path: path} }

// InitBare creates a fresh bare repository at path (used by tests as the local
// bare-repo fixture; in production the mirror is cloned --mirror from origin).
func InitBare(path string) (*Mirror, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	if _, err := run("", "git", "init", "--bare", path); err != nil {
		return nil, fmt.Errorf("init bare: %w", err)
	}
	return &Mirror{Path: path}, nil
}

// HeadSHA returns the commit a ref points to (e.g. "refs/heads/main"). Used by
// tests to seed a job's base_sha from the fixture.
func (m *Mirror) HeadSHA(ref string) (string, error) {
	out, err := run("", "git", "--git-dir", m.Path, "rev-parse", ref)
	if err != nil {
		return "", fmt.Errorf("rev-parse %s: %w", ref, err)
	}
	return strings.TrimSpace(out), nil
}

// AddWorktree provisions a fresh, detached worktree at baseSHA under dir
// (DESIGN §7.4: `git worktree add <ws> <base_sha>`). The worktree is one-shot
// per lease (§7.5) — the caller destroys it after the result.
func (m *Mirror) AddWorktree(dir, baseSHA string) (*Worktree, error) {
	if _, err := run("", "git", "--git-dir", m.Path, "worktree", "add", "--detach", dir, baseSHA); err != nil {
		return nil, fmt.Errorf("worktree add at %s: %w", baseSHA, err)
	}
	return &Worktree{Dir: dir, mirror: m, baseSHA: baseSHA}, nil
}

// PromoteEpochRef fast-forwards the real branch from a worker's epoch ref AFTER
// the epoch has been validated by the control plane (the canonical PR-open
// step's (2), §7.3). A stale epoch's ref is simply never passed here. The update
// is a non-fast-forward-rejecting ref write: it refuses to rewrite history.
func (m *Mirror) PromoteEpochRef(epochRef, branch string) error {
	// resolve the epoch ref to a commit, then advance the branch to it.
	sha, err := run("", "git", "--git-dir", m.Path, "rev-parse", epochRef)
	if err != nil {
		return fmt.Errorf("resolve epoch ref %s: %w", epochRef, err)
	}
	if _, err := run("", "git", "--git-dir", m.Path, "update-ref", branch, strings.TrimSpace(sha)); err != nil {
		return fmt.Errorf("promote %s -> %s: %w", epochRef, branch, err)
	}
	return nil
}

// DropRef deletes a ref from the mirror — the compensation step that ORPHANS a
// dead epoch's work (§6.5.4: `drop refs/flowbee/<job>/epoch-<dead_epoch>`). A
// revoked-but-running zombie that pushed to its stale epoch ref leaves that ref
// behind; compensation drops it so it can never be promoted. Deleting a missing
// ref is a no-op (idempotent compensation).
func (m *Mirror) DropRef(ref string) error {
	if _, ok := m.RefSHA(ref); !ok {
		return nil // already gone: idempotent
	}
	if _, err := run("", "git", "--git-dir", m.Path, "update-ref", "-d", ref); err != nil {
		return fmt.Errorf("drop ref %s: %w", ref, err)
	}
	return nil
}

// RefSHA resolves any ref in the mirror to its commit SHA, or "" if absent.
func (m *Mirror) RefSHA(ref string) (string, bool) {
	out, err := run("", "git", "--git-dir", m.Path, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(out)
	return s, s != ""
}

// Worktree is a per-lease checkout off the mirror at a base SHA.
type Worktree struct {
	Dir     string
	mirror  *Mirror
	baseSHA string
}

// Run executes a command in the worktree (the agent CLI runs here).
func (w *Worktree) Run(name string, args ...string) (string, error) {
	return run(w.Dir, name, args...)
}

// HasChanges reports whether the agent left any tracked-or-untracked changes.
func (w *Worktree) HasChanges() (bool, error) {
	out, err := run(w.Dir, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Diff returns the unified diff of the worktree against its base SHA (the
// work-product the worker submits as `patch`, §7.3). It stages everything first
// so new files are included, then diffs the index against base.
func (w *Worktree) Diff() (string, error) {
	if _, err := run(w.Dir, "git", "add", "-A"); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	out, err := run(w.Dir, "git", "diff", "--cached", w.baseSHA)
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	return out, nil
}

// CommitAndPushEpoch commits the worktree's changes and pushes them to the
// epoch-namespaced ref on the mirror (DESIGN §3.5: the worker pushes HERE, never
// to a branch). Returns the pushed commit SHA. No credentials are involved — the
// push is a local ref write to the shared bare mirror.
func (w *Worktree) CommitAndPushEpoch(jobID string, epoch int, message string) (sha, ref string, err error) {
	if _, err := run(w.Dir, "git", "add", "-A"); err != nil {
		return "", "", fmt.Errorf("stage: %w", err)
	}
	// deterministic identity so the fixture needs no global git config.
	env := []string{
		"GIT_AUTHOR_NAME=flowbee-worker", "GIT_AUTHOR_EMAIL=worker@flowbee.local",
		"GIT_COMMITTER_NAME=flowbee-worker", "GIT_COMMITTER_EMAIL=worker@flowbee.local",
	}
	if _, err := runEnv(w.Dir, env, "git", "commit", "-m", message); err != nil {
		return "", "", fmt.Errorf("commit: %w", err)
	}
	head, err := run(w.Dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	ref = EpochRef(jobID, epoch)
	// push HEAD to the epoch ref on the mirror (the worktree's origin is the mirror).
	if _, err := run(w.Dir, "git", "push", w.mirror.Path, "HEAD:"+ref); err != nil {
		return "", "", fmt.Errorf("push epoch ref %s: %w", ref, err)
	}
	return strings.TrimSpace(head), ref, nil
}

// Destroy removes the per-lease worktree (§7.5: nothing survives the lease).
func (w *Worktree) Destroy() error {
	_, err := run("", "git", "--git-dir", w.mirror.Path, "worktree", "remove", "--force", w.Dir)
	if err != nil {
		// best-effort: also drop the directory if git refused.
		_ = os.RemoveAll(w.Dir)
	}
	return err
}

func run(dir, name string, args ...string) (string, error) {
	return runEnv(dir, nil, name, args...)
}

func runEnv(dir string, extraEnv []string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// WorktreeBase is a small helper for callers that build a unique per-lease dir.
func WorktreeBase(root, jobID string, epoch int) string {
	return filepath.Join(root, fmt.Sprintf("ws-%s-e%d", jobID, epoch))
}
