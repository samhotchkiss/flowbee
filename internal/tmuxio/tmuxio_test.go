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
// "capture-pane" or a trailing " Enter" — and returns scripted output. This is how
// the whole delivery-verification state machine is exercised without a real tmux
// server (the real-server exercise lives in integration_test.go).
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

// countEnter counts the bare-Enter key events (a send-keys command ending in
// " Enter"), distinct from a text delivery via `send-keys -l -- '<msg>'`.
func (f *fakeRunner) countEnter() int {
	n := 0
	for _, c := range f.recorded() {
		if strings.HasSuffix(strings.TrimSpace(c), " Enter") {
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

// boxCapture renders Claude Code's bordered input box with interior as the text
// SITTING in the box — crucially, the last non-empty line is a "? for shortcuts"
// hint, NOT the input. This is the M1 layout the old last-line matcher got wrong.
func boxCapture(interior string) string {
	return "some prior output\n" +
		"╭──────────────────────────╮\n" +
		"│ > " + interior + " │\n" +
		"╰──────────────────────────╯\n" +
		"  ? for shortcuts"
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
	// shell(4242) -> node(4300) -> claude(4350). BFS finds the node child first.
	c, _, _ := newFakeClient(func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "pgrep -P 4242"):
			return "4300\n", nil
		case strings.Contains(cmd, "pgrep -P 4300"):
			return "4350\n", nil
		case strings.Contains(cmd, "ps -o comm= -p 4300"):
			return "node\n", nil
		case strings.Contains(cmd, "ps -o comm= -p 4350"):
			return "claude\n", nil
		}
		return "", errNoMatch{}
	})
	pane := Pane{PaneID: "%1", PanePID: 4242, CurrentCommand: "zsh"}
	got, ok, err := c.ResolveAgent(context.Background(), pane)
	if err != nil {
		t.Fatalf("ResolveAgent: %v", err)
	}
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
	if newCapture(a).Hash != newCapture(b).Hash {
		t.Fatalf("hashes differ for cosmetically-equal captures")
	}
	if newCapture("hello\nworld").Hash == newCapture("hello\nmars").Hash {
		t.Fatalf("distinct content produced identical hash")
	}
}

// ── input-line location (the M1/M2 core) ──

func TestExtractInputLine(t *testing.T) {
	cases := []struct {
		name        string
		capture     string
		wantText    string
		wantLocated bool
	}{
		{"bordered box with message", boxCapture("run the tests"), "run the tests", true},
		{"bordered box empty", boxCapture(""), "", true},
		{"bare prompt with message", "history\n❯ hello world", "hello world", true},
		{"bare prompt empty", "history\n❯", "", true},
		{"codex prompt", "output\n› do the thing", "do the thing", true},
		// M2: a message that itself begins with a prompt glyph must survive — only the
		// single matched prompt PREFIX is stripped, not a greedy glyph-class trim.
		{"glyph-leading message on bare prompt", "out\n❯ > deploy to prod", "> deploy to prod", true},
		{"glyph-leading message in box", boxCapture("> deploy to prod"), "> deploy to prod", true},
		{"no prompt at all", "just some\nplain output", "", false},
	}
	for _, c := range cases {
		text, ok := extractInputLine(c.capture)
		if ok != c.wantLocated || text != c.wantText {
			t.Errorf("%s: extractInputLine = (%q, %v), want (%q, %v)", c.name, text, ok, c.wantText, c.wantLocated)
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
		{"claude idle bare prompt", "some prior output\n\n❯ ", StateIdleAtPrompt},
		{"claude idle bordered box", boxCapture(""), StateIdleAtPrompt},
		{"codex idle prompt", "history\n› Improve documentation in @main.go", StateIdleAtPrompt},
		{"codex goal achieved", "  gpt-5.6 · ~/dev/russ                    Goal achieved (1h 52m)", StateIdleAtPrompt},
		{"claude working spinner", "✻ Cogitating… (12s · ↑ 2.1k tokens)\n❯ ", StateWorking},
		{"esc to interrupt working", "doing stuff (3s · esc to interrupt)\n❯ ", StateWorking},
		{"codex working banner", "• Working (30m 48s • esc to interrupt) · 1 background terminal", StateWorking},
		{"codex pursuing goal", "  gpt-5.6 · ~/dev/russ            Pursuing goal (2d 4h 12m)", StateWorking},
		{"claude permission dialog", "Do you want to proceed?\n❯ 1. Yes\n  2. No, and tell Claude", StateAwaitingInput},
		{"queued messages", "❯ my next thing\nPress up to edit queued messages", StateAwaitingInput},
		// m8: goal blocked/paused is its OWN state, NOT a keystroke-capturing menu.
		{"codex goal blocked", "  gpt-5.6 · ~/dev/russ            Goal blocked (/goal resume)", StateGoalBlocked},
		{"codex goal paused", "  gpt-5.6 · ~/dev/russ            Goal paused (/goal resume)", StateGoalBlocked},
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

// m9: transcript text the agent merely PRINTED far above the input box must not
// masquerade as the current state.
func TestClassifyIgnoresPrintedTranscript(t *testing.T) {
	// A long transcript that once quoted a permission prompt, now sitting idle.
	var b strings.Builder
	b.WriteString("assistant: earlier I asked 'Do you want to proceed?' and you said yes\n")
	for i := 0; i < 30; i++ {
		b.WriteString("... build log line ...\n")
	}
	b.WriteString("❯ ")
	if st, _ := Classify(b.String()); st != StateIdleAtPrompt {
		t.Fatalf("printed 'Do you want to' far up misclassified as %q, want idle", st)
	}
}

// m8: a goal-blocked pane must NOT read as a menu hazard (so a resume send is not
// wrongly capped at Weak).
func TestGoalBlockedIsNotMenuHazard(t *testing.T) {
	if isMenuHazard("  gpt-5.6 · ~/dev/russ            Goal blocked (/goal resume)") {
		t.Fatal("Goal blocked status hint should not be a menu hazard")
	}
}

// ── display width (m4) ──

func TestDisplayWidthWideRunes(t *testing.T) {
	if displayWidth("abc") != 3 {
		t.Errorf("ascii width = %d, want 3", displayWidth("abc"))
	}
	if w := displayWidth("日本語"); w != 6 { // 3 wide CJK runes
		t.Errorf("CJK width = %d, want 6", w)
	}
	if w := displayWidth("🚀🚀"); w != 4 { // 2 emoji, 2 cols each
		t.Errorf("emoji width = %d, want 4", w)
	}
	// The bug m4 flags: rune count would call a 50-emoji line "narrow" on an 80-col
	// pane; display width (100) correctly makes it a wrap risk.
	fifty := strings.Repeat("🚀", 50)
	if !isWrapRisk(fifty, 80) {
		t.Errorf("50 emoji on 80 cols should be a wrap risk (display width %d)", displayWidth(fifty))
	}
}

// ── send: the crown jewel ──

// sendHandler builds a fake handler for a Send. display gives the pane facts;
// capture is a function of how many bare-Enter events have been sent so far
// (enters), letting a test model a box that clears after N Enters.
func sendHandler(display string, capture func(enters int) string) *fakeRunner {
	var enters int
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "display-message"):
			return display, nil
		case strings.HasSuffix(strings.TrimSpace(cmd), " Enter"):
			enters++
			return "", nil
		case strings.Contains(cmd, "capture-pane"):
			return capture(enters), nil
		default: // set-buffer, paste-buffer, send-keys -l -- <msg>, -X cancel
			return "", nil
		}
	}
	return fr
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

const paneFacts80 = "%1" + fieldSep + "80" + fieldSep + "0"

func TestSendStrongCleanSubmit(t *testing.T) {
	msg := "run the test suite"
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 1 {
			return "output\n❯ "
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
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 2 {
			return "output\n❯ "
		}
		return "output\n❯ " + msg
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Strong || res.Attempts != 2 {
		t.Fatalf("swallowed-once: got %+v, want Strong/2", res)
	}
}

// M1/M3: the bordered-box layout. Swallowed Enter must be FAILED (retries engage),
// never a false Strong.
func TestSendBorderedBoxSwallowedEnterFailed(t *testing.T) {
	msg := "run the tests"
	fr := sendHandler(paneFacts80, func(enters int) string {
		return boxCapture(msg) // box NEVER clears — a menu swallows every Enter
	})
	res := runSend(t, fr, "%1", msg, SendOptions{MaxAttempts: 3})
	if res.Verification != Failed || res.Attempts != 3 {
		t.Fatalf("bordered-box swallowed: got %+v, want Failed/3 (old code false-Strong'd at attempt 1)", res)
	}
	if fr.countEnter() != 3 {
		t.Fatalf("retries did not engage: %d Enter presses, want 3", fr.countEnter())
	}
}

func TestSendBorderedBoxCleanSubmit(t *testing.T) {
	msg := "run the tests"
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 1 {
			return boxCapture("") // box cleared
		}
		return boxCapture(msg)
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Strong || res.Attempts != 1 {
		t.Fatalf("bordered-box clean submit: got %+v, want Strong/1", res)
	}
}

// M2/M3: a message that begins with a prompt glyph exact-matches correctly (drives
// the retry), instead of the old greedy strip false-clearing at attempt 1.
func TestSendGlyphLeadingMessage(t *testing.T) {
	msg := "> deploy to prod"
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 2 {
			return "out\n❯ "
		}
		return "out\n❯ " + msg // still sitting: "❯ > deploy to prod"
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Strong || res.Attempts != 2 {
		t.Fatalf("glyph-leading: got %+v, want Strong/2 (proves exact-match engaged the retry)", res)
	}
}

func TestSendFailedPersistentUnsubmitted(t *testing.T) {
	msg := "never lands"
	fr := sendHandler(paneFacts80, func(enters int) string {
		return "output\n❯ " + msg // never clears
	})
	res := runSend(t, fr, "%1", msg, SendOptions{MaxAttempts: 3})
	if res.Verification != Failed || res.Attempts != 3 {
		t.Fatalf("persistent: got %+v, want Failed/3", res)
	}
	if fr.countEnter() != 3 {
		t.Errorf("expected 3 Enter presses, got %d", fr.countEnter())
	}
}

func TestSendWeakOnMenuHazard(t *testing.T) {
	msg := "approve it"
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 1 {
			return "Do you want to proceed?\n❯ 1. Yes\n  2. No"
		}
		return "output\n❯ " + msg
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("menu hazard: got %+v, want Weak", res)
	}
}

func TestSendWeakWhenTextNeverLanded(t *testing.T) {
	// The text never shows up in the box (swallowed by a menu that has since
	// closed): the box reads empty after Enter, but we could never confirm it
	// landed, so the verdict must NOT be a false Strong.
	msg := "did this land?"
	fr := sendHandler(paneFacts80, func(enters int) string {
		return "output\n❯ " // box always empty, no fragment, no hazard
	})
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("text-never-landed: got %+v, want Weak (honest, not a false Strong)", res)
	}
}

func TestSendCopyModeCancel(t *testing.T) {
	msg := "hello"
	fr := sendHandler("%1"+fieldSep+"80"+fieldSep+"1", func(enters int) string { // in_mode = 1
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
			return paneFacts80, nil
		case strings.Contains(cmd, "paste-buffer"):
			pasted = true
			return "", nil
		case strings.Contains(cmd, "capture-pane"):
			if pasted {
				return "the agent started working\n✻ Thinking (2s · esc to interrupt)", nil
			}
			return "❯ ", nil
		default:
			return "", nil
		}
	}
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Weak {
		t.Fatalf("wrapped multiline: got %+v, want Weak (honest: exact-match unavailable)", res)
	}
	if fr.countEnter() != 1 {
		t.Errorf("wrapped path pressed Enter %d times, want exactly 1", fr.countEnter())
	}
	// A multiline message must be delivered via paste, not literal keys.
	if fr.countMatching("paste-buffer") != 1 {
		t.Errorf("multiline should deliver via paste-buffer; count=%d", fr.countMatching("paste-buffer"))
	}
}

// M3: a codex "[Pasted text #1 +N lines]" placeholder still sitting in the input
// region means the paste is unsubmitted.
func TestSendWrappedCodexPlaceholderFailed(t *testing.T) {
	msg := "line one\nline two\nline three" // multiline -> wrapped path, paste delivery
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.Contains(cmd, "display-message"):
			return paneFacts80, nil
		case strings.Contains(cmd, "capture-pane"):
			// Pane never changes and shows the placeholder still in the box.
			return "❯ [Pasted text #1 +2 lines]", nil
		default:
			return "", nil
		}
	}
	res := runSend(t, fr, "%1", msg, SendOptions{})
	if res.Verification != Failed {
		t.Fatalf("codex placeholder stuck: got %+v, want Failed", res)
	}
}

func TestSendShortSingleLineDeliversViaKeys(t *testing.T) {
	msg := "short line"
	fr := sendHandler(paneFacts80, func(enters int) string {
		if enters >= 1 {
			return "❯ "
		}
		return "❯ " + msg
	})
	_ = runSend(t, fr, "%1", msg, SendOptions{})
	if fr.countMatching(" -l -- ") != 1 {
		t.Errorf("short single line should deliver via send-keys -l --; count=%d", fr.countMatching(" -l -- "))
	}
	if fr.countMatching("paste-buffer") != 0 {
		t.Errorf("short single line should NOT paste; paste count=%d", fr.countMatching("paste-buffer"))
	}
}

func TestSendNoSubmitAndNoVerify(t *testing.T) {
	msg := "hi there"
	mk := func() *fakeRunner {
		return sendHandler(paneFacts80, func(int) string { return "❯ " + msg })
	}
	nsFR := mk()
	ns := runSend(t, nsFR, "%1", msg, SendOptions{NoSubmit: true})
	if ns.Verification != Weak || ns.Attempts != 0 {
		t.Fatalf("NoSubmit: got %+v, want Weak/0", ns)
	}
	if nsFR.countEnter() != 0 {
		t.Errorf("NoSubmit pressed Enter %d times, want 0", nsFR.countEnter())
	}
	nvFR := mk()
	nv := runSend(t, nvFR, "%1", msg, SendOptions{NoVerify: true})
	if nv.Verification != Weak || nv.Attempts != 1 {
		t.Fatalf("NoVerify: got %+v, want Weak/1", nv)
	}
	if nvFR.countEnter() != 1 {
		t.Errorf("NoVerify pressed Enter %d times, want 1", nvFR.countEnter())
	}
}

func TestSendEmptyMessageErrors(t *testing.T) {
	c := New(WithRunner(&fakeRunner{handle: func(string) (string, error) { return "", nil }}), WithClock(newFakeClock()))
	if _, err := c.Send(context.Background(), "%1", "\n\n", SendOptions{}); err == nil {
		t.Fatal("expected error for empty message")
	}
}

func TestSendRejectsNulAndOversize(t *testing.T) {
	c := New(WithRunner(&fakeRunner{handle: func(string) (string, error) { return "", nil }}), WithClock(newFakeClock()))
	if _, err := c.Send(context.Background(), "%1", "bad\x00byte", SendOptions{}); err == nil {
		t.Error("expected error for NUL in message")
	}
	big := strings.Repeat("x", maxMessageBytes+1)
	if _, err := c.Send(context.Background(), "%1", big, SendOptions{}); err == nil {
		t.Error("expected error for oversize message")
	}
}

func TestNudge(t *testing.T) {
	var enters int
	fr := &fakeRunner{}
	fr.handle = func(cmd string) (string, error) {
		switch {
		case strings.HasSuffix(strings.TrimSpace(cmd), " Enter"):
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

// ── identifier validation (m12) ──

func TestValidateIdentRejectsBadNames(t *testing.T) {
	c := New(WithRunner(&fakeRunner{handle: func(string) (string, error) { return "", nil }}), WithClock(newFakeClock()))
	bad := []string{"", "-rf", "-oProxyCommand=x", "has\x00nul", "ctrl\x1fchar"}
	for _, name := range bad {
		if _, err := c.Send(context.Background(), name, "hi", SendOptions{}); err == nil {
			t.Errorf("Send accepted invalid target %q", name)
		}
		if err := c.NewSession(context.Background(), SessionSpec{Name: name, Command: "codex"}); err == nil {
			t.Errorf("NewSession accepted invalid name %q", name)
		}
	}
	// A leading-dash HOST is caught at the first op.
	badHost := New(WithRunner(&fakeRunner{handle: func(string) (string, error) { return "", nil }}), WithClock(newFakeClock()), WithHost("-oProxyCommand=x"))
	if _, err := badHost.ListPanes(context.Background()); err == nil {
		t.Error("ListPanes accepted a leading-dash host")
	}
	// A valid name with an interior space is fine (tmux session names allow it).
	if err := assertValidIdent("session name", "my session"); err != nil {
		t.Errorf("interior space should be valid: %v", err)
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
	got := remoteWrap("-oProxyCommand=evil", "tmux ls")
	if !strings.Contains(got, " -- '-oProxyCommand=evil' ") {
		t.Errorf("remoteWrap must place `--` before the host: %s", got)
	}
}

// TestExactTarget: the wrong-target fix. A bare session name gains a `=` (forcing
// exact match so `-t flowbee` can never prefix-match `flowbee-claude`); a compound
// session:win.pane target gains the `=` on its session component; and the target
// forms that are already unambiguous — pane/window/session ids and an
// already-`=`'d target — are left untouched (a `=` would corrupt them). The empty
// input is the error path: it is returned unchanged, NEVER fabricated into a bare
// "=" catch-anything target.
func TestExactTarget(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare session", "flowbee", "=flowbee:"},
		{"session that is a prefix of another", "epic-fix", "=epic-fix:"},
		{"session with interior space", "my session", "=my session:"},
		{"session:window.pane", "flowbee:0.1", "=flowbee:0.1"},
		{"session:window", "flowbee:2", "=flowbee:2"},
		{"pane-id", "%5", "%5"},
		{"window-id", "@3", "@3"},
		{"session-id", "$2", "$2"},
		{"already =-prefixed", "=flowbee:", "=flowbee:"},
		{"already =-prefixed compound", "=flowbee:0.1", "=flowbee:0.1"},
		{"empty (error path — never a bare =)", "", ""},
	}
	for _, c := range cases {
		if got := exactTarget(c.in); got != c.want {
			t.Errorf("%s: exactTarget(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}
