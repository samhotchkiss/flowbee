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
// Build workers run in --remote mode (own local mirror + a worktree per job, so N
// builders on one box run in parallel); review/author workers are git-less. Distinct
// model_family tags keep a reviewer off the builder's family (anti-affinity). Auth,
// if the control plane requires it, comes from FLOWBEE_WORKER_TOKEN in the env.
func runFleet(args []string) error {
	fs := flag.NewFlagSet("fleet", flag.ContinueOnError)
	url := fs.String("url", envOr("FLOWBEE_URL", ""), "control-plane URL, e.g. http://100.67.2.108:7070")
	mirror := fs.String("mirror", envOr("FLOWBEE_WORKER_MIRRORS_DIR", ""), "directory for the worker's per-repo BARE mirrors (default ~/.flowbee/mirrors; NOT a working checkout)")
	builders := fs.Int("builders", 1, "parallel build workers on this box (each gets its own worktree off the shared mirror)")
	defaultAgent := `claude -p "$(cat "$FLOWBEE_TASK_FILE")" --dangerously-skip-permissions`
	buildAgent := `claude -p "$(cat "$FLOWBEE_TASK_FILE")

Create the file(s) on disk now. Make only the change described." --dangerously-skip-permissions`
	agentCmd := fs.String("agent-cmd", envOr("FLOWBEE_AGENT_CMD", defaultAgent), "agent CLI for review/author roles")
	buildCmd := fs.String("build-agent-cmd", envOr("FLOWBEE_BUILD_AGENT_CMD", buildAgent), "agent CLI for the build role (writes files)")
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
	if !*noSmoke {
		fmt.Printf("smoke-testing the build agent on %s ...\n", host)
		if err := smokeAgent(*buildCmd); err != nil {
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

	var kids []*exec.Cmd
	spawn := func(identity string, argv ...string) {
		c := exec.Command(self, argv...)
		c.Env = env
		if secret != "" {
			c.Env = append(c.Env, "FLOWBEE_WORKER_TOKEN="+auth.NewBearer([]byte(secret), nil, false).Mint(identity))
		}
		c.Stdout, c.Stderr = prefixWriter{identity, os.Stdout}, prefixWriter{identity, os.Stderr}
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "fleet: start %s: %v\n", identity, err)
			return
		}
		kids = append(kids, c)
	}

	// build workers (--remote): N parallel, each a worktree off the shared local mirror.
	for i := 0; i < *builders; i++ {
		id := fmt.Sprintf("%s-builder-%d", host, i)
		spawn(id, "work", "--role", "eng_worker", "--remote",
			"--mirror", *mirror, "--repo-url", repoURL,
			"--identity", id, "--model-family", "claude", "--agent-cmd", *buildCmd)
	}
	// review + spec roles. Distinct model_family per role so a reviewer is never the
	// builder's family (anti-affinity, enforced server-side). The code_reviewer now
	// lands an EMPTY findings-commit on the issue branch, so it gets the local mirror +
	// repo URL (it pushes with its key); spec roles stay git-less (they emit a verdict
	// only — no commit).
	for _, r := range []struct{ role, family string }{
		{"code_reviewer", "opus"},
		{"spec_author", "claude"},
		{"spec_reviewer", "sonnet"},
	} {
		id := host + "-" + r.role
		argv := []string{"work", "--role", r.role,
			"--identity", id, "--model-family", r.family, "--agent-cmd", *agentCmd}
		if r.role == "code_reviewer" {
			argv = append(argv, "--mirror", *mirror, "--repo-url", repoURL)
		}
		spawn(id, argv...)
	}

	fmt.Printf("🐝 flowbee fleet up on %s → %s  [%s]\n   %d build + 1 code-review + 1 author + 1 issue-review worker (all roles; this box is fungible capacity)\n",
		host, *url, buildVersion(), *builders)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nflowbee fleet: shutting down workers...")
	killAll(kids)
	return nil
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

	fmt.Printf("# 1. Write %s  (chmod 600 — may hold FLOWBEE_WORKER_AUTH_SECRET):\n", envPath)
	fmt.Printf("FLOWBEE_URL=%s\n", url)
	if v := os.Getenv("FLOWBEE_GITHUB_OWNER"); v != "" {
		fmt.Printf("FLOWBEE_GITHUB_OWNER=%s\n", v)
	}
	if v := os.Getenv("FLOWBEE_GITHUB_REPO"); v != "" {
		fmt.Printf("FLOWBEE_GITHUB_REPO=%s\n", v)
	}
	if v := os.Getenv("FLOWBEE_GIT_REMOTE"); v != "" {
		fmt.Printf("FLOWBEE_GIT_REMOTE=%s\n", v)
	}
	if v := os.Getenv("FLOWBEE_WORKER_AUTH_SECRET"); v != "" {
		fmt.Printf("FLOWBEE_WORKER_AUTH_SECRET=%s\n", v)
	}
	fmt.Printf("FLOWBEE_AGENT_CMD=%s\n", oneLine(agentCmd))
	fmt.Printf("FLOWBEE_BUILD_AGENT_CMD=%s\n\n", oneLine(buildCmd))

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
