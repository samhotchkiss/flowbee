package tmuxio

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// ── fakes ──

// fakeRunner records every shell command and delegates to a handler. The handler
// inspects the (fully-formed) command string — matching on substrings like
// "capture-pane" or "send-keys … Enter" — and returns scripted output. This is
// how the whole delivery-verification state machine is exercised without a real
// tmux server (the real-server exercise lives in integration_test.go).
type fakeRunner struct {
	mu     sync.Mutex
	calls  []string
	handle func(cmd string) (string, error)
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, cmd)
	f.mu.Unlock()
	return f.handle(cmd)
}

func (f *fakeRunner) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeRunner) countMatching(sub string) int {
	n := 0
	for _, c := range f.recorded() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

// fakeClock is instant: Sleep records the duration and advances Now, but never
// wall-clock-blocks, so verification tests run at memory speed.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1700000000, 0)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeClock) Sleep(_ context.Context, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept = append(f.slept, d)
	f.t = f.t.Add(d)
}

func newFakeClient(handle func(cmd string) (string, error)) (*Client, *fakeRunner, *fakeClock) {
	r := &fakeRunner{handle: handle}
	k := newFakeClock()
	return New(WithRunner(r), WithClock(k)), r, k
}

// ── discovery ──

func TestListPanes(t *testing.T) {
	rows := []string{
		strings.Join([]string{"russ-codex", "0", "%1", "4242", "node", "213", "58"}, fieldSep),
		strings.Join([]string{"my session", "2", "%7", "9001", "ssh", "80", "24"}, fieldSep), // name with a space
		"garbage-row-no-separators", // must be skipped, not fatal
	}
	c, _, _ := newFakeClient(func(cmd string) (string, error) {
		if strings.Contains(cmd, "list-panes") {
			return strings.Join(rows, "\n") + "\n", nil
		}
		t.Fatalf("unexpected command: %q", cmd)
		return "", nil
	})
	panes, err := c.ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("got %d panes, want 2 (garbage row skipped): %+v", len(panes), panes)
	}
	if panes[0] != (Pane{"russ-codex", 0, "%1", 4242, "node", 213, 58}) {
		t.Errorf("pane0 = %+v", panes[0])
	}
	if panes[1].SessionName != "my session" || panes[1].PaneID != "%7" || panes[1].CurrentCommand != "ssh" {
		t.Errorf("pane1 (space in name) = %+v", panes[1])
	}
}

func TestResolveAgentWalksChildren(t *testing.T) {
	// shell(4242) -> node(4300) -> claude(4350). The agent is the claude grandchild.
	c, _, _ := newFakeClient(func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "pgrep -P 4242"):
			return "4300\n", nil
		case strings.Contains(cmd, "pgrep -P 4300"):
			return "4350\n", nil
		case strings.Contains(cmd, "pgrep -P 4350"):
			return "", errNoMatch{} // pgrep exits 1 -> no children
		case strings.Contains(cmd, "ps -o comm= -p 4300"):
			return "node\n", nil
		case strings.Contains(cmd, "ps -o comm= -p 4350"):
			return "claude\n", nil
		}
		return "", nil
	})
	pane := Pane{PaneID: "%1", PanePID: 4242, CurrentCommand: "zsh"}
	got, ok, err := c.ResolveAgent(context.Background(), pane)
	if err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
	// BFS finds the node child first (it matches agentCommands too) — that is the
	// documented behavior: the FIRST agent-like descendant wins.
	if !ok || got.PID != 4300 || got.Command != "node" {
		t.Fatalf("ResolveAgent = %+v ok=%v, want node pid 4300", got, ok)
	}
}

func TestResolveAgentRemoteSSH(t *testing.T) {
	c, _, _ := newFakeClient(func(cmd string) (string, error) {
		if strings.Contains(cmd, "pgrep -P 4242") {
			return "4400\n", nil
		}
		if strings.Contains(cmd, "ps -o comm= -p 4400") {
			return "ssh\n", nil
		}
		return "", errNoMatch{}
	})
	pane := Pane{PaneID: "%1", PanePID: 4242, CurrentCommand: "ssh"}
	got, ok, err := c.ResolveAgent(context.Background(), pane)
	if err != nil || !ok {
		t.Fatalf("ResolveAgent err=%v ok=%v", err, ok)
	}
	if !got.Remote || got.Command != "ssh" || got.PID != 4400 {
		t.Fatalf("remote agent = %+v, want Remote ssh pid 4400", got)
	}
}

type errNoMatch struct{}

func (errNoMatch) Error() string { return "exit status 1" }

// ── capture / normalize ──

func TestNormalizeAndHashStability(t *testing.T) {
	a := "hello   \n\n\n\nworld\t\n   \n" // trailing ws, blank runs, trailing blanks
	b := "hello\n\nworld"                 // canonical form
	if normalize(a) != b {
		t.Fatalf("normalize(%q) = %q, want %q", a, normalize(a), b)
	}
	// Two captures differing only in cosmetic whitespace hash identically.
	if newCapture(a).Hash != newCapture(b).Hash {
		t.Fatalf("hashes differ for cosmetically-equal captures")
	}
	// Different content hashes differently.
	if newCapture("hello\nworld").Hash == newCapture("hello\nmars").Hash {
		t.Fatalf("distinct content produced identical hash")
	}
}

// ── verification helpers ──

func TestInputLineHoldsExactly(t *testing.T) {
	cases := []struct {
		name    string
		capture string
		msg     string
		want    bool
	}{
		{"exact sitting in box", "out\n❯ run the tests", "run the tests", true},
		{"box cleared", "out\n❯ ", "run the tests", false},
		{"contains but not exact (hint text)", "Goal blocked (/goal resume)\n❯ ", "/goal resume", false},
		{"human-edited input is not exact", "❯ /goal resume && rm -rf x", "/goal resume", false},
		{"multiline never exact", "❯ line two", "line one\nline two", false},
	}
	for _, c := range cases {
		if got := inputLineHoldsExactly(c.capture, c.msg); got != c.want {
			t.Errorf("%s: inputLineHoldsExactly = %v, want %v", c.name, got, c.want)
		}
	}
}

// ── state classification ──

func TestClassify(t *testing.T) {
	cases := []struct {
		name    string
		capture string
		want    State
	}{
		{"claude idle prompt", "some prior output\n\n❯ ", StateIdleAtPrompt},
		{"codex idle prompt", "history\n› Improve documentation in @main.go", StateIdleAtPrompt},
		{"codex goal achieved", "  gpt-5.6 · ~/dev/russ                    Goal achieved (1h 52m)", StateIdleAtPrompt},
		{"claude working spinner", "✻ Cogitating… (12s · ↑ 2.1k tokens)\n❯ ", StateWorking},
		{"esc to interrupt working", "doing stuff (3s · esc to interrupt)\n❯ ", StateWorking},
		{"codex working banner", "• Working (30m 48s • esc to interrupt) · 1 background terminal", StateWorking},
		{"codex pursuing goal", "  gpt-5.6 · ~/dev/russ            Pursuing goal (2d 4h 12m)", StateWorking},
		{"claude permission dialog", "Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell Claude", StateAwaitingInput},
		{"queued messages", "❯ my next thing\nPress up to edit queued messages", StateAwaitingInput},
		{"codex goal blocked", "  gpt-5.6 · ~/dev/russ            Goal blocked (/goal resume)", StateAwaitingInput},
		{"unknown garbage", "asdf qwer zxcv", StateUnknown},
		{"empty", "", StateUnknown},
	}
	for _, c := range cases {
		got, ev := Classify(c.capture)
		if got != c.want {
			t.Errorf("%s: Classify = %q (evidence %q), want %q", c.name, got, ev, c.want)
		}
		if got != StateUnknown && ev == "" {
			t.Errorf("%s: non-Unknown state %q returned empty evidence", c.name, got)
		}
	}
}

// ── send: the crown jewel ──

// sendHandler builds a fake handler for a single-line Send. display gives the
// pane facts; capture is a function of how many Enter events have been sent so far
// (enters), letting a test model a box that clears after N Enters. It also records
// whether copy-mode cancel was requested.
func sendHandler(display string, capture func(enters int) string) (*fakeRunner, func(cmd string) (string, error)) {
	var enters int
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "display-message"):
			return display, nil
		case strings.Contains(cmd, "send-keys") && strings.Contains(cmd, "Enter"):
			enters++
			return "", nil
		case strings.Contains(cmd, "capture-pane"):
			return capture(enters), nil
		default: // set-buffer, paste-buffer, send-keys -X cancel, etc.
			return "", nil
		}
	}
	return fr, fr.handle
}

func runSend(t *testing.T, fr *fakeRunner, target, msg string, opts SendOptions) SendResult {
	t.Helper()
	c := New(WithRunner(fr), WithClock(newFakeClock()))
	res, err := c.Send(context.Background(), target, msg, opts)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return res
}

func TestSendStrongCleanSubmit(t *testing.T) {
	msg := "run the test suite"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(enters int) string {
		if enters >= 1 {
			return "output\n❯ " // cleared after first Enter
		}
		return "output\n❯ " + msg
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Strong || res.Attempts != 1 {
		t.Fatalf("clean submit: got %+v, want Strong/1", res)
	}
}

func TestSendSwallowedEnterThenStrong(t *testing.T) {
	msg := "deploy now"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(enters int) string {
		if enters >= 2 {
			return "output\n❯ " // clears only after the SECOND Enter
		}
		return "output\n❯ " + msg
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Strong || res.Attempts != 2 {
		t.Fatalf("swallowed-once: got %+v, want Strong/2", res)
	}
}

func TestSendFailedPersistentUnsubmitted(t *testing.T) {
	msg := "never lands"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(enters int) string {
		return "output\n❯ " + msg // never clears (a menu swallows every Enter)
	})
	res := runSend(t, fr, "%1", msg, SendOptions{MaxAttempts: 3})
	if res.Verification != Failed || res.Attempts != 3 {
		t.Fatalf("persistent: got %+v, want Failed/3", res)
	}
	if fr.countMatching("send-keys") < 3 {
		t.Errorf("expected >=3 Enter presses, got %d", fr.countMatching("send-keys"))
	}
}

func TestSendWeakOnMenuHazard(t *testing.T) {
	msg := "approve it"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(enters int) string {
		if enters >= 1 {
			// box cleared, but a permission dialog is now up: our text may have gone
			// into the dialog, not the agent.
			return "Do you want to proceed?\n❯ 1. Yes\n  2. No"
		}
		return "output\n❯ " + msg
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("menu hazard: got %+v, want Weak", res)
	}
}

func TestSendWeakWhenPasteNeverLanded(t *testing.T) {
	// The paste never shows up in the input box (e.g. it was swallowed by a menu
	// that has since closed): even though the box reads clear after Enter, we could
	// never confirm the text landed, so the verdict must NOT be a false Strong.
	msg := "did this land?"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(enters int) string {
		return "❯ " // paste never visible, box always clear — no fragment, no hazard
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("paste-never-landed: got %+v, want Weak (honest, not a false Strong)", res)
	}
}

func TestSendCopyModeCancel(t *testing.T) {
	msg := "hello"
	fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"1", func(enters int) string { // in_mode = 1
		if enters >= 1 {
			return "❯ "
		}
		return "❯ " + msg
	})
	_ = runSend(t, fr, "%1", msg, SendOptions{})
	if fr.countMatching("-X cancel") != 1 {
		t.Fatalf("expected exactly one copy-mode cancel, got %d", fr.countMatching("-X cancel"))
	}
}

func TestSendWrappedMultilineWeak(t *testing.T) {
	msg := "first line of a long multiline message\nsecond line here"
	var pasted bool
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "display-message"):
			return "%1" + fieldSep + "80" + fieldSep + "0", nil
		case strings.Contains(cmd, "paste-buffer"):
			pasted = true
			return "", nil
		case strings.Contains(cmd, "capture-pane"):
			if pasted {
				return "the agent started working\n✻ Thinking (2s · esc to interrupt)", nil // changed, no fragment
			}
			return "❯ ", nil // pre-paste snapshot
		default:
			return "", nil
		}
	}
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("wrapped multiline: got %+v, want Weak (honest: exact-match unavailable)", res)
	}
	// Wrapped path must press Enter exactly once — never retry blindly.
	if fr.countMatching("send-keys") != 1 {
		t.Errorf("wrapped path pressed Enter %d times, want exactly 1", fr.countMatching("send-keys"))
	}
}

func TestSendWrappedFailedWhenStuck(t *testing.T) {
	msg := strings.Repeat("x", 500) // long single line -> wrap risk
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "display-message"):
			return "%1" + fieldSep + "80" + fieldSep + "0", nil
		case strings.Contains(cmd, "capture-pane"):
			// Pane never changes and still shows the pasted placeholder -> stuck.
			return "❯ [Pasted text #1 +0 lines]", nil
		default:
			return "", nil
		}
	}
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Failed {
		t.Fatalf("wrapped stuck: got %+v, want Failed", res)
	}
}

func TestSendNoSubmitAndNoVerify(t *testing.T) {
	msg := "hi"
	mk := func() *fakeRunner {
		fr, _ := sendHandler("%1"+fieldSep+"80"+fieldSep+"0", func(int) string { return "❯ " + msg })
		return fr
	}
	nsFR := mk()
	ns := runSend(t, nsFR, "%1", msg, SendOptions{NoSubmit: true})
	if ns.Verification != Weak || ns.Attempts != 0 {
		t.Fatalf("NoSubmit: got %+v, want Weak/0", ns)
	}
	if nsFR.countMatching("send-keys") != 0 {
		t.Errorf("NoSubmit pressed Enter %d times, want 0", nsFR.countMatching("send-keys"))
	}
	nvFR := mk()
	nv := runSend(t, nvFR, "%1", msg, SendOptions{NoVerify: true})
	if nv.Verification != Weak || nv.Attempts != 1 {
		t.Fatalf("NoVerify: got %+v, want Weak/1", nv)
	}
	if nvFR.countMatching("send-keys") != 1 {
		t.Errorf("NoVerify pressed Enter %d times, want 1", nvFR.countMatching("send-keys"))
	}
}

func TestSendEmptyMessageErrors(t *testing.T) {
	c := New(WithRunner(&fakeRunner{handle: func(string) (string, error) { return "", nil }}), WithClock(newFakeClock()))
	if _, err := c.Send(context.Background(), "%1", "\n\n", SendOptions{}); err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestNudge(t *testing.T) {
	// changed -> Weak
	var enters int
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "send-keys") && strings.Contains(cmd, "Enter"):
			enters++
			return "", nil
		case strings.Contains(cmd, "capture-pane"):
			if enters >= 1 {
				return "the turn started\n✻ Working (1s · esc to interrupt)", nil
			}
			return "❯ stuck text", nil
		default:
			return "", nil
		}
	}
	c := New(WithRunner(fr), WithClock(newFakeClock()))
	res, err := c.Nudge(context.Background(), "%1")
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if res.Verification != Weak {
		t.Fatalf("nudge changed: got %+v, want Weak", res)
	}

	// no change -> Failed
	frNo := &fakeRunner{handle: func(cmd string) (string, error) {
		if strings.Contains(cmd, "capture-pane") {
			return "❯ stuck text", nil
		}
		return "", nil
	}}
	c2 := New(WithRunner(frNo), WithClock(newFakeClock()))
	res2, _ := c2.Nudge(context.Background(), "%1")
	if res2.Verification != Failed {
		t.Fatalf("nudge no-change: got %+v, want Failed", res2)
	}
}

// ── lifecycle ──

func TestHasSession(t *testing.T) {
	yes, _, _ := newFakeClient(func(cmd string) (string, error) {
		if strings.Contains(cmd, "has-session") {
			return sessionExistsToken + "\n", nil
		}
		return "", nil
	})
	if ok, err := yes.HasSession(context.Background(), "russ"); err != nil || !ok {
		t.Fatalf("HasSession exists: ok=%v err=%v", ok, err)
	}
	no, _, _ := newFakeClient(func(cmd string) (string, error) {
		if strings.Contains(cmd, "has-session") {
			return sessionMissingToken + "\n", nil
		}
		return "", nil
	})
	if ok, err := no.HasSession(context.Background(), "russ"); err != nil || ok {
		t.Fatalf("HasSession missing: ok=%v err=%v", ok, err)
	}
}

func TestNewSessionCommandConstruction(t *testing.T) {
	// Local session.
	local, lr, _ := newFakeClient(func(string) (string, error) { return "", nil })
	if err := local.NewSession(context.Background(), SessionSpec{Name: "epic-x", Command: "codex", StartDir: "/tmp/wt"}); err != nil {
		t.Fatalf("NewSession local: %v", err)
	}
	call := lr.recorded()[0]
	for _, want := range []string{"new-session -d -s 'epic-x'", "-c '/tmp/wt'", "'codex'"} {
		if !strings.Contains(call, want) {
			t.Errorf("local new-session missing %q in: %s", want, call)
		}
	}

	// Pane-level ssh wrap (RemoteHost): the pane runs interactive ssh.
	remote, rr, _ := newFakeClient(func(string) (string, error) { return "", nil })
	if err := remote.NewSession(context.Background(), SessionSpec{Name: "epic-y", Command: "claude", RemoteHost: "box-7"}); err != nil {
		t.Fatalf("NewSession remote: %v", err)
	}
	// The pane command is shQuote'd as a whole when handed to tmux, so its inner
	// quotes appear escaped ('\'') — assert on the structural tokens, which survive.
	rcall := rr.recorded()[0]
	for _, want := range []string{"ssh -tt --", "box-7", "claude"} {
		if !strings.Contains(rcall, want) {
			t.Errorf("remote-host new-session missing %q in: %s", want, rcall)
		}
	}
}

func TestWithHostWrapsEveryCommandInSSH(t *testing.T) {
	r := &fakeRunner{handle: func(string) (string, error) { return "", nil }}
	c := New(WithRunner(r), WithClock(newFakeClock()), WithHost("far-box"), WithSocket("agents"))
	_, _ = c.ListPanes(context.Background())
	call := r.recorded()[0]
	if !strings.HasPrefix(call, "ssh -o BatchMode=yes -o ConnectTimeout=5 -- 'far-box' ") {
		t.Errorf("WithHost should ssh-wrap with BatchMode/ConnectTimeout/-- guard; got: %s", call)
	}
	for _, want := range []string{"tmux -L ", "agents", "list-panes"} {
		if !strings.Contains(call, want) {
			t.Errorf("WithHost command missing %q in: %s", want, call)
		}
	}
}

// ── quoting ──

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"simple":       "'simple'",
		"with space":   "'with space'",
		"it's tricky":  `'it'\''s tricky'`,
		"a; rm -rf /":  "'a; rm -rf /'",
		"$(evil)`x`":   "'$(evil)`x`'",
		"newline\nend": "'newline\nend'",
	}
	for in, want := range cases {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteWrapGuardsHost(t *testing.T) {
	if got := remoteWrap("", "tmux ls"); got != "tmux ls" {
		t.Errorf("local remoteWrap should pass through, got %q", got)
	}
	// A leading-dash host must be behind `--` so ssh never reads it as an option.
	got := remoteWrap("-oProxyCommand=evil", "tmux ls")
	if !strings.Contains(got, " -- '-oProxyCommand=evil' ") {
		t.Errorf("remoteWrap must place `--` before the host: %s", got)
	}
}
