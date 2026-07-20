package watchdog

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestEpicWorktreePathDerivation: the per-epic worktree path is DISTINCT per slug (two
// epics on one box never collide), lives OUTSIDE the shared base checkout, and the base
// path is the Phase-6 <home>/dev/<repo> convention.
func TestEpicWorktreePathDerivation(t *testing.T) {
	base := EpicBasePath("/home/ops", "russ")
	if base != "/home/ops/dev/russ" {
		t.Fatalf("EpicBasePath: got %q", base)
	}
	wtA := EpicWorktreePath("/home/ops", "russ", "2026-07-16-a")
	wtB := EpicWorktreePath("/home/ops", "russ", "2026-07-16-b")
	if wtA == wtB {
		t.Fatalf("two slugs must derive distinct worktree paths, both were %q", wtA)
	}
	if wtA != "/home/ops/dev/.flowbee-wt/russ/2026-07-16-a" {
		t.Fatalf("EpicWorktreePath: got %q", wtA)
	}
	// the worktree must be OUTSIDE the base tree (git worktree cannot nest a worktree
	// inside its own base) — assert it is not a subpath of base.
	if strings.HasPrefix(wtA, base+"/") {
		t.Fatalf("worktree %q must not be nested inside base %q", wtA, base)
	}
}

// TestProvisionEpicWorktree_IssuesAddAndFailsHard: ProvisionEpicWorktree issues exactly
// WorktreeAddCmd, and a Runner error is launch-BLOCKING (returned, never swallowed into a
// base-tree fallback).
func TestProvisionEpicWorktree_IssuesAddAndFailsHard(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil) // worktree add: OK
	base := "/home/ops/dev/russ"
	wt := "/home/ops/dev/.flowbee-wt/russ/slug"
	if err := ProvisionEpicWorktree(context.Background(), r, "buncher", base, wt, "main"); err != nil {
		t.Fatalf("ProvisionEpicWorktree: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != WorktreeAddCmd("buncher", base, wt, "main") {
		t.Fatalf("expected exactly the worktree-add command, got %v", r.calls)
	}

	// a failed worktree add is a HARD error — no fallback.
	rf := &queuedRunner{}
	rf.push("", errors.New("fatal: 'origin/main' is not a commit"))
	if err := ProvisionEpicWorktree(context.Background(), rf, "buncher", base, wt, "main"); err == nil {
		t.Fatal("a failed worktree add must be launch-blocking (returned as an error)")
	}
}

// TestProvisionEpicWorktreeConcurrentOnOneHost executes the real git commands that
// back two simultaneous launches sharing one host/repo. Both epics must receive their
// own valid worktree; a shared fetch or worktree-admin lock must never collapse host
// throughput back to one launch at a time by failing one contender.
func TestProvisionEpicWorktreeConcurrentOnOneHost(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	base := filepath.Join(root, "base")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	runGit("init", "--bare", origin)
	runGit("init", "-b", "main", seed)
	runGit("-C", seed, "config", "user.email", "flowbee@example.invalid")
	runGit("-C", seed, "config", "user.name", "Flowbee Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit("-C", seed, "add", "README.md")
	runGit("-C", seed, "commit", "-m", "seed")
	runGit("-C", seed, "remote", "add", "origin", origin)
	runGit("-C", seed, "push", "-u", "origin", "main")
	runGit("--git-dir", origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGit("clone", "--quiet", origin, base)

	worktrees := []string{filepath.Join(root, "worktrees", "epic-a"), filepath.Join(root, "worktrees", "epic-b")}
	start := make(chan struct{})
	errs := make(chan error, len(worktrees))
	var wg sync.WaitGroup
	for _, worktree := range worktrees {
		worktree := worktree
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- ProvisionEpicWorktree(context.Background(), ShellRunner{Timeout: 20 * time.Second}, "", base, worktree, "main")
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("simultaneous worktree provisioning failed: %v", err)
		}
	}
	for _, worktree := range worktrees {
		if _, err := os.Stat(filepath.Join(worktree, "README.md")); err != nil {
			t.Fatalf("worktree %s is not usable: %v", worktree, err)
		}
	}
}

// TestRemoveEpicWorktree_IssuesRemove: RemoveEpicWorktree issues exactly WorktreeRemoveCmd —
// the command the rollback + abandon cleanup paths depend on.
func TestRemoveEpicWorktree_IssuesRemove(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	base := "/home/ops/dev/russ"
	wt := "/home/ops/dev/.flowbee-wt/russ/slug"
	if err := RemoveEpicWorktree(context.Background(), r, "buncher", base, wt); err != nil {
		t.Fatalf("RemoveEpicWorktree: %v", err)
	}
	if len(r.calls) != 1 || r.calls[0] != WorktreeRemoveCmd("buncher", base, wt) {
		t.Fatalf("expected exactly the worktree-remove command, got %v", r.calls)
	}
}

func TestPreflight_HappyPath_ExistingCheckout(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)           // gh auth status: OK
	r.push("20971520\n", nil) // df: 20G free
	r.push("yes\n", nil)      // checkout already exists

	res, err := Preflight(context.Background(), r, PreflightParams{
		Box: "buncher", CheckoutPath: "/home/ops/dev/russ", DiskProbePath: "/home/ops",
		OwnerRepo: "acme/russ",
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
	// M1: df must target the EXISTING probe path (home), never the checkout —
	// which may not exist yet on a first launch (df against a missing path emits
	// nothing → parsed as 0 free → a misleading refusal of every first launch).
	if r.calls[1] != DiskFreeKBCmd("buncher", "/home/ops") {
		t.Fatalf("disk probe targets %q, want the probe path (home): %q", r.calls[1], DiskFreeKBCmd("buncher", "/home/ops"))
	}
}

func TestPreflight_ClonesWhenMissing(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)
	r.push("20971520\n", nil)
	r.push("no\n", nil) // checkout absent
	r.push("", nil)     // gh repo clone succeeds

	res, err := Preflight(context.Background(), r, PreflightParams{
		Box: "buncher", CheckoutPath: "/home/ops/dev/russ", DiskProbePath: "/home/ops",
		OwnerRepo: "acme/russ",
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !res.ClonedFresh {
		t.Fatalf("expected ClonedFresh=true, got %+v", res)
	}
	if res.DiskFreeKB != 20971520 {
		t.Fatalf("M1 regression: the fresh-box (not-yet-cloned) launch must still read real free space from the probe path, got %d", res.DiskFreeKB)
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls, got %d: %v", len(r.calls), r.calls)
	}
	if r.calls[3] != CloneRepoCmd("buncher", "acme/russ", "/home/ops/dev/russ") {
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

// TestLaunchEpicSession_HappyPath_PursuingOnFirstCheck asserts the EXACT
// send-keys sequence when the goal submits cleanly: new-session, send-goal, one
// verify capture whose pane parses as pursuing — verified with NO extra Enter and
// NO second capture.
func TestLaunchEpicSession_HappyPath_PursuingOnFirstCheck(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil) // new-session
	r.push("", nil) // send-keys goal
	// verify capture: the agent is off — the status line parses as pursuing.
	r.push("transcript...\n  gpt-5.6 high · ~/dev/russ · Main [default]   Pursuing goal (1m 2s)", nil)

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "buncher", TmuxName: "epic-frob", Dir: "/home/ops/dev/russ",
		StartCmd: "codex", Goal: "/goal execute the epic at epics/x.md",
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if !verified {
		t.Fatalf("expected verified=true")
	}
	want := []string{
		NewTmuxSessionCmd("buncher", "epic-frob", "/home/ops/dev/russ", "codex"),
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

// TestLaunchEpicSession_SecondCheckRescuesSlowTUI: the first verify capture shows
// neither signal (TUI still booting/rendering), the bounded second check (review
// m5) then parses as working — verified via two captures, no Enter ever sent.
func TestLaunchEpicSession_SecondCheckRescuesSlowTUI(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil)                                  // new-session
	r.push("", nil)                                  // send-keys goal
	r.push("still booting, blank-ish", nil)          // verify capture #1: no signal
	r.push("• Working (5s • esc to interrupt)", nil) // verify capture #2: working

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "", TmuxName: "epic-x", Dir: "/x", StartCmd: "codex", Goal: "/goal g",
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if !verified {
		t.Fatalf("expected verified=true after the second check")
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls (new-session, send-goal, capture, capture), got %d: %v", len(r.calls), r.calls)
	}
	if r.calls[3] != capturePaneCmd("", "epic-x") {
		t.Errorf("call[3] = %q, want a second verify capture (never an Enter)", r.calls[3])
	}
}

// TestLaunchEpicSession_NoSignalEitherCheck_NotVerified: neither capture shows
// the unsubmitted line nor a pursuing/working parse (the documented wrapped-goal
// blind spot, or a genuinely wedged TUI) — the launcher must return
// verified=FALSE (operator gets warned) and send NO keystroke, never a false
// verified=true (the pre-m5 bug: an epic marked running while its agent idled at
// an unsubmitted prompt).
func TestLaunchEpicSession_NoSignalEitherCheck_NotVerified(t *testing.T) {
	r := &queuedRunner{}
	r.push("", nil) // new-session
	r.push("", nil) // send-keys goal
	// a wrapped goal line: the tail fragment is the last line, so the exact-match
	// unsubmitted check cannot fire, and nothing parses as a known state.
	r.push("› /goal execute the epic at epics/x.md per\nepics/INSTRUCTIONS.md. Work on branch epic/x.", nil)
	r.push("› /goal execute the epic at epics/x.md per\nepics/INSTRUCTIONS.md. Work on branch epic/x.", nil)

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "", TmuxName: "epic-x", Dir: "/x", StartCmd: "codex",
		Goal: "/goal execute the epic at epics/x.md per epics/INSTRUCTIONS.md. Work on branch epic/x.",
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if verified {
		t.Fatalf("expected verified=FALSE when neither check shows a signal (wrapped-line blind spot)")
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls (no Enter ever sent on ambiguity), got %d: %v", len(r.calls), r.calls)
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
// human's edited input with extra trailing text) must NOT trigger a bare Enter —
// even across BOTH verify passes.
func TestLaunchEpicSession_UnsubmittedCheckIsExactNotSubstring(t *testing.T) {
	goal := "/goal execute the epic at epics/x.md"
	r := &queuedRunner{}
	r.push("", nil)
	r.push("", nil)
	r.push("echo of: "+goal+" (already ran)", nil) // capture #1: substring, not exact
	r.push("echo of: "+goal+" (already ran)", nil) // capture #2: same

	verified, err := LaunchEpicSession(context.Background(), r, LaunchParams{
		Box: "", TmuxName: "epic-x", Dir: "/x", StartCmd: "codex", Goal: goal,
	})
	if err != nil {
		t.Fatalf("LaunchEpicSession: %v", err)
	}
	if verified {
		t.Fatalf("expected verified=FALSE: a substring echo is not positive submission evidence")
	}
	if len(r.calls) != 4 {
		t.Fatalf("expected 4 calls and NO bare-Enter on a substring match, got %d: %v", len(r.calls), r.calls)
	}
	for _, c := range r.calls {
		if c == sendEnterCmd("", "epic-x") {
			t.Fatalf("a bare Enter was sent under a non-exact match: %v", r.calls)
		}
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
