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

// roleAgentCmd returns the agent CLI for a role: the operator override if non-empty,
// else the per-role default with `--model <family>` injected. writesFiles picks the
// file-writing build template (eng_worker, conflict_resolver) over the verdict/spec
// review template.
func roleAgentCmd(family string, writesFiles bool, agentOverride, buildOverride string) string {
	if writesFiles {
		if buildOverride != "" {
			return buildOverride
		}
		return fmt.Sprintf(buildAgentTmpl, family)
	}
	if agentOverride != "" {
		return agentOverride
	}
	return fmt.Sprintf(reviewAgentTmpl, family)
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
	// Per-role default agent commands inject `--model <family>` so the build and review
	// roles run GENUINELY DIFFERENT models — real §5.5 uncorrelated review, not the same
	// CLI-default model hiding behind distinct family labels (a reviewer that shares the
	// builder's model shares its blind spots). The claude CLI accepts the 'opus'/'sonnet'
	// aliases (claude --model opus|sonnet). Setting --agent-cmd / --build-agent-cmd (or
	// the env) OVERRIDES with one command for all review-author / all build roles,
	// forgoing per-role model diversity (the operator's explicit choice).
	// --output-format json makes claude print total_cost_usd + usage so the harness can
	// meter per-job cost (I-15); the harness unwraps `.result` for any text it needs.
	agentCmd := fs.String("agent-cmd", os.Getenv("FLOWBEE_AGENT_CMD"), "override the review/author agent CLI (empty = per-role --model defaults)")
	buildCmd := fs.String("build-agent-cmd", os.Getenv("FLOWBEE_BUILD_AGENT_CMD"), "override the build agent CLI (empty = per-role --model defaults)")
	noSmoke := fs.Bool("no-smoke", false, "skip the agent smoke test at startup")
	systemd := fs.Bool("systemd", false, "print a systemd unit + env file to run the fleet as a managed service (then exit), instead of starting it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return fmt.Errorf("flowbee fleet: --url (or FLOWBEE_URL) is required — the control-plane address")
	}
	if *systemd {
		printFleetSystemd(*url, *builders, *agentCmd, *buildCmd)
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
	builderCmd := roleAgentCmd(fleetBuilderFamily, true, *agentCmd, *buildCmd)
	if !*noSmoke {
		fmt.Printf("smoke-testing the build agent on %s ...\n", host)
		if err := smokeAgent(builderCmd); err != nil {
			return fmt.Errorf("agent smoke test FAILED on %s: %w\n   fix the agent (e.g. `claude --version` / re-auth) then retry, or --no-smoke to skip", host, err)
		}
		fmt.Println("✓ agent works")
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
	supervise := func(identity string, argv ...string) {
		go func() {
			backoff := time.Second
			for ctx.Err() == nil {
				c := exec.Command(self, argv...)
				c.Env = env
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
		supervise(id, "work", "--role", "eng_worker", "--remote",
			"--mirror", *mirror, "--repo-url", repoURL,
			"--identity", id, "--model-family", fleetBuilderFamily, "--agent-cmd", builderCmd)
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
		id := host + "-" + r.role
		argv := []string{"work", "--role", r.role, "--identity", id, "--model-family", r.family,
			"--agent-cmd", roleAgentCmd(r.family, r.writesFiles, *agentCmd, *buildCmd)}
		if r.writesFiles {
			argv = append(argv, "--remote")
		}
		if r.needsMirror {
			argv = append(argv, "--mirror", *mirror, "--repo-url", repoURL)
		}
		supervise(id, argv...)
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
func printFleetSystemd(url string, builders int, agentCmd, buildCmd string) {
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

	fmt.Printf("# 2. Write /etc/systemd/system/flowbee-fleet.service:\n")
	fmt.Printf(`[Unit]
Description=Flowbee fleet
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=%s
EnvironmentFile=%s
ExecStart=%s fleet --builders %d
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target

`, user, envPath, self, builders)

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
	cmd := exec.CommandContext(ctx, "sh", "-c", buildCmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"FLOWBEE_TASK_FILE="+taskFile,
		"FLOWBEE_TASK=Create a file named ok.txt containing ok.",
	)
	out, runErr := cmd.CombinedOutput()
	if _, statErr := os.Stat(filepath.Join(dir, "ok.txt")); statErr == nil {
		return nil // the agent wrote the file — it works
	}
	if runErr != nil {
		return fmt.Errorf("agent exited with error (is it installed + authed?): %v: %s", runErr, trunc(string(out), 300))
	}
	return fmt.Errorf("agent ran but wrote no file — likely not authed, rate-limited, or wrong --build-agent-cmd: %s", trunc(string(out), 300))
}

func trunc(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
