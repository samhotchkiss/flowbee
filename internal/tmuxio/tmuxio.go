// Package tmuxio is the substrate for Flowbee's shift from HEADLESS agent spawns
// to SUPERVISED interactive agent sessions.
//
// The old model ran each agent as a one-shot headless process (`claude -p …` /
// `codex exec …`): Flowbee started it, streamed its output, and waited for the
// process to exit. That is simple but blind — there is no running UI to inspect,
// no way to nudge a stuck prompt, no long-lived session to hand successive tasks
// to, and no story for agents that live on another box.
//
// The new model supervises LONG-LIVED INTERACTIVE agents (Claude Code, Codex)
// running inside tmux panes — locally, or on a remote host via `ssh`. Flowbee no
// longer owns the process; it owns a VIEW of the pane. Everything a supervisor
// needs is expressed against that view: discover the panes and their real agent
// PIDs, capture what the pane shows, classify what the agent is doing, and — the
// hard part — DELIVER a message into the agent's input box and VERIFY it was
// actually submitted. Epic supervision, attention routing, and the dashboard all
// build on these primitives, so their semantics must be exact.
//
// # Delivery verification (the crown jewel)
//
// `tmux send-keys "msg" Enter` is unreliable against Claude Code and Codex: the
// Enter arrives in the same input burst as the text and is absorbed by the TUI's
// paste handling, leaving the message sitting UNSUBMITTED in the input box. The
// fix, proven by hand first, is to deliver the text (literal keystrokes for a
// short single line, a bracketed paste for long/multiline), let the input box
// settle, send Enter as a SEPARATE key event, then VERIFY the input box cleared —
// re-pressing Enter with backoff until it did, and reporting honestly when it
// could not be confirmed.
//
// This package ports the semantics of the hand-rolled `tmux-send` skill (source
// at ~/.local/bin/tmux-send, skill doc ~/.claude/skills/tmux-send/SKILL.md,
// source tree /Users/sam/dev/tmux-send) into a typed, testable Go API, and folds
// in the hard lesson internal/watchdog already learned the hard way: verify the
// input box by an EXACT match of the LOCATED input line, NOT a fragment-Contains
// check. A Contains check misfires on status-line hint text (codex renders its own
// "Goal blocked (/goal resume)" hint) and, worse, could press Enter under a
// human's edited input — submitting keystrokes the supervisor never typed. Two
// polarity rules keep verification honest (see Send, verifyExact, extractInputLine):
//   - The input line is LOCATED (the last line matching the prompt regex — Claude
//     Code's input sits inside a bordered `│ > │` box whose LAST line is a
//     "? for shortcuts" hint, NOT the message), and only its EXACT contents drive
//     a decision: the message still present drives a bare-Enter RETRY; a located,
//     EMPTY prompt is the only Strong "submitted" signal.
//   - Absence is never success: a prompt line we cannot locate (or a
//     wrapped/multiline message that defeats the single-line match) is WEAK, never
//     Strong. The skill's fragment heuristics survive solely as a Weak-confidence
//     signal, never as a retry trigger.
//
// # Transport (local and remote), aligned with internal/watchdog
//
// Every tmux invocation is built as a shell-command STRING and run through an
// injected Runner (identical in shape to internal/watchdog.Runner, so its
// ShellRunner is a drop-in). Command construction — never the Runner — decides
// local vs remote: a Client bound to a Host (WithHost) wraps each command in
// `ssh -o BatchMode=yes -o ConnectTimeout=5 -- <host> '<cmd>'`, exactly as
// watchdog does, so the tmux SERVER itself can live on a remote box reached by
// per-op ssh. Every identifier that reaches the shell is shQuote'd, and the ssh
// host carries a `--` end-of-options guard (a host literal like `-oProxyCommand=…`
// must never be read as an ssh option). Callers pass PRE-VALIDATED identifiers;
// this package still quotes everything, and NEVER builds keystrokes from anything
// read off a pane or scrollback.
//
// # Design
//
// No global state. tmux/pgrep/ps go through the Runner; delays go through an
// injected Clock (the fake returns from Sleep instantly, so unit tests do not
// wall-clock-block). The integration tests (see integration_test.go) stand up a
// REAL tmux server on an isolated `-L` socket and exercise capture, delivery
// verification, and state transitions end to end, skipping when tmux is not
// installed. The delivery-verification path in particular is NOT faked-only.
package tmuxio

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner runs a single, fully-formed shell command and returns its combined
// output (stdout+stderr). Its shape is deliberately identical to
// internal/watchdog.Runner so that package's ShellRunner is a drop-in and the two
// share one mocking style. Command construction (local vs ssh-wrapped) happens in
// this package's builders, NOT in the Runner — the Runner only ever sees a
// finished `sh -c` string.
type Runner interface {
	Run(ctx context.Context, shellCmd string) (string, error)
}

// Clock supplies the current time and a cancellable sleep. It exists so the
// delivery-verification backoff and pre-submit settle delay do not wall-clock-block
// unit tests: the fake Clock returns from Sleep instantly. Real delays matter only
// against a real tmux server (the integration tests use the real Clock).
type Clock interface {
	Now() time.Time
	// Sleep blocks for d or until ctx is cancelled, whichever comes first.
	Sleep(ctx context.Context, d time.Duration)
}

// ShellRunner is the production Runner: `sh -c <cmd>`, timeout-bounded so a wedged
// ssh (a box that is down but not refusing the TCP connection outright) can never
// hang a supervision pass forever. Combined output is returned so a tmux error
// (which prints to stderr) is visible in the wrapped error. Mirrors
// internal/watchdog.ShellRunner.
type ShellRunner struct {
	Timeout time.Duration
}

// NewShellRunner builds a ShellRunner with a sane default timeout (15s, matching
// internal/watchdog).
func NewShellRunner() ShellRunner { return ShellRunner{Timeout: 15 * time.Second} }

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

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Client is a handle onto ONE tmux server: the default local server, an isolated
// local server selected by socket name (WithSocket, `-L`), or a server on a remote
// box reached per-op via ssh (WithHost). It holds no mutable state and is safe to
// share. Construct with New.
type Client struct {
	runner Runner
	clock  Clock
	host   string // "" = local; otherwise the ssh box the tmux server runs on
	socket string // tmux -L <socket>; "" = the default server
}

// Option configures a Client.
type Option func(*Client)

// WithRunner injects a Runner (default: a real ShellRunner).
func WithRunner(r Runner) Option { return func(c *Client) { c.runner = r } }

// WithClock injects a Clock (default: the real wall clock).
func WithClock(k Clock) Option { return func(c *Client) { c.clock = k } }

// WithSocket pins the Client to a named tmux server socket (`tmux -L <name>`).
// The empty default targets the default tmux server — the one an operator's
// pre-existing agent sessions live on, so adoption works out of the box. Isolated
// sockets are for tests (see newTestServer).
func WithSocket(name string) Option { return func(c *Client) { c.socket = name } }

// WithHost runs every tmux command against the tmux server on a remote box,
// reached per-operation via `ssh -o BatchMode=yes -o ConnectTimeout=5 -- <host>`
// (the internal/watchdog transport). The empty default is local. host must be a
// pre-validated identifier (an operator-registered box name); it is shQuote'd and
// `--`-guarded regardless. NOTE: this is distinct from a LOCAL pane whose own
// foreground command is `ssh` (SessionSpec.RemoteHost, and the Remote flag on
// AgentProcess) — that is an agent reached THROUGH a local pane, whereas WithHost
// puts the whole tmux server on the far side.
func WithHost(host string) Option { return func(c *Client) { c.host = host } }

// New builds a Client. With no options it drives the real tmux binary against the
// default local server using the wall clock — the production configuration.
func New(opts ...Option) *Client {
	c := &Client{runner: ShellRunner{Timeout: 15 * time.Second}, clock: realClock{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// tmuxInner builds the `tmux [-L socket] <sub>` portion (no ssh wrap). sub is an
// already-shQuote'd tmux subcommand string.
func (c *Client) tmuxInner(sub string) string {
	base := "tmux"
	if c.socket != "" {
		base += " -L " + shQuote(c.socket)
	}
	return base + " " + sub
}

// buildCmd assembles the full shell command for a tmux subcommand: the local
// `tmux [-L socket] <sub>`, wrapped in ssh when this Client is bound to a Host.
func (c *Client) buildCmd(sub string) string {
	return remoteWrap(c.host, c.tmuxInner(sub))
}

// run executes a tmux subcommand and returns its output, wrapping a non-zero exit
// with the (combined-output) error text for context.
func (c *Client) run(ctx context.Context, sub string) (string, error) {
	cmd := c.buildCmd(sub)
	out, err := c.runner.Run(ctx, cmd)
	if err != nil {
		return out, fmt.Errorf("tmuxio: %s: %w: %s", sub, err, strings.TrimSpace(out))
	}
	return out, nil
}

// remoteWrap wraps inner in an ssh invocation when host is non-empty ("" = local).
// The ssh form matches internal/watchdog exactly: BatchMode (never hang on a
// password / host-key prompt — this runs unattended), a short ConnectTimeout (a
// downed box fails fast instead of stalling a whole pass), and a `--` end-of-options
// marker BEFORE the host so a leading-dash host literal can never be read as an ssh
// option (local RCE otherwise). shQuote stops shell injection of the host and the
// inner command; the `--` closes the ssh-getopt hole shQuote cannot.
func remoteWrap(host, inner string) string {
	if host == "" {
		return inner
	}
	return "ssh -o BatchMode=yes -o ConnectTimeout=5 -- " + shQuote(host) + " " + shQuote(inner)
}

// shQuote single-quotes s for safe embedding in an `sh -c` string, escaping any
// embedded single quote. POSIX single-quoting is total — inside single quotes NO
// character is special except the single quote itself — so this is safe for
// arbitrary message payloads (newlines, $, backticks, etc.). Matches
// internal/watchdog.shQuote; when a command is remote-wrapped the whole inner
// command is shQuote'd again, and the nesting composes correctly.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// assertValidIdent enforces the caller-supplied-identifier contract at the API
// boundary (review m12), matching store.AddGoalSession's defense-in-depth posture:
// session names, tmux targets, and ssh hosts must be non-empty, must not start with
// '-' (an ssh/tmux getopt would read it as an option even behind shQuote), and must
// contain no control characters — including NUL, which cannot cross the argv
// boundary. Interior spaces ARE allowed (tmux session names legitimately contain
// them). This is defense in depth: shQuote still runs on every value regardless.
func assertValidIdent(kind, s string) error {
	if s == "" {
		return fmt.Errorf("tmuxio: %s must not be empty", kind)
	}
	if strings.HasPrefix(s, "-") {
		return fmt.Errorf("tmuxio: %s %q must not start with '-'", kind, s)
	}
	for _, r := range s {
		if r == 0 || r < 0x20 || r == 0x7f {
			return fmt.Errorf("tmuxio: %s %q contains a control character", kind, s)
		}
	}
	return nil
}

// validateIdent checks a per-call identifier (target/session name) AND this
// Client's configured ssh host, so a bad host is caught at the first operation.
func (c *Client) validateIdent(kind, s string) error {
	if c.host != "" {
		if err := assertValidIdent("host", c.host); err != nil {
			return err
		}
	}
	return assertValidIdent(kind, s)
}
