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
)

// ErrUnknownFamily is returned by For when the family is neither codex nor claude.
var ErrUnknownFamily = errors.New("verbs: unknown agent family")

// ErrInvalidKind is returned by NotifyMaster when topKind is not a member of the closed
// attention-kind enum — the push-to-wake template must NEVER carry free text (plan §15.10).
var ErrInvalidKind = errors.New("verbs: top kind is not a known attention kind")

// ErrUnsupported is returned by a verb that has NO real equivalent for a family. It is
// deliberate: a wrong guessed keystroke is worse than a typed "unsupported" the caller
// must handle (plan §1.7). Currently only Claude Code's Resume() is unsupported — there
// is no in-pane slash-command or keystroke that resumes a stopped autonomous task
// (`claude --resume`/`-c` are LAUNCH-time flags, not something typeable into a running
// pane). The caller (watchdog auto-resume / master) must handle this — e.g. skip the
// resume and surface an attention item instead of sending a bogus key.
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
	default:
		return Verbs{}, fmt.Errorf("%w: %q", ErrUnknownFamily, family)
	}
}

// Family returns the resolved family.
func (v Verbs) Family() Family { return v.family }

// Resume resolves the "continue a stopped/paused goal" verb.
//   - codex:  `/goal resume` (the Codex builtin; copied verbatim from the watchdog's
//     closed verb set — internal/watchdog/runner.go sendResumeCmd).
//   - claude: ErrUnsupported — Claude Code has NO in-pane resume verb (confirmed against
//     the Claude Code docs: `/resume` returns to a PRIOR conversation, `/goal` with no
//     arg only shows status, and `claude --resume`/`-c` are launch-time flags). The
//     caller MUST handle this rather than guess a keystroke.
func (v Verbs) Resume() (Send, error) {
	switch v.family {
	case FamilyCodex:
		return Send{Text: "/goal resume", SubmitEnter: true}, nil
	default: // FamilyClaude
		return Send{}, ErrUnsupported
	}
}

// Launch resolves the "start this epic" first payload for a fresh session. specPath is
// the epic's committed spec path (epics/YYYY-MM-DD-<slug>.md) and slug is its id.
//   - codex:  `/goal execute the epic at <specPath> per epics/INSTRUCTIONS.md. Work on
//     branch epic/<slug>.` — the EXACT string shape hardcoded today in cmd/flowbee's
//     runEpicStart (and threaded through internal/watchdog/launch.go's SendGoalCmd).
//   - claude: the SAME instruction WITHOUT the Codex `/goal execute ` builtin prefix —
//     Claude Code takes a plain natural-language first prompt (confirmed against the
//     docs). This is a plain-English instruction (DATA the agent acts on), not a guessed
//     slash-command, so it is a real equivalent rather than a typed-unsupported.
func (v Verbs) Launch(specPath, slug string) (Send, error) {
	switch v.family {
	case FamilyCodex:
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
// both Codex and Claude Code submit with a bare Enter.
func (v Verbs) NudgeEnter() Send { return Send{Key: "Enter"} }

// EscapeModal resolves the "dismiss a modal / menu / copy-mode" verb. Family-agnostic:
// both Codex and Claude Code use the Escape key (confirmed against the Claude Code
// keybindings docs — Esc closes dialogs/menus).
func (v Verbs) EscapeModal() Send { return Send{Key: "Escape"} }

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
	return Send{
		Text:        fmt.Sprintf("flowbee: %d attention items pending (top: %s). Run: flowbee master poll", count, topKind),
		SubmitEnter: true,
	}, nil
}

// ClearContext resolves the context-reset verb.
//   - claude: `/clear` (the documented Claude Code slash-command — starts a fresh
//     conversation, preserving project memory; the master's every-K-iterations reset).
//   - codex:  `/clear` (the Codex CLI builtin that starts a fresh task in the same
//     session — confirmed against the Codex CLI slash-command reference). Both families
//     share the spelling, but it is resolved through the table (not hardcoded) so a
//     future divergence is a one-line table change.
func (v Verbs) ClearContext() (Send, error) {
	return Send{Text: "/clear", SubmitEnter: true}, nil
}
