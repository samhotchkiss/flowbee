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

// ── epic-lane Phase 2 additions ──
//
// The closed verb set above (capture-pane / send "/goal resume" / bare Enter) was
// Phase 1's whole surface. Phase 2 deliberately extends it — the design doc calls
// out "this is the ONE other permitted send-keys payload" — to launch a NEW tmux
// session and send an epic's goal text, plus a handful of read-only PREFLIGHT
// checks (gh auth, disk space, checkout presence) that never touch tmux at all,
// and a best-effort kill-session used ONLY on a failed launch's rollback path.
//
// TRUST BOUNDARY (corrected per review MAJOR M2 — this comment originally
// overclaimed "never from epic file content"): most inputs are the caller's own
// trusted values (a registered host name, a slug already gated by safeSlugRe, a
// goal-text TEMPLATE the launcher builds), but TWO fields DO originate in the
// epic file's frontmatter — `agent:` (which becomes NewTmuxSessionCmd's
// shell-executed startCmd) and `host:`. Both are therefore validated by the
// caller BEFORE they reach these builders: agent against cmd/flowbee's strict
// safeAgentRe charset, host against the epic_hosts registry (a name an operator
// explicitly `flowbee host add`-ed). shQuote alone is NOT sufficient for
// startCmd — it is handed to tmux as the session's command and executed BY A
// SHELL on the box, so a frontmatter `agent: "codex; curl …|sh"` would be remote
// code execution without that upstream validation. Nothing here is ever
// assembled from anything read off a pane, scrollback, or an epic file's body.

// HomeDirCmd resolves a box's home directory as a LITERAL path (`echo $HOME`,
// expanded by the target shell since it's embedded unquoted in the whole inner
// command, not inside a shQuote'd argument). The epic launcher calls this ONCE per
// launch and builds the checkout path (home + "/epics/" + repoID) from the
// returned literal string — deliberately NOT by embedding "$HOME" itself inside a
// path that other commands later shQuote() as an argument, since shQuote's single
// quotes would suppress the shell's own variable expansion right when it's needed.
func HomeDirCmd(box string) string {
	return remoteWrap(box, "echo $HOME")
}

// GhAuthStatusCmd checks whether the box's `gh` CLI is authenticated — the epic
// preflight's first gate (epics/INSTRUCTIONS.md's Finish step opens a PR via `gh`,
// so an epic that can't authenticate now will just fail hours or days later at the
// worst possible moment). A non-nil Runner error (nonzero exit) means NOT
// authenticated; the caller decides what to do with that.
func GhAuthStatusCmd(box string) string {
	return remoteWrap(box, "gh auth status")
}

// DiskFreeKBCmd reports free disk space (in KB) at path via `df`, as a bare
// number the caller parses — the epic preflight's "disk ≥10G free" gate.
func DiskFreeKBCmd(box, path string) string {
	return remoteWrap(box, "df -Pk -- "+shQuote(path)+" 2>/dev/null | tail -n1 | awk '{print $4}'")
}

// RepoCheckoutExistsCmd reports "yes"/"no" for whether a git checkout already
// exists at path — the epic preflight's "repo checkout exists or clone it fresh"
// branch point.
func RepoCheckoutExistsCmd(box, path string) string {
	return remoteWrap(box, "test -d "+shQuote(path)+"/.git && echo yes || echo no")
}

// CloneRepoCmd clones ownerRepo ("owner/repo") to path via `gh repo clone` —
// deliberately `gh`, not a raw `git clone` with a token-bearing URL: the preflight
// already required `gh auth status` to pass, so the same credential (never placed
// in argv, unlike a token-bearing https URL would be — `ps` on the remote box is
// world-readable) clones the epic's checkout. mkdir -p's the parent first since
// the ~/epics/ convention directory may not exist yet on a freshly provisioned box.
func CloneRepoCmd(box, ownerRepo, path string) string {
	return remoteWrap(box, "mkdir -p -- "+shQuote(parentDirUnix(path))+
		" && gh repo clone "+shQuote(ownerRepo)+" "+shQuote(path)+" -- --quiet")
}

// TimezoneCmd probes a box's IANA timezone name (the `flowbee epic start --tz`
// auto-detect path, per the task brief's "get tz from `ssh <host> date` zone").
// timedatectl is the modern/reliable source on systemd Linux; /etc/timezone and a
// bare `date +%Z` (which yields an abbreviation like "MST", NOT an IANA name, but
// is the last-resort signal something responded at all) are fallbacks for boxes
// without systemd. The caller validates the result via time.LoadLocation and
// falls back to "" (assume serve-local, the documented goal_sessions default) on
// anything that doesn't resolve — mirroring AddGoalSession's own tz validation.
func TimezoneCmd(box string) string {
	return remoteWrap(box, "timedatectl show --property=Timezone --value 2>/dev/null || "+
		"cat /etc/timezone 2>/dev/null || date +%Z")
}

// NewTmuxSessionCmd creates a fresh DETACHED tmux session running startCmd in dir
// — the epic launch's first step (`tmux new-session -d -s <name> -c <dir>
// '<startCmd>'`). startCmd is the coding agent's own launch invocation (e.g.
// "codex"), sourced from the epic's `agent:` frontmatter or the --agent flag —
// i.e. PARTLY FROM EPIC FILE CONTENT, and executed by a shell on the box, so the
// caller MUST have validated it against a strict charset first (safeAgentRe in
// cmd/flowbee/epic.go; see the trust-boundary section doc above — review M2).
func NewTmuxSessionCmd(box, tmuxName, dir, startCmd string) string {
	return remoteWrap(box, "tmux new-session -d -s "+shQuote(tmuxName)+
		" -c "+shQuote(dir)+" "+shQuote(startCmd))
}

// KillTmuxSessionCmd kills a tmux session by name — used ONLY on the failed-launch
// ROLLBACK path (review m7): a launch that created the session but then failed to
// send its goal would otherwise leak the session, and a same-slug retry would then
// permanently fail on tmux's duplicate-session error. Best-effort by contract (the
// caller logs but ignores a failure — the session may legitimately not exist when
// the failure was at create time). NEVER used on a live epic: `flowbee epic
// abandon` deliberately leaves the session running (operator decision).
func KillTmuxSessionCmd(box, tmuxName string) string {
	return remoteWrap(box, "tmux kill-session -t "+shQuote(tmuxName))
}

// SendGoalCmd sends literal text + Enter into an existing tmux pane — the ONE
// additional send-keys payload epic-lane Phase 2 adds to the closed verb set (see
// the section doc above). text is always the fixed "/goal execute the epic at
// epics/<file>.md per epics/INSTRUCTIONS.md. Work on branch epic/<slug>." template
// the launcher builds from trusted inputs (repo id, slug), mirroring sendResumeCmd's
// shape exactly but parameterized since the payload differs per epic.
func SendGoalCmd(box, tmuxName, text string) string {
	return remoteWrap(box, "tmux send-keys -t "+shQuote(tmuxName)+" "+shQuote(text)+" Enter")
}

// parentDirUnix returns the parent directory of a UNIX-style (forward-slash) path,
// without relying on path/filepath (which is OS-path-separator-aware and would be
// wrong when this control plane runs on a different OS than the remote box). A
// path with no "/" has no parent worth creating ("."; mkdir -p . is a harmless
// no-op).
func parentDirUnix(p string) string {
	if idx := strings.LastIndexByte(p, '/'); idx > 0 {
		return p[:idx]
	}
	return "."
}

// remoteWrap wraps inner in an ssh invocation when box is non-empty (” == local,
// matching the Repo.DefaultBranch-style convention used elsewhere in the store).
// The ssh form from the task brief: BatchMode (never prompt/hang on a password/
// host-key question — this runs unattended) + a short ConnectTimeout (a downed box
// fails fast into the consecutive_failures counter instead of stalling the whole
// watch pass) + `--` before the host (adversarial-review MAJOR #3): shQuote stops
// SHELL injection, but ssh's own getopt still reads a leading-dash "host" as an
// OPTION — a box registered as `-oProxyCommand=...` would be local RCE. The `--`
// end-of-options marker makes the next argv element unconditionally the hostname.
// (Registration-time validation in store.AddGoalSession additionally rejects
// leading-dash / whitespace values — defense in depth, not the primary fix.)
func remoteWrap(box, inner string) string {
	if box == "" {
		return inner
	}
	return "ssh -o BatchMode=yes -o ConnectTimeout=5 -- " + shQuote(box) + " " + shQuote(inner)
}

// shQuote single-quotes s for safe embedding in an `sh -c` string, escaping any
// embedded single quotes. Standard POSIX shell-quoting trick: close the quote,
// emit an escaped literal quote, reopen.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
