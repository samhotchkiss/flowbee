package watchdog

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/tmuxio"
	"github.com/samhotchkiss/flowbee/internal/verbs"
)

// randNonce returns a short random hex token for the remote-host confirmation marker.
// crypto/rand failure falls back to a fixed token — the nonce is a de-dup anchor, not a
// secret, so a non-unique fallback only risks matching stale scrollback, never safety.
func randNonce() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}

// The LAUNCH LADDER (plan §15.15) supervises an epic launch through a LOCAL tmux
// session on the control-plane box — the operator's single attachable pane of glass —
// driving a staged state machine where each stage is pane-CLASSIFIED before the next
// fires. It DELIBERATELY REVERSES Phase 2's remote-tmux/BatchMode one-shot
// (LaunchEpicSession, retained above for the current cmd/flowbee caller until Phase 6b
// rewires it): the agent lives in a REMOTE tmux that survives disconnects, and the
// local pane is only the attachment. Composed entirely from merged tmuxio primitives
// (NewSession + verified Send + Classify) + the family verb table; it needs NO new
// tmux transport (the reviewer-endorsed SessionSpec.RemoteHost deletion stands — the
// ladder is orchestration, not transport).
//
// The per-op ssh FALLBACK control path (runner.go's remoteWrap, retained) is
// unchanged: goal_sessions.box still records the box, so a lost local attachment can be
// re-driven per-op against the remote tmux — dual-path supervision.
//
// EXACT-MATCH TARGETS: bare `tmux -t <name>` PREFIX-matches session names (`-t epic-fix`
// can hit `epic-fix-v2`). tmuxio.Client already forces an exact match INTERNALLY —
// every Send/Capture/KillSession runs its target through tmuxio.exactTarget, which
// produces the pane-parser-valid `=name:` form — so this ladder passes BARE session
// names and must NOT pre-`=`-prefix them (a lone `=name` with no trailing colon is
// rejected by tmux's target-pane parser: "can't find pane"). NewSession's -s create
// name and the remote-side `tmux new -A -s <name>` are likewise bare (exact by
// construction).

// LaunchStage names each rung of the ladder, so a failure reports EXACTLY where it
// stopped and the caller (Phase 6b) can raise launch_failed with that stage.
type LaunchStage string

const (
	StageCreateLocal      LaunchStage = "create_local"       // create the local epic-<slug> session
	StageRemoteAttach     LaunchStage = "remote_attach"      // send ssh -t … tmux new -A (remote seat only)
	StageAwaitRemoteShell LaunchStage = "await_remote_shell" // classify-wait for the remote shell (remote only)
	StageSendCLI          LaunchStage = "send_cli"           // send the seat env + agent CLI line
	StageAwaitPrompt      LaunchStage = "await_prompt"       // classify-wait IDLE_AT_PROMPT (CLI up)
	StageSendGoal         LaunchStage = "send_goal"          // verified-send the family Launch prompt
	StageConfirmWorking   LaunchStage = "confirm_working"    // classify-confirm WORKING
	StageDone             LaunchStage = "done"               // verified
)

// LaunchOutcome is the ladder's typed verdict.
type LaunchOutcome string

const (
	// LaunchVerified: every stage passed and the pane classified WORKING.
	LaunchVerified LaunchOutcome = "verified"
	// LaunchAwaitingAuth: an INTERACTIVE auth prompt (password/2FA) appeared at the
	// remote-attach stage — NOT a failure (BatchMode is deliberately not used here). The
	// human answers once in the pane; the caller routes a launch-stage attention item.
	// The local session is LEFT ALIVE for the human to complete the auth.
	LaunchAwaitingAuth LaunchOutcome = "awaiting_auth"
	// LaunchFailed: a stage timed out or misclassified. The ladder best-effort kills the
	// local session it created; the caller releases seat/host/scope (launching-reaper).
	LaunchFailed LaunchOutcome = "failed"
)

// LaunchSeat is the VALIDATED seat inputs the ladder needs. Every field originates in
// the seats registry (store.AddSeat argv-safe-validated the box / config_dir / codex_home
// / extra-env at registration) — the ladder builds the ssh + CLI lines from a CLOSED
// literal template over these fields and never from pane/scrollback content.
type LaunchSeat struct {
	Box         string // '' = a LOCAL seat (control-plane box); else the ssh destination
	AgentFamily string // claude | codex
	ConfigDir   string // CLAUDE_CONFIG_DIR (claude seats)
	CodexHome   string // CODEX_HOME (codex seats)
	Account     string // FLOWBEE_ACCOUNT value (provider:email); '' to omit
	ExtraEnv    map[string]string
}

// LadderParams configures one launch.
type LadderParams struct {
	Slug     string // epic id → session name "epic-<slug>"; caller safeSlugRe-gated
	Seat     LaunchSeat
	SpecPath string // the committed spec path (epics/…-<slug>.md); caller-registered
	Dir      string // the local pane's working dir (the epic checkout); optional

	// LoginShell is the interactive shell the local session runs so the ladder can type
	// the ssh / CLI lines into it. Default "${SHELL:-/bin/sh}" (expanded by tmux's own
	// sh, since the literal survives NewSession's shQuote — see the package trace).
	LoginShell string

	// LocalHostname is the control-plane box's hostname, used by the remote-host
	// confirmation (M3) to detect an ssh drop-back to the LOCAL shell. Default os.Hostname().
	LocalHostname string
	// RemoteMarkerNonce makes the confirmation marker unique per launch so stale
	// scrollback cannot match. Default a fresh random hex; tests inject a fixed value.
	RemoteMarkerNonce string

	// per-stage timeouts and the classify poll interval (sane defaults via withDefaults).
	ShellTimeout   time.Duration
	PromptTimeout  time.Duration
	WorkingTimeout time.Duration
	PollInterval   time.Duration
}

func (p LadderParams) withDefaults() LadderParams {
	if p.LoginShell == "" {
		p.LoginShell = "${SHELL:-/bin/sh}"
	}
	if p.LocalHostname == "" {
		p.LocalHostname, _ = os.Hostname() // "" on failure ⇒ confirmRemoteHost FAILS CLOSED (refuses to launch remotely)
	}
	if p.RemoteMarkerNonce == "" {
		p.RemoteMarkerNonce = randNonce()
	}
	if p.ShellTimeout <= 0 {
		p.ShellTimeout = 30 * time.Second
	}
	if p.PromptTimeout <= 0 {
		p.PromptTimeout = 60 * time.Second
	}
	if p.WorkingTimeout <= 0 {
		p.WorkingTimeout = 30 * time.Second
	}
	if p.PollInterval <= 0 {
		p.PollInterval = 2 * time.Second
	}
	return p
}

// LadderResult is the ladder's typed progress: the outcome, the stage reached (or where
// it stopped), and human-readable evidence for the operator/ledger.
type LadderResult struct {
	Outcome  LaunchOutcome
	Stage    LaunchStage
	Session  string // the local tmux session name (epic-<slug>), the iTerm-focus target
	Evidence string
}

// RunLadder drives the staged launch. client MUST be a LOCAL tmuxio.Client (host="" —
// the control-plane box; the ladder puts the agent on the far side itself, via the
// typed ssh line, not via a WithHost client). clk is the same clock the client was built
// with (tests inject one fake for both); it bounds the classify-waits. A non-nil error
// is an INFRASTRUCTURE failure (tmux itself could not be driven); the launch semantics
// are in LadderResult.Outcome.
func RunLadder(ctx context.Context, client *tmuxio.Client, clk tmuxio.Clock, params LadderParams) (LadderResult, error) {
	p := params.withDefaults()
	family, err := verbs.For(p.Seat.AgentFamily)
	if err != nil {
		return LadderResult{Outcome: LaunchFailed, Stage: StageCreateLocal}, err
	}
	session := "epic-" + p.Slug
	// BARE name: tmuxio.Client exactifies every target internally to `=name:` (a lone
	// `=name` is rejected by tmux's pane parser), so the ladder must never pre-prefix.
	target := session

	// Stage 1: create the LOCAL session running an interactive shell.
	if err := client.NewSession(ctx, tmuxio.SessionSpec{
		Name: session, Command: p.LoginShell, StartDir: p.Dir, WindowName: p.Slug,
	}); err != nil {
		return LadderResult{Outcome: LaunchFailed, Stage: StageCreateLocal, Session: session}, fmt.Errorf("create local session: %w", err)
	}

	// Stages 2–3 (remote seat only): send the ssh attach line, then wait for the remote
	// shell — with an AWAITING_INPUT there classified as interactive auth (not a fail).
	if p.Seat.Box != "" {
		if res, done, err := runRemoteAttach(ctx, client, clk, p, session, target); done {
			return res, err
		}
	}

	// Stage 4: send the seat env + agent CLI line into the shell.
	cliLine := buildCLILine(p.Seat, family.Family())
	if r, err := client.Send(ctx, target, cliLine, tmuxio.SendOptions{}); err != nil {
		return killAndFail(ctx, client, session, target, StageSendCLI, "send CLI line: "+err.Error()), nil
	} else if r.Verification == tmuxio.Failed {
		return killAndFail(ctx, client, session, target, StageSendCLI, "CLI line left unsubmitted: "+r.Evidence), nil
	}

	// Stage 5: wait for the agent CLI to come up idle at its prompt.
	if st, matched, err := classifyWait(ctx, client, target, clk, p.PromptTimeout, p.PollInterval, tmuxio.StateIdleAtPrompt); err != nil {
		return killAndFail(ctx, client, session, target, StageAwaitPrompt, "await CLI prompt: "+err.Error()), nil
	} else if !matched {
		return killAndFail(ctx, client, session, target, StageAwaitPrompt,
			fmt.Sprintf("CLI did not reach an idle prompt (last state %q)", st)), nil
	}

	// Stage 6: verified-send the family Launch prompt (the goal).
	launch, err := family.Launch(p.SpecPath, p.Slug)
	if err != nil {
		return killAndFail(ctx, client, session, target, StageSendGoal, "resolve launch verb: "+err.Error()), nil
	}
	if r, err := client.Send(ctx, target, launch.Text, tmuxio.SendOptions{}); err != nil {
		return killAndFail(ctx, client, session, target, StageSendGoal, "send goal: "+err.Error()), nil
	} else if r.Verification == tmuxio.Failed {
		return killAndFail(ctx, client, session, target, StageSendGoal, "goal left unsubmitted: "+r.Evidence), nil
	}

	// Stage 7: confirm the agent is WORKING.
	if st, matched, err := classifyWait(ctx, client, target, clk, p.WorkingTimeout, p.PollInterval, tmuxio.StateWorking); err != nil {
		return killAndFail(ctx, client, session, target, StageConfirmWorking, "confirm working: "+err.Error()), nil
	} else if !matched {
		return killAndFail(ctx, client, session, target, StageConfirmWorking,
			fmt.Sprintf("goal sent but the pane never classified working (last state %q)", st)), nil
	}

	return LadderResult{Outcome: LaunchVerified, Stage: StageDone, Session: session,
		Evidence: "launched: CLI up, goal submitted, pane classified working"}, nil
}

// runRemoteAttach runs the remote-seat stages 2–3. It returns done=true (and the result)
// only when the ladder must STOP here (awaiting auth, or a stage failure); done=false
// means the remote shell arrived and the ladder should continue to stage 4.
func runRemoteAttach(ctx context.Context, client *tmuxio.Client, clk tmuxio.Clock, p LadderParams, session, target string) (LadderResult, bool, error) {
	// Stage 2: send the ssh attach line into the local pane. -t forces a PTY (so an auth
	// prompt renders); tmux new -A -s creates-or-attaches by EXACT name on the far box.
	sshLine := buildSSHAttachLine(p.Seat.Box, session)
	if r, err := client.Send(ctx, target, sshLine, tmuxio.SendOptions{}); err != nil {
		return killAndFail(ctx, client, session, target, StageRemoteAttach, "send ssh attach: "+err.Error()), true, nil
	} else if r.Verification == tmuxio.Failed {
		return killAndFail(ctx, client, session, target, StageRemoteAttach, "ssh attach line left unsubmitted: "+r.Evidence), true, nil
	}

	// Stage 3: wait for the remote shell — OR an interactive auth prompt.
	ready, auth, last, err := awaitRemoteShell(ctx, client, clk, target, p.ShellTimeout, p.PollInterval)
	if err != nil {
		return killAndFail(ctx, client, session, target, StageAwaitRemoteShell, "await remote shell: "+err.Error()), true, nil
	}
	switch {
	case auth:
		// Interactive auth (password/2FA) — a HUMAN answers once in the pane. Leave the
		// session ALIVE and route a launch-stage attention item (caller's job).
		return LadderResult{Outcome: LaunchAwaitingAuth, Stage: StageRemoteAttach, Session: session,
			Evidence: "remote box is prompting for interactive auth (" + last + ") — a human must answer in the pane"}, true, nil
	case !ready:
		return killAndFail(ctx, client, session, target, StageAwaitRemoteShell,
			"remote shell did not arrive (last: "+last+")"), true, nil
	}

	// A shell-looking prompt appeared — but looksLikeShell CANNOT tell the remote shell
	// from the LOCAL control-plane shell (both end in `$`/`%`). If the ssh line
	// instant-exited (conn refused / host-key mismatch / net drop / auth exhaustion),
	// control fell back to the LOCAL prompt; proceeding would type the CLI launch line
	// into the WRONG (local) shell — and if the control-plane box is ALSO a seat host,
	// claude would launch locally and reach WORKING while bound to a REMOTE seat (a
	// wrong-box scheduling hazard). Confirm we are on the far box by echoing a per-launch
	// marker suffixed with $(hostname) and requiring a host that DIFFERS from the local
	// one (plan §15.15 M3).
	confirmed, cev, cerr := confirmRemoteHost(ctx, client, clk, session, p.RemoteMarkerNonce, p.LocalHostname, p.ShellTimeout, p.PollInterval)
	if cerr != nil {
		return killAndFail(ctx, client, session, target, StageAwaitRemoteShell, "confirm remote host: "+cerr.Error()), true, nil
	}
	if !confirmed {
		return killAndFail(ctx, client, session, target, StageAwaitRemoteShell,
			"remote-host confirmation failed ("+cev+") — refusing to type the launch line into a possibly-local shell"), true, nil
	}
	return LadderResult{}, false, nil
}

// confirmRemoteHost proves the arrived shell is on the FAR box, not the local
// control-plane shell an instant-exited ssh would drop back to (plan §15.15 M3). It
// echoes `<marker>_$(hostname)` into the pane — $(hostname) is evaluated by whatever
// shell we actually landed in — and reads back the resolved host: a host EQUAL to the
// local one means ssh dropped to the local shell (confirmed=false); a DIFFERENT host is
// the remote box (confirmed=true). The marker carries a per-launch nonce so a prior
// launch's scrollback cannot match. Returns (confirmed, evidence, infra-error).
//
// FAIL-CLOSED on an unknown local hostname: if localHost is "" (os.Hostname() failed),
// it CANNOT tell local from remote, so it refuses to confirm — a control plane that
// cannot name itself must not launch remotely (a guard that silently disabled itself
// would restore the M3 race exactly when we can't identify ourselves).
//
// ACCEPTED LIMITATION (fail-closed): if a remote box genuinely SHARES the local hostname
// (two machines both named e.g. "mac-mini"), host==localHost reads as a local drop-back
// and a LEGITIMATE remote seat can never launch. That is the safe direction, and the
// evidence names it explicitly so an operator can rename the box or set a distinct
// LadderParams.LocalHostname. (A future refinement could match an expected remote
// hostname recorded at seat registration for an exact-match confirmation.)
func confirmRemoteHost(ctx context.Context, client *tmuxio.Client, clk tmuxio.Clock, session, nonce, localHost string, timeout, interval time.Duration) (bool, string, error) {
	if localHost == "" {
		// fail closed: we cannot distinguish local from remote without our own name.
		return false, "cannot verify remote host: local hostname unknown (os.Hostname failed)", nil
	}
	marker := remoteMarkerPrefix + nonce
	if _, err := client.Send(ctx, session, "echo "+marker+"_$(hostname)", tmuxio.SendOptions{}); err != nil {
		return false, "", err
	}
	deadline := clk.Now().Add(timeout)
	for {
		capt, err := client.Capture(ctx, session, 0)
		if err != nil {
			return false, "", err
		}
		if host, ok := extractMarkerHost(capt.Raw, marker); ok {
			if host == localHost {
				return false, "remote host name " + host + " matches the local host — an ssh drop-back to the local shell, OR the remote box shares this hostname (rename the box or set a distinct LocalHostname)", nil
			}
			return true, "remote host " + host + " confirmed", nil
		}
		if !clk.Now().Before(deadline) {
			return false, "remote-host marker never appeared", nil
		}
		clk.Sleep(ctx, interval)
	}
}

// remoteMarkerPrefix anchors the remote-host confirmation marker line in a capture.
const remoteMarkerPrefix = "FLOWBEE_REMOTE_"

// extractMarkerHost finds the RESOLVED marker line (`<marker>_<host>`) in a capture and
// returns the host token. It skips the command ECHO line itself — the line still holding
// the literal `$(hostname)` / `echo ` — so it reads the shell's OUTPUT, not the input.
func extractMarkerHost(capture, marker string) (string, bool) {
	needle := marker + "_"
	for _, ln := range strings.Split(capture, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.Contains(ln, "$(") || strings.Contains(ln, "echo ") {
			continue // the typed command, not its output
		}
		i := strings.Index(ln, needle)
		if i < 0 {
			continue
		}
		host := ln[i+len(needle):]
		if sp := strings.IndexAny(host, " \t"); sp >= 0 {
			host = host[:sp]
		}
		if host != "" {
			return host, true
		}
	}
	return "", false
}

// awaitRemoteShell polls until the remote shell is ready, an interactive auth prompt
// appears, or the timeout. The tmuxio classifier is tuned for AGENT panes — a bare
// remote shell prompt reads as Unknown and an ssh password/2FA prompt matches NO agent
// rule — so this stage uses two positive heuristics over the raw capture: an auth-prompt
// pattern (looksLikeAuthPrompt) short-circuits to the human, and a shell-prompt pattern
// (looksLikeShell) means the remote shell arrived. A tmuxio AWAITING_INPUT (a menu) also
// counts as auth-adjacent (something interactive is blocking). Returns (ready, auth, the
// last non-empty line for evidence, infra error).
func awaitRemoteShell(ctx context.Context, client *tmuxio.Client, clk tmuxio.Clock, target string, timeout, interval time.Duration) (ready, auth bool, lastLine string, err error) {
	deadline := clk.Now().Add(timeout)
	for {
		capt, cerr := client.Capture(ctx, target, 0)
		if cerr != nil {
			return false, false, lastLine, cerr
		}
		lastLine = lastNonEmpty(capt.Raw)
		if looksLikeAuthPrompt(capt.Raw) {
			return false, true, lastLine, nil
		}
		if st, _ := tmuxio.Classify(capt.Raw); st == tmuxio.StateAwaitingInput {
			return false, true, lastLine, nil
		}
		if looksLikeShell(capt.Raw) {
			return true, false, lastLine, nil
		}
		if !clk.Now().Before(deadline) {
			return false, false, lastLine, nil
		}
		clk.Sleep(ctx, interval)
	}
}

// classifyWait polls the pane until Classify returns want, an AWAITING_INPUT hazard
// appears, or the timeout. It returns the last observed state and whether want matched.
// An AWAITING_INPUT during a prompt/working wait is a launch HAZARD (a dialog swallowing
// the launch), so it stops the wait with matched=false — the caller fails the stage.
func classifyWait(ctx context.Context, client *tmuxio.Client, target string, clk tmuxio.Clock, timeout, interval time.Duration, want tmuxio.State) (tmuxio.State, bool, error) {
	deadline := clk.Now().Add(timeout)
	var last tmuxio.State
	for {
		capt, err := client.Capture(ctx, target, 0)
		if err != nil {
			return last, false, err
		}
		st, _ := tmuxio.Classify(capt.Raw)
		last = st
		if st == want {
			return st, true, nil
		}
		if st == tmuxio.StateAwaitingInput && want != tmuxio.StateAwaitingInput {
			return st, false, nil // a dialog is on screen — not the state we wanted
		}
		if !clk.Now().Before(deadline) {
			return st, false, nil
		}
		clk.Sleep(ctx, interval)
	}
}

// killAndFail best-effort kills the local session the ladder created (rolling back the
// tmux side of a failed launch — the caller still releases the DB-side seat/host/scope)
// and returns a Failed result stamped with the stage + evidence.
func killAndFail(ctx context.Context, client *tmuxio.Client, session, target string, stage LaunchStage, evidence string) LadderResult {
	_ = client.KillSession(ctx, target) // best-effort; a not-yet-created session is fine
	return LadderResult{Outcome: LaunchFailed, Stage: stage, Session: session, Evidence: evidence}
}

// buildSSHAttachLine builds the remote-attach shell line typed into the local pane:
// `ssh -t -- <box> tmux new -A -s epic-<slug>`. box is argv-safe (seat registration);
// -t forces a PTY so an auth prompt renders; `--` guards a leading-dash box; `new -A -s`
// creates-or-attaches by EXACT name (no prefix match). CLOSED template — no pane content.
func buildSSHAttachLine(box, session string) string {
	return "ssh -t -- " + box + " tmux new -A -s " + session
}

// buildCLILine builds the seat's env + agent CLI launch line typed into the shell:
// `CLAUDE_CONFIG_DIR=<dir> [FLOWBEE_ACCOUNT=<acct>] [extra…] claude` (or the codex
// equivalent). Every interpolated value is argv-safe (seat registration), so the line
// needs no shell quoting; the binary is a per-family literal. CLOSED template.
func buildCLILine(seat LaunchSeat, family verbs.Family) string {
	var parts []string
	switch family {
	case verbs.FamilyCodex:
		parts = append(parts, "CODEX_HOME="+seat.CodexHome)
	default: // claude
		parts = append(parts, "CLAUDE_CONFIG_DIR="+seat.ConfigDir)
	}
	if seat.Account != "" {
		parts = append(parts, "FLOWBEE_ACCOUNT="+seat.Account)
	}
	for _, k := range sortedKeys(seat.ExtraEnv) {
		parts = append(parts, k+"="+seat.ExtraEnv[k])
	}
	parts = append(parts, binaryFor(family))
	return strings.Join(parts, " ")
}

// binaryFor returns the fixed CLI binary for a family (a closed literal, never derived).
func binaryFor(family verbs.Family) string {
	if family == verbs.FamilyCodex {
		return "codex"
	}
	return "claude"
}

// sortedKeys returns a map's keys sorted, so the CLI line renders deterministically.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// small n; insertion sort keeps this dependency-free and stable.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// looksLikeShell reports whether a captured pane's last non-empty line looks like an
// interactive shell prompt (ends in one of the common prompt terminators $ % # > ] )
// or the starship/powerline glyphs ❯ › »). It only GATES when to attempt the remote-host
// confirmation (confirmRemoteHost); that marker echo — not this glyph guess — is the
// AUTHORITATIVE arrival proof, so a false positive here merely wastes one confirmation
// attempt (the marker will not resolve) rather than mis-driving a launch. Kept broad on
// purpose so an unusual PS1 does not strand a legit seat.
func looksLikeShell(capture string) bool {
	t := lastNonEmpty(capture)
	if t == "" {
		return false
	}
	switch t[len(t)-1:] {
	case "$", "%", "#", ">", "]", ")":
		return true
	}
	return strings.HasSuffix(t, "❯") || strings.HasSuffix(t, "›") || strings.HasSuffix(t, "»")
}

// authPromptPatterns are the common interactive-auth prompts an `ssh -t` shows before a
// remote shell — a password/passphrase/2FA the human must answer once in the pane. They
// are matched case-insensitively over the pane's last few lines. Conservative: only
// well-known auth phrasings match; anything else is left to time out as "shell absent".
var authPromptPatterns = []string{
	"password:",
	"password for",
	"passphrase",
	"verification code",
	"one-time",
	"2fa",
	"otp",
	"(yes/no", // host-key TOFU prompt (also interactive)
	"authenticity of host",
	"duo",
	"touch your", // security-key touch prompt
}

// looksLikeAuthPrompt reports whether the pane's bottom shows an interactive auth prompt.
// This is a best-effort HEURISTIC, not a guarantee: a localized or unusual prompt (a bare
// "Token:", a non-English password prompt) may miss. That is low-risk by construction —
// an auth prompt ends in ':' (not a shell glyph), so a miss leaves the ladder timing out
// at StageAwaitRemoteShell rather than typing the CLI line into a password field.
func looksLikeAuthPrompt(capture string) bool {
	lines := strings.Split(capture, "\n")
	from := len(lines) - 6
	if from < 0 {
		from = 0
	}
	hay := strings.ToLower(strings.Join(lines[from:], "\n"))
	for _, pat := range authPromptPatterns {
		if strings.Contains(hay, pat) {
			return true
		}
	}
	return false
}

// lastNonEmpty returns the last line of a capture with non-whitespace content, trimmed.
func lastNonEmpty(capture string) string {
	lines := strings.Split(capture, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}
