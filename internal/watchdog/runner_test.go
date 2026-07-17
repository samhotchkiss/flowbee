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
		"capture":    capturePaneCmd("box1", "sess"),
		"scrollback": captureScrollbackCmd("box1", "sess"),
		"resume":     sendResumeCmd("box1", "sess"),
		"enter":      sendEnterCmd("box1", "sess"),
	} {
		if !strings.Contains(cmd, " -- 'box1' ") {
			t.Errorf("%s: missing `-- <host>` separator: %q", name, cmd)
		}
	}
}

// TestRepoRefreshAndCloneCmdStrings pins the EXACT shell strings for the reused-
// checkout refresh (Bug 2) and the temp-then-mv clone (Bug 1), so the shQuoting and
// the shQuote("origin/"+branch) form can't silently drift.
func TestRepoRefreshAndCloneCmdStrings(t *testing.T) {
	gotRefresh := RepoRefreshCmd("", "/home/ops/epics/russ", "main")
	wantRefresh := "cd '/home/ops/epics/russ' && git fetch --quiet origin 'main' && git checkout --quiet 'main' && git reset --hard -q 'origin/main'"
	if gotRefresh != wantRefresh {
		t.Fatalf("RepoRefreshCmd:\n got %q\nwant %q", gotRefresh, wantRefresh)
	}

	// temp-then-mv: clone lands in <path>.partial and is atomically mv'd in only on
	// success; the leading rm -rf clears any partial a prior SIGKILL'd attempt left.
	gotClone := CloneRepoCmd("", "acme/russ", "/home/ops/epics/russ")
	wantClone := "rm -rf -- '/home/ops/epics/russ.partial' && mkdir -p -- '/home/ops/epics' && gh repo clone 'acme/russ' '/home/ops/epics/russ.partial' -- --quiet && mv -- '/home/ops/epics/russ.partial' '/home/ops/epics/russ'"
	if gotClone != wantClone {
		t.Fatalf("CloneRepoCmd:\n got %q\nwant %q", gotClone, wantClone)
	}

	// the origin/<branch> token must be a single shQuote'd argument — never string-
	// concatenated with an unquoted branch, so a branch carrying shell metacharacters
	// can't break out of its argument.
	hostile := RepoRefreshCmd("", "/x", "main; rm -rf /")
	if !strings.Contains(hostile, "git reset --hard -q 'origin/main; rm -rf /'") {
		t.Fatalf("origin/<branch> must be shQuote'd as one argument, got %q", hostile)
	}

	// the ssh remote form quotes the whole inner command as one argument behind `--`.
	if remote := RepoRefreshCmd("buncher", "/x", "main"); !strings.Contains(remote, " -- 'buncher' ") {
		t.Fatalf("remote RepoRefreshCmd missing `-- <host>` separator: %q", remote)
	}
}
