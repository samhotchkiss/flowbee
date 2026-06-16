package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
)

// runUp is the single-box "just run it" supervisor (build-list: one-command fleet).
// It ensures a local mirror, starts the control plane, and starts one worker per
// pipeline role — each a real-agent loop — then prints the dashboard URL and
// supervises everything until Ctrl-C. This is the whole fleet in one command:
//
//	flowbee up
//
// Production multi-box topology still uses `flowbee serve` here + `flowbee work`
// on each remote; `up` is the local all-in-one.
func runUp(args []string) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	defaultAgent := `claude -p "$(cat "$FLOWBEE_TASK_FILE")" --dangerously-skip-permissions`
	agentCmd := fs.String("agent-cmd", envOr("FLOWBEE_AGENT_CMD", defaultAgent), "agent CLI spawned per job (reads $FLOWBEE_TASK_FILE)")
	mirror := fs.String("mirror", envOr("FLOWBEE_MIRROR_PATH", filepath.Join(os.TempDir(), "flowbee-mirror.git")), "local bare mirror path")
	selfMerge := fs.Bool("self-merge", envOr("FLOWBEE_ALLOW_SELF_MERGE", "") != "", "enable Branch B autonomous merge")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, _ := config.Load()
	owner := envOr("FLOWBEE_GITHUB_OWNER", cfg.GithubOwner)
	repo := envOr("FLOWBEE_GITHUB_REPO", cfg.GithubRepo)
	token := os.Getenv("FLOWBEE_GITHUB_TOKEN")
	if owner == "" || repo == "" {
		return fmt.Errorf("flowbee up: set github_owner/github_repo (flowbee.yaml) or FLOWBEE_GITHUB_OWNER/REPO")
	}
	if token == "" {
		return fmt.Errorf("flowbee up: set FLOWBEE_GITHUB_TOKEN (a fine-grained PAT with contents+PR+issues write)")
	}

	// 1. ensure the local mirror (Flowbee pushes build branches here, then to GitHub).
	if _, err := os.Stat(*mirror); err != nil {
		fmt.Printf("flowbee up: cloning mirror %s/%s -> %s\n", owner, repo, *mirror)
		clone := exec.Command("git", "clone", "--bare", "--quiet",
			fmt.Sprintf("https://github.com/%s/%s.git", owner, repo), *mirror)
		clone.Stderr = os.Stderr
		if err := clone.Run(); err != nil {
			return fmt.Errorf("clone mirror: %w", err)
		}
	}

	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	env := append(os.Environ(),
		"FLOWBEE_MIRROR_PATH="+*mirror,
		"FLOWBEE_GITHUB_OWNER="+owner,
		"FLOWBEE_GITHUB_REPO="+repo,
		// `up` is the single-box local convenience: its workers dial loopback, so
		// accept the open (no-auth) API rather than refusing to start.
		"FLOWBEE_INSECURE=1",
	)
	if *selfMerge {
		env = append(env, "FLOWBEE_ALLOW_SELF_MERGE=1")
	}

	var kids []*exec.Cmd
	spawn := func(label string, argv ...string) {
		c := exec.Command(self, argv...)
		c.Env = env
		c.Stdout, c.Stderr = prefixWriter{label, os.Stdout}, prefixWriter{label, os.Stderr}
		if err := c.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "flowbee up: start %s: %v\n", label, err)
			return
		}
		kids = append(kids, c)
	}

	// 2. control plane.
	spawn("serve", "serve")

	// 3. wait for the private API to answer before starting workers.
	base := "http://127.0.0.1" + orDefault(cfg.PrivateAddr, ":7070")
	if !waitHealthy(base+"/v1/roster", 20*time.Second) {
		killAll(kids)
		return fmt.Errorf("control plane did not come up at %s", base)
	}

	// 4. one worker per pipeline role. Distinct model_family tags keep code-review
	// off the same family as the build (anti-affinity); the issue-reviewer lens is
	// distinct from the author's, enforced server-side. All real-agent loops.
	roles := []struct{ role, identity, family string }{
		{"spec_author", "spec-author", "claude"},
		{"spec_reviewer", "issue-reviewer", "sonnet"},
		{"eng_worker", "builder", "claude"},
		{"code_reviewer", "code-reviewer", "opus"},
	}
	for _, r := range roles {
		spawn(r.role, "work", "--role", r.role, "--identity", r.identity, "--model-family", r.family, "--agent-cmd", *agentCmd)
	}

	fmt.Printf("\n🐝 Flowbee is up.\n   dashboard: %s/dashboard\n   board:     %s/board\n   intake:    label a GitHub issue `flowbee:build`, or POST %s/v1/specs\n   merge:     %s\n\n",
		base, base, base, branchBMsg(*selfMerge))

	// 5. supervise until signal, then tear the fleet down.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\nflowbee up: shutting down fleet...")
	killAll(kids)
	return nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func branchBMsg(b bool) string {
	if b {
		return "autonomous (Branch B, no human gate)"
	}
	return "handoff (Branch A — pass --self-merge for autonomous merge)"
}

func waitHealthy(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(url); err == nil {
			_ = resp.Body.Close()
			return true
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

func killAll(kids []*exec.Cmd) {
	for _, c := range kids {
		if c.Process != nil {
			_ = c.Process.Signal(syscall.SIGTERM)
		}
	}
	time.Sleep(500 * time.Millisecond)
	for _, c := range kids {
		if c.Process != nil {
			_ = c.Process.Kill()
		}
	}
}

// prefixWriter tags each child's output line with its role label.
type prefixWriter struct {
	label string
	w     *os.File
}

func (p prefixWriter) Write(b []byte) (int, error) {
	fmt.Fprintf(p.w, "[%s] %s", p.label, b)
	return len(b), nil
}
