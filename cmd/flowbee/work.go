package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// runWork runs the Mode-A harness (DESIGN §7.1): the thin pull loop around an
// agent CLI. --stub runs the built-in echo stub (no worktree). Otherwise it runs
// the real harness: provision a per-lease worktree at base_sha off the shared
// mirror, spawn FLOWBEE_AGENT_CMD in it, push to the epoch ref, submit the patch.
// --once runs a single lease cycle (used by tests); default loops forever.
//
// F1 default agent-cmd convention: before spawning, the harness writes the lease's
// resolved task into .flowbee/task.md at the worktree root and exports the env
// FLOWBEE_TASK_FILE (absolute path to it), FLOWBEE_TASK, FLOWBEE_SPEC,
// FLOWBEE_ACCEPTANCE, FLOWBEE_IDENTITY, FLOWBEE_LENS (plus FLOWBEE_JOB_ID/
// FLOWBEE_BASE_SHA/FLOWBEE_ROLE). The agent-cmd should read $FLOWBEE_TASK_FILE
// (or .flowbee/task.md) and act on it — e.g. `claude -p "$(cat $FLOWBEE_TASK_FILE)"`
// or `codex exec "$FLOWBEE_TASK"`. The .flowbee/ scaffolding is stripped before the
// work-product diff is collected, so it never enters the untrusted patch.
func runWork(args []string) error {
	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	stub := fs.Bool("stub", false, "run the built-in echo stub worker (no worktree)")
	once := fs.Bool("once", false, "run a single lease cycle, then exit")
	// credential-less BUNDLE mode (F3): a REMOTE worker holds no GitHub creds and no
	// mirror — it fetches a read-only bundle of base_sha over the worker channel,
	// returns ONLY a diff, and the control plane does all git writes (push/PR/merge).
	// Set on workers across the network (the serve must run FLOWBEE_BUNDLE_PROVISIONING).
	bundle := fs.Bool("bundle", os.Getenv("FLOWBEE_BUNDLE") != "", "credential-less bundle mode for remote workers (control plane does git writes)")
	identity := fs.String("identity", envOr("FLOWBEE_IDENTITY", "worker"), "worker identity")
	family := fs.String("model-family", envOr("FLOWBEE_MODEL_TAG", "stub"), "model family tag")
	role := fs.String("role", envOr("FLOWBEE_ROLE", ""), "role filter")
	agentCmd := fs.String("agent-cmd", envOr("FLOWBEE_AGENT_CMD", ""), "agent CLI to spawn per lease (reads $FLOWBEE_TASK_FILE / .flowbee/task.md)")
	// F6 capacity advertisement (optional). --model-slots "claude:3,codex:3" is the
	// box's PER-MODEL concurrency; --weight is the per-box distribution bias;
	// --accounts "claude-primary:claude:90:0,claude-fallback:claude:90:1" is the
	// named per-model rollover chain (account:model:ceiling_pct:rank).
	modelSlots := fs.String("model-slots", envOr("FLOWBEE_MODEL_SLOTS", ""), "per-model concurrency, e.g. claude:3,codex:3")
	weight := fs.Int("weight", 0, "per-box distribution weight (default 1)")
	accounts := fs.String("accounts", envOr("FLOWBEE_ACCOUNTS", ""), "named accounts: account:model:ceiling_pct:rank,...")
	// --remote: a build box that keeps its OWN local mirror + worktrees per job (many
	// workers in parallel) and returns a diff for the control plane to push/PR/merge.
	// Needs only repo READ; --mirror is its local bare mirror, --repo-url where to
	// clone/pull it from (defaults from FLOWBEE_GITHUB_OWNER/REPO).
	remote := fs.Bool("remote", os.Getenv("FLOWBEE_REMOTE") != "", "remote build box: own local mirror + per-job worktrees, diff back to the control plane")
	mirror := fs.String("mirror", envOr("FLOWBEE_MIRROR_PATH", ""), "local bare mirror path (--remote)")
	repoURL := fs.String("repo-url", envOr("FLOWBEE_REPO_URL", ""), "git URL to clone/pull the local mirror from (--remote)")
	branch := fs.String("branch", envOr("FLOWBEE_GITHUB_DEFAULT_BRANCH", "main"), "integration branch to track (--remote)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repoURL == "" {
		if o, r := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"); o != "" && r != "" {
			*repoURL = "https://github.com/" + o + "/" + r + ".git"
		}
	}
	// the worker keeps its OWN local copy of the repo (Sam's model): default it to
	// ~/dev/<repo> — matching how repos are laid out — overridable via --mirror /
	// FLOWBEE_MIRROR_PATH. The worker fetches each job here and works in per-job
	// worktrees off it, and (worker-push) commits + pushes the issue branch from it.
	if *mirror == "" {
		*mirror = defaultWorkerMirror(*repoURL)
	}
	slots := parseModelSlots(*modelSlots)
	accts := parseAccounts(*accounts)
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	ctx := context.Background()

	if *stub {
		out, err := worker.RunOnce(ctx, worker.StubConfig{
			BaseURL: url, Identity: *identity, ModelFamily: *family,
		})
		if err != nil {
			return err
		}
		if !out.Got {
			fmt.Println("no work available (204)")
			return nil
		}
		fmt.Printf("stub completed job %s -> %s (epoch %d)\n", out.JobID, out.JobState, out.LeaseEpoch)
		return nil
	}

	// Fail LOUD on a missing agent (mirrors the fleet smoke-test philosophy). With an
	// empty agent command the harness runs the "agent" as a no-op that produces no
	// output, so this worker would CLAIM jobs and complete them with empty results —
	// silently bouncing every job it touches toward needs_human. A misconfigured worker
	// must refuse to start, not quietly strand the queue.
	if *agentCmd == "" {
		return fmt.Errorf("no agent configured — set --agent-cmd or FLOWBEE_AGENT_CMD " +
			"(the CLI to run per job, e.g. 'claude --model sonnet -p \"$(cat \"$FLOWBEE_TASK_FILE\")\"'), " +
			"or pass --stub for the built-in no-op echo worker. Without one, this worker would claim jobs " +
			"and bounce them with empty results.\n" +
			"   For production, prefer `flowbee fleet` or `flowbee up` — they wire a real per-role agent " +
			"(with model diversity) automatically and smoke-test it before starting.")
	}

	token := os.Getenv("FLOWBEE_WORKER_TOKEN")
	cfg := worker.HarnessConfig{
		BaseURL: url, Identity: *identity, ModelFamily: *family, Role: *role,
		AgentCmd: *agentCmd, BearerToken: token,
		ModelSlots: slots, Weight: *weight, Accounts: accts,
		MirrorPath: *mirror, RepoURL: *repoURL, Branch: *branch,
	}
	run := func() error {
		// review/author roles emit a DECISION (verdict file) — not a patch — so they
		// run the verdict-file harness, which posts /spec, /spec-review, or /review.
		// eng_worker (and conflict_resolver) run the worktree build harness.
		var out worker.HarnessOutcome
		var err error
		switch {
		case worker.IsReviewRole(*role):
			// review/author roles judge from context (diff/spec) + emit a verdict —
			// no git, no creds, so they run identically local or remote.
			out, err = worker.RunOnceReviewHarness(ctx, cfg)
		case *remote:
			// remote build box: own local mirror + per-job worktree, diff back.
			out, err = worker.RunOnceHarnessRemote(ctx, cfg)
		case *bundle:
			// zero-trust remote: credential-less fetched bundle, diff back.
			out, err = worker.RunOnceHarnessBundle(ctx, cfg)
		default:
			// same-box eng_worker: worktree off the control plane's shared mirror.
			out, err = worker.RunOnceHarness(ctx, cfg)
		}
		if err != nil {
			return err
		}
		if !out.Got {
			fmt.Println("no work available (204)")
			return nil
		}
		if out.Skipped {
			// a code_review job whose CI isn't green yet. CI runs for MINUTES, so a tight
			// re-poll just churns the lease epoch (each claim bumps it) + bloats the
			// ledger for no benefit — back off generously. Tunable via
			// FLOWBEE_CI_WAIT_BACKOFF_S (default 30s).
			fmt.Printf("skipped job %s (waiting for CI) — backing off\n", out.JobID)
			time.Sleep(ciWaitBackoff())
			return nil
		}
		fmt.Printf("completed job %s -> %s (epoch %d) pushed %s @ %s\n",
			out.JobID, out.JobState, out.LeaseEpoch, out.PushedRef, out.PushedSHA)
		return nil
	}

	if *once {
		return run()
	}
	for {
		if err := run(); err != nil {
			fmt.Fprintf(envErr(), "work cycle: %v\n", err)
			// back off after a FAILED attempt (a no-output build, a provision error, a
			// transient API error) so the worker does not immediately re-poll and
			// re-claim the SAME job — the ready↔leased churn that floods the ledger and
			// burns the job's attempt budget in a tight spin. A short pause lets another
			// worker pick it up or conditions settle; healthy throughput is unaffected
			// (a successful run returns nil and loops straight to the next long-poll).
			time.Sleep(errBackoff())
		}
	}
}

// errBackoff is the pause after a failed work attempt before re-polling (default 5s),
// so a worker that just errored on a job does not tight-spin re-claiming it. Tunable
// via FLOWBEE_ERR_BACKOFF_S.
func errBackoff() time.Duration {
	if v := os.Getenv("FLOWBEE_ERR_BACKOFF_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 5 * time.Second
}

func envErr() *os.File { return os.Stderr }

// ciWaitBackoff is how long a reviewer waits before re-polling a job whose CI is
// still running (default 30s). CI takes minutes, so a short interval only churns the
// lease epoch; 30s keeps reaction prompt without hammering. Tunable for tests/tuning.
func ciWaitBackoff() time.Duration {
	if v := os.Getenv("FLOWBEE_CI_WAIT_BACKOFF_S"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 30 * time.Second
}

// parseModelSlots parses the F6 per-model concurrency advertisement
// "claude:3,codex:3" into a map. Malformed entries are skipped.
func parseModelSlots(s string) map[string]int {
	if s == "" {
		return nil
	}
	out := map[string]int{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, ":", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(kv[1]))
		if err != nil {
			continue
		}
		out[strings.TrimSpace(kv[0])] = n
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseAccounts parses the F6 rollover chain "account:model:ceiling_pct:rank,..."
// into client account specs. ceiling_pct/rank default to 90/0 when omitted.
func parseAccounts(s string) []client.AccountSpecMsg {
	if s == "" {
		return nil
	}
	var out []client.AccountSpecMsg
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Split(part, ":")
		if len(f) < 2 || f[0] == "" || f[1] == "" {
			continue
		}
		a := client.AccountSpecMsg{AccountID: f[0], ModelFamily: f[1], CeilingPct: 90}
		if len(f) >= 3 {
			if n, err := strconv.Atoi(f[2]); err == nil {
				a.CeilingPct = n
			}
		}
		if len(f) >= 4 {
			if n, err := strconv.Atoi(f[3]); err == nil {
				a.PreferenceRank = n
			}
		}
		out = append(out, a)
	}
	return out
}

// runLease is the Mode-B thin client: one GET /v1/lease, print JSON.
func runLease(args []string) error {
	fs := flag.NewFlagSet("lease", flag.ContinueOnError)
	identity := fs.String("identity", envOr("FLOWBEE_IDENTITY", "modeb"), "worker identity")
	family := fs.String("model-family", envOr("FLOWBEE_MODEL_TAG", "stub"), "model family tag")
	role := fs.String("role", "", "role filter")
	if err := fs.Parse(args); err != nil {
		return err
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))
	grant, ok, err := c.Lease(context.Background(), *identity, *family, *role)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Println(`{"lease":null}`)
		return nil
	}
	b, _ := json.Marshal(grant)
	fmt.Println(string(b))
	return nil
}

// runRequeue re-arms a stranded job (one that escalated to needs_human from a
// now-fixed transient failure) for a fresh attempt: `flowbee requeue <job-id>`. Run
// it on the control-plane box (loopback) or with FLOWBEE_WORKER_TOKEN set.
func runRequeue(args []string) error {
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	force := fs.Bool("force", false, "requeue even if the job is actively leased (fences the live worker, discarding its in-flight work)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: flowbee requeue [--force] <job-id>")
	}
	jobID := fs.Arg(0)
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))
	st, err := c.Requeue(context.Background(), jobID, *force)
	if err != nil {
		return err
	}
	if st == 409 {
		return fmt.Errorf("job %s is actively leased — a worker is building/reviewing it now; "+
			"re-run with --force to requeue anyway (this discards the live worker's work)", jobID)
	}
	if st != 200 {
		return fmt.Errorf("requeue status %d", st)
	}
	fmt.Printf("requeued %s -> ready (fresh attempt budget)\n", jobID)
	return nil
}

// runSubmit is the Mode-B thin client (DESIGN §7.1, the MCP-shim surface): post a
// result / heartbeat / release for a held lease. For a build job it can also
// PROVISION a worktree off the mirror, spawn an agent CLI, and push to the epoch
// ref before submitting — the same work the Mode-A harness does, driven from the
// CLI so Mode B completes the identical build thread.
func runSubmit(args []string) error {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	jobID := fs.String("job", "", "job id")
	epoch := fs.Int("epoch", 0, "lease epoch (the fence)")
	action := fs.String("action", "result", "result|heartbeat|release")
	idem := fs.String("idempotency-key", "", "idempotency key (result only)")
	// build-result provisioning (Mode-B does the git work itself, no creds):
	mirror := fs.String("mirror", "", "shared bare mirror path (from the lease envelope)")
	baseSHA := fs.String("base-sha", "", "base SHA to provision the worktree at")
	agentCmd := fs.String("agent-cmd", envOr("FLOWBEE_AGENT_CMD", ""), "agent CLI to spawn in the worktree")
	identity := fs.String("identity", envOr("FLOWBEE_IDENTITY", "modeb"), "worker identity (for the commit)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *jobID == "" {
		return fmt.Errorf("--job is required")
	}
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))
	ctx := context.Background()

	switch *action {
	case "heartbeat":
		dir, st, err := c.Heartbeat(ctx, *jobID, *epoch)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d directive=%s\n", st, dir)
	case "release":
		st, err := c.Release(ctx, *jobID, *epoch)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d\n", st)
	case "result":
		body := map[string]any{"kind": "patch", "blast_radius": map[string]any{"scope": "modeb"}}
		// if a mirror was supplied, provision + push a real patch to the epoch ref.
		if *mirror != "" {
			workRoot, err := os.MkdirTemp("", "flowbee-modeb-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(workRoot)
			m := gitops.Open(*mirror)
			ws := gitops.WorktreeBase(workRoot, *jobID, *epoch)
			wt, err := m.AddWorktree(ws, *baseSHA)
			if err != nil {
				return fmt.Errorf("provision worktree: %w", err)
			}
			defer wt.Destroy()
			if *agentCmd != "" {
				if _, err := wt.Run("sh", "-c", *agentCmd); err != nil {
					return fmt.Errorf("agent cmd: %w", err)
				}
			}
			sha, ref, err := wt.CommitAndPushEpoch(*jobID, *epoch,
				fmt.Sprintf("flowbee: %s mode-b build %s@e%d", *identity, *jobID, *epoch))
			if err != nil {
				return fmt.Errorf("push epoch ref: %w", err)
			}
			body["base_sha"] = *baseSHA
			body["pushed_ref"] = ref
			fmt.Printf("pushed %s @ %s\n", ref, sha)
		}
		res, st, err := c.Result(ctx, *jobID, *epoch, *idem, body)
		if err != nil {
			return err
		}
		fmt.Printf("status=%d accepted=%v job_state=%s\n", st, res.Accepted, res.JobState)
	default:
		return fmt.Errorf("unknown action %q", *action)
	}
	return nil
}

// defaultWorkerMirror is the worker's local repo-copy path when --mirror is unset:
// ~/dev/<repo> by default (matching how repos are laid out on the boxes), derived
// from FLOWBEE_GITHUB_REPO or the repo URL's last path segment. Returns "" when the
// repo can't be derived (the harness then falls back to a temp mirror). Overridable
// via --mirror / FLOWBEE_MIRROR_PATH.
func defaultWorkerMirror(repoURL string) string {
	// Leave empty: the harness derives a managed PER-REPO BARE mirror
	// (~/.flowbee/mirrors/<repo>.git) from the lease's repo URL. The old default
	// (~/dev/<repo>) pointed at a working-tree CHECKOUT, which `git --git-dir` cannot
	// use as a mirror — Flowbee keeps its own bare mirrors, separate from your clones.
	_ = repoURL
	return ""
}
