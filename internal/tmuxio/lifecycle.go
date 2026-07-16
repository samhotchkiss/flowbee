package tmuxio

import (
	"context"
	"fmt"
	"strings"
)

// SessionSpec describes a tmux session to create. Name and Command are required;
// the rest are optional.
//
// Remote deployment is a Client-level concern, NOT a per-session one: build the
// Client with WithHost to run the whole tmux server (and every session on it) on a
// remote box, the model internal/watchdog and the epic launcher use. Discovery's
// AgentProcess.Remote separately flags an ADOPTED local pane whose own foreground
// is `ssh`. (An earlier SessionSpec.RemoteHost that wrapped the pane command in
// interactive ssh was dropped per review m15 — it had no consumer and doubled the
// surface.)
type SessionSpec struct {
	// Name is the tmux session name (`-s`). Validated (assertValidIdent) and
	// shQuote'd.
	Name string
	// Command is the shell command the session's first pane runs (the agent's own
	// launch invocation, e.g. "codex" or "claude --resume"). It is executed by a
	// shell on the box, so — like internal/watchdog's NewTmuxSessionCmd startCmd —
	// the caller MUST have validated it against a strict charset before it reaches
	// here; shQuote alone does not make an untrusted command safe to run.
	Command string
	// StartDir is the pane's working directory (`-c`). Optional.
	StartDir string
	// WindowName is the initial window's name (`-n`). Optional.
	WindowName string
}

// NewSession creates a new DETACHED tmux session per spec (`new-session -d`). It
// errors if a session with the same name already exists (check HasSession first to
// adopt a pre-existing one). The session outlives this call — it is a long-lived
// interactive agent, not a one-shot.
func (c *Client) NewSession(ctx context.Context, spec SessionSpec) error {
	if err := c.validateIdent("session name", spec.Name); err != nil {
		return err
	}
	if strings.TrimSpace(spec.Command) == "" {
		return fmt.Errorf("tmuxio: NewSession requires a command")
	}
	sub := "new-session -d -s " + shQuote(spec.Name)
	if spec.StartDir != "" {
		sub += " -c " + shQuote(spec.StartDir)
	}
	if spec.WindowName != "" {
		sub += " -n " + shQuote(spec.WindowName)
	}
	// The command is the trailing shell-command argument; tmux runs it via sh -c.
	sub += " " + shQuote(spec.Command)
	_, err := c.run(ctx, sub)
	return err
}

const (
	sessionExistsToken  = "FLOWBEE_TMUXIO_SESSION_EXISTS"
	sessionMissingToken = "FLOWBEE_TMUXIO_SESSION_MISSING"
)

// HasSession reports whether a session named name exists on this Client's server.
// It resolves the answer from a printed token rather than an exit code, so it works
// uniformly across local and ssh transports.
//
// The distinction it CAN and cannot draw: a reachable server with no such session —
// AND a reachable box whose tmux server is not running at all (`has-session` there
// prints "no server running" and exits non-zero, so the `|| echo MISSING` fires) —
// both return (false, nil). Only a failure of the TRANSPORT itself (ssh cannot
// reach the box, or tmux/sh is absent) surfaces as an error. In short: (false, nil)
// means "no such live session to use", not "the box is definitely up".
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
	if err := c.validateIdent("session name", name); err != nil {
		return false, err
	}
	sub := "has-session -t " + shQuote(name) + " >/dev/null 2>&1 && echo " +
		shQuote(sessionExistsToken) + " || echo " + shQuote(sessionMissingToken)
	out, err := c.run(ctx, sub)
	if err != nil {
		return false, err
	}
	if strings.Contains(out, sessionExistsToken) {
		return true, nil
	}
	if strings.Contains(out, sessionMissingToken) {
		return false, nil
	}
	return false, fmt.Errorf("tmuxio: HasSession: unexpected output %q", strings.TrimSpace(out))
}

// KillSession kills the session named name (`kill-session`). Killing a nonexistent
// session is an error from tmux; callers doing best-effort cleanup should ignore it.
func (c *Client) KillSession(ctx context.Context, name string) error {
	if err := c.validateIdent("session name", name); err != nil {
		return err
	}
	_, err := c.run(ctx, "kill-session -t "+shQuote(name))
	return err
}

// ListSessions returns the names of all sessions on this Client's server. A server
// that is not running yet (no sessions ever created) yields an empty slice, not an
// error — the natural "nothing to adopt" answer.
//
// NOTE (review n16): the "no server running" detection here is a stderr STRING
// match, which is more brittle than HasSession's printed-token approach. It is kept
// simple deliberately — ListSessions is a best-effort adoption helper — but if tmux
// changes that message, an empty server would surface as an error rather than an
// empty list. A caller that needs a hard answer should prefer HasSession per name.
func (c *Client) ListSessions(ctx context.Context) ([]string, error) {
	if c.host != "" {
		if err := assertValidIdent("host", c.host); err != nil {
			return nil, err
		}
	}
	out, err := c.run(ctx, "list-sessions -F "+shQuote("#{session_name}"))
	if err != nil {
		// tmux prints "no server running on ..." (or "error connecting ...") and exits
		// non-zero when the server isn't up — that is an empty listing, not a failure,
		// for adoption purposes.
		if strings.Contains(out, "no server running") || strings.Contains(out, "no current session") {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			names = append(names, s)
		}
	}
	return names, nil
}

// KillServer tears down this Client's entire tmux server (`kill-server`). Intended
// for isolated test sockets (see integration_test.go cleanup); never call it on a
// shared/default server holding an operator's real sessions.
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	return err
}
