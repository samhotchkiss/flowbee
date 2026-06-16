package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/gitops"
)

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
}

// HarnessOutcome reports what one lease cycle did.
type HarnessOutcome struct {
	Got        bool
	JobID      string
	LeaseEpoch int
	JobState   string
	PushedRef  string
	PushedSHA  string
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

	// ── spawn the agent CLI in the worktree (a black box, §7.1) ──
	if cfg.AgentCmd != "" {
		cmd := exec.CommandContext(ctx, "sh", "-c", cfg.AgentCmd)
		cmd.Dir = wsDir
		cmd.Env = append(os.Environ(),
			"FLOWBEE_JOB_ID="+grant.JobID,
			"FLOWBEE_BASE_SHA="+grant.BaseSHA,
			"FLOWBEE_ROLE="+grant.Role,
		)
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Run(); err != nil {
			return out, fmt.Errorf("agent cmd: %v: %s", err, strings.TrimSpace(errb.String()))
		}
	}

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

func hostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}
