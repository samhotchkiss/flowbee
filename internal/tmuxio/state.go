package tmuxio

import (
	"regexp"
	"strings"
)

// State is the best-effort classification of what an agent pane is doing. It is a
// SUPERVISION hint, not ground truth: the Claude Code / Codex TUI formats churn
// often, so the classifier is deliberately conservative — anything it cannot
// confidently place is Unknown, and callers must treat Unknown as "do not act".
// (Compare internal/watchdog.ParseStatus, which classifies the narrower codex
// goal-session status bar; this classifier is the general pane-activity view.)
type State string

const (
	// StateIdleAtPrompt: the agent is sitting at its input prompt with no turn
	// running — safe to send it a message.
	StateIdleAtPrompt State = "idle_at_prompt"
	// StateWorking: a turn is running (spinner / elapsed timer / "esc to interrupt").
	// A message sent now queues behind the running turn.
	StateWorking State = "working"
	// StateAwaitingInput: the pane is showing a dialog, menu, or permission prompt
	// that is capturing keystrokes — the delivery hazard the whole verification
	// dance exists for. Text sent now goes into the DIALOG, not the agent's input.
	StateAwaitingInput State = "awaiting_input"
	// StateUnknown: none of the known shapes matched. Never acted on.
	StateUnknown State = "unknown"
)

// classifyRule is one table row: a pattern, the State it implies, whether it must
// match the LAST non-empty line (a prompt sitting at the bottom) versus anywhere
// in the visible tail (a spinner/dialog a few lines up from the input box), and
// the exact observed string it was built against. Rules are evaluated in slice
// order and FIRST MATCH WINS, so ordering encodes precedence:
// AwaitingInput > Working > IdleAtPrompt.
//
// This is the ONE place the heuristics live. They WILL evolve; each carries the
// verbatim sample (from the task brief and observed transcripts) it matches so a
// format change is easy to re-anchor.
type classifyRule struct {
	state        State
	re           *regexp.Regexp
	lastLineOnly bool
	example      string // the exact observed string this rule was written against
}

var classifyRules = []classifyRule{
	// ── AwaitingInput (most urgent: keystrokes are being captured by a dialog) ──
	{StateAwaitingInput, regexp.MustCompile(`Press up to edit queued messages`), false,
		`Claude Code, below the input box when messages are queued: "Press up to edit queued messages"`},
	{StateAwaitingInput, regexp.MustCompile(`Do you want to `), false,
		`Claude Code permission dialog header: "Do you want to proceed?" / "Do you want to make this edit?"`},
	{StateAwaitingInput, regexp.MustCompile(`Do you trust the files in`), false,
		`Claude Code trust prompt: "Do you trust the files in this folder?"`},
	{StateAwaitingInput, regexp.MustCompile(`❯\s*\d+\.`), false,
		`A numbered selection menu with the caret on an option: "❯ 1. Yes" / "❯ 2. No, and tell Claude..."`},
	{StateAwaitingInput, regexp.MustCompile(`Goal blocked`), false,
		`Codex goal-session status bar: "Goal blocked (/goal resume)" — needs a resume nudge`},

	// ── Working (a turn is running) ──
	{StateWorking, regexp.MustCompile(`esc to interrupt`), false,
		`Claude Code & Codex, shown while a turn runs: "(12s · esc to interrupt)"`},
	{StateWorking, regexp.MustCompile(`Working \(`), false,
		`Codex mid-turn line: "• Working (30m 48s • esc to interrupt)"`},
	{StateWorking, regexp.MustCompile(`Pursuing goal`), false,
		`Codex goal-session status bar while active: "Pursuing goal (2d 4h 12m)"`},
	{StateWorking, regexp.MustCompile(`\(\d+s\s*·`), false,
		`Claude Code spinner counter: "✻ Cogitating… (12s · ↑ 2.1k tokens)"`},

	// ── IdleAtPrompt (a bare prompt sits at the bottom, no turn running) ──
	{StateIdleAtPrompt, regexp.MustCompile(`^❯(\s|$)`), true,
		`Claude Code idle input prompt on the last line: "❯"`},
	{StateIdleAtPrompt, regexp.MustCompile(`^›`), true,
		`Codex idle input prompt on the last line: "› Improve documentation in @filename"`},
	{StateIdleAtPrompt, regexp.MustCompile(`Goal achieved`), false,
		`Codex goal-session status bar when done: "Goal achieved (1h 52m)"`},
	{StateIdleAtPrompt, regexp.MustCompile(`Worked for \d+`), false,
		`Codex "Worked for Xm" completion banner above an idle prompt`},
}

// classifyTailLines bounds how much of a capture the classifier scans, so a long
// scrollback (history capture) cannot let an old spinner in the transcript
// masquerade as the current state. Callers SHOULD pass a visible capture
// (history=0); this is a backstop.
const classifyTailLines = 40

// Classify returns the best-effort State of a captured pane, plus a short evidence
// string (the observed-sample description of the matched rule, or "" for Unknown).
// It expects a VISIBLE capture (Capture with history=0). Unknown means "no known
// shape matched" — callers must not act on it.
func Classify(capture string) (State, string) {
	lines := strings.Split(capture, "\n")
	// Trim to the last classifyTailLines lines to avoid matching scrollback.
	if len(lines) > classifyTailLines {
		lines = lines[len(lines)-classifyTailLines:]
	}
	tail := strings.Join(lines, "\n")
	last := lastNonEmptyLineOf(lines)

	for _, r := range classifyRules {
		if r.lastLineOnly {
			if r.re.MatchString(last) {
				return r.state, r.example
			}
			continue
		}
		if r.re.MatchString(tail) {
			return r.state, r.example
		}
	}
	return StateUnknown, ""
}

// ── shared line helpers (used by classification and delivery verification) ──

// lastNonEmptyLine returns the last line of s with non-whitespace content, trimmed
// of surrounding whitespace ("" if none). tmux pads a pane with trailing blank
// lines to its height, so the last line is almost never the content line — scan
// backward. Same contract as internal/watchdog.lastNonEmptyLine.
func lastNonEmptyLine(s string) string {
	return lastNonEmptyLineOf(strings.Split(s, "\n"))
}

func lastNonEmptyLineOf(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// promptGlyphs are the input-prompt glyphs Claude Code and Codex draw at the start
// of the input line (and the box border that can precede them). Stripping these
// (then trimming space) reduces an input line to the raw text the user/agent
// typed, so an exact-match verify compares like with like.
const promptGlyphs = "❯›»>│| \t"

// stripPromptGlyph removes leading prompt glyphs and surrounding whitespace from a
// line, yielding the bare input text. Mirrors internal/watchdog's
// `TrimSpace(TrimLeft(line, "›>"))`, widened to the full glyph set this package
// recognizes.
func stripPromptGlyph(line string) string {
	return strings.TrimSpace(strings.TrimLeft(line, promptGlyphs))
}

// classifyPromptLineRe matches a line that looks like a TUI input-prompt line: an
// optional box border (│ or |), optional leading space, then a prompt glyph
// (❯ › » >) followed by space or end-of-line. Used to anchor the fragment-fallback
// input region (see inputRegion). Mirrors the tmux-send awk prompt regex.
var classifyPromptLineRe = regexp.MustCompile(`^[[:space:]]*(│|\|)?[[:space:]]*(❯|›|»|>)([[:space:]]|$)`)
