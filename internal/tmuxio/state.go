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
	// StateIdleAtPrompt: the agent is sitting at its input prompt (a bare `❯`/`›`
	// line, or Claude Code's bordered `│ > │` box) with no turn running — safe to
	// send it a message.
	StateIdleAtPrompt State = "idle_at_prompt"
	// StateWorking: a turn is running (spinner / elapsed timer / "esc to interrupt").
	// A message sent now queues behind the running turn.
	StateWorking State = "working"
	// StateAwaitingInput: the pane is showing a dialog, menu, or permission prompt
	// that is CAPTURING KEYSTROKES — the delivery hazard the whole verification
	// dance exists for. Text sent now goes into the DIALOG, not the agent's input.
	StateAwaitingInput State = "awaiting_input"
	// StateGoalBlocked: a codex goal-session is blocked/paused and needs a
	// `/goal resume` nudge (its status-bar hint text). This is DISTINCT from
	// StateAwaitingInput: it is a status line, NOT a keystroke-capturing menu, so a
	// `/goal resume` send lands in the input box normally and is NOT a menu hazard.
	// (Split out per field review m8 so a watchdog-on-tmuxio resume send is not
	// wrongly capped at Weak.)
	StateGoalBlocked State = "goal_blocked"
	// StateUnknown: none of the known shapes matched. Never acted on.
	StateUnknown State = "unknown"
)

// ruleScope bounds where a classifier rule may match, so transcript text an agent
// merely PRINTED (a quoted "Do you want to…", a past "Goal achieved") does not get
// read as the CURRENT pane state (field review m9). scopeBottom matches only the
// live UI near the input box; scopeLastLine matches only the last non-empty line
// (the persistent status bar / a bare prompt).
type ruleScope int

const (
	scopeBottom ruleScope = iota
	scopeLastLine
)

// classifyRule is one table row: a pattern, the State it implies, the scope it may
// match in, and the exact observed string it was built against. Rules are
// evaluated in slice order and FIRST MATCH WINS, so ordering encodes precedence:
// AwaitingInput > GoalBlocked > Working > IdleAtPrompt.
//
// This is the ONE place the regex heuristics live. They WILL evolve; each carries
// the verbatim sample (from the task brief and observed transcripts) it matches so
// a format change is easy to re-anchor. The box-aware idle fallback below the
// table (Classify) is the only non-regex heuristic.
type classifyRule struct {
	state   State
	re      *regexp.Regexp
	scope   ruleScope
	example string
}

var classifyRules = []classifyRule{
	// ── AwaitingInput (most urgent: keystrokes are captured by a dialog/menu) ──
	{StateAwaitingInput, regexp.MustCompile(`Press up to edit queued messages`), scopeBottom,
		`Claude Code, below the input box when messages are queued: "Press up to edit queued messages"`},
	{StateAwaitingInput, regexp.MustCompile(`Do you want to `), scopeBottom,
		`Claude Code permission dialog header: "Do you want to proceed?" / "Do you want to make this edit?"`},
	{StateAwaitingInput, regexp.MustCompile(`Do you trust the files in`), scopeBottom,
		`Claude Code trust prompt: "Do you trust the files in this folder?"`},
	{StateAwaitingInput, regexp.MustCompile(`❯\s*\d+\.`), scopeBottom,
		`A numbered selection menu with the caret on an option: "❯ 1. Yes" / "❯ 2. No, and tell Claude..."`},

	// ── GoalBlocked (codex status-bar hint, NOT a keystroke-capturing menu) ──
	{StateGoalBlocked, regexp.MustCompile(`Goal blocked`), scopeBottom,
		`Codex goal-session status bar: "Goal blocked (/goal resume)" — needs a resume nudge, not a menu`},
	{StateGoalBlocked, regexp.MustCompile(`Goal paused`), scopeBottom,
		`Codex goal-session status bar: "Goal paused (/goal resume)" — a distinct blocked-like state`},

	// ── Working (a turn is running) ──
	{StateWorking, regexp.MustCompile(`esc to interrupt`), scopeBottom,
		`Claude Code & Codex, shown while a turn runs: "(12s · esc to interrupt)"`},
	{StateWorking, regexp.MustCompile(`Working \(`), scopeBottom,
		`Codex mid-turn line: "• Working (30m 48s • esc to interrupt)"`},
	{StateWorking, regexp.MustCompile(`Pursuing goal`), scopeBottom,
		`Codex goal-session status bar while active: "Pursuing goal (2d 4h 12m)"`},
	{StateWorking, regexp.MustCompile(`\(\d+s\s*·`), scopeBottom,
		`Claude Code spinner counter: "✻ Cogitating… (12s · ↑ 2.1k tokens)"`},

	// ── IdleAtPrompt (a bare prompt / completion banner) ──
	{StateIdleAtPrompt, regexp.MustCompile(`^❯(\s|$)`), scopeLastLine,
		`Claude Code idle input prompt on the last line: "❯"`},
	{StateIdleAtPrompt, regexp.MustCompile(`^›`), scopeLastLine,
		`Codex idle input prompt on the last line: "› Improve documentation in @filename"`},
	{StateIdleAtPrompt, regexp.MustCompile(`Goal achieved`), scopeBottom,
		`Codex goal-session status bar when done: "Goal achieved (1h 52m)"`},
	{StateIdleAtPrompt, regexp.MustCompile(`Worked for \d+`), scopeBottom,
		`Codex "Worked for Xm" completion banner above an idle prompt`},
}

// classifyBottomLines bounds the scopeBottom window: the live UI (input box,
// spinner, dialog) lives in the last few lines, so matching there — not across the
// whole scrollback — keeps printed transcript text from misclassifying the current
// state.
const classifyBottomLines = 10

// Classify returns the best-effort State of a captured pane, plus a short evidence
// string (the observed-sample description of the matched rule, or "" for Unknown).
// It expects a VISIBLE capture (Capture with history=0). Unknown means "no known
// shape matched" — callers must not act on it.
func Classify(capture string) (State, string) {
	lines := strings.Split(capture, "\n")
	// Drop trailing blank padding (tmux pads a pane to its full height with blank
	// lines) so the bottom window covers the last CONTENT lines — the live UI — not
	// the padding.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	last := lastNonEmptyLineOf(lines)
	bottom := bottomRegion(lines, classifyBottomLines)

	for _, r := range classifyRules {
		target := bottom
		if r.scope == scopeLastLine {
			target = last
		}
		if r.re.MatchString(target) {
			return r.state, r.example
		}
	}
	// Box-aware idle fallback: if we can LOCATE an input-prompt line near the bottom
	// (Claude Code's bordered box, whose LAST line is a "? for shortcuts" hint rather
	// than the prompt), and none of the working/dialog rules fired, the agent is
	// sitting at its input box. This is what lets IDLE_AT_PROMPT work on Claude panes
	// (field review m10).
	if _, ok := extractInputLine(bottom); ok {
		return StateIdleAtPrompt, "input-prompt box located near the bottom (box-aware idle)"
	}
	return StateUnknown, ""
}

// ── shared line helpers (used by classification and delivery verification) ──

// lastNonEmptyLineOf returns the last line with non-whitespace content, trimmed of
// surrounding whitespace ("" if none). tmux pads a pane with trailing blank lines
// to its height, so the last line is almost never the content line — scan backward.
// Same contract as internal/watchdog.lastNonEmptyLine.
func lastNonEmptyLineOf(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// bottomRegion returns the last n lines of the capture joined back together (all
// lines, including blank padding — harmless for regex). Used to scope
// dialog/menu/working detection to the live UI.
func bottomRegion(lines []string, n int) string {
	from := len(lines) - n
	if from < 0 {
		from = 0
	}
	return strings.Join(lines[from:], "\n")
}

// classifyPromptLineRe matches a TUI input-prompt line: optional leading space, an
// optional box border (│ or |), optional space, then a prompt glyph (❯ › » >)
// followed by a single space or end-of-line. The match END is exactly the prompt
// PREFIX — everything after it is the input text (see extractInputLine). Mirrors
// the tmux-send awk prompt regex.
var classifyPromptLineRe = regexp.MustCompile(`^[[:space:]]*(│|\|)?[[:space:]]*(❯|›|»|>)([[:space:]]|$)`)

// extractInputLine locates the pane's input line — the LAST line matching the
// prompt regex — and returns the input TEXT it holds (the glyph prefix and any
// trailing box border/padding stripped), plus located=true. located=false when no
// prompt line is present at all.
//
// This is the core of the M1 false-Strong fix: Claude Code renders the input
// inside a bordered box ("│ > msg │") with a hint line BELOW it, so the message is
// NOT the last non-empty line. We must locate the actual prompt line and read the
// text SITTING ON IT, rather than assuming the message is the last line. Per M2,
// exactly the regex-matched prompt prefix is stripped (never a greedy glyph-class
// trim), so a message that itself begins with `>`/`›`/`»`/`|` is compared correctly.
func extractInputLine(capture string) (string, bool) {
	lines := strings.Split(capture, "\n")
	idx := -1
	for i, ln := range lines {
		if classifyPromptLineRe.MatchString(ln) {
			idx = i
		}
	}
	if idx < 0 {
		return "", false
	}
	line := lines[idx]
	loc := classifyPromptLineRe.FindStringIndex(line)
	rest := line[loc[1]:]
	return strings.TrimSpace(stripTrailingBorder(rest)), true
}

// stripTrailingBorder removes a trailing box border (a │ or | with optional
// surrounding whitespace) from an input line's interior, so "msg      │" becomes
// "msg". It strips at most ONE border rune.
func stripTrailingBorder(s string) string {
	s = strings.TrimRight(s, " \t")
	s = strings.TrimRight(s, "│|")
	return strings.TrimRight(s, " \t")
}
