package watchdog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewLaunchRunnerKeepsLargeCloneBudget(t *testing.T) {
	if got := NewLaunchRunner().Timeout; got != 15*time.Minute {
		t.Fatalf("launch timeout = %s, want 15m", got)
	}
}

func installFakeGH(t *testing.T, root string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir fake-gh bin: %v", err)
	}
	script := `#!/bin/sh
set -eu
if test "$#" -lt 4 || test "$1" != repo || test "$2" != clone; then
  echo "unexpected fake gh arguments: $*" >&2
  exit 64
fi
printf '%s\n' "$$" >>"$FLOWBEE_FAKE_GH_LOG"
destination=$4
if test "${FLOWBEE_FAKE_GH_MODE:-success}" = fail; then
  mkdir -p -- "$destination/.git"
  exit 42
fi
sleep "${FLOWBEE_FAKE_GH_DELAY:-0}"
mkdir -p -- "$destination/.git"
printf '%s\n' complete >"$destination/.git/flowbee-test-marker"
`
	path := filepath.Join(bin, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	return bin
}

// TestCloneRepoCmdConcurrentFirstClone executes two real shells against the same
// absent target. Exactly one may invoke gh; the other waits for the host-local
// lock and observes only the atomically published, complete checkout.
func TestCloneRepoCmdConcurrentFirstClone(t *testing.T) {
	root := t.TempDir()
	bin := installFakeGH(t, root)
	logPath := filepath.Join(root, "gh.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FLOWBEE_FAKE_GH_LOG", logPath)
	t.Setenv("FLOWBEE_FAKE_GH_DELAY", "0.25")

	target := filepath.Join(root, "repos", "repo with 'quote")
	cmd := CloneRepoCmd("", "acme/widgets", target)
	runner := ShellRunner{Timeout: 5 * time.Second}
	type result struct {
		out string
		err error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			out, err := runner.Run(context.Background(), cmd)
			results <- result{out: out, err: err}
		}()
	}
	close(start)
	for i := 0; i < 2; i++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("concurrent clone %d failed: %v\n%s", i+1, res.err, res.out)
		}
	}

	logBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake-gh log: %v", err)
	}
	if calls := len(strings.Fields(string(logBytes))); calls != 1 {
		t.Fatalf("gh clone calls = %d, want exactly 1; log=%q", calls, logBytes)
	}
	marker := filepath.Join(target, ".git", "flowbee-test-marker")
	if got, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(got)) != "complete" {
		t.Fatalf("published checkout is not complete: marker=%q err=%v", got, err)
	}
	if leftovers, err := filepath.Glob(target + ".flowbee-clone.*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("clone lock/temp leftovers = %v (glob err=%v)", leftovers, err)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatalf("read checkout: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ".git" {
		t.Fatalf("checkout contains a nested losing clone: %v", entries)
	}
}

// TestCloneRepoCmdFailureCleansLockAndPartial proves a failed gh cannot expose a
// half-cloned target or strand the serialization lock; a later retry can proceed.
func TestCloneRepoCmdFailureCleansLockAndPartial(t *testing.T) {
	root := t.TempDir()
	bin := installFakeGH(t, root)
	logPath := filepath.Join(root, "gh.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FLOWBEE_FAKE_GH_LOG", logPath)
	t.Setenv("FLOWBEE_FAKE_GH_MODE", "fail")

	target := filepath.Join(root, "repos", "widgets")
	cmd := CloneRepoCmd("", "acme/widgets", target)
	runner := ShellRunner{Timeout: 5 * time.Second}
	if out, err := runner.Run(context.Background(), cmd); err == nil {
		t.Fatalf("failed gh unexpectedly succeeded: %s", out)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("failed clone exposed canonical target; stat err=%v", err)
	}
	if leftovers, err := filepath.Glob(target + ".flowbee-clone.*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("failed clone left lock/temp state: %v (glob err=%v)", leftovers, err)
	}

	t.Setenv("FLOWBEE_FAKE_GH_MODE", "success")
	if out, err := runner.Run(context.Background(), cmd); err != nil {
		t.Fatalf("retry after failed clone did not recover: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", "flowbee-test-marker")); err != nil {
		t.Fatalf("retry did not publish a complete checkout: %v", err)
	}
}

// TestCloneRepoCmdReclaimsDeadOwner covers the crash-only path where EXIT traps
// did not run: the next launch reclaims the dead PID's lock and recorded partial
// before cloning normally.
func TestCloneRepoCmdReclaimsDeadOwner(t *testing.T) {
	root := t.TempDir()
	bin := installFakeGH(t, root)
	logPath := filepath.Join(root, "gh.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FLOWBEE_FAKE_GH_LOG", logPath)

	target := filepath.Join(root, "repos", "widgets")
	lock := target + ".flowbee-clone.lock"
	orphan := target + ".flowbee-clone.orphaned"
	if err := os.MkdirAll(filepath.Join(orphan, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir orphaned partial: %v", err)
	}
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatalf("mkdir stale lock: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lock, "pid"), []byte("999999999\n"), 0o600); err != nil {
		t.Fatalf("write stale pid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lock, "partial"), []byte(orphan+"\n"), 0o600); err != nil {
		t.Fatalf("write stale partial: %v", err)
	}

	out, err := (ShellRunner{Timeout: 5 * time.Second}).Run(
		context.Background(), CloneRepoCmd("", "acme/widgets", target))
	if err != nil {
		t.Fatalf("clone did not recover a dead owner: %v\n%s", err, out)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("dead owner's partial was not reclaimed; stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git", "flowbee-test-marker")); err != nil {
		t.Fatalf("recovered clone was not published: %v", err)
	}
	if leftovers, err := filepath.Glob(target + ".flowbee-clone.*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("dead-owner recovery left lock/temp state: %v (glob err=%v)", leftovers, err)
	}
}

// TestCloneRepoCmdRefusesInvalidTarget ensures an existing non-checkout is never
// overwritten, deleted, or used as a directory into which the clone is nested.
func TestCloneRepoCmdRefusesInvalidTarget(t *testing.T) {
	root := t.TempDir()
	bin := installFakeGH(t, root)
	logPath := filepath.Join(root, "gh.log")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FLOWBEE_FAKE_GH_LOG", logPath)

	target := filepath.Join(root, "repos", "widgets")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir invalid target: %v", err)
	}
	sentinel := filepath.Join(target, "operator-data")
	if err := os.WriteFile(sentinel, []byte("keep me"), 0o600); err != nil {
		t.Fatalf("write invalid-target sentinel: %v", err)
	}

	out, err := (ShellRunner{Timeout: 5 * time.Second}).Run(
		context.Background(), CloneRepoCmd("", "acme/widgets", target))
	if err == nil {
		t.Fatalf("invalid target unexpectedly accepted: %s", out)
	}
	if !strings.Contains(out, "exists but is not a git checkout") {
		t.Fatalf("invalid-target error is not diagnostic: %q", out)
	}
	if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep me" {
		t.Fatalf("invalid target was modified: sentinel=%q err=%v", got, err)
	}
	if logBytes, err := os.ReadFile(logPath); err == nil && len(strings.TrimSpace(string(logBytes))) != 0 {
		t.Fatalf("gh ran against an invalid existing target: %q", logBytes)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read fake-gh log: %v", err)
	}
	if leftovers, err := filepath.Glob(target + ".flowbee-clone.*"); err != nil || len(leftovers) != 0 {
		t.Fatalf("invalid target left lock/temp state: %v (glob err=%v)", leftovers, err)
	}
}

// TestRemoteWrapSSHOptionInjection: the `--` end-of-options marker must precede the
// host in every constructed ssh command (review MAJOR #3). shQuote stops SHELL
// injection, but ssh's own getopt reads a leading-dash "host" as an option —
// `-oProxyCommand=...` would be local RCE. With `--`, the next argv element is
// unconditionally the hostname, options over.
func TestRemoteWrapSSHOptionInjection(t *testing.T) {
	got := remoteWrap("buncher", "tmux capture-pane -t 'x' -p")
	want := "ssh -o BatchMode=yes -o ConnectTimeout=5 -- 'buncher' 'tmux capture-pane -t '\\''x'\\'' -p'"
	if got != want {
		t.Fatalf("remoteWrap:\n got %q\nwant %q", got, want)
	}

	// even a hostile registered box value (registration-time validation is the
	// second layer; this is the primary one) lands AFTER `--` and is inert.
	hostile := remoteWrap("-oProxyCommand=evil", "tmux capture-pane -t 'x' -p")
	if !strings.Contains(hostile, " -- '-oProxyCommand=evil' ") {
		t.Fatalf("hostile box must be pinned behind the -- separator, got %q", hostile)
	}

	// local ('' box) commands never grow an ssh prefix.
	if local := remoteWrap("", "tmux capture-pane -t 'x' -p"); strings.Contains(local, "ssh") {
		t.Fatalf("local command must not be ssh-wrapped: %q", local)
	}
}

// TestAllCommandBuildersUseSeparator: every box-aware builder goes through
// remoteWrap, so each constructed remote command carries the `--`.
func TestAllCommandBuildersUseSeparator(t *testing.T) {
	for name, cmd := range map[string]string{
		"capture":      capturePaneCmd("box1", "sess"),
		"scrollback":   captureScrollbackCmd("box1", "sess"),
		"resume":       sendResumeCmd("box1", "sess"),
		"enter":        sendEnterCmd("box1", "sess"),
		"worktree-add": WorktreeAddCmd("box1", "/base", "/wt", "main"),
		"worktree-rm":  WorktreeRemoveCmd("box1", "/base", "/wt"),
	} {
		if !strings.Contains(cmd, " -- 'box1' ") {
			t.Errorf("%s: missing `-- <host>` separator: %q", name, cmd)
		}
	}
}

// TestWorktreeCmdStrings pins the EXACT shell strings for the per-epic worktree add/remove
// builders — the shQuoting, the shQuote("origin/"+branch) checkout target, and the parent
// mkdir can't silently drift, and a branch carrying shell metacharacters must stay inside
// its single argument.
func TestWorktreeCmdStrings(t *testing.T) {
	gotAdd := WorktreeAddCmd("", "/home/ops/dev/russ", "/home/ops/dev/.flowbee-wt/russ/2026-07-16-widget", "main")
	wantAdd := "git -C '/home/ops/dev/russ' fetch --quiet origin 'main'" +
		" && mkdir -p -- '/home/ops/dev/.flowbee-wt/russ'" +
		" && git -C '/home/ops/dev/russ' worktree add --detach '/home/ops/dev/.flowbee-wt/russ/2026-07-16-widget' 'origin/main'"
	if gotAdd != wantAdd {
		t.Fatalf("WorktreeAddCmd:\n got %q\nwant %q", gotAdd, wantAdd)
	}

	gotRm := WorktreeRemoveCmd("", "/home/ops/dev/russ", "/home/ops/dev/.flowbee-wt/russ/2026-07-16-widget")
	wantRm := "git -C '/home/ops/dev/russ' worktree remove --force '/home/ops/dev/.flowbee-wt/russ/2026-07-16-widget'"
	if gotRm != wantRm {
		t.Fatalf("WorktreeRemoveCmd:\n got %q\nwant %q", gotRm, wantRm)
	}

	// the origin/<branch> checkout target must be one shQuote'd argument.
	hostile := WorktreeAddCmd("", "/b", "/w", "main; rm -rf /")
	if !strings.Contains(hostile, "worktree add --detach '/w' 'origin/main; rm -rf /'") {
		t.Fatalf("origin/<branch> must be shQuote'd as one argument, got %q", hostile)
	}
}

// TestExactTarget: the wrong-target fix. A registered session name gains a `=` so
// `-t goal-s1` can never prefix-match `goal-s1-old`; ids and already-`=`'d targets
// are left untouched; the empty string is never fabricated into a bare "=".
func TestExactTarget(t *testing.T) {
	cases := []struct{ in, want string }{
		{"goal-s1", "=goal-s1:"},
		{"epic-fix", "=epic-fix:"},
		{"sess:0.1", "=sess:0.1"},
		{"%5", "%5"},
		{"@3", "@3"},
		{"$2", "$2"},
		{"=already:", "=already:"},
		{"", ""},
	}
	for _, c := range cases {
		if got := exactTarget(c.in); got != c.want {
			t.Errorf("exactTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestCommandBuildersUseExactMatchTargets locks in the fix at the builder level:
// every tmux command that TARGETS a session by name (-t) must exact-match it with a
// `=` prefix so a bare name cannot prefix-match a longer session name. The
// new-session builder is deliberately excluded — its `-s` is a CREATE name, not a
// lookup target, and must stay the literal name.
func TestCommandBuildersUseExactMatchTargets(t *testing.T) {
	for name, cmd := range map[string]string{
		"capture":    capturePaneCmd("", "goal-s1"),
		"scrollback": captureScrollbackCmd("", "goal-s1"),
		"resume":     sendResumeCmd("", "goal-s1"),
		"enter":      sendEnterCmd("", "goal-s1"),
		"kill":       KillTmuxSessionCmd("", "goal-s1"),
		"send-goal":  SendGoalCmd("", "goal-s1", "/goal go"),
	} {
		if !strings.Contains(cmd, "-t '=goal-s1:'") {
			t.Errorf("%s: target is not exact-matched (want -t '=goal-s1:'): %q", name, cmd)
		}
	}
	// new-session's -s is a create name, NOT a target: it must stay literal.
	if create := NewTmuxSessionCmd("", "goal-s1", "/dir", "codex"); !strings.Contains(create, "-s 'goal-s1'") {
		t.Errorf("new-session -s must remain the literal create name, got: %q", create)
	}
}

// TestKillTmuxSessionCmdConfirmsAbsence pins the abandon safety contract: success
// means tmux was available and the exact session was observed absent after any kill.
// The second has-session probe is what lets a caller retain reservations when a kill
// fails or leaves the target alive; the command remains successful when it was absent.
func TestKillTmuxSessionCmdConfirmsAbsence(t *testing.T) {
	cmd := KillTmuxSessionCmd("", "epic-e1")
	if !strings.Contains(cmd, "command -v tmux") {
		t.Fatalf("missing tmux-availability check: %q", cmd)
	}
	if got := strings.Count(cmd, "tmux has-session -t '=epic-e1:'"); got != 2 {
		t.Fatalf("has-session probes = %d, want initial idempotence check + post-kill confirmation: %q", got, cmd)
	}
}
