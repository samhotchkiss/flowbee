package watchdog

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/tmuxio"
)

// These tests exercise the launch ladder against a REAL tmux server on an isolated `-L`
// socket — the part a fake Runner cannot vouch for. A fake matched Contains(cmd,
// "capture-pane") and never modeled tmux's "can't find pane" rejection, so it green-lit a
// lone `=name` target form that real tmux refuses (the §15.15 M2 bug). These tests close
// that gap: one walks create→await→send→confirm end to end against a substrate `agent`,
// and one asserts the lone `=name` form actually fails while tmuxio's `=name:` succeeds.
// They skip when tmux is not installed.

// realClk is the production-shaped clock for the integration ladder (the unit tests use a
// fake instant clock; a real tmux substrate renders asynchronously, so this one really
// sleeps).
type realClk struct{}

func (realClk) Now() time.Time { return time.Now() }
func (realClk) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// agentSubstrate simulates an agent CLI in a tmux pane: it renders a `❯ ` idle prompt
// (classified IDLE_AT_PROMPT), echoes the FIRST submitted line and reprints the prompt
// (the CLI-line stage), and on the SECOND submitted line (the goal) renders a
// "• Working (…)" line (classified WORKING) and stays there — exactly the state sequence
// the ladder's classify-waits key on.
const agentSubstrate = `printf '❯ '
n=0
while IFS= read -r line; do
  n=$((n+1))
  if [ "$n" -ge 2 ]; then
    printf '\n• Working (1s · esc to interrupt)\n'
  else
    printf '\nGOT[%s]\n❯ ' "$line"
  fi
done`

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping real-server ladder integration test")
	}
}

func TestIntegrationLadderLocalWalksRealTmux(t *testing.T) {
	requireTmux(t)
	socket := fmt.Sprintf("flowbee-ladder-%d", time.Now().UnixNano())
	client := tmuxio.New(tmuxio.WithSocket(socket), tmuxio.WithClock(realClk{}))
	t.Cleanup(func() { _ = client.KillServer(context.Background()) })

	res, err := RunLadder(context.Background(), client, realClk{}, LadderParams{
		Slug:           "itest",
		Seat:           LaunchSeat{Box: "", AgentFamily: "codex", CodexHome: "/tmp/codex-home"},
		SpecPath:       "epics/itest.md",
		LoginShell:     agentSubstrate, // the local session IS the fake agent
		PromptTimeout:  5 * time.Second,
		WorkingTimeout: 5 * time.Second,
		PollInterval:   100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunLadder against real tmux: %v", err)
	}
	if res.Outcome != LaunchVerified || res.Stage != StageDone {
		t.Fatalf("expected the ladder to verify against real tmux, got %+v", res)
	}
	// prove the pane really reached WORKING via the exact (=name:) target tmuxio built.
	capt, cerr := client.Capture(context.Background(), "epic-itest", 0)
	if cerr != nil {
		t.Fatalf("capture the launched pane: %v", cerr)
	}
	if st, _ := tmuxio.Classify(capt.Raw); st != tmuxio.StateWorking {
		t.Fatalf("launched pane is not WORKING: %q", capt.Raw)
	}
}

// TestIntegrationLoneEqualsTargetRejectedByRealTmux is the direct M2 proof: tmux's
// target-PANE parser rejects a lone `=name` (no trailing colon) — the form the ladder
// used to build — while accepting tmuxio's `=name:`. This is the fact the fake could not
// model, and the reason the ladder must pass BARE names through tmuxio.Client (which
// appends the colon) rather than pre-`=`-prefixing.
func TestIntegrationLoneEqualsTargetRejectedByRealTmux(t *testing.T) {
	requireTmux(t)
	socket := fmt.Sprintf("flowbee-eqtgt-%d", time.Now().UnixNano())
	ctx := context.Background()
	defer func() { _ = exec.CommandContext(ctx, "tmux", "-L", socket, "kill-server").Run() }()

	if out, err := exec.CommandContext(ctx, "tmux", "-L", socket,
		"new-session", "-d", "-s", "epicreal", "sleep 60").CombinedOutput(); err != nil {
		t.Fatalf("create real session: %v (%s)", err, out)
	}

	// lone `=epicreal` (no colon) — the M2 bug form — must FAIL for a pane command.
	if out, err := exec.CommandContext(ctx, "tmux", "-L", socket,
		"capture-pane", "-p", "-t", "=epicreal").CombinedOutput(); err == nil {
		t.Fatalf("expected tmux to REJECT a lone =name pane target, but it succeeded: %q", out)
	} else if !strings.Contains(strings.ToLower(string(out)), "can't find pane") &&
		!strings.Contains(strings.ToLower(string(out)), "cannot find pane") {
		t.Logf("note: tmux rejected =epicreal with a different message: %q", out) // still a failure, which is what we require
	}

	// `=epicreal:` — tmuxio's form — must SUCCEED.
	if out, err := exec.CommandContext(ctx, "tmux", "-L", socket,
		"capture-pane", "-p", "-t", "=epicreal:").CombinedOutput(); err != nil {
		t.Fatalf("tmux rejected the =name: exact form tmuxio builds: %v (%s)", err, out)
	}
}

// TestIntegrationConfirmSameHostDifferentUserVerifies proves the REAL fleet topology
// against a LIVE tmux pane: a seat reached via `ssh <user>@localhost` shares the control
// plane's HOSTNAME and differs only by USER. It shadows whoami/hostname in the pane's shell
// so confirmRemoteHost's `$(whoami)@$(hostname)` resolves to a crafted
// claude1@Mac-Studio.local while the control plane's LocalIdentity is sam@Mac-Studio.local
// (SAME host, DIFFERENT user) — the case that FAILED CLOSED under hostname-only
// discrimination and must now VERIFY. No multi-user ssh is needed: the crafted marker is
// echoed by a real shell in a real tmux pane, and command substitution inherits the shell's
// function definitions, so `$(whoami)@$(hostname)` yields the shimmed identity.
func TestIntegrationConfirmSameHostDifferentUserVerifies(t *testing.T) {
	requireTmux(t)
	socket := fmt.Sprintf("flowbee-idtuple-%d", time.Now().UnixNano())
	ctx := context.Background()
	client := tmuxio.New(tmuxio.WithSocket(socket), tmuxio.WithClock(realClk{}))
	t.Cleanup(func() { _ = client.KillServer(ctx) })

	session := "epic-idtuple"
	if err := client.NewSession(ctx, tmuxio.SessionSpec{Name: session, Command: "/bin/sh"}); err != nil {
		t.Fatalf("create real shell session: %v", err)
	}
	// Shadow whoami/hostname so `$(whoami)@$(hostname)` resolves to a same-host-different-user
	// identity. POSIX function definitions are inherited by the command-substitution subshell.
	if _, err := client.Send(ctx, session,
		"whoami() { echo claude1; }; hostname() { echo Mac-Studio.local; }", tmuxio.SendOptions{}); err != nil {
		t.Fatalf("define identity shims: %v", err)
	}

	ok, ev, err := confirmRemoteHost(ctx, client, realClk{}, session, "itnonce",
		"sam@Mac-Studio.local", 5*time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("confirmRemoteHost infra error: %v", err)
	}
	if !ok {
		t.Fatalf("a same-host-different-user seat MUST verify against real tmux (the field bug), got not-verified: %q", ev)
	}
	if !strings.Contains(ev, "claude1@Mac-Studio.local") {
		t.Fatalf("evidence should name the confirmed remote identity, got %q", ev)
	}
}
