package watchdog

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/tmuxio"
	"github.com/samhotchkiss/flowbee/internal/verbs"
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
	identity      string // what $(whoami)@$(hostname) resolves to in the pane (default "ops@remote-box")
	markerOut     string // the resolved remote-identity marker line, set on the marker echo
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
		if strings.Contains(cmd, "FLOWBEE_REMOTE_") && strings.Contains(cmd, "$(whoami)@$(hostname)") {
			// model the shell evaluating `echo <marker>_$(whoami)@$(hostname)`.
			id := f.identity
			if id == "" {
				id = "ops@remote-box"
			}
			f.markerOut = extractFakeMarker(cmd) + "_" + id
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

// extractFakeMarker pulls "FLOWBEE_REMOTE_<nonce>" out of an `echo
// <marker>_$(whoami)@$(hostname)` send-keys command, mirroring the ladder's own marker
// construction (it splits at the first `_$(`, so the added `@$(hostname)` does not matter).
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
		return "ops@buncher:~/dev/russ$"
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
	f := &ladderFake{identity: "ops@remote-box"}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/2026-07-16-frob.md", Checkout: "/home/ops/dev/russ",
		LocalIdentity: "sam@control-plane", RemoteMarkerNonce: "testnonce",
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
	// the ssh attach line was sent to the far box by EXACT name, starting the remote
	// session IN the epic's checkout (-c) so the agent comes up cwd'd into the repo (#9).
	if f.countMatching("ssh -t -- buncher tmux new -A -s epic-frob -c /home/ops/dev/russ") == 0 {
		t.Fatalf("ssh attach line (with -c checkout) missing: %v", f.recorded())
	}
	// the remote-identity confirmation ran and matched an identity != the local one.
	if f.countMatching("echo FLOWBEE_REMOTE_testnonce_$(whoami)@$(hostname)") == 0 {
		t.Fatalf("remote-identity confirmation echo missing: %v", f.recorded())
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
// shell). The remote-identity confirmation resolves $(whoami)@$(hostname) to the control
// plane's OWN user@host == LocalIdentity, so the ladder must REFUSE to type the launch line
// there — LaunchFailed, never LaunchVerified, with rollback.
func TestRunLadder_SSHExitToLocalShell_NotVerified(t *testing.T) {
	f := &ladderFake{identity: "sam@control-plane"} // resolves == LocalIdentity: an ssh drop-back to the local shell
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
		LocalIdentity: "sam@control-plane", RemoteMarkerNonce: "testnonce",
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

// TestRunLadder_SameHostDifferentUser_Verified is the REAL-FLEET topology the hostname-only
// discriminator broke: seats are reached via `ssh <user>@localhost`, so a seat shares the
// control plane's HOSTNAME (Mac-Studio.local) and differs only by USER. Under hostname-only
// comparison the seat's host == the local host and EVERY real seat failed closed; the
// user@host tuple (claude1@Mac-Studio.local != sam@Mac-Studio.local) correctly VERIFIES.
func TestRunLadder_SameHostDifferentUser_Verified(t *testing.T) {
	f := &ladderFake{identity: "claude1@Mac-Studio.local"} // SAME host as the control plane, DIFFERENT user
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/x.md",
		LocalIdentity: "sam@Mac-Studio.local", RemoteMarkerNonce: "testnonce",
	})
	if err != nil {
		t.Fatalf("RunLadder: %v", err)
	}
	if res.Outcome != LaunchVerified || res.Stage != StageDone {
		t.Fatalf("a same-host-different-user seat MUST verify (this was the field bug), got %+v", res)
	}
	// it proceeded past confirmation and typed the CLI launch line onto the seat.
	if f.countMatching("CLAUDE_CONFIG_DIR=") == 0 {
		t.Fatalf("a verified same-host seat must reach the CLI launch stage: %v", f.recorded())
	}
	if f.countMatching("kill-session") != 0 {
		t.Fatalf("a verified launch must not roll back the session")
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
	claude := buildCLILine(LaunchSeat{AgentFamily: "claude", ConfigDir: "/c", Account: "claude:pearl@swh.me", ExtraEnv: map[string]string{"B": "2", "A": "1"}}, "claude", "/home/ops/dev/russ")
	// claude/codex append no CLI flags (cwd comes from the tmux session's start dir).
	if claude != "CLAUDE_CONFIG_DIR=/c FLOWBEE_ACCOUNT=claude:pearl@swh.me A=1 B=2 claude" {
		t.Fatalf("claude CLI line: %q", claude)
	}
	codex := buildCLILine(LaunchSeat{AgentFamily: "codex", CodexHome: "/h"}, "codex", "/home/ops/dev/russ")
	if codex != "CODEX_HOME=/h codex" {
		t.Fatalf("codex CLI line: %q", codex)
	}
	// grok: GROK_HOME from the (reused) ConfigDir field, plus --yolo --cwd <checkout>.
	grok := buildCLILine(LaunchSeat{AgentFamily: "grok", ConfigDir: "/home/gk/.grok", Account: "grok:me@x.ai"}, "grok", "/home/gk/dev/russ")
	if grok != "GROK_HOME=/home/gk/.grok FLOWBEE_ACCOUNT=grok:me@x.ai grok --yolo --cwd /home/gk/dev/russ" {
		t.Fatalf("grok CLI line: %q", grok)
	}
	// grok with no checkout: still --yolo, but no --cwd.
	if g := buildCLILine(LaunchSeat{AgentFamily: "grok", ConfigDir: "/g"}, "grok", ""); g != "GROK_HOME=/g grok --yolo" {
		t.Fatalf("grok CLI line (no checkout): %q", g)
	}
	if got := buildSSHAttachLine("buncher", "epic-frob", ""); got != "ssh -t -- buncher tmux new -A -s epic-frob" {
		t.Fatalf("ssh attach: %q", got)
	}
	if got := buildSSHAttachLine("buncher", "epic-frob", "/home/ops/dev/russ"); got != "ssh -t -- buncher tmux new -A -s epic-frob -c /home/ops/dev/russ" {
		t.Fatalf("ssh attach with checkout: %q", got)
	}
}

// TestBinaryFor pins the per-family launch binary literals.
func TestBinaryFor(t *testing.T) {
	for fam, want := range map[verbs.Family]string{"claude": "claude", "codex": "codex", "grok": "grok"} {
		if got := binaryFor(fam); got != want {
			t.Errorf("binaryFor(%q)=%q want %q", fam, got, want)
		}
	}
}

func TestExtractMarkerIdentity(t *testing.T) {
	cap := "ops@remote-box:~$ echo FLOWBEE_REMOTE_n1_$(whoami)@$(hostname)\nFLOWBEE_REMOTE_n1_ops@remote-box\nops@remote-box:~$"
	id, ok := extractMarkerIdentity(cap, "FLOWBEE_REMOTE_n1")
	if !ok || id != "ops@remote-box" {
		t.Fatalf("extractMarkerIdentity = %q,%v — the command-echo line must be skipped and only the OUTPUT read", id, ok)
	}
	// a capture with ONLY the command echo (no output yet) must not resolve.
	if _, ok := extractMarkerIdentity("$ echo FLOWBEE_REMOTE_n1_$(whoami)@$(hostname)", "FLOWBEE_REMOTE_n1"); ok {
		t.Fatal("the command-echo line alone must not resolve an identity")
	}
}

// TestExtractMarkerIdentity_ReEchoedInput guards against an ssh -t / PS2 setup that
// RE-ECHOES the typed command (so the pane holds the marker in BOTH the echoed input line
// AND the resolved-output line): the extractor must pick the OUTPUT `user@host`, never a
// token off the still-`$(whoami)@$(hostname)` input line.
func TestExtractMarkerIdentity_ReEchoedInput(t *testing.T) {
	// row 1: primary prompt echo of the typed command (has `echo ` and `$(`)
	// row 2: a PS2/bracketed re-echo of the same command (still has `$(`)
	// row 3: the resolved OUTPUT — the only line the extractor may read
	capt := "remote:~$ echo FLOWBEE_REMOTE_z9_$(whoami)@$(hostname)\n" +
		"> FLOWBEE_REMOTE_z9_$(whoami)@$(hostname)\n" +
		"FLOWBEE_REMOTE_z9_claude1@far-box\n" +
		"remote:~$ "
	id, ok := extractMarkerIdentity(capt, "FLOWBEE_REMOTE_z9")
	if !ok {
		t.Fatal("expected the resolved output identity to be found")
	}
	if id != "claude1@far-box" {
		t.Fatalf("extractMarkerIdentity picked %q — it must read the OUTPUT user@host, not a $(whoami)@$(hostname) input token", id)
	}
	if strings.Contains(id, "$(") {
		t.Fatal("extractMarkerIdentity was fooled by the echoed input line")
	}
}

// TestBuildIdentity_EitherEmptyFailsClosed proves the identity builder yields the
// fail-closed empty string when EITHER the user OR the host is unknown — the M3 signal that
// confirmRemoteHost keys on. A control plane that cannot fully name itself must not launch.
func TestBuildIdentity_EitherEmptyFailsClosed(t *testing.T) {
	if id := buildIdentity("", "Mac-Studio.local"); id != "" {
		t.Fatalf("empty USER must fail closed (empty identity), got %q", id)
	}
	if id := buildIdentity("sam", ""); id != "" {
		t.Fatalf("empty HOST must fail closed (empty identity), got %q", id)
	}
	if id := buildIdentity("sam", "Mac-Studio.local"); id != "sam@Mac-Studio.local" {
		t.Fatalf("a full user+host must join into user@host, got %q", id)
	}
}

// TestConfirmRemoteHost_EmptyLocalIdentityFailsClosed is the §15.15 M3 fail-closed guard:
// if the control plane cannot name itself (empty user OR empty host ⇒ LocalIdentity==""),
// the confirmation CANNOT tell the local shell from a remote one, so it must REFUSE to
// confirm arrival — returning NOT-verified with a clear cause and WITHOUT even echoing a
// marker. The full-ladder consequence (rollback + no CLI line typed on a !confirmed result)
// is the same !confirmed → killAndFail path proven by
// TestRunLadder_SSHExitToLocalShell_NotVerified. (This branch is unreachable through
// RunLadder — withDefaults fills LocalIdentity from resolveLocalIdentity() when empty — so
// it is exercised directly here.)
func TestConfirmRemoteHost_EmptyLocalIdentityFailsClosed(t *testing.T) {
	f := &ladderFake{identity: "ops@remote-box"} // a genuine remote — but we can't prove which
	client, clk := newLadderClient(f)
	ok, ev, err := confirmRemoteHost(context.Background(), client, clk, "epic-frob", "n1", "", 5*time.Second, time.Second)
	if err != nil {
		t.Fatalf("unexpected infra error: %v", err)
	}
	if ok {
		t.Fatalf("empty local identity must FAIL CLOSED, got confirmed=true (ev=%q)", ev)
	}
	if !strings.Contains(ev, "local user@host unknown") {
		t.Fatalf("evidence should name the cause, got %q", ev)
	}
	// it must refuse BEFORE echoing any marker (no probe into a possibly-local shell).
	if f.countMatching("FLOWBEE_REMOTE_") != 0 {
		t.Fatalf("fail-closed must refuse before echoing a marker: %v", f.recorded())
	}
}
