package watchdog

import (
	"strings"
	"testing"
)

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
