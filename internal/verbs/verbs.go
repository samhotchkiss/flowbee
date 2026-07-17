// Package verbs is the per-agent-family control-plane VERB TABLE (epic-lane Phase 5,
// plan §1.7). The control verbs `/goal resume` and the `/goal execute …` launch
// payload are CODEX builtins; the master and many epics run Claude Code, whose
// goal/loop mechanisms differ. Hardcoding `/goal` would type Codex slash-commands into
// Claude panes — the keystroke analogue of the migration-number bug the numbering
// discipline exists to prevent. This table resolves every control-plane send through
// the target's recorded `agent` family, so no call site ever hardcodes a family literal.
//
// PROVIDER-NEUTRALITY (plan §14): every provider/keystroke literal in the codebase's
// control surface is confined to THIS one table. Flowbee's neutrality lint
// (internal/flow.LintNeutrality) is a parser-level check over flows/flows.yaml's Config
// (role/stage/when/independence positions) — it does NOT scan Go source, so this table
// needs no lint-exception registration; the discipline is honored structurally by
// keeping the literals here and nowhere else. tools/archcheck (the deterministic-core
// import boundary) is likewise satisfied: this package imports only stdlib.
//
// DATA vs VERBS: a master's free-text reply is DATA delivered as a bracketed paste and
// never routed through this table (plan §1.5). Only the CLOSED control set — resume,
// launch, bare-Enter nudge, escape-modal, clear-context — is a verb.
//
// INPUT CONTRACT (what a caller must guarantee): every Send this package returns is
// delivered into a pane via the tmuxio verified-send primitive as a BRACKETED PASTE, so
// its Text is inert data — no byte is interpreted as a terminal control sequence. The
// interpolated parameters are the CALLER's responsibility to validate BEFORE they reach
// here: Launch's `slug` is a cmd/flowbee safeSlugRe-gated epic id and `specPath` is a
// registered epics/ path (never pane/scrollback/epic-body content); NotifyMaster's
// `topKind` is validated here against the closed enum and its `count` is clamped to ≥0.
// This package performs NO shell/argv quoting — it emits payloads, not commands.
package verbs

import (
	"errors"
	"fmt"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

// Family is a coding-agent family (the epics.agent / supervisors.model_family value).
type Family string

const (
	FamilyCodex  Family = "codex"
	FamilyClaude Family = "claude"
	// FamilyGrok is xAI's Grok Build CLI (grok-4.5). Live-confirmed against the seat box
	// (grok1@localhost, /opt/homebrew/bin/grok, config dir ~/.grok / $GROK_HOME): it has a
	// `/goal` autonomous-goal builtin (so Launch/Resume are SUPPORTED like codex) and a
	// `/clear` context reset (an alias of /new). Its ONE keystroke divergence — Esc is a
	// no-op, cancel is Ctrl+C — is handled in EscapeModal below.
	FamilyGrok Family = "grok"
)

// ErrUnknownFamily is returned by For when the family is not one of the known families.
var ErrUnknownFamily = errors.New("verbs: unknown agent family")

// ErrInvalidKind is returned by NotifyMaster when topKind is not a member of the closed
// attention-kind enum — the push-to-wake template must NEVER carry free text (plan §15.10).
var ErrInvalidKind = errors.New("verbs: top kind is not a known attention kind")

// ErrUnsupported is returned by a verb that has NO real equivalent for a family. It is
// deliberate: a wrong guessed keystroke is worse than a typed "unsupported" the caller
// must handle (plan §1.7). It is currently RESERVED — no verb returns it today (every
// family has a real Resume: codex/grok `/goal resume`, claude the literal `CONTINUE`), but
// it remains part of the closed verb API so a future family with a genuinely-absent verb
// can signal it rather than emit a bogus key, and callers may branch on it defensively.
var ErrUnsupported = errors.New("verbs: no in-pane verb for this agent family")

// Send is a RESOLVED control-plane keystroke payload — exactly what the caller hands to
// the tmuxio verified-send primitive. Either Text (typed, optionally submitted with a
// trailing Enter) or Key (a single named tmux key) is set, never both.
type Send struct {
	// Text is literal text to type into the pane's input line (empty for a bare key).
	Text string
	// SubmitEnter is whether a trailing Enter submits Text (a slash-command / prompt).
	SubmitEnter bool
	// Key is a single named tmux key to send with no text (e.g. "Enter", "Escape").
	Key string
}

// Verbs is the resolved verb table for one agent family. Obtain it via For(family).
// The methods return the exact Send (or a typed error) for each control action; the
// literals live in the package tables below and nowhere else.
type Verbs struct {
	family Family
}

// For resolves the verb table for an agent family (case-insensitive, trimmed). The
// family string is the epics.agent / supervisors.model_family value the target recorded.
func For(family string) (Verbs, error) {
	switch Family(strings.ToLower(strings.TrimSpace(family))) {
	case FamilyCodex:
		return Verbs{family: FamilyCodex}, nil
	case FamilyClaude:
		return Verbs{family: FamilyClaude}, nil
	case FamilyGrok:
		return Verbs{family: FamilyGrok}, nil
	default:
		return Verbs{}, fmt.Errorf("%w: %q", ErrUnknownFamily, family)
	}
}

// Family returns the resolved family.
func (v Verbs) Family() Family { return v.family }

// Resume resolves the "continue a stopped/paused goal" verb.
//   - codex:  `/goal resume` (the Codex builtin; copied verbatim from the watchdog's
//     closed verb set — internal/watchdog/runner.go sendResumeCmd).
//   - grok:   `/goal resume` — grok's `/goal` autonomous-goal builtin has a `resume`
//     subcommand (live-confirmed: `/goal` autocompletes to "Set, manage, or check an
//     autonomous goal"), so resume is SUPPORTED, exactly like codex. Same spelling as
//     codex, resolved through the table for a future split.
//   - claude: the literal word `CONTINUE` + Enter. This is the OPERATOR-CONFIRMED max-out
//     recovery: when a Claude Code session hits its USAGE CAP it STOPS the goal, and the
//     resume is to type `CONTINUE` into the pane (Claude Code has no `/goal resume`
//     slash-command, but this literal DOES resume a usage-blocked stopped task — the
//     load-bearing input the supervision watchdog types, timed off acctprobe resets_at,
//     to auto-recover a maxed-out claude session or master pane, plan §15.17). A fixed
//     literal keystroke sequence in the closed verb set — never derived from pane content.
func (v Verbs) Resume() (Send, error) {
	switch v.family {
	case FamilyCodex, FamilyGrok:
		return Send{Text: "/goal resume", SubmitEnter: true}, nil
	default: // FamilyClaude
		return Send{Text: "CONTINUE", SubmitEnter: true}, nil
	}
}

// Launch resolves the "start this epic" first payload for a fresh session. specPath is
// the epic's committed spec path (epics/YYYY-MM-DD-<slug>.md) and slug is its id.
//   - codex:  `/goal execute the epic at <specPath> per epics/INSTRUCTIONS.md. Work on
//     branch epic/<slug>.` — the EXACT string shape hardcoded today in cmd/flowbee's
//     runEpicStart (and threaded through internal/watchdog/launch.go's SendGoalCmd).
//   - grok:   the SAME `/goal execute …` builtin shape as codex — grok has a `/goal`
//     autonomous-goal builtin (live-confirmed), so the objective typed after `/goal ` is
//     the epic instruction, identical to codex. (The interactive `grok "<prompt>" --yolo`
//     comes up bare at its `❯` box in Stage 4; the goal is typed in Stage 6 exactly like
//     codex/claude, so the positional-prompt form is not used on the ladder path.)
//   - claude: the SAME instruction WITHOUT the Codex/Grok `/goal execute ` builtin prefix —
//     Claude Code takes a plain natural-language first prompt (confirmed against the
//     docs). This is a plain-English instruction (DATA the agent acts on), not a guessed
//     slash-command, so it is a real equivalent rather than a typed-unsupported.
func (v Verbs) Launch(specPath, slug string) (Send, error) {
	switch v.family {
	case FamilyCodex, FamilyGrok:
		return Send{
			Text:        fmt.Sprintf("/goal execute the epic at %s per epics/INSTRUCTIONS.md. Work on branch epic/%s.", specPath, slug),
			SubmitEnter: true,
		}, nil
	default: // FamilyClaude
		return Send{
			Text:        fmt.Sprintf("execute the epic at %s per epics/INSTRUCTIONS.md. Work on branch epic/%s.", specPath, slug),
			SubmitEnter: true,
		}, nil
	}
}

// NudgeEnter resolves the bare-Enter nudge (the "TUI swallowed the first Enter, text is
// sitting unsubmitted" recovery — internal/watchdog's proven fix). Family-agnostic:
// Codex, Claude Code, and grok all submit with a bare Enter (grok's input bar shows
// "Enter:send", live-confirmed).
func (v Verbs) NudgeEnter() Send { return Send{Key: "Enter"} }

// EscapeModal resolves the "dismiss a modal / menu / copy-mode" verb.
//   - codex / claude: the Escape key (confirmed against the Claude Code keybindings docs —
//     Esc closes dialogs/menus).
//   - grok: Ctrl+U (kill-line), NOT Escape. This is the ONE grok keystroke divergence and
//     it is CRITICAL: in grok, Esc is a documented NO-OP (live-verified — a typed `/` and
//     its command menu both SURVIVED an Escape), so mapping EscapeModal to Esc would
//     silently fail to dismiss anything. grok's cancel is Ctrl+C, but Ctrl+C is
//     DESTRUCTIVE (it cancels the running turn, and a double Ctrl+C exits grok entirely),
//     so it must NEVER back a generic "dismiss a modal" verb. Ctrl+U clears the input
//     line, which dismisses grok's input-driven `/` command menu (the menu opens/closes
//     with the `/`-prefixed input), and is a harmless no-op on an already-clean prompt —
//     it can neither cancel a turn nor exit the app, so it "won't mis-fire". (A full
//     arrow-key picker like `/model` is a human-initiated modal outside the --yolo
//     autonomous launch path — EscapeModal is not wired onto that path today.)
func (v Verbs) EscapeModal() Send {
	if v.family == FamilyGrok {
		return Send{Key: "C-u"}
	}
	return Send{Key: "Escape"}
}

// NotifyMaster resolves the push-to-wake ping flowbee types into an IDLE registered
// master's pane so it re-polls (plan §15.10). count is the pending-item count; topKind
// is the most-urgent pending kind and is VALIDATED against the closed attention-kind
// enum first — the template must never carry free text (a hostile/unknown kind is
// rejected with ErrInvalidKind, never templated). Family-agnostic: a plain-text message,
// not a slash-command, so it is identical for Codex and Claude. The ticker that decides
// WHEN to ping is separate wiring; this only resolves the payload.
func (v Verbs) NotifyMaster(count int, topKind string) (Send, error) {
	if !attention.ValidKind(topKind) {
		return Send{}, fmt.Errorf("%w: %q", ErrInvalidKind, topKind)
	}
	if count < 0 {
		count = 0 // clamp: a negative count is never rendered into a master's pane
	}
	return Send{
		Text:        fmt.Sprintf("flowbee: %d attention items pending (top: %s). Run: flowbee master poll", count, topKind),
		SubmitEnter: true,
	}, nil
}

// ClearContext resolves the context-reset verb.
//   - claude: `/clear` (the documented Claude Code slash-command — starts a fresh
//     conversation, preserving project memory; the master's every-K-iterations reset).
//   - codex:  `/clear` (the Codex CLI builtin that starts a fresh task in the same
//     session — confirmed against the Codex CLI slash-command reference).
//   - grok:   `/clear` (live-confirmed: `/clear` autocompletes to "Start a new session" —
//     it is grok's alias of `/new`). All three families share the spelling, but it is
//     resolved through the table (not hardcoded) so a future divergence is a one-line
//     table change.
func (v Verbs) ClearContext() (Send, error) {
	return Send{Text: "/clear", SubmitEnter: true}, nil
}
