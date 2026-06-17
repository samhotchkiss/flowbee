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

// gitCredHelper is an inline git credential helper that supplies a GitHub token from the
// ENVIRONMENT (FLOWBEE_GITHUB_TOKEN, which the control-plane process already holds), so
// the token never appears in argv. argv (/proc/pid/cmdline) is world-readable and shows
// in `ps`; the env (/proc/pid/environ) is owner+root only. Git invokes it as `sh -c
// '<helper> "$@"' <name> <op>`, so $1 is the operation.
const gitCredHelper = `!f() { test "$1" = get && printf 'username=x-access-token\npassword=%s\n' "$FLOWBEE_GITHUB_TOKEN"; }; f`

// gitCredFor splits a token-bearing https URL (https://x-access-token:TOKEN@host/path)
// into a CLEAN url plus the `-c credential.helper=…` args that feed the token from the
// environment. The token is DISCARDED from the url — it is the same FLOWBEE_GITHUB_TOKEN
// the git process inherits, so the helper reads it from env. SSH and already-clean urls
// pass through with no helper (SSH auths by key; the helper only fires on an http 401).
func gitCredFor(remoteURL string) (clean string, credArgs []string) {
	const marker = "https://x-access-token:"
	if !strings.HasPrefix(remoteURL, marker) {
		return remoteURL, nil
	}
	rest := remoteURL[len(marker):]
	at := strings.IndexByte(rest, '@')
	if at < 0 {
		return remoteURL, nil
	}
	return "https://" + rest[at+1:], []string{"-c", "credential.helper=" + gitCredHelper}
}

// CloneBareMirror clones url into a bare mirror at path, keeping any GitHub token out of
// BOTH argv and the persisted config. It clones with a clean url + the env-based
// credential helper, then writes that helper into the mirror config so later
// `git fetch origin` authenticates from FLOWBEE_GITHUB_TOKEN in the environment — the
// token never lands on disk. A non-token (SSH/clean) url clones plainly.
func CloneBareMirror(path, url string) error {
	clean, cred := gitCredFor(url)
	args := append(append([]string{}, cred...), "clone", "--bare", "--quiet", clean, path)
	if _, err := run("", "git", args...); err != nil {
		return fmt.Errorf("clone bare mirror: %w", err)
	}
	if len(cred) > 0 {
		// https token clone: persist the env-reading helper so future fetches auth
		// without a token in the config (the origin url is already clean).
		_, _ = run("", "git", "--git-dir", path, "config", "credential.helper", gitCredHelper)
	}
	return nil
}

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

// FetchBranch force-updates a local branch from the mirror's origin (GitHub), so
// after a merge advances main on GitHub the mirror tracks it and the NEXT build is
// cut from the current tip (build-list: base_sha refresh after merge). Idempotent.
func (m *Mirror) FetchBranch(branch string) error {
	spec := "+refs/heads/" + branch + ":refs/heads/" + branch
	if _, err := run("", "git", "--git-dir", m.Path, "fetch", "--quiet", "origin", spec); err != nil {
		return fmt.Errorf("fetch %s: %w", branch, err)
	}
	return nil
}

// AddWorktree provisions a fresh, detached worktree at baseSHA under dir
// (DESIGN §7.4: `git worktree add <ws> <base_sha>`). The worktree is one-shot
// per lease (§7.5) — the caller destroys it after the result.
func (m *Mirror) AddWorktree(dir, baseSHA string) (*Worktree, error) {
	// Reap worktrees leaked by a crashed worker before adding. Worktree.Destroy is
	// defer-only, so a worker killed mid-build leaves a stale .git/worktrees/ entry
	// that survives reboots and accumulates without bound on a long-running fleet.
	// Pruning here makes every new build self-heal prior leaks; it is best-effort
	// (a prune failure must not block a legitimate add).
	_ = m.PruneWorktrees()
	// Keep the mirror from growing unbounded over a long-running fleet: every build
	// fetches + commits objects, so loose objects + dangling history (abandoned builds,
	// deleted issue branches) pile up. `git gc --auto` is a no-op below git's threshold
	// and self-batches the occasional real repack, so this is a cheap routine maintenance
	// hook at the per-build prune point — best-effort, never blocks a legitimate add.
	_ = m.GCAuto()
	if _, err := run("", "git", "--git-dir", m.Path, "worktree", "add", "--detach", dir, baseSHA); err != nil {
		return nil, fmt.Errorf("worktree add at %s: %w", baseSHA, err)
	}
	return &Worktree{Dir: dir, mirror: m, baseSHA: baseSHA}, nil
}

// PruneWorktrees reaps worktree metadata whose working directory is already gone —
// the residue of a worker that crashed before Worktree.Destroy could run. `git
// worktree prune` only removes entries with a missing working tree; it never touches
// a live worktree, so it is safe and idempotent to call before every add.
func (m *Mirror) PruneWorktrees() error {
	_, err := run("", "git", "--git-dir", m.Path, "worktree", "prune")
	return err
}

// GCAuto runs `git gc --auto` on the bare mirror: a near-instant no-op below git's
// loose-object threshold, a full repack + prune of unreachable objects above it. A
// mirror accumulates objects from every fetch and force-push (and dangling objects from
// abandoned builds and deleted issue branches), so without periodic gc its object store
// grows unboundedly over months — disk pressure + slower git ops. --auto self-batches
// the cost, so calling it routinely (after a job, on a serve tick) is cheap and safe;
// it also no-ops while another gc holds the lock. Errors are non-fatal (best-effort
// maintenance), so callers may ignore the return.
func (m *Mirror) GCAuto() error {
	_, err := run("", "git", "--git-dir", m.Path, "gc", "--auto", "--quiet")
	return err
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

// PushCommit publishes a commit that exists in this mirror to a remote as
// refs/heads/<branch>. This is the control-plane's credential-bearing write to
// GitHub (R4, build-list F3): the eng_worker pushed only a local epoch ref and
// holds no credential, so Flowbee — under its own token, baked into remoteURL —
// publishes the build commit as a branch a PR can be opened against. Force-updates
// the branch so a re-arm at a new epoch republishes cleanly.
func (m *Mirror) PushCommit(remoteURL, sha, branch string) error {
	clean, cred := gitCredFor(remoteURL)
	args := append(append([]string{}, cred...), "--git-dir", m.Path, "push", "--force", clean, sha+":refs/heads/"+branch)
	if _, err := run("", "git", args...); err != nil {
		return fmt.Errorf("push commit to %s: %w", branch, err)
	}
	return nil
}

// RemoteBranchTip returns the tip SHA of a branch on a remote WITHOUT fetching it
// (a cheap `git ls-remote`), so the worker-push harness can decide its start point:
// when the issue branch already exists (a revise / a review after a build), the next
// node starts from its tip so its commit STACKS on the prior nodes' commits; when it
// does not exist yet (the first build), the node starts from base. exists=false means
// the branch is absent on the remote (a brand-new issue branch).
func (m *Mirror) RemoteBranchTip(remoteURL, branch string) (sha string, exists bool, err error) {
	clean, cred := gitCredFor(remoteURL)
	args := append(append([]string{}, cred...), "--git-dir", m.Path, "ls-remote", clean, "refs/heads/"+branch)
	out, lerr := run("", "git", args...)
	if lerr != nil {
		return "", false, fmt.Errorf("ls-remote %s: %w", branch, lerr)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", false, nil
	}
	// "<sha>\trefs/heads/<branch>" — take the first field of the first line.
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", false, nil
	}
	return fields[0], true, nil
}

// FetchRef fetches a single ref from a remote into the mirror under a local name, so
// a worktree can be cut from a branch the mirror did not yet have (the issue branch
// tip the worker-push harness stacks on). Idempotent; force-updates the local ref.
func (m *Mirror) FetchRef(remoteURL, remoteRef, localRef string) error {
	spec := "+" + remoteRef + ":" + localRef
	clean, cred := gitCredFor(remoteURL)
	args := append(append([]string{}, cred...), "--git-dir", m.Path, "fetch", "--quiet", clean, spec)
	if _, err := run("", "git", args...); err != nil {
		return fmt.Errorf("fetch %s: %w", remoteRef, err)
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

// Bundle produces a self-contained `git bundle` of baseSHA as raw bytes (DESIGN
// §7.4 mode (a)): the control plane serves this over the authenticated worker
// channel so a CROSS-BOX worker can materialize a read-only working tree WITHOUT
// any credential (R4, I-14). The bundle carries exactly the one commit reachable
// from baseSHA under the ref `refs/flowbee/base` — no history beyond what the
// worker needs, no live repo access, no token. The returned bytes are pure
// read-only data: the worst a hostile worker can do with them is read code it was
// already going to build.
func (m *Mirror) Bundle(baseSHA string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "flowbee-bundle-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	bundlePath := filepath.Join(tmp, "base.bundle")
	// `git bundle create` refuses a bundle with no named ref ("empty bundle"). We
	// write a throwaway ref at baseSHA, bundle THAT ref (so the worker gets a real
	// branch tip to clone + check out), then delete the temp ref. The temp ref is
	// unique per call so concurrent bundles never collide.
	// the ref lives under refs/heads/ so `git clone` of the bundle treats it as a
	// branch tip and checks it out — the worker gets a populated working tree. The
	// short base SHA + a temp-dir-derived suffix make the ref unique so concurrent
	// /v1/bundle requests on the same mirror never collide.
	short := baseSHA
	if len(short) > 12 {
		short = short[:12]
	}
	tmpRef := "refs/heads/flowbee-bundle-base-" + short + "-" + filepath.Base(tmp)
	if _, err := run("", "git", "--git-dir", m.Path, "update-ref", tmpRef, baseSHA); err != nil {
		return nil, fmt.Errorf("bundle temp ref: %w", err)
	}
	defer func() { _, _ = run("", "git", "--git-dir", m.Path, "update-ref", "-d", tmpRef) }()
	if _, err := run("", "git", "--git-dir", m.Path, "bundle", "create", bundlePath, tmpRef); err != nil {
		return nil, fmt.Errorf("bundle create: %w", err)
	}
	return os.ReadFile(bundlePath)
}

// CloneFromBundle materializes a working tree from bundle bytes at baseSHA into
// dir (DESIGN §7.4 mode (a), WORKER side). The worker holds NO credential and
// never reaches GitHub — it clones from the bundle the control plane served. The
// returned BundleWorkspace runs the agent and computes the diff against base; it
// has NO push path (the control plane performs the git write, F3/R4). It is a
// package-level function because the worker has no Mirror.
func CloneFromBundle(dir string, bundle []byte, baseSHA string) (*BundleWorkspace, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp("", "flowbee-bw-")
	if err != nil {
		return nil, err
	}
	bundlePath := filepath.Join(tmp, "base.bundle")
	if err := os.WriteFile(bundlePath, bundle, 0o644); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	// clone from the bundle file (no network, no creds) then check out base.
	if _, err := run("", "git", "clone", "--quiet", bundlePath, dir); err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("clone from bundle: %w", err)
	}
	os.RemoveAll(tmp)
	// detach onto the exact base SHA the lease named (the bundle's only tip).
	if _, err := run(dir, "git", "checkout", "--quiet", "--detach", baseSHA); err != nil {
		return nil, fmt.Errorf("checkout base %s: %w", baseSHA, err)
	}
	return &BundleWorkspace{Dir: dir, baseSHA: baseSHA, bundleTmp: tmp}, nil
}

// BundleWorkspace is a credential-less worker workspace cloned from a bundle. The
// worker runs the agent in Dir and returns the diff against base; it CANNOT push
// (it has no mirror and no creds — the control plane does the git write, F3).
type BundleWorkspace struct {
	Dir       string
	baseSHA   string
	bundleTmp string
}

// Run executes a command in the bundle workspace (the agent CLI runs here).
func (b *BundleWorkspace) Run(name string, args ...string) (string, error) {
	return run(b.Dir, name, args...)
}

// HasChanges reports whether the agent left any tracked-or-untracked changes.
func (b *BundleWorkspace) HasChanges() (bool, error) {
	out, err := run(b.Dir, "git", "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// Diff returns the unified diff of the workspace against base (the `patch`
// work-product the worker returns, §7.3). It stages everything first so new files
// are included. This is the ONLY thing the bundle worker hands back — no ref, no
// commit, no push.
func (b *BundleWorkspace) Diff() (string, error) {
	if _, err := run(b.Dir, "git", "add", "-A"); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	out, err := run(b.Dir, "git", "diff", "--cached", b.baseSHA)
	if err != nil {
		return "", fmt.Errorf("diff: %w", err)
	}
	return out, nil
}

// Destroy removes the bundle workspace (§7.5: nothing survives the lease).
func (b *BundleWorkspace) Destroy() error { return os.RemoveAll(b.Dir) }

// ApplyPatchAndPushEpoch is the CONTROL-PLANE credential-less write path (F3,
// R4/§8): when a worker returns ONLY a diff (bundle/scoped-read provisioning, no
// epoch ref pushed back), Flowbee itself applies the untrusted patch onto baseSHA
// in a throwaway worktree, commits it under the Flowbee identity, and pushes the
// epoch-namespaced ref on the mirror — exactly the ref a same-box worker would
// have pushed, so the downstream promote/PR-open path is identical. The worker
// never touched git or GitHub. Returns the committed SHA + the epoch ref.
//
// The patch is UNTRUSTED DATA: `git apply` is run with --3way disabled and the
// worktree is discarded on any failure, so a malformed/hostile diff cannot corrupt
// the mirror — it simply fails to apply and the caller declines the result.
func (m *Mirror) ApplyPatchAndPushEpoch(jobID string, epoch int, baseSHA, diff, message string) (sha, ref string, err error) {
	if strings.TrimSpace(diff) == "" {
		return "", "", fmt.Errorf("apply patch: empty diff")
	}
	wsRoot, err := os.MkdirTemp("", "flowbee-apply-")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(wsRoot)
	wsDir := filepath.Join(wsRoot, "ws")
	wt, err := m.AddWorktree(wsDir, baseSHA)
	if err != nil {
		return "", "", fmt.Errorf("apply patch worktree: %w", err)
	}
	defer wt.Destroy()

	// feed the diff to `git apply` via stdin. --index stages applied changes so a
	// subsequent `git add -A` captures any new files the diff created.
	patchFile := filepath.Join(wsRoot, "patch.diff")
	if err := os.WriteFile(patchFile, []byte(diff), 0o644); err != nil {
		return "", "", err
	}
	if _, err := run(wsDir, "git", "apply", "--index", "--whitespace=nowarn", patchFile); err != nil {
		return "", "", fmt.Errorf("apply patch: %w", err)
	}
	sha, ref, err = wt.CommitAndPushEpoch(jobID, epoch, message)
	if err != nil {
		return "", "", err
	}
	return sha, ref, nil
}

// RebaseOutcome reports whether a build's patch replays cleanly onto a new base
// (F8 §E). Clean=true means the diff applied with no conflict (a trivial auto-rebase
// Flowbee performs with no agent: re-validate CI/review at NewBaseSHA and merge).
// Clean=false means a REAL conflict (overlapping edits) that needs a conflict_resolver
// agent. NewSHA is the rebased commit on a clean rebase (the new head to re-validate);
// empty on a conflict.
type RebaseOutcome struct {
	Clean      bool
	NewSHA     string
	NewBaseSHA string
	// Conflicts lists the paths git reported as conflicting (best-effort, for the
	// resolver's context). Empty on a clean rebase.
	Conflicts []string
}

// TryRebasePatch attempts to replay a build's untrusted diff onto newBaseSHA in a
// throwaway worktree (F8 §E "trivial case"): it `git apply`s the patch at the new
// base. A CLEAN apply is a trivial auto-rebase — it commits the replayed change and
// returns the new SHA (Flowbee re-validates CI/review at this head, no agent). A
// FAILED apply is a REAL conflict — it returns Clean=false + the conflicting paths so
// the caller can spawn a conflict_resolver job. The mirror is never mutated on a
// conflict; on a clean rebase the new commit is pushed to the job's epoch ref.
//
// The patch is UNTRUSTED DATA: it is applied in a disposable worktree discarded on
// any failure, so a hostile/malformed diff cannot corrupt the mirror.
func (m *Mirror) TryRebasePatch(jobID string, epoch int, newBaseSHA, diff, message string) (RebaseOutcome, error) {
	out := RebaseOutcome{NewBaseSHA: newBaseSHA}
	if strings.TrimSpace(diff) == "" {
		return out, fmt.Errorf("rebase: empty diff")
	}
	wsRoot, err := os.MkdirTemp("", "flowbee-rebase-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(wsRoot)
	wsDir := filepath.Join(wsRoot, "ws")
	wt, err := m.AddWorktree(wsDir, newBaseSHA)
	if err != nil {
		return out, fmt.Errorf("rebase worktree: %w", err)
	}
	defer wt.Destroy()

	patchFile := filepath.Join(wsRoot, "patch.diff")
	if err := os.WriteFile(patchFile, []byte(diff), 0o644); err != nil {
		return out, err
	}
	// dry-run first: does the diff apply cleanly at the NEW base? A failure here is
	// the REAL-conflict signal (overlapping edits the new base changed underneath).
	if _, err := run(wsDir, "git", "apply", "--check", "--whitespace=nowarn", patchFile); err != nil {
		out.Clean = false
		out.Conflicts = conflictPaths(diff)
		return out, nil // a conflict is not an error: the caller spawns a resolver job
	}
	// clean: apply for real, commit, push the rebased epoch ref.
	if _, err := run(wsDir, "git", "apply", "--index", "--whitespace=nowarn", patchFile); err != nil {
		// raced from check to apply (shouldn't happen with MaxOpenConns=1): treat as conflict.
		out.Clean = false
		out.Conflicts = conflictPaths(diff)
		return out, nil
	}
	sha, _, err := wt.CommitAndPushEpoch(jobID, epoch, message)
	if err != nil {
		return out, err
	}
	out.Clean = true
	out.NewSHA = sha
	return out, nil
}

// conflictPaths returns the touched paths of a diff (best-effort context for the
// resolver). It reuses the diff-header scan rather than depending on internal/content
// (which would create an import cycle for the deterministic core's consumers).
func conflictPaths(diff string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			rest := strings.TrimPrefix(line, "diff --git ")
			fields := strings.SplitN(rest, " ", 2)
			if len(fields) == 2 {
				p := strings.TrimPrefix(strings.TrimSpace(fields[1]), "b/")
				if p != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	return out
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

// DiffAgainst returns the unified diff of the worktree against an EXPLICIT ref —
// used by the worker-push harness to compute the FULL change vs main even when the
// worktree was cut from the issue-branch tip (a revise stacks on prior commits, but
// the content gate + reviewer want the whole PR change vs the integration base). It
// stages everything first so new files are included. Call BEFORE committing.
func (w *Worktree) DiffAgainst(ref string) (string, error) {
	if _, err := run(w.Dir, "git", "add", "-A"); err != nil {
		return "", fmt.Errorf("stage: %w", err)
	}
	out, err := run(w.Dir, "git", "diff", "--cached", ref)
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

// CommitAuthored commits the worktree's changes AUTHORED AS the node (author =
// identity), with the node's detailed message — the worker-push model where each
// node is its own committer (build-list: "each node commits"). allowEmpty=true makes
// the empty findings-commit a reviewer lands (no file changes, the verdict in the
// message). Returns the new commit SHA. Author and committer are both the node, so
// `git log` attributes the work to the agent that did it.
func (w *Worktree) CommitAuthored(identity, message string, allowEmpty bool) (sha string, err error) {
	if !allowEmpty {
		if _, err := run(w.Dir, "git", "add", "-A"); err != nil {
			return "", fmt.Errorf("stage: %w", err)
		}
	}
	email := identity + "@flowbee.local"
	env := []string{
		"GIT_AUTHOR_NAME=" + identity, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + identity, "GIT_COMMITTER_EMAIL=" + email,
	}
	args := []string{"commit", "-m", message}
	if allowEmpty {
		args = []string{"commit", "--allow-empty", "-m", message}
	}
	if _, err := runEnv(w.Dir, env, "git", args...); err != nil {
		return "", fmt.Errorf("commit: %w", err)
	}
	head, err := run(w.Dir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(head), nil
}

// PushTo publishes the worktree's HEAD to refs/heads/<branch> on a remote — the
// worker-push write (build-list): the node holds a key and pushes its own commit to
// the issue branch on GitHub. force=false is a fast-forward push (the node started
// from the branch tip, so its commit stacks cleanly); force=true republishes a
// rebased/re-armed head. remoteURL bears the node's credential (only the control
// plane caller ever uses the GitHub API — a push is plain git, not the API).
func (w *Worktree) PushTo(remoteURL, branch string, force bool) error {
	clean, cred := gitCredFor(remoteURL)
	args := append([]string{}, cred...)
	args = append(args, "push")
	if force {
		args = append(args, "--force")
	}
	args = append(args, clean, "HEAD:refs/heads/"+branch)
	if _, err := run(w.Dir, "git", args...); err != nil {
		return fmt.Errorf("push to %s: %w", branch, err)
	}
	return nil
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

// ReadFileAtRef returns the content of a repo-relative path as committed at ref
// (e.g. "refs/heads/main"). ok=false if the path does not exist at that ref. Used
// to read back Flowbee-authored files (e.g. the history archive) without a
// worktree. No network, no credentials — a plain object read on the mirror.
func (m *Mirror) ReadFileAtRef(ref, path string) (string, bool, error) {
	out, err := run("", "git", "--git-dir", m.Path, "cat-file", "-p",
		ref+":"+filepath.ToSlash(path))
	if err != nil {
		return "", false, nil // absent at ref (or bad path): not an error to the caller
	}
	return out, true, nil
}

// HistoryFile is one file the post-merge history commit writes (path relative to
// the repo root + its content). Mirrors store.HistoryArtifact without importing it
// (gitops is the I/O leaf; the control plane wires the two together).
type HistoryFile struct {
	Path    string
	Content string
}

// CommitHistory lands the issue-archive markdown projection (build-list §F) as a
// DEDICATED commit on branch (the default branch), authored as Flowbee — the sole
// writer. It is NOT entangled with any feature PR: Flowbee checks out the branch in
// a throwaway worktree, writes the curated card(s) + the regenerated TOC, commits
// under the Flowbee identity, and advances the branch ref. A no-op (no file content
// change) commits nothing and returns ok=false. Returns the new commit SHA on a
// real write.
//
// The write is done in a disposable worktree off the mirror so it never disturbs a
// live worktree, and the branch is advanced by the worktree's commit (a fast-forward
// of the local branch ref). No network, no credentials — the archive lives in the
// same mirror the rest of Flowbee's git writes use.
func (m *Mirror) CommitHistory(branch, message string, files []HistoryFile) (sha string, ok bool, err error) {
	if len(files) == 0 {
		return "", false, nil
	}
	// fully-qualify the branch so `update-ref` writes refs/heads/<branch> (a bare
	// `update-ref main` would create a stray top-level `main` ref, not the branch).
	ref := branch
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + branch
	}
	baseSHA, hasBase := m.RefSHA(ref)
	wsRoot, err := os.MkdirTemp("", "flowbee-history-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(wsRoot)
	wsDir := filepath.Join(wsRoot, "ws")

	if hasBase {
		// check out the current branch tip into a detached worktree so the commit's
		// parent is the live branch head (the archive accumulates over merges).
		if _, err := run("", "git", "--git-dir", m.Path, "worktree", "add", "--detach", wsDir, baseSHA); err != nil {
			return "", false, fmt.Errorf("history worktree: %w", err)
		}
	} else {
		// the branch does not exist yet (fresh repo): start an orphan history root.
		if _, err := run("", "git", "--git-dir", m.Path, "worktree", "add", "--detach", wsDir); err != nil {
			return "", false, fmt.Errorf("history worktree (orphan): %w", err)
		}
		if _, err := run(wsDir, "git", "checkout", "--orphan", "flowbee-history-tmp"); err != nil {
			return "", false, fmt.Errorf("history orphan checkout: %w", err)
		}
		if _, err := run(wsDir, "git", "rm", "-rf", "--ignore-unmatch", "."); err != nil {
			return "", false, fmt.Errorf("history orphan clean: %w", err)
		}
	}
	defer func() { _, _ = run("", "git", "--git-dir", m.Path, "worktree", "remove", "--force", wsDir) }()

	for _, f := range files {
		full := filepath.Join(wsDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return "", false, err
		}
		if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
			return "", false, err
		}
		if _, err := run(wsDir, "git", "add", "--", f.Path); err != nil {
			return "", false, fmt.Errorf("history add %s: %w", f.Path, err)
		}
	}

	// nothing changed? do not author an empty commit (idempotent re-drain).
	if out, _ := run(wsDir, "git", "status", "--porcelain"); strings.TrimSpace(out) == "" {
		return "", false, nil
	}

	env := []string{
		"GIT_AUTHOR_NAME=flowbee", "GIT_AUTHOR_EMAIL=flowbee@flowbee.local",
		"GIT_COMMITTER_NAME=flowbee", "GIT_COMMITTER_EMAIL=flowbee@flowbee.local",
	}
	if _, err := runEnv(wsDir, env, "git", "commit", "-m", message); err != nil {
		return "", false, fmt.Errorf("history commit: %w", err)
	}
	head, err := run(wsDir, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", false, fmt.Errorf("history rev-parse: %w", err)
	}
	head = strings.TrimSpace(head)
	// advance the real branch ref to the new commit (Flowbee's dedicated commit).
	if _, err := run("", "git", "--git-dir", m.Path, "update-ref", ref, head); err != nil {
		return "", false, fmt.Errorf("history advance %s: %w", ref, err)
	}
	return head, true, nil
}
