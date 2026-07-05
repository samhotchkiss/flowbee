package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/llm"
)

// runFleet is the "fungible worker box" command: it starts one worker per pipeline
// role on THIS machine, pointed at a remote control plane (--url). Every box runs
// EVERY role, so jobs flow to whichever box has free capacity — the machines are
// interchangeable; we care where work can run, not which job runs where.
//
//	flowbee fleet --url http://<control-plane>:7070
//
// fleetBuilderFamily is the model_family — and the --model alias — the N build workers
// register and run under. Any role that judges or resolves a build carries a
// server-side anti-affinity against it, so such a role must NOT share this family or it
// could never claim. It is a real claude model alias (Sonnet builds; Opus reviews).
const fleetBuilderFamily = "sonnet"

// Per-role default agent commands. The family doubles as the `--model` alias, so build
// and review run genuinely different models (§5.5). %s is the model alias.
const (
	reviewAgentTmpl = `claude -p "$(cat "$FLOWBEE_TASK_FILE")" --model %s --output-format json --dangerously-skip-permissions`
	buildAgentTmpl  = `claude -p "$(cat "$FLOWBEE_TASK_FILE") Create the file(s) on disk now. Make only the change described." --model %s --output-format json --dangerously-skip-permissions`
)

// Codex (OpenAI) agent commands, selected by `--agent codex`. Both build and review run
// the SAME codex model — the per-role difference is the TASK CONTEXT (the harness renders
// a role-specific task into $FLOWBEE_TASK_FILE: a builder is told to write code, a
// reviewer to write a verdict file), not the model (so this forgoes §5.5 cross-MODEL
// diversity to spend Codex quota instead of the Claude weekly limit — the operator's
// explicit choice). Codex still runs under DISTINCT model_family anti-affinity tags so a
// build's own worker can never review/resolve it (§I-10).
//
//	< /dev/null  — `codex exec` reads stdin IN ADDITION to the prompt arg and BLOCKS on an
//	               open stdin; redirecting from /dev/null gives it immediate EOF.
//	--dangerously-bypass-approvals-and-sandbox — non-interactive, may write files + run
//	               commands without prompting (the trusted-tailnet equivalent of claude's
//	               --dangerously-skip-permissions); needed so it can write the work-product
//	               (build) or the absolute $FLOWBEE_VERDICT_FILE / $FLOWBEE_SPEC_FILE (review).
//	--skip-git-repo-check — review roles run in a non-git temp dir; without this codex aborts.
//	The build prompt forbids git: Flowbee owns the commit (§3.5), and a self-committing agent
//	is also normalized harness-side (Worktree.SoftResetTo), but telling codex up front avoids
//	the wasted round-trip.
const (
	codexReviewTmpl = `codex exec "$(cat "$FLOWBEE_TASK_FILE")" --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check < /dev/null`
	codexBuildTmpl  = `codex exec "$(cat "$FLOWBEE_TASK_FILE") Create the file(s) on disk now. Make only the change described. Do NOT run git add, git commit, or any git command — Flowbee records the commit for you." --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check < /dev/null`
)

// roleAgentCmd returns the agent CLI for a role: the operator override if non-empty,
// else the per-role default for the selected agent backend. For claude the family doubles
// as the `--model` alias (genuine cross-model review); for codex both roles share one
// model and differ only by task context. writesFiles picks the file-writing build template
// (eng_worker, conflict_resolver) over the verdict/spec review template.
func roleAgentCmd(agent, family string, writesFiles bool, agentOverride, buildOverride string) string {
	if writesFiles {
		if buildOverride != "" {
			return buildOverride
		}
		if agent == "codex" {
			return codexBuildTmpl
		}
		return fmt.Sprintf(buildAgentTmpl, family)
	}
	if agentOverride != "" {
		return agentOverride
	}
	if agent == "codex" {
		return codexReviewTmpl
	}
	return fmt.Sprintf(reviewAgentTmpl, family)
}

// modelLabelFor is the worker's ACTUAL model label for the §F card: under --agent codex
// every role runs Codex (so the family tag sonnet/opus would mislead), else the family IS
// the real claude model. Sent via --model-label so a card shows which model did each node.
// accountForAgentCmd returns the F6 account a worker running `cmd` authenticates as: the
// box's codex login if the command runs codex, its claude login if it runs claude, else ""
// (that agent isn't metered on this box). This is how a split-stack box attributes its
// codex-build and claude-review workers to their two separate accounts.
func accountForAgentCmd(cmd, codexAcct, claudeAcct string) string {
	switch {
	case strings.Contains(cmd, "codex"):
		return codexAcct
	case strings.Contains(cmd, "claude"):
		return claudeAcct
	}
	return ""
}

func modelLabelFor(agent, family string) string {
	if agent == "codex" {
		return "codex"
	}
	return family
}

// fleetRole is one non-builder worker a fleet box runs (exactly one each).
type fleetRole struct {
	role        string
	family      string // model_family + --model alias; anti-affinity tag (see fleetBuilderFamily)
	writesFiles bool   // runs the file-writing BUILD harness (--remote + build agent) vs the verdict-only review agent
	needsMirror bool   // pushes to the issue branch, so it needs --mirror + --repo-url
}

// nonBuilderFleetRoles is the role roster a fleet box runs beyond its N build workers.
// conflict_resolver is REQUIRED: the server routes a real merge conflict to a
// resolving_conflict job that only a conflict_resolver can claim, so omitting it makes
// every conflict escalate to needs_human instead of resolving autonomously. It writes
// files (resolves markers) so it runs the build harness, under a NON-builder family
// (§I-10: a build's own model may not resolve its own conflict). Each family is a real
// claude --model alias, so Opus reviews/resolves what Sonnet built — genuine diversity.
func nonBuilderFleetRoles() []fleetRole {
	return []fleetRole{
		{role: "conflict_resolver", family: "opus", writesFiles: true, needsMirror: true},
		{role: "code_reviewer", family: "opus", writesFiles: false, needsMirror: true},
		{role: "spec_author", family: "sonnet", writesFiles: false, needsMirror: false},
		{role: "spec_reviewer", family: "opus", writesFiles: false, needsMirror: false},
	}
}

// Build workers run in --remote mode (own local mirror + a worktree per job, so N
// builders on one box run in parallel); review/author workers are git-less. Distinct
// model_family tags keep a reviewer off the builder's family (anti-affinity). Auth,
// if the control plane requires it, comes from FLOWBEE_WORKER_TOKEN in the env.
func runFleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	url := fs.String("url", envOr("FLOWBEE_URL", ""), "control-plane URL, e.g. http://100.67.2.108:7070")
	mirror := fs.String("mirror", envOr("FLOWBEE_WORKER_MIRRORS_DIR", ""), "directory for the worker's per-repo BARE mirrors (default ~/.flowbee/mirrors; NOT a working checkout)")
	builders := fs.Int("builders", 1, "parallel build workers on this box (each gets its own worktree off the shared mirror)")
	// conflict resolution is a slow, file-writing agent task; ONE resolver can't keep up with
	// a burst of conflicts (the waiters get stall-governed out before a resolver frees up).
	// Run several so a backlog of resolving_conflict jobs drains concurrently.
	resolvers := fs.Int("resolvers", 1, "parallel conflict_resolver workers on this box (scale up when conflicts queue faster than one can resolve)")
	// Per-role default agent commands inject `--model <family>` so the build and review
	// roles run GENUINELY DIFFERENT models — real §5.5 uncorrelated review, not the same
	// CLI-default model hiding behind distinct family labels (a reviewer that shares the
	// builder's model shares its blind spots). The claude CLI accepts the 'opus'/'sonnet'
	// aliases (claude --model opus|sonnet). Setting --agent-cmd / --build-agent-cmd (or
	// the env) OVERRIDES with one command for all review-author / all build roles,
	// forgoing per-role model diversity (the operator's explicit choice).
	// --output-format json makes claude print total_cost_usd + usage so the harness can
	// meter per-job cost (I-15); the harness unwraps `.result` for any text it needs.
	agentCmd := fs.String("agent-cmd", os.Getenv("FLOWBEE_AGENT_CMD"), "override the review/author agent CLI (empty = per-role defaults for --agent)")
	buildCmd := fs.String("build-agent-cmd", os.Getenv("FLOWBEE_BUILD_AGENT_CMD"), "override the build agent CLI (empty = per-role defaults for --agent)")
	// --resolver-agent-cmd overrides ONLY the conflict_resolver's agent, independent of
	// --agent / --build-agent-cmd. Conflict resolution is a heavy file-writing task; a
	// builder backend that stalls on it (hitting the lease cap, never resolving) can be
	// swapped for a more reliable model here WITHOUT changing the eng_worker build agent —
	// e.g. keep codex builds but run claude/opus for resolution.
	resolverCmd := fs.String("resolver-agent-cmd", os.Getenv("FLOWBEE_RESOLVER_AGENT_CMD"), "override ONLY the conflict_resolver agent CLI (empty = same default as a build role)")
	// --agent selects the backend CLI for the built-in per-role commands: claude (default,
	// Sonnet builds / Opus reviews — genuine §5.5 cross-model review) or codex (one Codex
	// model for all roles, differing only by task context — spends Codex quota instead of
	// the Claude weekly limit). Explicit --agent-cmd / --build-agent-cmd still override either.
	agent := fs.String("agent", envOr("FLOWBEE_FLEET_AGENT", "claude"), "agent backend for the built-in role commands: claude|codex")
	noSmoke := fs.Bool("no-smoke", false, "skip the agent smoke test at startup")
	systemd := fs.Bool("systemd", false, "print a systemd unit + env file to run the fleet as a managed service (then exit), instead of starting it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return fmt.Errorf("flowbee fleet: --url (or FLOWBEE_URL) is required — the control-plane address")
	}
	if *agent != "claude" && *agent != "codex" {
		return fmt.Errorf("flowbee fleet: --agent must be claude or codex, got %q", *agent)
	}
	if *systemd {
		printFleetSystemd(*url, *builders, *agent, *agentCmd, *buildCmd)
		return nil
	}
	repoURL := os.Getenv("FLOWBEE_REPO_URL")
	if repoURL == "" {
		if o, r := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"); o != "" && r != "" {
			repoURL = "https://github.com/" + o + "/" + r + ".git"
		}
	}
	if repoURL == "" {
		return fmt.Errorf("flowbee fleet: set FLOWBEE_REPO_URL (or FLOWBEE_GITHUB_OWNER+REPO) so build workers can clone/pull the local mirror")
	}

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	// verify the agent actually works BEFORE starting workers — a not-authed /
	// rate-limited / mis-configured agent otherwise stalls every job as the vague
	// "agent produced no changes". Fail loudly here instead.
	builderCmd := roleAgentCmd(*agent, fleetBuilderFamily, true, *agentCmd, *buildCmd)
	// the review roles default to a DIFFERENT model than the builder (§5.5), so resolve a
	// representative review command (the code_reviewer's) to smoke-test separately.
	reviewFamily := "opus"
	for _, r := range nonBuilderFleetRoles() {
		if r.role == "code_reviewer" {
			reviewFamily = r.family
		}
	}
	reviewerCmd := roleAgentCmd(*agent, reviewFamily, false, *agentCmd, *buildCmd)
	if !*noSmoke {
		fmt.Printf("smoke-testing the build agent on %s ...\n", host)
		if err := smokeAgent(builderCmd); err != nil {
			return fmt.Errorf("build agent smoke test FAILED on %s: %w\n   fix the agent (e.g. `claude --version` / re-auth) then retry, or --no-smoke to skip", host, err)
		}
		fmt.Println("✓ build agent works")
		// Validate the REVIEW model too. It defaults to a different model than the builder,
		// so an account that has the build model but not the review model would pass the
		// build smoke, start the fleet, then fail EVERY review at runtime — a silent stall
		// the build smoke alone can't catch. Skip only if the operator pinned both roles to
		// the same command (then the build smoke already covered it).
		if reviewerCmd != builderCmd {
			fmt.Printf("smoke-testing the review agent on %s ...\n", host)
			if err := smokeReviewAgent(reviewerCmd); err != nil {
				return fmt.Errorf("review agent smoke test FAILED on %s: %w\n   the review model differs from the build model (§5.5); ensure it's available + authed, or override --agent-cmd, or --no-smoke to skip", host, err)
			}
			fmt.Println("✓ review agent works")
		}
	}

	env := append(os.Environ(), "FLOWBEE_URL="+*url)
	// If the control plane requires auth, mint a per-worker token from the secret (a
	// trusted box mints its own identities' tokens). Without a secret the box runs
	// open — Tailscale is the trust boundary. Each worker's token must match its
	// identity, so it's minted per worker, not shared.
	secret := os.Getenv("FLOWBEE_WORKER_AUTH_SECRET")

	// Supervise each worker: if it exits while the fleet is up (a crash, an OOM, an
	// agent that wedged and got killed), respawn it with capped backoff. Without this a
	// dead worker stays dead until the whole fleet is restarted — silent capacity loss.
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	var mu sync.Mutex
	var kids []*exec.Cmd
	// F6: each box has at most one codex + one claude login; a worker's account is its
	// AGENT's login (a codex builder uses the codex account, a claude reviewer the claude
	// account — so a split-stack box reports to TWO accounts correctly). Set per box via
	// FLOWBEE_CODEX_ACCOUNT / FLOWBEE_CLAUDE_ACCOUNT; the worker advertises it + reports its
	// usage/limit, and dispatch gates a maxed account. Empty => that agent isn't metered.
	codexAcct := os.Getenv("FLOWBEE_CODEX_ACCOUNT")
	claudeAcct := os.Getenv("FLOWBEE_CLAUDE_ACCOUNT")
	supervise := func(identity, account string, argv ...string) {
		go func() {
			backoff := time.Second
			for ctx.Err() == nil {
				c := exec.Command(self, argv...)
				c.Env = env
				if account != "" {
					// per-worker FLOWBEE_ACCOUNT (its agent's login), without mutating the shared env.
					c.Env = append(append([]string(nil), env...), "FLOWBEE_ACCOUNT="+account)
				}
				if secret != "" {
					c.Env = append(c.Env, "FLOWBEE_WORKER_TOKEN="+auth.NewBearer([]byte(secret), nil, false).Mint(identity))
				}
				c.Stdout, c.Stderr = prefixWriter{identity, os.Stdout}, prefixWriter{identity, os.Stderr}
				if err := c.Start(); err != nil {
					fmt.Fprintf(os.Stderr, "fleet: start %s: %v\n", identity, err)
				} else {
					mu.Lock()
					kids = append(kids, c)
					mu.Unlock()
					ran := time.Now()
					_ = c.Wait()
					if ctx.Err() != nil {
						return // fleet shutting down — the exit was our kill, not a crash
					}
					fmt.Fprintf(os.Stderr, "fleet: worker %s exited; respawning in %s\n", identity, backoff)
					if time.Since(ran) > 60*time.Second {
						backoff = time.Second // it ran healthy for a while — reset the backoff
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = nextRespawnBackoff(backoff)
			}
		}()
	}

	// build workers (--remote): N parallel, each a worktree off the shared local mirror.
	// They run the builder model (fleetBuilderFamily) so reviews/resolutions can be a
	// genuinely different model.
	for i := 0; i < *builders; i++ {
		id := fmt.Sprintf("%s-builder-%d", host, i)
		supervise(id, accountForAgentCmd(builderCmd, codexAcct, claudeAcct),
			"work", "--role", "eng_worker", "--remote",
			"--mirror", *mirror, "--repo-url", repoURL,
			"--identity", id, "--model-family", fleetBuilderFamily,
			"--model-label", modelLabelFor(*agent, fleetBuilderFamily), "--agent-cmd", builderCmd)
	}
	// The non-builder roles (one each). Distinct model_family per role so a reviewer/
	// resolver is never the builder's family (anti-affinity, enforced server-side) AND
	// genuinely runs a different model (roleAgentCmd injects --model <family>). A
	// conflict_resolver is REQUIRED — without it every real merge conflict finds no
	// eligible worker and escalates to needs_human instead of resolving autonomously.
	// Roles that author files on a branch (conflict_resolver resolves markers) run the
	// file-writing BUILD harness (--remote + build agent); ones that push an empty
	// findings-commit (code_reviewer) or only emit a verdict (spec roles) use the review
	// agent. needsMirror roles push to the issue branch, so they get --mirror + repo-url.
	for _, r := range nonBuilderFleetRoles() {
		roleCmd := roleAgentCmd(*agent, r.family, r.writesFiles, *agentCmd, *buildCmd)
		label := modelLabelFor(*agent, r.family)
		// the conflict_resolver can run a different (more reliable) backend than the build
		// agent: heavy resolution stalls some builders out, so swap just this role when set.
		// It also scales out (--resolvers N) since one resolver can't keep up with a burst.
		count := 1
		if r.role == "conflict_resolver" {
			if *resolverCmd != "" {
				roleCmd = *resolverCmd
				label = "resolver-custom"
			}
			if *resolvers > 1 {
				count = *resolvers
			}
		}
		for n := 0; n < count; n++ {
			id := host + "-" + r.role
			if count > 1 {
				id = fmt.Sprintf("%s-%d", id, n)
			}
			argv := []string{"work", "--role", r.role, "--identity", id, "--model-family", r.family,
				"--model-label", label,
				"--agent-cmd", roleCmd}
			if r.writesFiles {
				argv = append(argv, "--remote")
			}
			if r.needsMirror {
				argv = append(argv, "--mirror", *mirror, "--repo-url", repoURL)
			}
			supervise(id, accountForAgentCmd(roleCmd, codexAcct, claudeAcct), argv...)
		}
	}

	fmt.Printf("🐝 flowbee fleet up on %s → %s  [%s]\n   %d build + 1 code-review + 1 conflict-resolver + 1 author + 1 issue-review worker (all roles; this box is fungible capacity)\n",
		host, *url, buildVersion(), *builders)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nflowbee fleet: shutting down workers...")
	shutdown() // stop the supervisors respawning before we kill the children
	mu.Lock()
	snapshot := append([]*exec.Cmd(nil), kids...)
	mu.Unlock()
	killAll(snapshot)
	return nil
}

// nextRespawnBackoff doubles the respawn delay up to a 30s cap, so a worker that
// crash-loops backs off instead of hot-spinning, while a healthy worker (which resets
// the backoff after a good run) respawns promptly.
func nextRespawnBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

// printFleetSystemd emits a ready-to-install systemd unit + env file so the fleet runs
// as a managed service: clean `systemctl restart` (no stale processes), reboot
// survival, logs via journalctl. Solves the operability pain the first live run hit —
// nohup/pkill fragility, stale binaries, fleets not surviving restarts.
func printFleetSystemd(url string, builders int, agent, agentCmd, buildCmd string) {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	user := envOr("USER", "sam")
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/home/" + user
	}
	envPath := home + "/.flowbee/fleet.env"
	// systemd env files are single-line KEY=VALUE; collapse any newlines in the agent
	// commands (the build prompt is multi-line) to a space.
	oneLine := func(s string) string { return strings.Join(strings.Fields(s), " ") }

	fmt.Printf("# 1. Write %s  (chmod 600 — holds FLOWBEE_WORKER_AUTH_SECRET):\n", envPath)
	fmt.Printf("FLOWBEE_URL=%s\n", url)
	// FLOWBEE_REPO_URL is REQUIRED — `flowbee fleet` refuses to start without it (build
	// workers clone/pull the repo through it). Echo the resolved value; if neither it
	// nor OWNER+REPO is set in this shell, emit an obvious placeholder so the operator
	// fills it in rather than installing a unit that dies at startup. systemd env files
	// take the whole line after `=` as the value, so this stays comment-free.
	repoURL := os.Getenv("FLOWBEE_REPO_URL")
	if repoURL == "" {
		if o, r := os.Getenv("FLOWBEE_GITHUB_OWNER"), os.Getenv("FLOWBEE_GITHUB_REPO"); o != "" && r != "" {
			repoURL = "https://github.com/" + o + "/" + r + ".git"
		}
	}
	if repoURL == "" {
		repoURL = "git@github.com:OWNER/REPO.git"
	}
	fmt.Printf("FLOWBEE_REPO_URL=%s\n", repoURL)
	// A production control plane enforces worker auth, so the fleet needs the shared
	// secret. Always emit the line as a PLACEHOLDER (never the live value — the unit
	// text is often pasted, committed, or logged); delete it only for an insecure dev CP.
	fmt.Printf("FLOWBEE_WORKER_AUTH_SECRET=<shared-worker-secret>\n")
	// Only pin the agent commands when the operator OVERRODE them. Left unset, the fleet
	// applies per-role defaults with distinct --model (Sonnet builds, Opus reviews) for
	// genuine §5.5 diversity — pinning a single command here would forgo that.
	if oneLine(agentCmd) != "" {
		fmt.Printf("FLOWBEE_AGENT_CMD=%s\n", oneLine(agentCmd))
	}
	if oneLine(buildCmd) != "" {
		fmt.Printf("FLOWBEE_BUILD_AGENT_CMD=%s\n", oneLine(buildCmd))
	}
	fmt.Printf("\n")

	// Pin the agent backend in ExecStart only when it's not the default (claude), so a
	// codex fleet survives restarts/reboots without an env var.
	agentFlag := ""
	if agent != "" && agent != "claude" {
		agentFlag = " --agent " + agent
	}
	fmt.Printf("# 2. Write /etc/systemd/system/flowbee-fleet.service:\n")
	fmt.Printf(`[Unit]
Description=Flowbee fleet
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
EnvironmentFile=%s
ExecStart=%s fleet --builders %d%s
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target

`, user, envPath, self, builders, agentFlag)

	fmt.Printf("# 3. Enable + start (clean restart any time with `systemctl restart flowbee-fleet`):\n")
	fmt.Printf("sudo systemctl daemon-reload && sudo systemctl enable --now flowbee-fleet\n")
	fmt.Printf("journalctl -u flowbee-fleet -f   # tail logs; the startup line shows the build SHA\n")
}

// smokeAgent runs the build agent on a trivial "write ok.txt" task in a temp dir and
// confirms it produced the file — proving the agent CLI is installed, authed, and
// responsive. A failure here is the single most common silent stall (an un-authed or
// rate-limited agent that exits cleanly having written nothing).
func smokeAgent(buildCmd string) error {
	dir, err := os.MkdirTemp("", "flowbee-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	taskFile := filepath.Join(dir, "task.md")
	if err := os.WriteFile(taskFile, []byte("# Smoke test\n\nCreate a file named `ok.txt` containing the word ok. Make no other changes."), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := llm.EnsureDefaultAgentRouter(ctx); err != nil {
		return fmt.Errorf("llm router: %w", err)
	}
	resp, runErr := llm.Call(ctx, llm.SlotDraftingComplex, llm.Request{
		Prompt: "smoke-test build agent",
		Input: llm.AgentCommand{
			Command: buildCmd,
			Dir:     dir,
			Env: append(os.Environ(),
				"FLOWBEE_TASK_FILE="+taskFile,
				"FLOWBEE_TASK=Create a file named ok.txt containing ok.",
			),
			TTLSeconds: 120,
		},
	})
	if _, statErr := os.Stat(filepath.Join(dir, "ok.txt")); statErr == nil {
		return nil // the agent wrote the file — it works
	}
	if runErr != nil {
		return fmt.Errorf("agent exited with error (is it installed + authed?): %v: %s", runErr, trunc(resp.Text, 300))
	}
	return fmt.Errorf("agent ran but wrote no file — likely not authed, rate-limited, or wrong --build-agent-cmd: %s", trunc(resp.Text, 300))
}

// smokeReviewAgent runs the REVIEW agent command on a trivial prompt and confirms it
// exits cleanly with output — proving the review model (which defaults to a DIFFERENT
// model than the builder, e.g. Opus) is installed, authed, and reachable. The review
// agent emits a verdict, not files, so it can't be checked for ok.txt like the build
// agent; a clean exit with non-empty output is the signal. Without this, an account
// missing the review model passes the build smoke and fails every review at runtime.
func smokeReviewAgent(reviewCmd string) error {
	dir, err := os.MkdirTemp("", "flowbee-smoke-rv-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	taskFile := filepath.Join(dir, "task.md")
	if err := os.WriteFile(taskFile, []byte("Reply with exactly the word: ok"), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := llm.EnsureDefaultAgentRouter(ctx); err != nil {
		return fmt.Errorf("llm router: %w", err)
	}
	resp, runErr := llm.Call(ctx, llm.SlotJudge, llm.Request{
		Prompt: "smoke-test review agent",
		Input: llm.AgentCommand{
			Command: reviewCmd,
			Dir:     dir,
			Env: append(os.Environ(),
				"FLOWBEE_TASK_FILE="+taskFile,
				"FLOWBEE_TASK=Reply with ok.",
			),
			TTLSeconds: 120,
		},
	})
	if runErr != nil {
		return fmt.Errorf("review agent exited with error (is the review model available + authed?): %v: %s", runErr, trunc(resp.Text, 300))
	}
	if strings.TrimSpace(resp.Text) == "" {
		return fmt.Errorf("review agent ran but produced no output — likely not authed or wrong --agent-cmd: %s", trunc(resp.Text, 300))
	}
	return nil
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
