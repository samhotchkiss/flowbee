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
	phase         string // shell | prompt | working | auth | limbo
	newSessionErr error
	goalStuck     bool
	shellArrival  string // "" = normal shell; else force this capture during the shell wait
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

func (f *ladderFake) captureFor() string {
	switch f.phase {
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
	f := &ladderFake{}
	client, clk := newLadderClient(f)
	res, err := RunLadder(context.Background(), client, clk, LadderParams{
		Slug: "frob", Seat: remoteSeat(), SpecPath: "epics/2026-07-16-frob.md", Dir: "/home/ops/epics/frob",
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
	// every tmux target the ladder built is exact-match (=epic-frob), never bare.
	for _, c := range f.recorded() {
		if strings.Contains(c, "-t 'epic-frob'") {
			t.Fatalf("a BARE (prefix-matching) target was used: %q", c)
		}
	}
	if f.countMatching("=epic-frob") == 0 {
		t.Fatalf("expected exact-match targets: %v", f.recorded())
	}
	// no rollback kill on a clean launch.
	if f.countMatching("kill-session") != 0 {
		t.Fatalf("a verified launch must not kill the session")
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
	if exactTarget("epic-frob") != "=epic-frob" {
		t.Fatalf("exactTarget")
	}
}
