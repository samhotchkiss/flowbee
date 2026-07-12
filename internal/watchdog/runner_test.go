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
