package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

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
	mirror := fs.String("mirror", envOr("FLOWBEE_MIRROR_PATH", "/tmp/flowbee-worker-mirror.git"), "local bare mirror shared by this box's build workers")
	builders := fs.Int("builders", 1, "parallel build workers on this box (each gets its own worktree off the shared mirror)")
	defaultAgent := `claude -p "$(cat "$FLOWBEE_TASK_FILE")" --dangerously-skip-permissions`
	buildAgent := `claude -p "$(cat "$FLOWBEE_TASK_FILE")

Create the file(s) on disk now. Make only the change described." --dangerously-skip-permissions`
	agentCmd := fs.String("agent-cmd", envOr("FLOWBEE_AGENT_CMD", defaultAgent), "agent CLI for review/author roles")
	buildCmd := fs.String("build-agent-cmd", envOr("FLOWBEE_BUILD_AGENT_CMD", buildAgent), "agent CLI for the build role (writes files)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" {
		return fmt.Errorf("flowbee fleet: --url (or FLOWBEE_URL) is required — the control-plane address")
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
	// review + spec roles (git-less, run anywhere). Distinct model_family per role so a
	// reviewer is never the builder's family (anti-affinity, enforced server-side).
	for _, r := range []struct{ role, family string }{
		{"code_reviewer", "opus"},
		{"spec_author", "claude"},
		{"spec_reviewer", "sonnet"},
	} {
		id := host + "-" + r.role
		spawn(id, "work", "--role", r.role,
			"--identity", id, "--model-family", r.family, "--agent-cmd", *agentCmd)
	}

	fmt.Printf("🐝 flowbee fleet up on %s → %s\n   %d build + 1 code-review + 1 author + 1 issue-review worker (all roles; this box is fungible capacity)\n",
		host, *url, *builders)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nflowbee fleet: shutting down workers...")
	killAll(kids)
	return nil
}
