package tmuxio

import (
	"context"
	"strconv"
	"strings"
)

// fieldSep is the record separator this package feeds to tmux `-F` format strings
// and splits on. tmux session names can legitimately contain spaces, so a
// space-delimited format (the shape shown in the task brief) is ambiguous to
// parse; the ASCII Unit Separator (0x1f) cannot appear in any of these fields, so
// splitting on it is unambiguous. This is a deliberate improvement over the
// literal brief format.
const fieldSep = "\x1f"

// Pane is one tmux pane as supervision sees it: enough to route attention and to
// resolve the real agent process behind the shell. Fields are read verbatim from
// `tmux list-panes -a -F …`.
type Pane struct {
	SessionName    string // #{session_name}
	WindowIndex    int    // #{window_index}
	PaneID         string // #{pane_id}, e.g. "%5" — the stable handle for send/capture
	PanePID        int    // #{pane_pid} — the pane's SHELL pid (not the agent; see ResolveAgent)
	CurrentCommand string // #{pane_current_command}, e.g. "claude", "node", "zsh", "ssh"
	Width          int    // #{pane_width}
	Height         int    // #{pane_height}
}

// Target returns the canonical `session:window.pane`-free handle used for tmux
// -t: the pane id (e.g. "%5"), which is stable across renames and reordering.
func (p Pane) Target() string { return p.PaneID }

// ListPanes enumerates every pane on this Client's tmux server with the fields
// supervision needs. It returns an empty slice (not an error) when the server is
// running but has no panes; a genuinely unreachable server surfaces as an error.
func (c *Client) ListPanes(ctx context.Context) ([]Pane, error) {
	format := strings.Join([]string{
		"#{session_name}", "#{window_index}", "#{pane_id}", "#{pane_pid}",
		"#{pane_current_command}", "#{pane_width}", "#{pane_height}",
	}, fieldSep)
	out, err := c.run(ctx, "list-panes -a -F "+shQuote(format))
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, fieldSep)
		if len(f) != 7 {
			// A malformed row (e.g. a stray warning line) is skipped rather than
			// failing the whole listing — discovery must degrade, not crash.
			continue
		}
		panes = append(panes, Pane{
			SessionName:    f[0],
			WindowIndex:    atoiOr(f[1], 0),
			PaneID:         f[2],
			PanePID:        atoiOr(f[3], 0),
			CurrentCommand: f[4],
			Width:          atoiOr(f[5], 0),
			Height:         atoiOr(f[6], 0),
		})
	}
	return panes, nil
}

// AgentProcess is the real agent process behind a pane's shell — the thing a
// supervisor actually wants to inspect (`ps eww <pid>` for account resolution,
// which is another package's job; here we only expose the pid and command).
type AgentProcess struct {
	// PID is the resolved agent process id, or the ssh pid when Remote is true, or
	// 0 when nothing agent-like was found under the pane's shell.
	PID int
	// Command is the process command name (comm), e.g. "claude", "node", "codex",
	// or "ssh".
	Command string
	// Remote is true when the pane's foreground is `ssh`: the agent runs on the far
	// side of that ssh and cannot be walked locally. The caller resolves it on the
	// remote host. (Independent of Client.WithHost, which puts the whole tmux server
	// remote; this Remote is a LOCAL pane whose command happens to be ssh.)
	Remote bool
}

// agentCommands are the process command names that identify an interactive coding
// agent behind the pane shell. "node" is included because Codex (and Claude Code
// in some installs) present as a node process; the caller disambiguates via
// `ps eww`. Kept as data, not code, so it can grow.
var agentCommands = map[string]bool{
	"claude": true,
	"codex":  true,
	"node":   true,
}

// isAgentCommand reports whether comm names an interactive coding agent. Exact
// match against agentCommands, plus a substring allowance for wrapper names like
// "claude-code" that embed a known agent token.
func isAgentCommand(comm string) bool {
	comm = strings.TrimSpace(comm)
	if agentCommands[comm] {
		return true
	}
	lc := strings.ToLower(comm)
	return strings.Contains(lc, "claude") || strings.Contains(lc, "codex")
}

// ResolveAgent walks the process tree under a pane's shell (pane_pid) to find the
// real agent process. It returns ok=false when no agent-like process is found
// (e.g. the pane is a plain shell). When the pane's foreground command is `ssh`,
// it returns Remote=true with the ssh pid — the agent lives on the far side and
// must be resolved on that host.
//
// The walk is breadth-first via `pgrep -P`, bounded in depth and total nodes so a
// pathological tree can never spin. Command names come from `ps -o comm=`. All
// process commands run through the same Runner (and ssh wrap, if any) as tmux, so
// a WithHost client resolves processes on the remote box.
func (c *Client) ResolveAgent(ctx context.Context, pane Pane) (AgentProcess, bool, error) {
	// Fast path: the pane's own foreground is ssh -> remote agent. Still try to
	// find the ssh pid for completeness, but the flag is what matters.
	if pane.CurrentCommand == "ssh" {
		pid, _ := c.findDescendant(ctx, pane.PanePID, func(comm string) bool { return comm == "ssh" })
		if pid == 0 {
			pid = pane.PanePID
		}
		return AgentProcess{PID: pid, Command: "ssh", Remote: true}, true, nil
	}
	if pane.PanePID <= 0 {
		return AgentProcess{}, false, nil
	}
	pid, comm := c.findDescendant(ctx, pane.PanePID, isAgentCommand)
	if pid == 0 {
		// No agent under the shell. If the foreground command itself looks like an
		// agent (rare: agent is the pane's direct process), report the shell pid.
		if isAgentCommand(pane.CurrentCommand) {
			return AgentProcess{PID: pane.PanePID, Command: pane.CurrentCommand}, true, nil
		}
		return AgentProcess{}, false, nil
	}
	return AgentProcess{PID: pid, Command: comm, Remote: comm == "ssh"}, true, nil
}

// findDescendant breadth-first-searches the process tree rooted at pid for the
// first descendant whose command name satisfies match. Returns (pid, comm) or
// (0, "") if none within the bounds. Bounds: maxDepth generations, maxNodes total.
func (c *Client) findDescendant(ctx context.Context, root int, match func(comm string) bool) (int, string) {
	const (
		maxDepth = 8
		maxNodes = 256
	)
	type node struct {
		pid   int
		depth int
	}
	seen := map[int]bool{root: true}
	queue := []node{{root, 0}}
	visited := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		visited++
		if visited > maxNodes {
			break
		}
		children := c.childPIDs(ctx, n.pid)
		for _, child := range children {
			if seen[child] {
				continue
			}
			seen[child] = true
			comm := c.commOf(ctx, child)
			if match(comm) {
				return child, comm
			}
			if n.depth+1 < maxDepth {
				queue = append(queue, node{child, n.depth + 1})
			}
		}
	}
	return 0, ""
}

// childPIDs returns the direct child pids of pid via `pgrep -P`. A non-zero exit
// (no children) yields an empty slice, not an error — a childless process is the
// normal leaf case.
func (c *Client) childPIDs(ctx context.Context, pid int) []int {
	out, err := c.runner.Run(ctx, remoteWrap(c.host, "pgrep -P "+strconv.Itoa(pid)))
	if err != nil {
		return nil // pgrep exits 1 when there are no matches
	}
	var pids []int
	for _, line := range strings.Fields(out) {
		if v, err := strconv.Atoi(line); err == nil {
			pids = append(pids, v)
		}
	}
	return pids
}

// commOf returns the command name (comm) of a pid via `ps -o comm=`, trimmed to
// its basename. Empty string on any failure.
func (c *Client) commOf(ctx context.Context, pid int) string {
	out, err := c.runner.Run(ctx, remoteWrap(c.host, "ps -o comm= -p "+strconv.Itoa(pid)))
	if err != nil {
		return ""
	}
	comm := strings.TrimSpace(out)
	// `ps -o comm=` may print an absolute path on some platforms; take the base.
	if i := strings.LastIndexByte(comm, '/'); i >= 0 {
		comm = comm[i+1:]
	}
	return comm
}

// atoiOr parses s as an int, returning def on failure.
func atoiOr(s string, def int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return v
	}
	return def
}
