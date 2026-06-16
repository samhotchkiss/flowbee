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
	if c.PriorVerdict != nil {
		if raw, err := json.Marshal(c.PriorVerdict); err == nil {
			b.WriteString("\n## Prior verdict\n\n```json\n")
			b.Write(raw)
			b.WriteString("\n```\n")
		}
	}
	b.WriteString("\n## How to complete this\n\nMake the change by creating or editing files in THIS working directory. " +
		"Write the actual files to disk now — do not just describe or print them. Touch only what the task requires.\n")
	return b.String()
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
	if cfg.AgentCmd != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cfg.AgentCmd)
		cmd.Dir = wsDir
		cmd.Env = append(os.Environ(),
			"FLOWBEE_JOB_ID="+grant.JobID,
			"FLOWBEE_BASE_SHA="+grant.BaseSHA,
			"FLOWBEE_ROLE="+grant.Role,
		)
		cmd.Env = append(cmd.Env, taskEnv...)
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return out, fmt.Errorf("agent cmd: %v: %s", err, strings.TrimSpace(errb.String()))
		}
	}

	// The .flowbee/ scaffolding is Flowbee's INPUT to the agent, not the agent's
	// work-product: strip it before computing the diff so it never enters the
	// untrusted patch the content-integrity gate judges (§9.2).
	_ = os.RemoveAll(filepath.Join(wsDir, ".flowbee"))

	// ── collect the work-product: commit + push to the epoch ref (§7.3) ──
	changed, err := wt.HasChanges()
	if err != nil {
		return out, fmt.Errorf("inspect worktree: %w", err)
	}
	if !changed {
		// nothing produced: abandon the lease (does not burn an attempt as failure).
		_, _ = c.Release(ctx, grant.JobID, grant.LeaseEpoch)
		return out, fmt.Errorf("agent produced no changes")
	}
	// the unified diff against base is the UNTRUSTED work-product the M9
	// content-integrity gate (§9.2, I-11) judges. We compute it BEFORE the commit so
	// the staged tree is captured; an error here is non-fatal (the gate then runs
	// over an empty diff — the safe default is not-eligible only if the worker also
	// asked for self_merge).
	diff, _ := wt.Diff()

	sha, ref, err := wt.CommitAndPushEpoch(grant.JobID, grant.LeaseEpoch,
		fmt.Sprintf("flowbee: %s build %s@e%d", cfg.Identity, grant.JobID, grant.LeaseEpoch))
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

	if cfg.AgentCmd != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cfg.AgentCmd)
		cmd.Dir = wsDir
		cmd.Env = append(os.Environ(),
			"FLOWBEE_JOB_ID="+grant.JobID,
			"FLOWBEE_BASE_SHA="+grant.BaseSHA,
			"FLOWBEE_ROLE="+grant.Role,
		)
		cmd.Env = append(cmd.Env, taskEnv...)
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return out, fmt.Errorf("agent cmd: %v: %s", err, strings.TrimSpace(errb.String()))
		}
	}

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
		"diff": diff,
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

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}
