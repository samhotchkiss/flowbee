package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

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
	if err := fs.Parse(args); err != nil {
		return err
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

	token := os.Getenv("FLOWBEE_WORKER_TOKEN")
	run := func() error {
		out, err := worker.RunOnceHarness(ctx, worker.HarnessConfig{
			BaseURL: url, Identity: *identity, ModelFamily: *family, Role: *role,
			AgentCmd: *agentCmd, BearerToken: token,
			ModelSlots: slots, Weight: *weight, Accounts: accts,
		})
		if err != nil {
			return err
		}
		if !out.Got {
			fmt.Println("no work available (204)")
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
		}
	}
}

func envErr() *os.File { return os.Stderr }

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
