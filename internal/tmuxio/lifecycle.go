package tmuxio

import (
	"context"
	"fmt"
	"strings"
)

// SessionSpec describes a tmux session to create. Name and Command are required;
// the rest are optional.
type SessionSpec struct {
	// Name is the tmux session name (`-s`). Must be a caller-validated identifier;
	// it is shQuote'd regardless.
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
	// RemoteHost, when set, wraps Command so the PANE runs `ssh -tt -- <host>
	// <command>` — an interactive agent living on another host, reached through a
	// pane on THIS tmux server. This is the deployment the discovery layer flags via
	// AgentProcess.Remote (pane_current_command == "ssh"). It is DISTINCT from
	// Client.WithHost, which instead puts the whole tmux server on a remote box. Use
	// RemoteHost when the tmux server (and thus adoptability/attachment) stays local
	// but the agent is remote; use WithHost when the server itself is remote.
	RemoteHost string
}

// NewSession creates a new DETACHED tmux session per spec (`new-session -d`). It
// errors if a session with the same name already exists (check HasSession first
// to adopt a pre-existing one). The session outlives this call — it is a
// long-lived interactive agent, not a one-shot.
func (c *Client) NewSession(ctx context.Context, spec SessionSpec) error {
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("tmuxio: NewSession requires a session name")
	}
	if strings.TrimSpace(spec.Command) == "" {
		return fmt.Errorf("tmuxio: NewSession requires a command")
	}
	command := spec.Command
	if spec.RemoteHost != "" {
		// The pane runs interactive ssh. -tt forces a remote TTY (the pane already
		// has a pty); `--` guards a leading-dash host; both host and command are
		// shQuote'd so tmux's own `sh -c` cannot reinterpret them. No BatchMode here
		// (unlike the per-op remoteWrap): an interactive first-connect may need to
		// answer a host-key prompt.
		command = "ssh -tt -- " + shQuote(spec.RemoteHost) + " " + shQuote(spec.Command)
	}
	sub := "new-session -d -s " + shQuote(spec.Name)
	if spec.StartDir != "" {
		sub += " -c " + shQuote(spec.StartDir)
	}
	if spec.WindowName != "" {
		sub += " -n " + shQuote(spec.WindowName)
	}
	// The command is the trailing shell-command argument; tmux runs it via sh -c.
	sub += " " + shQuote(command)
	_, err := c.run(ctx, sub)
	return err
}

const (
	sessionExistsToken  = "FLOWBEE_TMUXIO_SESSION_EXISTS"
	sessionMissingToken = "FLOWBEE_TMUXIO_SESSION_MISSING"
)

// HasSession reports whether a session named name exists on this Client's server.
// It resolves the answer from a printed token rather than an exit code, so it
// works uniformly across local and ssh transports: a genuinely unreachable server
// (ssh down) still surfaces as an error, while a reachable server that simply has
// no such session returns (false, nil).
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
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

// KillSession kills the session named name (`kill-session`). Killing a
// nonexistent session is an error from tmux; callers doing best-effort cleanup
// should ignore it.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.run(ctx, "kill-session -t "+shQuote(name))
	return err
}

// ListSessions returns the names of all sessions on this Client's server. A
// server that is not running yet (no sessions ever created) yields an empty slice,
// not an error — the natural "nothing to adopt" answer.
func (c *Client) ListSessions(ctx context.Context) ([]string, error) {
	out, err := c.run(ctx, "list-sessions -F "+shQuote("#{session_name}"))
	if err != nil {
		// tmux prints "no server running on ..." (or "error connecting ...") and
		// exits non-zero when the server isn't up — that is an empty listing, not a
		// failure, for adoption purposes.
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
