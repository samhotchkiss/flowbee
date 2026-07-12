package watchdog

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a single shell command and returns its combined output. It exists so
// tmux/ssh is MOCKABLE in tests (internal/watchdog/watchdog_test.go drives the
// entire state machine — blocked->resume, rate limiting, infra classification —
// against a fake Runner, never a real tmux session). The real implementation
// (ShellRunner) shells out via `sh -c`; command construction (local vs
// `ssh -o BatchMode=yes -o ConnectTimeout=5 <box> '<cmd>'`) happens in the
// box-aware helpers below, NOT in Runner itself — Runner only ever sees a fully
// formed shell command string.
type Runner interface {
	Run(ctx context.Context, shellCmd string) (string, error)
}

// ShellRunner is the real Runner: `sh -c <cmd>`, timeout-bounded so a wedged ssh
// (e.g. a box that's down but not refusing the TCP connection outright) can never
// hang the watch pass forever.
type ShellRunner struct {
	Timeout time.Duration
}

// NewShellRunner builds a ShellRunner with a sane default timeout.
func NewShellRunner() ShellRunner {
	return ShellRunner{Timeout: 15 * time.Second}
}

func (r ShellRunner) Run(ctx context.Context, shellCmd string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, "sh", "-c", shellCmd)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ── command construction ──
//
// SAFETY (§ task brief): the watcher only ever sends keys to sessions in the
// registry, and the ONLY input it ever sends is the literal string "/goal resume"
// and a bare Enter. Every command below is built from a closed set of literal
// verbs (capture-pane / send-keys Enter) plus the session's OWN registered
// tmux_name/box — never from anything read off a pane or scrollback.

// capturePaneCmd builds the "read the current status line" command — just the
// visible pane, matching the exact form given in the task brief.
func capturePaneCmd(box, tmuxName string) string {
	return remoteWrap(box, "tmux capture-pane -t "+shQuote(tmuxName)+" -p")
}

// captureScrollbackCmd adds -S -60 for diagnosis (classifying WHY a session is
// blocked needs the reason text that scrolled by, not just the current line).
func captureScrollbackCmd(box, tmuxName string) string {
	return remoteWrap(box, "tmux capture-pane -t "+shQuote(tmuxName)+" -p -S -60")
}

// sendResumeCmd sends the literal "/goal resume" text followed by Enter.
func sendResumeCmd(box, tmuxName string) string {
	return remoteWrap(box, "tmux send-keys -t "+shQuote(tmuxName)+" "+shQuote("/goal resume")+" Enter")
}

// sendEnterCmd sends a bare Enter — the fix for the observed "TUI's slash-command
// menu swallows the first Enter, leaving the text unsubmitted in the input line"
// failure mode.
func sendEnterCmd(box, tmuxName string) string {
	return remoteWrap(box, "tmux send-keys -t "+shQuote(tmuxName)+" Enter")
}

// remoteWrap wraps inner in an ssh invocation when box is non-empty (” == local,
// matching the Repo.DefaultBranch-style convention used elsewhere in the store).
// Exactly the ssh form specified in the task brief: BatchMode (never prompt/hang on
// a password/host-key question — this runs unattended) + a short ConnectTimeout (a
// downed box fails fast into the consecutive_failures counter instead of stalling
// the whole watch pass).
func remoteWrap(box, inner string) string {
	if box == "" {
		return inner
	}
	return "ssh -o BatchMode=yes -o ConnectTimeout=5 " + shQuote(box) + " " + shQuote(inner)
}

// shQuote single-quotes s for safe embedding in an `sh -c` string, escaping any
// embedded single quotes. Standard POSIX shell-quoting trick: close the quote,
// emit an escaped literal quote, reopen.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
