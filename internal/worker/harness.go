package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/gitops"
)

// TaskFileRel is the worktree-relative path the harness writes the resolved task
// into (§B). The default agent-cmd convention is: the agent reads $FLOWBEE_TASK_FILE
// (an absolute path to this file) — or, equivalently, .flowbee/task.md at the repo
// root — and acts on it. $FLOWBEE_SPEC / $FLOWBEE_ACCEPTANCE carry the spec and
// acceptance criteria inline for agents that prefer env over a file.
const TaskFileRel = ".flowbee/task.md"

// writeTaskContext materializes the F1 lease context block (grant.Context) into
// the worktree and returns the env vars an agent CLI reads. It writes
// .flowbee/task.md (a human/agent-readable task brief) plus .flowbee/context.json
// (the raw resolved context, for tool-driven agents), and returns
// FLOWBEE_TASK_FILE / FLOWBEE_SPEC / FLOWBEE_ACCEPTANCE / FLOWBEE_IDENTITY /
// FLOWBEE_LENS so any agent reads the task without knowing Flowbee. A nil context
// (old server) is tolerated: the file/env still describe the bare job.
func writeTaskContext(wsDir string, grant client.LeaseGrant) ([]string, error) {
	dir := filepath.Join(wsDir, ".flowbee")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := grant.Context
	if c == nil {
		c = &client.LeaseContext{Role: grant.Role, BaseSHA: grant.BaseSHA}
	}

	md := renderTaskMarkdown(grant.JobID, c)
	taskFile := filepath.Join(dir, "task.md")
	if err := os.WriteFile(taskFile, []byte(md), 0o644); err != nil {
		return nil, err
	}
	if raw, err := json.MarshalIndent(c, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "context.json"), raw, 0o644)
	}

	return []string{
		"FLOWBEE_TASK_FILE=" + taskFile,
		"FLOWBEE_TASK=" + c.Task,
		"FLOWBEE_SPEC=" + c.Spec,
		"FLOWBEE_ACCEPTANCE=" + c.AcceptanceCriteria,
		"FLOWBEE_IDENTITY=" + c.Identity,
		"FLOWBEE_LENS=" + c.Lens,
	}, nil
}

// renderTaskMarkdown renders the resolved context block as the .flowbee/task.md
// brief the agent reads. Deterministic given the inputs (no clock).
func renderTaskMarkdown(jobID string, c *client.LeaseContext) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Flowbee task: %s\n\n", jobID)
	if c.Identity != "" {
		fmt.Fprintf(&b, "- **Act as:** %s\n", c.Identity)
	}
	if c.Lens != "" {
		fmt.Fprintf(&b, "- **Lens:** %s\n", c.Lens)
	}
	if c.Role != "" {
		fmt.Fprintf(&b, "- **Role:** %s\n", c.Role)
	}
	if c.BaseSHA != "" {
		fmt.Fprintf(&b, "- **Base SHA:** %s\n", c.BaseSHA)
	}
	b.WriteString("\n## Task\n\n")
	if c.Task != "" {
		b.WriteString(c.Task)
	} else {
		b.WriteString("(no task text provided)")
	}
	b.WriteString("\n")
	if c.Spec != "" {
		b.WriteString("\n## Spec / context\n\n")
		b.WriteString(c.Spec)
		b.WriteString("\n")
	}
	if c.AcceptanceCriteria != "" {
		b.WriteString("\n## Acceptance criteria\n\n")
		b.WriteString(c.AcceptanceCriteria)
		b.WriteString("\n")
	}
	if c.Role == "eng_worker" || c.Role == "spec_author" {
		// §F compounding memory (cross-issue read): point the PRODUCING roles at the durable
		// issue archive and let the AGENT judge relevance (its strength) — no brittle
		// server-side matching heuristic. Self-gating: absent dir => the agent skips it.
		b.WriteString("\n## Precedent — consult the issue archive first\n\n" +
			"If this repo has a `docs/history/` directory, it archives how PRIOR issues were built — " +
			"status, attempts, the reviewers' findings, and the lessons learned. Before committing to an " +
			"approach, grep it for relevant precedent (e.g. `grep -rli \"<keyword>\" docs/history/`) so you " +
			"do NOT re-derive a dead end or repeat an approach that already failed review. " +
			"Skip this if the directory is absent.\n")
	}
	if c.PriorReviewFindings != "" {
		// the actionable feedback FIRST (before the raw verdict JSON): a prior review
		// requested changes — the agent must address these, not re-submit the same patch.
		b.WriteString("\n## A prior review requested changes — ADDRESS THESE\n\n")
		b.WriteString("Your previous attempt was rejected in review with this feedback. " +
			"Fix exactly what is called out below before resubmitting; do not repeat the rejected approach.\n\n")
		b.WriteString(c.PriorReviewFindings)
		b.WriteString("\n")
	}
	if c.PriorVerdict != nil {
		if raw, err := json.Marshal(c.PriorVerdict); err == nil {
			b.WriteString("\n## Prior verdict\n\n```json\n")
			b.Write(raw)
			b.WriteString("\n```\n")
		}
	}
	if c.Rebuild {
		b.WriteString("\n## ⚠️ This is a RE-ATTEMPT — a previous build was rejected\n\n" +
			"A prior attempt at this task FAILED (CI was red — build/lint/tests — or a reviewer requested changes). " +
			"The prior change is ALREADY in this working directory. Do NOT just re-submit it. " +
			"Carefully review the existing change for the failure: build errors, linter violations (e.g. golangci-lint), " +
			"and failing/smoke tests. Run the linter and tests if they are available, and FIX what is broken so CI passes this time.\n")
	}
	if c.Conflict {
		b.WriteString("\n## ⚠️ This is a CONFLICT RESOLUTION — resolve the merge conflict markers\n\n" +
			"This working directory is at the LATEST main, and YOUR original change has ALREADY been applied on top " +
			"with a 3-way merge. Where your change overlapped a sibling change that merged into the same area, git left " +
			"conflict markers in the files:\n\n" +
			"```\n<<<<<<< ours\n(the sibling's version, now on main)\n=======\n(your change)\n>>>>>>> theirs\n```\n\n" +
			"Your job: find every `<<<<<<<` / `=======` / `>>>>>>>` marker and resolve it by KEEPING BOTH sides' " +
			"intent — combine them into one coherent result, then DELETE the marker lines. Run `grep -rn '<<<<<<<' .` " +
			"to find them all; none may remain. If there are NO markers, your change applied cleanly and is already " +
			"present — verify it is correct and make no further edits. Do NOT discard the sibling's change, and do NOT " +
			"revert your own.\n")
		if strings.TrimSpace(c.Diff) != "" {
			b.WriteString("\n### Your original intended change, for reference (already applied above)\n\n```diff\n")
			b.WriteString(strings.TrimSpace(c.Diff))
			b.WriteString("\n```\n")
		}
	}
	b.WriteString("\n## How to complete this\n\nMake the change by creating or editing files in THIS working directory. " +
		"Write the actual files to disk now — do not just describe or print them. Touch only what the task requires.\n")
	b.WriteString("\nWhen done, write `.flowbee/commit.md` with a clear, DETAILED commit message for your change: " +
		"a concise one-line summary, a blank line, then a body explaining WHAT you changed and WHY. " +
		"This becomes the commit on the issue branch, so the history shows how the work was built.\n")
	return b.String()
}

// nodeCommitMessage is the commit message a build/resolve node commits with. It
// prefers the agent's OWN description of its change — the agent writes detailed
// notes to .flowbee/commit.md (instructed by the brief), so the commit body is the
// node author's account of WHAT changed and WHY (build-list: "detailed notes in
// each commit"). When the agent wrote none, it falls back to a rendered default
// built from the task. Must be called BEFORE the .flowbee scaffolding is stripped.
func nodeCommitMessage(wsDir, identity, role, jobID string, c *client.LeaseContext) string {
	if raw, err := os.ReadFile(filepath.Join(wsDir, ".flowbee", "commit.md")); err == nil {
		if msg := strings.TrimSpace(string(raw)); msg != "" {
			return msg
		}
	}
	verb := "build"
	if role == "conflict_resolver" {
		verb = "resolve"
	}
	title := jobID
	if c != nil {
		if t := firstLineOf(c.Task); t != "" {
			title = strings.TrimLeft(t, "# ")
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s(%s): %s\n\n", verb, identity, title)
	if c != nil && strings.TrimSpace(c.Task) != "" {
		b.WriteString(strings.TrimSpace(c.Task))
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Job: %s — committed by the %s node (no .flowbee/commit.md written).", jobID, identity)
	return b.String()
}

// workerMirrorFor resolves the worker's local mirror path for a given repo URL (F9
// multi-repo): a fungible worker keeps one mirror PER repo so flowbee + russ jobs
// never share a clone. It places per-repo mirrors as siblings of the configured
// --mirror (so `--mirror ~/dev/flowbee` => russ jobs land at `~/dev/russ`), or under
// the system temp dir when nothing is configured. With no repo URL it returns the
// configured path (single-repo) or a temp default.
func workerMirrorFor(configured, repoURL string) string {
	name := repoNameFromURL(repoURL)
	if name == "" {
		// single-repo, no lease URL: `configured` is the specific bare mirror.
		if configured != "" {
			return configured
		}
		return filepath.Join(os.TempDir(), "flowbee-worker-mirror.git")
	}
	// per-repo BARE mirror, ".git"-suffixed so it NEVER collides with a working-tree
	// checkout at <dir>/<repo> (Sam keeps working clones at ~/dev/<repo>; `git
	// --git-dir <working-tree>` fails — the worker needs its OWN bare mirror). --mirror
	// is the mirrors DIRECTORY (a bare-mirror path ending in .git uses its parent);
	// default ~/.flowbee/mirrors.
	return filepath.Join(mirrorsDir(configured), name+".git")
}

// repoNameFromURL extracts the bare repo name ("russ") from a clone URL.
func repoNameFromURL(repoURL string) string {
	if repoURL == "" {
		return ""
	}
	name := repoURL
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".git")
}

// mirrorsDir resolves the directory the worker keeps its per-repo bare mirrors in.
func mirrorsDir(configured string) string {
	if configured != "" {
		if strings.HasSuffix(configured, ".git") {
			return filepath.Dir(configured) // a bare-mirror path -> its parent dir
		}
		return configured // a directory
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".flowbee", "mirrors")
	}
	return filepath.Join(os.TempDir(), "flowbee-mirrors")
}

func firstLineOf(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// HarnessConfig parameterizes the Mode-A worker harness (DESIGN §7.1): the thin
// pull loop around an agent CLI it knows nothing about. The provider appears in
// exactly two data values — AgentCmd (the CLI to spawn) and ModelFamily (a
// capability TAG) — and never in control flow.
type HarnessConfig struct {
	BaseURL     string
	Identity    string
	ModelFamily string
	Role        string
	// AgentCmd is the command line spawned per lease inside the fresh worktree
	// (FLOWBEE_AGENT_CMD). It is a black box; the harness extracts a diff against
	// base_sha afterward, never a parsed transcript.
	AgentCmd string
	// WorkRoot is where per-lease worktrees are provisioned (§7.5). Defaults to a
	// temp dir under the OS temp root.
	WorkRoot string
	// Arch / OS are the attestation handshake; default to this host's runtime.
	Arch string
	OS   string
	// Capabilities are CLAIMED at registration; the server attests them.
	Capabilities []string
	// BearerToken is the signed per-worker token (§7.6) for a non-loopback
	// (cross-box) listener. Empty on loopback (the server's loopback bypass).
	BearerToken string
	// F6 capacity advertisement. ModelSlots is the box's PER-MODEL concurrency
	// (claude:3, codex:3); Weight is the per-box distribution bias; Accounts are
	// the named per-model credentials (the rollover chain). All optional.
	ModelSlots map[string]int
	Weight     int
	Accounts   []client.AccountSpecMsg
	// MirrorPath + RepoURL drive --remote mode (RunOnceHarnessRemote): the worker
	// keeps its OWN bare mirror at MirrorPath (cloned from RepoURL, fetched each job
	// to stay current) and provisions a worktree per job off it — so many workers on
	// one box build in parallel. It returns only a diff; the control plane (which
	// alone holds write creds) does the GitHub writes. Branch is the integration
	// branch to track (default main).
	MirrorPath string
	RepoURL    string
	Branch     string
}

// HarnessOutcome reports what one lease cycle did.
type HarnessOutcome struct {
	Got        bool
	JobID      string
	LeaseEpoch int
	JobState   string
	PushedRef  string
	PushedSHA  string
	// Skipped is set when a review lease was released without acting (e.g. a
	// code_review job whose CI is not yet green): the work loop should back off
	// before retrying so it does not spin claiming+releasing the same job.
	Skipped bool
}

// RunOnceHarness performs ONE full Mode-A cycle against a real lease (§7.1):
// register+attest, long-poll-lease, provision a per-lease worktree at base_sha
// off the shared mirror, spawn the agent CLI in it, collect the work-product
// (commit + push to the epoch ref), submit the result, release, destroy the
// worktree. It never opens a PR and never calls GitHub (R4). Got=false on a 204.
func RunOnceHarness(ctx context.Context, cfg HarnessConfig) (HarnessOutcome, error) {
	arch, osName := cfg.Arch, cfg.OS
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName == "" {
		osName = runtime.GOOS
	}
	caps := cfg.Capabilities
	if len(caps) == 0 {
		caps = []string{"role:eng_worker", "model_family:" + cfg.ModelFamily, "arch:" + arch, "os:" + osName}
	}

	c := client.NewWithToken(cfg.BaseURL, cfg.BearerToken)
	if _, err := c.Register(ctx, client.Registration{
		Identity: cfg.Identity, Host: hostname(), Capabilities: caps, Arch: arch, OS: osName,
		ModelSlots: cfg.ModelSlots, Weight: cfg.Weight, Accounts: cfg.Accounts,
	}); err != nil {
		return HarnessOutcome{}, fmt.Errorf("register: %w", err)
	}

	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return HarnessOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return HarnessOutcome{Got: false}, nil
	}
	out := HarnessOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}

	// liveness: one heartbeat so the worker proves its loop is alive (§7.2).
	if _, st, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		return out, fmt.Errorf("heartbeat: %w", err)
	} else if st != 200 {
		return out, fmt.Errorf("heartbeat status %d", st)
	}

	// ── one-shot isolation: provision a fresh worktree at base_sha (§7.4/§7.5) ──
	if grant.Provisioning != "worktree" || grant.MirrorPath == "" {
		return out, fmt.Errorf("harness requires worktree provisioning; got %q", grant.Provisioning)
	}
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot, err = os.MkdirTemp("", "flowbee-ws-")
		if err != nil {
			return out, fmt.Errorf("mkdir workroot: %w", err)
		}
		defer os.RemoveAll(workRoot)
	}
	mirror := gitops.Open(grant.MirrorPath)
	wsDir := gitops.WorktreeBase(workRoot, grant.JobID, grant.LeaseEpoch)
	wt, err := mirror.AddWorktree(wsDir, grant.BaseSHA)
	if err != nil {
		return out, fmt.Errorf("provision worktree: %w", err)
	}
	defer wt.Destroy()

	// ── F1: materialize the resolved task/context into the worktree (§B) ──
	// The lease grant ships a self-contained context block. We write it to
	// .flowbee/task.md inside the worktree AND export FLOWBEE_TASK_FILE/FLOWBEE_SPEC
	// (+ the resolved identity/lens/role) so ANY agent CLI reads the task without
	// knowing Flowbee. The worker is UNTRUSTED: it acts AS grant.Context.Identity
	// (fenced by the server) and never chooses its own identity or task.
	taskEnv, err := writeTaskContext(wsDir, grant)
	if err != nil {
		return out, fmt.Errorf("write task context: %w", err)
	}

	// ── spawn the agent CLI in the worktree (a black box, §7.1) ──
	agentEnv := append(os.Environ(),
		"FLOWBEE_JOB_ID="+grant.JobID,
		"FLOWBEE_BASE_SHA="+grant.BaseSHA,
		"FLOWBEE_ROLE="+grant.Role,
	)
	agentEnv = append(agentEnv, taskEnv...)
	if err := runAgentHeartbeat(ctx, c, grant.JobID, grant.LeaseEpoch, grant.LeaseTTLS, wsDir, cfg.AgentCmd, agentEnv); err != nil {
		return out, err
	}
	// Normalize a self-committing agent: an agentic CLI (codex) may `git commit` its own
	// work, which would leave a clean worktree that HasChanges/CommitAndPushEpoch misread
	// as "no changes". Soft-reset any agent commits back to base so the changes are pending
	// again (no-op for a non-committing agent like claude). See Worktree.SoftResetTo.
	if err := wt.SoftResetTo(grant.BaseSHA); err != nil {
		return out, fmt.Errorf("normalize worktree: %w", err)
	}

	// The .flowbee/ scaffolding is Flowbee's INPUT to the agent, not the agent's
	// capture the node's own detailed commit message (the agent's .flowbee/commit.md)
	// BEFORE the scaffolding is stripped — it becomes the commit on the issue branch.
	commitMsg := nodeCommitMessage(wsDir, cfg.Identity, grant.Role, grant.JobID, grant.Context)
	// work-product: strip it before computing the diff so it never enters the
	// untrusted patch the content-integrity gate judges (§9.2).
	_ = os.RemoveAll(filepath.Join(wsDir, ".flowbee"))

	// ── collect the work-product: commit + push to the epoch ref (§7.3) ──
	changed, err := wt.HasChanges()
	if err != nil {
		return out, fmt.Errorf("inspect worktree: %w", err)
	}
	if !changed {
		// nothing produced: abandon the lease as a FAILED attempt. c.Release (not
		// ReleaseNoPenalty) burns an attempt, so an agent that consistently produces no
		// output escalates to needs_human once max_attempts is reached instead of
		// churning forever — switching this to ReleaseNoPenalty would reintroduce that
		// infinite no-output loop (see store.Release's exhaustion escalation).
		_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
		return out, fmt.Errorf("agent produced no changes")
	}
	// the unified diff against base is the UNTRUSTED work-product the M9
	// content-integrity gate (§9.2, I-11) judges. We compute it BEFORE the commit so
	// the staged tree is captured; an error here is non-fatal (the gate then runs
	// over an empty diff — the safe default is not-eligible only if the worker also
	// asked for self_merge).
	diff, _ := wt.Diff()

	sha, ref, err := wt.CommitAndPushEpoch(grant.JobID, grant.LeaseEpoch, commitMsg)
	if err != nil {
		return out, fmt.Errorf("push epoch ref: %w", err)
	}
	out.PushedRef, out.PushedSHA = ref, sha

	// ── submit the real patch result (fenced, idempotent), then release (§7.1) ──
	// An HONEST worker declares exactly the paths its diff touches; Flowbee verifies
	// the declaration against the actual diff (§9.2b) — they agree for an honest
	// worker and diverge for a tampering one.
	idem := fmt.Sprintf("%s-e%d", grant.JobID, grant.LeaseEpoch)
	body := map[string]any{
		"kind":       "patch",
		"base_sha":   grant.BaseSHA,
		"pushed_ref": ref,
		"diff":       diff,
		"blast_radius": map[string]any{
			"scope": "worktree",
			"paths": content.TouchedPaths(diff),
		},
		"status": "succeeded", // a HINT only — never the verdict (I-9)
	}
	res, st, err := c.Result(ctx, grant.JobID, grant.LeaseEpoch, idem, body)
	if err != nil {
		return out, fmt.Errorf("result: %w", err)
	}
	if st != 200 {
		return out, fmt.Errorf("result status %d", st)
	}
	out.JobState = res.JobState

	if _, err := c.Release(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		// a non-fatal: the server also reaps on TTL (§7.2). The result already landed.
		_ = err
	}
	return out, nil
}

// RunOnceHarnessBundle performs ONE full CREDENTIAL-LESS Mode-A cycle (F3, §7.4
// mode (a)): register+attest, long-poll-lease, fetch a read-only git BUNDLE of
// base_sha from the control plane (NO GitHub credential, no local mirror, no push
// path), clone a working tree from it, spawn the agent CLI, collect ONLY the diff,
// and submit it. The control plane applies the patch + pushes the epoch ref + opens
// the PR — Flowbee does ALL git writes (R4/§8). The worker holds no creds and never
// touches git history or GitHub. Got=false on a 204.
//
// This is the cross-box topology: the worker needs no inbound repo access and no
// outbound GitHub access — only the authenticated worker channel to Flowbee.
func RunOnceHarnessBundle(ctx context.Context, cfg HarnessConfig) (HarnessOutcome, error) {
	arch, osName := cfg.Arch, cfg.OS
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName == "" {
		osName = runtime.GOOS
	}
	caps := cfg.Capabilities
	if len(caps) == 0 {
		caps = []string{"role:eng_worker", "model_family:" + cfg.ModelFamily, "arch:" + arch, "os:" + osName}
	}

	c := client.NewWithToken(cfg.BaseURL, cfg.BearerToken)
	if _, err := c.Register(ctx, client.Registration{
		Identity: cfg.Identity, Host: hostname(), Capabilities: caps, Arch: arch, OS: osName,
		ModelSlots: cfg.ModelSlots, Weight: cfg.Weight, Accounts: cfg.Accounts,
	}); err != nil {
		return HarnessOutcome{}, fmt.Errorf("register: %w", err)
	}

	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return HarnessOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return HarnessOutcome{Got: false}, nil
	}
	out := HarnessOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}

	if _, st, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		return out, fmt.Errorf("heartbeat: %w", err)
	} else if st != 200 {
		return out, fmt.Errorf("heartbeat status %d", st)
	}

	// ── credential-less provisioning: fetch + clone a read-only bundle (§7.4 (a)) ──
	if grant.Provisioning != "bundle" {
		return out, fmt.Errorf("bundle harness requires bundle provisioning; got %q", grant.Provisioning)
	}
	// The worker holds NO GitHub credential and NO mirror path: it cannot push and
	// cannot reach GitHub. It pulls repo bytes over the authenticated worker channel.
	bundle, err := c.Bundle(ctx, grant.JobID)
	if err != nil {
		return out, fmt.Errorf("fetch bundle: %w", err)
	}
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot, err = os.MkdirTemp("", "flowbee-bw-")
		if err != nil {
			return out, fmt.Errorf("mkdir workroot: %w", err)
		}
		defer os.RemoveAll(workRoot)
	}
	wsDir := filepath.Join(workRoot, "ws-"+grant.JobID)
	ws, err := gitops.CloneFromBundle(wsDir, bundle, grant.BaseSHA)
	if err != nil {
		return out, fmt.Errorf("clone from bundle: %w", err)
	}
	defer ws.Destroy()

	// materialize the F1 task context into the workspace (same as the worktree path).
	taskEnv, err := writeTaskContext(wsDir, grant)
	if err != nil {
		return out, fmt.Errorf("write task context: %w", err)
	}

	agentEnv := append(os.Environ(),
		"FLOWBEE_JOB_ID="+grant.JobID,
		"FLOWBEE_BASE_SHA="+grant.BaseSHA,
		"FLOWBEE_ROLE="+grant.Role,
	)
	agentEnv = append(agentEnv, taskEnv...)
	if err := runAgentHeartbeat(ctx, c, grant.JobID, grant.LeaseEpoch, grant.LeaseTTLS, wsDir, cfg.AgentCmd, agentEnv); err != nil {
		return out, err
	}

	// capture the node's own detailed commit message before stripping the scaffolding;
	// the control plane commits the patch WITH it (the worker is credential-less).
	commitMsg := nodeCommitMessage(wsDir, cfg.Identity, grant.Role, grant.JobID, grant.Context)
	// strip the Flowbee scaffolding before computing the untrusted diff (§9.2).
	_ = os.RemoveAll(filepath.Join(wsDir, ".flowbee"))

	changed, err := ws.HasChanges()
	if err != nil {
		return out, fmt.Errorf("inspect workspace: %w", err)
	}
	if !changed {
		_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
		return out, fmt.Errorf("agent produced no changes")
	}
	diff, err := ws.Diff()
	if err != nil {
		return out, fmt.Errorf("diff: %w", err)
	}

	// ── submit ONLY the patch: NO pushed_ref (the worker pushed nothing). The control
	// plane applies the patch + pushes the epoch ref + opens the PR (F3/R4/§8). ──
	idem := fmt.Sprintf("%s-e%d", grant.JobID, grant.LeaseEpoch)
	body := map[string]any{
		"kind":     "patch",
		"base_sha": grant.BaseSHA,
		// NO pushed_ref: the worker is credential-less and pushed nothing.
		"diff":           diff,
		"commit_message": commitMsg,
		"blast_radius": map[string]any{
			"scope": "bundle",
			"paths": content.TouchedPaths(diff),
		},
		"status": "succeeded", // a HINT only — never the verdict (I-9)
	}
	res, st, err := c.Result(ctx, grant.JobID, grant.LeaseEpoch, idem, body)
	if err != nil {
		return out, fmt.Errorf("result: %w", err)
	}
	if st != 200 {
		return out, fmt.Errorf("result status %d", st)
	}
	out.JobState = res.JobState

	if _, err := c.Release(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		_ = err
	}
	return out, nil
}

// RunOnceHarnessRemote performs ONE build cycle for a REMOTE worker that keeps its
// OWN local mirror (Sam's multi-box model): clone-if-absent + fetch the integration
// branch to stay current, provision a per-job worktree off the shared local mirror
// (so MANY workers on one box build in parallel, each in its own tree), run the
// agent, and return ONLY a diff. The control plane — which alone holds write
// credentials — applies the patch + pushes + opens the PR (R4). The worker needs
// only repo READ (clone/pull); it never pushes to GitHub. Got=false on a 204.
func RunOnceHarnessRemote(ctx context.Context, cfg HarnessConfig) (HarnessOutcome, error) {
	arch, osName := cfg.Arch, cfg.OS
	if arch == "" {
		arch = runtime.GOARCH
	}
	if osName == "" {
		osName = runtime.GOOS
	}
	role := cfg.Role
	if role == "" {
		role = "eng_worker"
	}
	caps := cfg.Capabilities
	if len(caps) == 0 {
		caps = []string{"role:" + role, "model_family:" + cfg.ModelFamily, "arch:" + arch, "os:" + osName}
	}

	c := client.NewWithToken(cfg.BaseURL, cfg.BearerToken)
	if _, err := c.Register(ctx, client.Registration{
		Identity: cfg.Identity, Host: hostname(), Capabilities: caps, Arch: arch, OS: osName,
		ModelSlots: cfg.ModelSlots, Weight: cfg.Weight, Accounts: cfg.Accounts,
	}); err != nil {
		return HarnessOutcome{}, fmt.Errorf("register: %w", err)
	}
	grant, ok, err := c.Lease(ctx, cfg.Identity, cfg.ModelFamily, cfg.Role)
	if err != nil {
		return HarnessOutcome{}, fmt.Errorf("lease: %w", err)
	}
	if !ok {
		return HarnessOutcome{Got: false}, nil
	}
	out := HarnessOutcome{Got: true, JobID: grant.JobID, LeaseEpoch: grant.LeaseEpoch}
	if _, st, err := c.Heartbeat(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		return out, fmt.Errorf("heartbeat: %w", err)
	} else if st != 200 {
		return out, fmt.Errorf("heartbeat status %d", st)
	}

	// F9 multi-repo: a fungible worker leases jobs across repos, so it pushes to the
	// repo the LEASE named (not a single configured one), and keeps a SEPARATE local
	// mirror per repo. Fall back to the configured --repo-url / --mirror in single-repo
	// deployments (the lease ships no repo URL).
	repoURL := cfg.RepoURL
	if grant.Context != nil && grant.Context.RepoURL != "" {
		repoURL = grant.Context.RepoURL
	}
	mirrorPath := workerMirrorFor(cfg.MirrorPath, repoURL)
	branch := cfg.Branch
	if branch == "" {
		branch = "main"
	}
	if err := ensureMirror(ctx, mirrorPath, repoURL, branch); err != nil {
		return out, fmt.Errorf("ensure mirror: %w", err)
	}
	mirror := gitops.Open(mirrorPath)

	// per-job worktree off the shared local mirror — parallel-safe across workers.
	workRoot := cfg.WorkRoot
	if workRoot == "" {
		workRoot, err = os.MkdirTemp("", "flowbee-rw-")
		if err != nil {
			return out, fmt.Errorf("mkdir workroot: %w", err)
		}
		defer os.RemoveAll(workRoot)
	}
	// worker-push start point: when the issue branch already has commits (a revise, or
	// a build after an earlier node), START from its tip so this node's commit STACKS
	// on the prior nodes' — the branch accumulates the node-by-node history. The first
	// build (branch absent) starts from base. Falls back to base on any fetch trouble.
	issueBranch := ""
	if grant.Context != nil {
		issueBranch = grant.Context.IssueBranch
	}
	startRef := grant.BaseSHA
	isConflict := grant.Context != nil && grant.Context.Conflict
	// A conflict_resolver starts from CURRENT MAIN (BaseSHA, with the sibling's merged
	// change), NOT the issue-branch tip — it re-applies this job's change on top of main
	// below. Every other role stacks on the issue branch so node commits accumulate.
	if issueBranch != "" && repoURL != "" && !isConflict {
		if tip, exists, terr := mirror.RemoteBranchTip(repoURL, issueBranch); terr == nil && exists && tip != "" {
			if ferr := mirror.FetchRef(repoURL, "refs/heads/"+issueBranch, "refs/flowbee/issue-tip/"+grant.JobID); ferr == nil {
				startRef = tip
			}
		}
	}
	wsDir := gitops.WorktreeBase(workRoot, grant.JobID, grant.LeaseEpoch)
	wt, err := mirror.AddWorktree(wsDir, startRef)
	if err != nil {
		return out, fmt.Errorf("provision worktree at %s: %w", startRef, err)
	}
	defer wt.Destroy()

	// Git-native conflict resolution: the worktree is at current main (with the sibling's
	// change); apply THIS job's ORIGINAL change with a 3-way merge so overlapping edits
	// surface as real <<<<<<< conflict markers the agent resolves MECHANICALLY (keep both
	// sides' intent) — far more reliable than asking it to re-derive its change from a
	// description (which it judges redundant). A clean apply (no overlap) just lands the
	// change; the non-zero exit on conflict is EXPECTED (markers are left in the files).
	if isConflict && strings.TrimSpace(grant.Context.Diff) != "" {
		patchFile := filepath.Join(workRoot, "conflict-"+grant.JobID+".patch")
		if werr := os.WriteFile(patchFile, []byte(grant.Context.Diff), 0o644); werr == nil {
			_, _ = wt.Run("apply", "--3way", "--whitespace=nowarn", patchFile)
		}
	}

	taskEnv, err := writeTaskContext(wsDir, grant)
	if err != nil {
		return out, fmt.Errorf("write task context: %w", err)
	}
	// run the agent WHILE heartbeating the lease (a real build takes minutes — longer
	// than the lease TTL — so without this the lease is revoked mid-build and the
	// pushed result is fenced 409, exactly the #2217 churn: the branch lands but the
	// result is rejected). Mirrors the worktree + bundle harnesses.
	agentEnv := append(os.Environ(),
		"FLOWBEE_JOB_ID="+grant.JobID, "FLOWBEE_BASE_SHA="+grant.BaseSHA, "FLOWBEE_ROLE="+grant.Role)
	agentEnv = append(agentEnv, taskEnv...)
	if err := runAgentHeartbeat(ctx, c, grant.JobID, grant.LeaseEpoch, grant.LeaseTTLS, wsDir, cfg.AgentCmd, agentEnv); err != nil {
		return out, err
	}
	// Normalize a self-committing agent (codex may `git commit` on its own): soft-reset
	// any agent commits back to the worktree's cut point (startRef) so the changes are
	// pending again — otherwise the clean tree reads as "no changes" and CommitAuthored
	// fails on the empty tree. No-op for a non-committing agent. See Worktree.SoftResetTo.
	if err := wt.SoftResetTo(startRef); err != nil {
		return out, fmt.Errorf("normalize worktree: %w", err)
	}
	commitMsg := nodeCommitMessage(wsDir, cfg.Identity, grant.Role, grant.JobID, grant.Context)
	_ = os.RemoveAll(filepath.Join(wsDir, ".flowbee"))
	changed, err := wt.HasChanges()
	if err != nil {
		return out, fmt.Errorf("inspect worktree: %w", err)
	}
	if !changed {
		// a re-build of an ALREADY-BUILT issue branch (e.g. after a requeue): the agent
		// added nothing new, but the branch already carries a build — we started from its
		// tip (startRef != base), and it's already pushed. Submit that EXISTING build for
		// review instead of churning to needs_human. No new commit/push; point the result
		// at the branch tip so the control plane opens the PR.
		if issueBranch != "" && repoURL != "" && startRef != grant.BaseSHA {
			diff, _ := wt.DiffAgainst(grant.BaseSHA)
			idem := fmt.Sprintf("%s-e%d", grant.JobID, grant.LeaseEpoch)
			body := map[string]any{
				"kind": "patch", "base_sha": grant.BaseSHA, "diff": diff,
				"commit_message": "existing build re-submitted for review (re-build produced no new changes)",
				"blast_radius":   map[string]any{"scope": "worktree", "paths": content.TouchedPaths(diff)},
				"status":         "succeeded",
				"pushed_branch":  issueBranch,
				"head_sha":       startRef,
			}
			if res, st, rerr := c.Result(ctx, grant.JobID, grant.LeaseEpoch, idem, body); rerr == nil && st == 200 {
				out.JobState = res.JobState
				out.PushedSHA = startRef
				_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
				return out, nil
			}
		}
		_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
		return out, fmt.Errorf("agent produced no changes")
	}
	// the FULL change vs the integration base (main), even when we stacked on the
	// issue-branch tip — the content gate + reviewer judge the whole PR change.
	diff, err := wt.DiffAgainst(grant.BaseSHA)
	if err != nil {
		return out, fmt.Errorf("diff: %w", err)
	}

	idem := fmt.Sprintf("%s-e%d", grant.JobID, grant.LeaseEpoch)
	body := map[string]any{
		"kind": "patch", "base_sha": grant.BaseSHA, "diff": diff,
		"commit_message": commitMsg,
		"blast_radius":   map[string]any{"scope": "worktree", "paths": content.TouchedPaths(diff)},
		"status":         "succeeded",
	}
	// ── worker-push: this node commits its own work and pushes the issue branch to
	// GitHub with its key (build-list: "each node commits"). The control plane then
	// only opens the PR (sole GitHub-API caller + merger). When no branch/remote is
	// configured, fall back to returning ONLY the diff (the control plane applies it).
	if issueBranch != "" && repoURL != "" {
		sha, cerr := wt.CommitAuthored(cfg.Identity, commitMsg, false)
		if cerr != nil {
			return out, fmt.Errorf("commit: %w", cerr)
		}
		// A conflict_resolver FORCE-pushes: its worktree started at current main and
		// re-applied this job's change (resolving the markers), so the resolved commit is
		// based on main, NOT a descendant of the issue branch's pre-conflict commits — a
		// plain push is non-fast-forward and rejected. The resolution deliberately rebases
		// the issue branch onto main, so replacing it is correct. Every other role
		// fast-forwards (stacks on the branch tip).
		if perr := wt.PushTo(repoURL, issueBranch, isConflict); perr != nil {
			// the branch moved under us (e.g. a rebase-before-review force-update): the
			// build SUCCEEDED, we just lost the fast-forward race. Re-arm WITHOUT burning
			// an attempt (re-validation churn must not exhaust the failure budget and
			// escalate a good change to needs_human) — the next attempt restarts from the
			// new tip.
			_, _ = c.ReleaseNoPenalty(ctx, grant.JobID, grant.LeaseEpoch)
			return out, fmt.Errorf("push %s: %w", issueBranch, perr)
		}
		body["pushed_branch"] = issueBranch
		body["head_sha"] = sha
		out.PushedSHA = sha
	}
	res, st, err := c.Result(ctx, grant.JobID, grant.LeaseEpoch, idem, body)
	if err != nil {
		return out, fmt.Errorf("result: %w", err)
	}
	if st != 200 {
		return out, fmt.Errorf("result status %d", st)
	}
	out.JobState = res.JobState
	if _, err := c.Release(ctx, grant.JobID, grant.LeaseEpoch); err != nil {
		_ = err
	}
	return out, nil
}

// ensureMirror makes the worker's local bare mirror present + current: clone --bare
// from repoURL if absent, else fetch the integration branch. Read-only — the worker
// never needs write creds (the control plane does the GitHub writes).
func ensureMirror(ctx context.Context, mirrorPath, repoURL, branch string) error {
	if _, err := os.Stat(mirrorPath); err != nil {
		if repoURL == "" {
			return fmt.Errorf("no local mirror at %s and no --repo-url to clone from", mirrorPath)
		}
		if out, cerr := exec.CommandContext(ctx, "git", "clone", "--bare", "--quiet", repoURL, mirrorPath).CombinedOutput(); cerr != nil {
			return fmt.Errorf("clone %s: %v: %s", repoURL, cerr, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// the path exists: it MUST be a bare repo. A working-tree checkout here (e.g.
	// --mirror pointed at ~/dev/<repo>) makes `git --git-dir <path>` fail cryptically
	// at worktree-add time; catch it now with an actionable error.
	if out, berr := exec.CommandContext(ctx, "git", "--git-dir", mirrorPath, "rev-parse", "--is-bare-repository").CombinedOutput(); berr != nil || strings.TrimSpace(string(out)) != "true" {
		return fmt.Errorf("mirror path %s exists but is not a bare repository (is-bare=%q) — point --mirror at a mirrors DIRECTORY (e.g. ~/.flowbee/mirrors) so Flowbee manages per-repo bare mirrors, NOT a working checkout like ~/dev/<repo>",
			mirrorPath, strings.TrimSpace(string(out)))
	}
	// fetch latest — best-effort: with several parallel builders sharing one mirror a
	// concurrent fetch can briefly lose the git lock; another builder's fetch wins, and
	// if base_sha is genuinely absent the AddWorktree below fails clearly + the worker
	// retries. So a fetch error never fails the job.
	_ = gitops.Open(mirrorPath).FetchBranch(branch)
	return nil
}

// runAgentHeartbeat spawns the agent CLI in dir and, WHILE it runs, keeps the lease
// alive with periodic heartbeats. A real build/review can take longer than the lease
// TTL; without this its lease would expire mid-run and the result be fenced as a
// stale epoch (wasting the work + re-arming the job). Heartbeats fire at ~1/3 of the
// TTL (min 20s). errb captures stderr for a useful failure message.
func runAgentHeartbeat(ctx context.Context, c *client.Client, jobID string, epoch, ttlS int, dir, agentCmd string, env []string) error {
	_, err := runAgentHeartbeatIO(ctx, c, jobID, epoch, ttlS, dir, agentCmd, env, false)
	return err
}

// runAgentHeartbeatIO is runAgentHeartbeat with an optional stdout capture. The
// review/author harness sets capture=true so it can fall back to the agent's stdout
// when the agent emitted its result (e.g. a spec) to stdout instead of writing the
// expected $FLOWBEE_SPEC_FILE / $FLOWBEE_VERDICT_FILE. Build roles leave it false
// (their agents write files, and their stdout can be large — don't buffer it).
// agent output caps (untrusted black-box stdout/stderr). 16 MiB of stdout is far past any
// legitimate agent result/diff text; 256 KiB of stderr is ample for an error tail.
const (
	maxAgentStdoutBytes = 16 << 20
	maxAgentStderrBytes = 256 << 10
)

// boundedWriter accumulates up to max bytes then DISCARDS the rest while still reporting
// full consumption, so the child process's stdout/stderr pipe never blocks (back-pressure
// from a stalled reader would hang the agent). Bounds the worker's memory against a
// runaway agent and bounds the artifact a review role submits from stdout.
type boundedWriter struct {
	b   strings.Builder
	max int
}

func newBoundedWriter(max int) *boundedWriter { return &boundedWriter{max: max} }

func (w *boundedWriter) Write(p []byte) (int, error) {
	if rem := w.max - w.b.Len(); rem > 0 {
		if len(p) <= rem {
			w.b.Write(p)
		} else {
			w.b.Write(p[:rem])
		}
	}
	return len(p), nil // claim full consumption so os/exec keeps draining the pipe
}

func (w *boundedWriter) String() string { return w.b.String() }

func runAgentHeartbeatIO(ctx context.Context, c *client.Client, jobID string, epoch, ttlS int, dir, agentCmd string, env []string, capture bool) (string, error) {
	if agentCmd == "" {
		return "", nil
	}
	// Bound the agent by the lease's absolute cap (+margin). The CP revokes a lease at
	// lease_ttl_s EVEN while heartbeating (the un-gameable cap), so an agent that outruns
	// it is a zombie working a job the CP already reassigned. A hung/slow agent must NEVER
	// block the worker loop forever — cmd.Wait would never return, silently removing the
	// worker from the fleet. Kill it when the deadline passes OR when the CP revokes.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(ttlS+30)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", agentCmd)
	cmd.Dir = dir
	cmd.Env = env
	// Run the agent in its OWN process group, and on cancel/timeout kill the WHOLE
	// group — not just `sh`. A real agent forks children (the model CLI, git, …) that
	// INHERIT the stdout/stderr pipe; killing only the direct child leaves those orphans
	// holding the pipe open, so cmd.Wait() blocks until THEY exit (up to the full agent
	// run) — re-wedging the worker the timeout was meant to free. Setpgid + a group-kill
	// Cancel closes the pipe at once; WaitDelay force-closes it as a backstop. (This is
	// also what made the unit test pass locally but hang in CI: a shell that exec-optimized
	// `sh -c "cmd"` into one process vs one that forked a pipe-holding child.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // -pid = the whole group
		}
		return nil
	}
	cmd.WaitDelay = 10 * time.Second
	// BOUNDED capture: the agent CLI is an untrusted black box, so its stdout/stderr must
	// NOT buffer without limit — a chatty or runaway agent (or one dumping a file to
	// stdout) would OOM the worker, and for review/author roles the whole stdout is
	// submitted as the artifact when the verdict/spec file is absent. Cap both; the writer
	// keeps consuming past the cap so the child's pipe never blocks.
	errb, outb := newBoundedWriter(maxAgentStderrBytes), newBoundedWriter(maxAgentStdoutBytes)
	cmd.Stderr = errb
	cmd.Stdout = outb // capture: needed to parse the agent's reported cost/usage
	_ = capture       // (capture is now always on; the param is kept for call-site clarity)
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("agent start: %w", err)
	}
	// heartbeat well under the soft rungs (phase budget ~TTL/2, stale threshold
	// ~3×heartbeat) so the lease never lapses mid-build — cap at 60s so a long TTL
	// doesn't stretch the interval past those thresholds (#40).
	interval := ttlS / 3
	if interval > 60 {
		interval = 60
	}
	if interval < 20 {
		interval = 20
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Duration(interval) * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				// act on the CP's verdict: a `cancel` directive (over-budget / two-rung kill)
				// or a stale-epoch 409 means the lease is GONE — stop the now-orphaned agent
				// at once and free the worker, instead of finishing reassigned work.
				if hbDir, st, _ := c.Heartbeat(runCtx, jobID, epoch); hbDir == "cancel" || st == 409 {
					cancel()
					return
				}
			}
		}
	}()
	werr := cmd.Wait()
	close(done)
	out := outb.String()
	// report the agent's metered cost (claude --output-format json prints total_cost_usd
	// + usage) in a final heartbeat: the server folds the delta into the per-job meter,
	// and a delta that crosses the ceiling escalates the job to needs_human (I-15). A
	// text-output agent parses to nothing -> no report (backward compatible).
	if ti, to, micro, ok := parseAgentUsage(out); ok && (ti != 0 || to != 0 || micro != 0) {
		_, _, _ = c.HeartbeatWith(ctx, jobID, epoch, client.HeartbeatObs{
			TokensInDelta: ti, TokensOutDelta: to, MicroUSDDelta: micro,
		})
	}
	if werr != nil {
		// distinguish a deadline/revoke kill (the lease is gone — the caller releases and
		// loops to fresh work) from a genuine agent failure, so the log isn't misleading.
		if runCtx.Err() != nil {
			return out, fmt.Errorf("agent aborted (lease revoked or exceeded lease_ttl): %w", runCtx.Err())
		}
		return out, fmt.Errorf("agent cmd: %v: %s", werr, strings.TrimSpace(errb.String()))
	}
	return out, nil
}

// parseAgentUsage extracts the agent's reported token/cost usage from its stdout when it
// ran with `claude --output-format json` (a single result object carrying total_cost_usd
// + usage). $ is taken directly from the agent (no price table); text output (no JSON)
// returns ok=false so cost reporting is opt-in by output format.
func parseAgentUsage(stdout string) (tokensIn, tokensOut, microUSD int64, ok bool) {
	s := strings.TrimSpace(stdout)
	if !strings.HasPrefix(s, "{") {
		return 0, 0, 0, false
	}
	var r struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
		Usage        struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(s), &r) != nil {
		return 0, 0, 0, false
	}
	return r.Usage.InputTokens, r.Usage.OutputTokens, int64(r.TotalCostUSD * 1_000_000), true
}

// agentResultText returns the agent's response text: the `.result` of a claude JSON
// output, else the raw stdout — so the spec_author stdout fallback works whether the
// agent ran in JSON (for cost capture) or plain text mode.
func agentResultText(stdout string) string {
	s := strings.TrimSpace(stdout)
	if strings.HasPrefix(s, "{") {
		var r struct {
			Result string `json:"result"`
		}
		if json.Unmarshal([]byte(s), &r) == nil && r.Result != "" {
			return r.Result
		}
	}
	return stdout
}

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}
