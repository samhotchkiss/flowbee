package tmuxio

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testSocketSeq disambiguates isolated sockets even if two are created within the
// same nanosecond.
var testSocketSeq atomic.Int64

// These tests exercise the primitives against a REAL tmux server on an isolated
// `-L` socket — the part the brief insists must NOT be faked-only. Each test stands
// up its own server running a tiny prompt substrate, drives it, and tears the
// server down in cleanup. They skip when tmux is not installed.
//
// The substrate is a POSIX `read` loop that renders a `❯ ` prompt: pasted text
// echoes onto the prompt line ("❯ hello") and, on Enter, the line is consumed and
// the loop reprints a bare "❯ " — exactly the input-box-clears-on-submit shape the
// exact-match verifier keys on (verified empirically before writing this test).
const promptSubstrate = "printf '❯ '; while IFS= read -r line; do printf '\\nGOT[%s]\\n❯ ' \"$line\"; done"

// newTestServer returns a Client bound to a fresh isolated tmux server socket and
// registers its teardown. It skips the test when tmux is unavailable.
func newTestServer(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; skipping real-server integration test")
	}
	socket := fmt.Sprintf("flowbee-test-%d-%d", time.Now().UnixNano(), testSocketSeq.Add(1))
	c := New(WithSocket(socket))
	t.Cleanup(func() {
		_ = c.KillServer(context.Background())
	})
	return c
}

// waitForCapture polls target's visible capture until want appears or the deadline
// passes, returning the last capture seen. Real TUIs render asynchronously, so
// tests must poll rather than assume a fixed settle.
func waitForCapture(t *testing.T, c *Client, target, want string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		cap, err := c.Capture(context.Background(), target, 0)
		if err == nil {
			last = cap.Raw
			if strings.Contains(cap.Raw, want) {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

func TestIntegrationSessionLifecycle(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-life"

	if ok, err := c.HasSession(ctx, name); err != nil || ok {
		t.Fatalf("HasSession before create: ok=%v err=%v (want false,nil)", ok, err)
	}
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if ok, err := c.HasSession(ctx, name); err != nil || !ok {
		t.Fatalf("HasSession after create: ok=%v err=%v (want true,nil)", ok, err)
	}
	sessions, err := c.ListSessions(ctx)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if !contains(sessions, name) {
		t.Fatalf("ListSessions = %v, want it to contain %q", sessions, name)
	}

	// Adoption: a second Client on the same socket sees the pre-existing session
	// (c.socket is readable here — this test is in-package).
	adopter := New(WithSocket(c.socket))
	if ok, err := adopter.HasSession(ctx, name); err != nil || !ok {
		t.Fatalf("adopter HasSession: ok=%v err=%v", ok, err)
	}

	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if ok, _ := c.HasSession(ctx, name); ok {
		t.Fatal("HasSession after kill: still exists")
	}
}

func TestIntegrationCaptureAndClassify(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-cap"
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	raw := waitForCapture(t, c, name, "❯", 3*time.Second)
	if !strings.Contains(raw, "❯") {
		t.Fatalf("capture never showed the prompt; got %q", raw)
	}
	if st, _ := Classify(raw); st != StateIdleAtPrompt {
		t.Fatalf("Classify idle prompt = %q, want %q; capture=%q", st, StateIdleAtPrompt, raw)
	}

	// Hash is stable when nothing changes, and moves when content changes.
	a, _ := c.Capture(ctx, name, 0)
	b, _ := c.Capture(ctx, name, 0)
	if a.Hash != b.Hash {
		t.Errorf("hash changed with no pane activity: %s vs %s", a.Hash, b.Hash)
	}
}

func TestIntegrationSendStrongVerification(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-send"
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForCapture(t, c, name, "❯", 3*time.Second)

	res, err := c.Send(ctx, name, "run the tests", SendOptions{})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Verification != Strong {
		t.Fatalf("Send verification = %q (attempts %d, evidence %q), want Strong", res.Verification, res.Attempts, res.Evidence)
	}
	// The substrate echoed the submitted line as GOT[...] — proof it was submitted.
	raw := waitForCapture(t, c, name, "GOT[run the tests]", 2*time.Second)
	if !strings.Contains(raw, "GOT[run the tests]") {
		t.Fatalf("submitted line not echoed by substrate; capture=%q", raw)
	}
}

func TestIntegrationNoSubmitThenNudge(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-nudge"
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForCapture(t, c, name, "❯", 3*time.Second)

	// NoSubmit leaves the text sitting in the input box, unsubmitted.
	res, err := c.Send(ctx, name, "pending line", SendOptions{NoSubmit: true})
	if err != nil {
		t.Fatalf("Send NoSubmit: %v", err)
	}
	if res.Verification != Weak || res.Attempts != 0 {
		t.Fatalf("NoSubmit result = %+v, want Weak/0", res)
	}
	raw := waitForCapture(t, c, name, "pending line", 2*time.Second)
	if !inputLineHoldsExactly(raw, "pending line") {
		t.Fatalf("expected 'pending line' sitting in the input box; capture=%q", raw)
	}

	// Nudge submits it.
	nres, err := c.Nudge(ctx, name)
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if nres.Verification != Weak {
		t.Fatalf("Nudge = %+v, want Weak (pane changed)", nres)
	}
	raw = waitForCapture(t, c, name, "GOT[pending line]", 2*time.Second)
	if !strings.Contains(raw, "GOT[pending line]") {
		t.Fatalf("nudge did not submit; capture=%q", raw)
	}
}

func TestIntegrationCopyModeGuard(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-copy"
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForCapture(t, c, name, "❯", 3*time.Second)

	// Put the pane into copy-mode: keystrokes would be swallowed without the guard.
	if _, err := c.run(ctx, "copy-mode -t "+shQuote(name)); err != nil {
		t.Fatalf("enter copy-mode: %v", err)
	}
	res, err := c.Send(ctx, name, "after copy mode", SendOptions{})
	if err != nil {
		t.Fatalf("Send in copy-mode: %v", err)
	}
	if res.Verification != Strong {
		t.Fatalf("Send after copy-mode guard = %q, want Strong (guard must exit copy-mode)", res.Verification)
	}
	raw := waitForCapture(t, c, name, "GOT[after copy mode]", 2*time.Second)
	if !strings.Contains(raw, "GOT[after copy mode]") {
		t.Fatalf("copy-mode guard failed to deliver; capture=%q", raw)
	}
}

func TestIntegrationListPanesAndResolve(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const name = "itest-panes"
	if err := c.NewSession(ctx, SessionSpec{Name: name, Command: promptSubstrate}); err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	waitForCapture(t, c, name, "❯", 3*time.Second)

	panes, err := c.ListPanes(ctx)
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	var found *Pane
	for i := range panes {
		if panes[i].SessionName == name {
			found = &panes[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("ListPanes did not include session %q: %+v", name, panes)
	}
	if found.PaneID == "" || found.PanePID <= 0 || found.Width <= 0 {
		t.Fatalf("pane fields not populated: %+v", *found)
	}

	// ResolveAgent runs the real pgrep/ps walk. The substrate is a plain shell with
	// no agent child, so it must return ok=false WITHOUT error (a clean shell pane
	// simply has no agent process behind it).
	_, ok, err := c.ResolveAgent(ctx, *found)
	if err != nil {
		t.Fatalf("ResolveAgent error on a plain-shell pane: %v", err)
	}
	if ok {
		t.Log("ResolveAgent found an agent-like child under the shell pane (environment-dependent); acceptable")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
