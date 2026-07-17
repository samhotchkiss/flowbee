package watchdog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/tmuxio"
)

// ladderClock is an instant clock: Sleep advances Now (so a classify-wait's timeout loop
// terminates at memory speed) without ever wall-clock-blocking.
type ladderClock struct {
	mu sync.Mutex
	t  time.Time
}

func newLadderClock() *ladderClock { return &ladderClock{t: time.Unix(1700000000, 0)} }

func (c *ladderClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *ladderClock) Sleep(_ context.Context, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// ladderFake is a stateful scripted tmux Runner: it drives capture-pane output off a
// `phase` that the send-keys commands advance (seeing the CLI line -> the agent boots to
// a prompt; seeing the goal -> the agent starts working). newSessionErr forces a
// stage-1 failure; goalStuck keeps the pane idle after the goal (a confirm-working
// timeout); shellArrival controls what the remote shell wait sees.
type ladderFake struct {
	mu            sync.Mutex
	calls         []string
	phase         string // shell | marker | prompt | working | auth | limbo
	hostname      string // what $(hostname) resolves to in the pane (default "remote-box")
	markerOut     string // the resolved remote-host marker line, set on the marker echo
	newSessionErr error
	goalStuck     bool
}

func (f *ladderFake) Run(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmd)
	switch {
	case strings.Contains(cmd, "new-session"):
		if f.phase == "" {
			f.phase = "shell"
		}
		return "", f.newSessionErr
	case strings.Contains(cmd, "kill-session"):
		return "", nil
	case strings.Contains(cmd, "display-message"):
		return "%1\x1f200\x1f0", nil
	case strings.Contains(cmd, "set-buffer"), strings.Contains(cmd, "paste-buffer"):
		return "", nil
	case strings.Contains(cmd, "send-keys"):
		if strings.Contains(cmd, "FLOWBEE_REMOTE_") && strings.Contains(cmd, "$(hostname)") {
			// model the shell evaluating `echo <marker>_$(hostname)`.
			host := f.hostname
			if host == "" {
				host = "remote-box"
			}
			f.markerOut = extractFakeMarker(cmd) + "_" + host
			f.phase = "marker"
		}
		if strings.Contains(cmd, "CLAUDE_CONFIG_DIR") || strings.Contains(cmd, "CODEX_HOME") {
			f.phase = "prompt"
		}
		if strings.Contains(cmd, "execute the epic") {
			if f.goalStuck {
				f.phase = "prompt" // goal "sent" but the pane never advances to working
			} else {
				f.phase = "working"
			}
		}
		return "", nil
	case strings.Contains(cmd, "capture-pane"):
		return f.captureFor(), nil
	}
	return "", nil
}

// extractFakeMarker pulls "FLOWBEE_REMOTE_<nonce>" out of an `echo <marker>_$(hostname)`
// send-keys command, mirroring the ladder's own marker construction.
func extractFakeMarker(cmd string) string {
	i := strings.Index(cmd, "FLOWBEE_REMOTE_")
	if i < 0 {
		return "FLOWBEE_REMOTE_"
	}
	rest := cmd[i:]
	if j := strings.Index(rest, "_$("); j >= 0 {
		return rest[:j]
	}
	return "FLOWBEE_REMOTE_"
}

func (f *ladderFake) captureFor() string {
	switch f.phase {
	case "marker":
		// the shell's OUTPUT line for the confirmation echo (no `$(`/`echo ` — the
		// ladder skips the command-echo line).
		return "ops@remote-box:~$ echo cmd\n" + f.markerOut + "\nops@remote-box:~$"
	case "prompt":
		return "booting the agent…\n❯"
	case "working":
		return "• Working (5s • esc to interrupt)"
	case "auth":
		return "ops@buncher's password:"
	case "limbo":
		return "Connecting to buncher…" // neither a shell nor an auth prompt
	default: // shell
		return "ops@buncher:~/epics/frob$"
	}
}

func (f *ladderFake) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *ladderFake) countMatching(sub string) int {
	n := 0
	for _, c := range f.recorded() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func newLadderClient(f *ladderFake) (*tmuxio.Client, *ladderClock) {
	clk := newLadderClock()
	return tmuxio.New(tmuxio.WithRunner(f), tmuxio.WithClock(clk)), clk
}

func remoteSeat() LaunchSeat {
	return LaunchSeat{Box: "buncher", AgentFamily: "claude", ConfigDir: "/home/ops/.claude-pearl", Account: "claude:pearl@swh.me"}
}

func TestRunLadder_RemoteHappyPath(t *testing.T) {
	f := &ladderFake{hostname: "remote-box"}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/2026-07-16-frob.md", Dir: "/home/ops/epics/frob",
		LocalHostname: "control-plane", RemoteMarkerNonce: "testnonce",
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchVerified || res.Stage != StageDone {
		t.Fatalf("expected verified/done, got %+v", res)
	}
	if res.Session != "epic-frob" {
		t.Fatalf("session name: %q", res.Session)
	}
	// the ssh attach line was sent to the far box by EXACT name.
	if f.countMatching("ssh -t -- buncher tmux new -A -s epic-frob") == 0 {
		t.Fatalf("ssh attach line missing: %v", f.recorded())
	}
	// the remote-host confirmation ran and matched a host != the local one.
	if f.countMatching("echo FLOWBEE_REMOTE_testnonce_$(hostname)") == 0 {
		t.Fatalf("remote-host confirmation echo missing: %v", f.recorded())
	}
	// M2: tmuxio.Client exactifies targets to the pane-valid `=epic-frob:` form; the
	// ladder must never emit a bare `-t 'epic-frob'` NOR a lone (colon-less) `=epic-frob`.
	for _, c := range f.recorded() {
		if strings.Contains(c, "-t 'epic-frob'") {
			t.Fatalf("a BARE (prefix-matching) target was used: %q", c)
		}
		if strings.Contains(c, "'=epic-frob'") {
			t.Fatalf("a lone (colon-less) =epic-frob target — tmux's pane parser rejects it: %q", c)
		}
	}
	if f.countMatching("'=epic-frob:'") == 0 {
		t.Fatalf("expected the pane-valid =epic-frob: exact target: %v", f.recorded())
	}
	// no rollback kill on a clean launch.
	if f.countMatching("kill-session") != 0 {
		t.Fatalf("a verified launch must not kill the session")
	}
}

// TestRunLadder_SSHExitToLocalShell_NotVerified is the §15.15 M3 guard: if the ssh line
// instant-exits, control drops to the LOCAL control-plane shell (which also looks like a
// shell). The remote-host confirmation resolves $(hostname) to the LOCAL host, so the
// ladder must REFUSE to type the launch line there — LaunchFailed, never LaunchVerified,
// with rollback.
func TestRunLadder_SSHExitToLocalShell_NotVerified(t *testing.T) {
	f := &ladderFake{hostname: "control-plane"} // $(hostname) == the local host: an ssh drop-back
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
		LocalHostname: "control-plane", RemoteMarkerNonce: "testnonce",
		ShellTimeout: 6 * time.Second, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome == LaunchVerified {
		t.Fatalf("M3 regression: a launch into the LOCAL shell must NOT verify: %+v", res)
	}
	if res.Outcome != LaunchFailed || res.Stage != StageAwaitRemoteShell {
		t.Fatalf("expected failed at await_remote_shell, got %+v", res)
	}
	// it must NOT have typed the CLI launch line into the local shell.
	if f.countMatching("CLAUDE_CONFIG_DIR=") != 0 {
		t.Fatalf("the CLI launch line was typed into a possibly-local shell: %v", f.recorded())
	}
	if f.countMatching("kill-session") == 0 {
		t.Fatalf("a confirmation failure must roll back the local session")
	}
}

func TestRunLadder_LocalSeatSkipsSSH(t *testing.T) {
	f := &ladderFake{}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: LaunchSeat{Box: "", AgentFamily: "codex", CodexHome: "/home/ops/.codex"},
		SpecPath: "epics/2026-07-16-frob.md",
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchVerified {
		t.Fatalf("expected verified, got %+v", res)
	}
	if f.countMatching("ssh -t") != 0 {
		t.Fatalf("a LOCAL seat must skip the ssh stages: %v", f.recorded())
	}
	// codex CLI line, not claude.
	if f.countMatching("CODEX_HOME=/home/ops/.codex") == 0 || f.countMatching(" codex") == 0 {
		t.Fatalf("codex CLI line missing: %v", f.recorded())
	}
}

func TestRunLadder_InteractiveAuthIsNotAFailure(t *testing.T) {
	f := &ladderFake{phase: "auth"}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchAwaitingAuth || res.Stage != StageRemoteAttach {
		t.Fatalf("expected awaiting_auth at remote_attach, got %+v", res)
	}
	// the session is LEFT ALIVE for the human to answer.
	if f.countMatching("kill-session") != 0 {
		t.Fatalf("an auth-wait must NOT kill the session (the human answers in it)")
	}
}

func TestRunLadder_ShellTimeoutFails(t *testing.T) {
	f := &ladderFake{phase: "limbo"} // never a shell, never an auth prompt
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
		ShellTimeout: 6 * time.Second, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchFailed || res.Stage != StageAwaitRemoteShell {
		t.Fatalf("expected failed at await_remote_shell, got %+v", res)
	}
	if f.countMatching("kill-session") == 0 {
		t.Fatalf("a stage failure must roll back (kill) the local session")
	}
}

func TestRunLadder_GoalNeverWorkingFails(t *testing.T) {
	f := &ladderFake{goalStuck: true}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: LaunchSeat{Box: "", AgentFamily: "claude", ConfigDir: "/c"},
		SpecPath: "epics/x.md", WorkingTimeout: 6 * time.Second, PollInterval: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchFailed || res.Stage != StageConfirmWorking {
		t.Fatalf("expected failed at confirm_working, got %+v", res)
	}
	if f.countMatching("kill-session") == 0 {
		t.Fatalf("a confirm-working failure must roll back the session")
	}
}

func TestRunLadder_SessionCreateFailureIsInfraError(t *testing.T) {
	f := &ladderFake{newSessionErr: errors.New("tmux: session exists")}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
	})
	if err == nil {
		t.Fatal("expected an infra error on new-session failure")
	}
	if res.Outcome != LaunchFailed || res.Stage != StageCreateLocal {
		t.Fatalf("expected failed at create_local, got %+v", res)
	}
}

func TestRunLadder_UnknownFamilyRejected(t *testing.T) {
	f := &ladderFake{}
	client, clk := newLadderClient(f)
	_, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: LaunchSeat{Box: "", AgentFamily: "gpt", CodexHome: "/c"}, SpecPath: "epics/x.md",
	})
	if err == nil {
		t.Fatal("expected an error for an unknown agent family")
	}
}

func TestBuildCLILineAndSSHAttach(t *testing.T) {
	claude := buildCLILine(LaunchSeat{AgentFamily: "claude", ConfigDir: "/c", Account: "claude:pearl@swh.me", ExtraEnv: map[string]string{"B": "2", "A": "1"}}, "claude")
	if claude != "CLAUDE_CONFIG_DIR=/c FLOWBEE_ACCOUNT=claude:pearl@swh.me A=1 B=2 claude" {
		t.Fatalf("claude CLI line: %q", claude)
	}
	codex := buildCLILine(LaunchSeat{AgentFamily: "codex", CodexHome: "/h"}, "codex")
	if codex != "CODEX_HOME=/h codex" {
		t.Fatalf("codex CLI line: %q", codex)
	}
	if got := buildSSHAttachLine("buncher", "epic-frob"); got != "ssh -t -- buncher tmux new -A -s epic-frob" {
		t.Fatalf("ssh attach: %q", got)
	}
}

func TestExtractMarkerHost(t *testing.T) {
	cap := "ops@remote-box:~$ echo FLOWBEE_REMOTE_n1_$(hostname)\nFLOWBEE_REMOTE_n1_remote-box\nops@remote-box:~$"
	host, ok := extractMarkerHost(cap, "FLOWBEE_REMOTE_n1")
	if !ok || host != "remote-box" {
		t.Fatalf("extractMarkerHost = %q,%v — the command-echo line must be skipped and only the OUTPUT read", host, ok)
	}
	// a capture with ONLY the command echo (no output yet) must not resolve.
	if _, ok := extractMarkerHost("$ echo FLOWBEE_REMOTE_n1_$(hostname)", "FLOWBEE_REMOTE_n1"); ok {
		t.Fatal("the command-echo line alone must not resolve a host")
	}
}

// TestExtractMarkerHost_ReEchoedInput guards against an ssh -t / PS2 setup that RE-ECHOES
// the typed command (so the pane holds the marker in BOTH the echoed input line AND the
// resolved-output line): the extractor must pick the OUTPUT host, never a token off the
// still-`$(hostname)` input line.
func TestExtractMarkerHost_ReEchoedInput(t *testing.T) {
	// row 1: primary prompt echo of the typed command (has `echo ` and `$(`)
	// row 2: a PS2/bracketed re-echo of the same command (still has `$(`)
	// row 3: the resolved OUTPUT — the only line the extractor may read
	capt := "remote:~$ echo FLOWBEE_REMOTE_z9_$(hostname)\n" +
		"> FLOWBEE_REMOTE_z9_$(hostname)\n" +
		"FLOWBEE_REMOTE_z9_far-box\n" +
		"remote:~$ "
	host, ok := extractMarkerHost(capt, "FLOWBEE_REMOTE_z9")
	if !ok {
		t.Fatal("expected the resolved output host to be found")
	}
	if host != "far-box" {
		t.Fatalf("extractMarkerHost picked %q — it must read the OUTPUT host, not a $(hostname) input token", host)
	}
	if host == "$(hostname)" {
		t.Fatal("extractMarkerHost was fooled by the echoed input line")
	}
}

// TestConfirmRemoteHost_EmptyLocalHostnameFailsClosed is the §15.15 M3 fail-closed guard:
// if the control plane cannot name itself (os.Hostname() == "" ⇒ LocalHostname==""), the
// confirmation CANNOT tell the local shell from a remote one, so it must REFUSE to confirm
// arrival — returning NOT-verified with a clear cause and WITHOUT even echoing a marker.
// The full-ladder consequence (rollback + no CLI line typed on a !confirmed result) is the
// same !confirmed → killAndFail path proven by TestRunLadder_SSHExitToLocalShell_NotVerified.
// (This branch is unreachable through RunLadder — withDefaults fills LocalHostname from
// os.Hostname() when empty — so it is exercised directly here.)
func TestConfirmRemoteHost_EmptyLocalHostnameFailsClosed(t *testing.T) {
	f := &ladderFake{hostname: "remote-box"} // a genuine remote — but we can't prove which
	client, clk := newLadderClient(f)
	ok, ev, err := confirmRemoteHost(context.Background(), client, clk, "epic-frob", "n1", "", 5*time.Second, time.Second)
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if ok {
		t.Fatalf("empty local hostname must FAIL CLOSED, got confirmed=true (ev=%q)", ev)
	}
	if !strings.Contains(ev, "local hostname unknown") {
		t.Fatalf("evidence should name the cause, got %q", ev)
	}
	// it must refuse BEFORE echoing any marker (no probe into a possibly-local shell).
	if f.countMatching("FLOWBEE_REMOTE_") != 0 {
		t.Fatalf("fail-closed must refuse before echoing a marker: %v", f.recorded())
	}
}
