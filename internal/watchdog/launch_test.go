package watchdog

import (
	"context"
	"errors"
	"testing"
)

// queuedRunner is a minimal fake Runner for the launch/preflight tests: unlike
// watchdog_test.go's fakeRunner (exact-string keyed), commands here vary per-call
// (df/test/clone all differ), so this drives a canned response QUEUE plus records
// every command in order — the assertion surface for "the exact send-keys
// sequence incl. submit-verify".
type queuedRunner struct {
	outs  []string
	errs  []error
	i     int
	calls []string
}

func (r *queuedRunner) Run(_ context.Context, cmd string) (string, error) {
	r.calls = append(r.calls, cmd)
	if r.i >= len(r.outs) {
		return "", nil
	}
	out, err := r.outs[r.i], r.errs[r.i]
	r.i++
	return out, err
}

func (r *queuedRunner) push(out string, err error) {
	r.outs = append(r.outs, out)
	r.errs = append(r.errs, err)
}

func TestPreflight_HappyPath_ExistingCheckout(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)           // gh auth status: OK
	r.push("20971520\n", nil) // df: 20G free
	r.push("yes\n", nil)      // checkout already exists

	res, err := Preflight(context.Background(), r, PreflightParams{
		Box: "buncher", CheckoutPath: "$HOME/epics/russ", OwnerRepo: "acme/russ",
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !res.GhAuthOK || res.DiskFreeKB != 20971520 || res.ClonedFresh {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected 3 calls (auth, disk, exists — no clone), got %d: %v", len(r.calls), r.calls)
	}
}

func TestPreflight_ClonesWhenMissing(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	r.push("20971520\n", nil)
	r.push("no\n", nil) // checkout absent
	r.push("", nil)     // gh repo clone succeeds

	res, err := Preflight(context.Background(), r, PreflightParams{
		Box: "buncher", CheckoutPath: "$HOME/epics/russ", OwnerRepo: "acme/russ",
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !res.ClonedFresh {
		t.Fatalf("expected ClonedFresh=true, got %+v", res)
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %v", len(r.calls), r.calls)
	}
	if r.calls[3] != CloneRepoCmd("buncher", "acme/russ", "$HOME/epics/russ") {
		t.Fatalf("clone command mismatch: %q", r.calls[3])
	}
}

func TestPreflight_GhAuthFailed_StillReportsRestNotFatal(t *testing.T) {
	r := &queuedRunner{}
	r.push("", errors.New("not logged in")) // gh auth status fails
	r.push("20971520\n", nil)
	r.push("yes\n", nil)

	res, err := Preflight(context.Background(), r, PreflightParams{Box: "", CheckoutPath: "/x"})
	if err != nil {
		t.Fatalf("Preflight should not error on a gate condition: %v", err)
	}
	if res.GhAuthOK {
		t.Fatalf("expected GhAuthOK=false")
	}
}

func TestPreflight_LowDiskParsesAsZeroNotFatal(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	r.push("garbage-not-a-number\n", nil)
	r.push("yes\n", nil)

	res, err := Preflight(context.Background(), r, PreflightParams{Box: "", CheckoutPath: "/x"})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if res.DiskFreeKB != 0 {
		t.Fatalf("expected DiskFreeKB=0 on unparseable df output, got %d", res.DiskFreeKB)
	}
}

func TestPreflight_RunnerFailureIsFatal(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	r.push("", errors.New("ssh: connection refused"))
	if _, err := Preflight(context.Background(), r, PreflightParams{Box: "down-box", CheckoutPath: "/x"}); err == nil {
		t.Fatal("expected an error when the disk-check Runner call itself fails")
	}
}

// TestLaunchEpicSession_HappyPath_NoSwallowedEnter asserts the EXACT send-keys
// sequence: new-session, send-goal, capture-to-verify — and that a CLEAN capture
// (the goal already submitted/echoed, not sitting bare in the input line) sends
// NO extra Enter.
func TestLaunchEpicSession_HappyPath_NoSwallowedEnter(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)                                           // new-session
	r.push("", nil)                                           // send-keys goal
	r.push("some other pane content\nnot the goal line", nil) // verify capture

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "buncher", TmuxName: "epic-frob", Dir: "$HOME/epics/russ",
		StartCmd: "codex", Goal: "/goal execute the epic at epics/x.md",
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if !verified {
		t.Fatalf("expected verified=true")
	}
	want := []string{
		NewTmuxSessionCmd("buncher", "epic-frob", "$HOME/epics/russ", "codex"),
		SendGoalCmd("buncher", "epic-frob", "/goal execute the epic at epics/x.md"),
		capturePaneCmd("buncher", "epic-frob"),
	}
	if len(r.calls) != len(want) {
		t.Fatalf("call count = %d, want %d: %v", len(r.calls), len(want), r.calls)
	}
	for i, w := range want {
		if r.calls[i] != w {
			t.Errorf("call[%d] = %q, want %q", i, r.calls[i], w)
		}
	}
}

// TestLaunchEpicSession_SwallowedEnter_SendsBareEnter is the exact scenario the
// design doc calls out: the TUI ate the first Enter, the goal sits unsubmitted —
// the launcher must send exactly one bare Enter to recover, mirroring
// Watcher.autoResume's own verified behavior for "/goal resume".
func TestLaunchEpicSession_SwallowedEnter_SendsBareEnter(t *testing.T) {
	goal := "/goal execute the epic at epics/x.md per epics/INSTRUCTIONS.md. Work on branch epic/x."
	r := &queuedRunner{}
	r.push("", nil)        // new-session
	r.push("", nil)        // send-keys goal
	r.push("› "+goal, nil) // verify capture: unsubmitted, still in the input line
	r.push("", nil)        // bare Enter

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "", TmuxName: "epic-x", Dir: "/x", StartCmd: "codex", Goal: goal,
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if !verified {
		t.Fatalf("expected verified=true even after a swallowed-Enter recovery")
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls (new-session, send-goal, verify-capture, bare-enter), got %d: %v", len(r.calls), r.calls)
	}
	if r.calls[3] != sendEnterCmd("", "epic-x") {
		t.Errorf("call[3] = %q, want the bare-Enter command", r.calls[3])
	}
}

// TestLaunchEpicSession_UnsubmittedCheckIsExactNotSubstring guards against the
// same false-positive classes paneShowsUnsubmittedResume's own doc warns about:
// a pane that merely CONTAINS the goal text (e.g. an echoed transcript line, or a
// human's edited input with extra trailing text) must NOT trigger a bare Enter.
func TestLaunchEpicSession_UnsubmittedCheckIsExactNotSubstring(t *testing.T) {
	goal := "/goal execute the epic at epics/x.md"
	r := &queuedRunner{}
	r.push("", nil)
	r.push("", nil)
	r.push("echo of: "+goal+" (already ran)", nil) // substring, not an exact unsubmitted line

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "", TmuxName: "epic-x", Dir: "/x", StartCmd: "codex", Goal: goal,
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if !verified {
		t.Fatalf("expected verified=true")
	}
	if len(r.calls) != 3 {
		t.Fatalf("expected NO bare-Enter call on a substring match, got %d calls: %v", len(r.calls), r.calls)
	}
}

func TestLaunchEpicSession_SessionCreateFailureIsFatal(t *testing.T) {
	r := &queuedRunner{}
	r.push("", errors.New("tmux: session already exists"))
	if _, err := LaunchEpicSession(context.Background(), r, LaunchParams{TmuxName: "epic-x", StartCmd: "codex", Goal: "g"}); err == nil {
		t.Fatal("expected an error when tmux new-session itself fails")
	}
	if len(r.calls) != 1 {
		t.Fatalf("expected the launch to stop after the failed new-session call, got %d calls", len(r.calls))
	}
}

func TestLaunchEpicSession_GoalSendFailureIsFatal(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	r.push("", errors.New("tmux: no such session"))
	if _, err := LaunchEpicSession(context.Background(), r, LaunchParams{TmuxName: "epic-x", StartCmd: "codex", Goal: "g"}); err == nil {
		t.Fatal("expected an error when send-keys itself fails")
	}
}
