package watchdog

import (
	"regexp"
	"strings"
)

// State is the closed set of goal-session states the parser can produce. Anything
// it can't confidently classify is Unknown — and Unknown is NEVER acted on by the
// watcher (§ design goal: the codex TUI status-line format churns weekly in
// practice, so ParseStatus is kept as ONE small isolated function with a tiny blast
// radius — a format change degrades a session to "invisible to automation", never
// to a wrong action).
type State string

const (
	StatePursuing    State = "pursuing"
	StateWorking     State = "working"
	StateBlocked     State = "blocked"
	StateAchieved    State = "achieved"
	StateUnknown     State = "unknown"
	StateUnreachable State = "unreachable" // set directly by the watcher on capture failure, never by ParseStatus
)

// The exact captured samples (see task brief) this parser is built against:
//
//	"  gpt-5.6-terra high · ~/dev/russ · Main [default]                    Pursuing goal (2d 4h 12m)"
//	"  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
//	"  gpt-5.6-sol medium · ~/dev/russ                                     Goal achieved (1h 52m)"
//	"• Working (30m 48s • esc to interrupt) · 1 background terminal running · /ps to view · /stop to close"
//
// Order matters: "Goal achieved"/"Goal blocked" must be checked before "Pursuing
// goal" would ever be considered, and the bullet "Working" line is checked last
// since it's a DIFFERENT pane shape (mid-turn work, not the persistent status bar).
var (
	achievedRe = regexp.MustCompile(`Goal achieved\s*\(([^)]*)\)`)
	blockedRe  = regexp.MustCompile(`Goal blocked\s*\(([^)]*)\)`)
	pursuingRe = regexp.MustCompile(`Pursuing goal\s*\(([^)]*)\)`)
	workingRe  = regexp.MustCompile(`Working\s*\(([^)]*)\)`)
)

// ParseStatus extracts (state, detail) from a captured tmux pane: the LAST
// non-empty line is the codex TUI's status line (per the observed samples), so
// everything above it — scrollback, prior turns — is ignored. detail is the raw
// parenthetical/trailing text off that line where one exists (an elapsed duration
// for pursuing/working/achieved, or codex's own resume hint for blocked) — purely
// informational; it is NOT the watcher's classification (that's a separate,
// scrollback-driven decision — see internal/watchdog/classify.go). Garbage/empty
// input, or a last line that matches none of the known shapes, returns
// (StateUnknown, "") — deliberately never guessed.
func ParseStatus(pane string) (State, string) {
	line := lastNonEmptyLine(pane)
	if line == "" {
		return StateUnknown, ""
	}
	if m := achievedRe.FindStringSubmatch(line); m != nil {
		return StateAchieved, strings.TrimSpace(m[1])
	}
	if m := blockedRe.FindStringSubmatch(line); m != nil {
		return StateBlocked, strings.TrimSpace(m[1])
	}
	if m := pursuingRe.FindStringSubmatch(line); m != nil {
		return StatePursuing, strings.TrimSpace(m[1])
	}
	// the mid-turn "• Working (...)" line is a DIFFERENT pane shape than the
	// persistent status bar (it appears above the input box while a turn is
	// running) — require the leading bullet so we don't false-positive on some
	// unrelated line that happens to contain the word "Working".
	if strings.HasPrefix(line, "•") {
		if m := workingRe.FindStringSubmatch(line); m != nil {
			// "Working (30m 48s • esc to interrupt)" — the parenthetical carries
			// BOTH the elapsed time and a UI hint separated by another "•"; keep
			// only the elapsed portion so detail is comparable to the other states'.
			detail := strings.TrimSpace(strings.SplitN(m[1], "•", 2)[0])
			return StateWorking, detail
		}
	}
	return StateUnknown, ""
}

// lastNonEmptyLine returns the last line of s with non-whitespace content, trimmed
// of surrounding whitespace. "" if s has no such line. tmux capture-pane pads the
// pane with trailing blank lines up to its height, so the LAST line is almost never
// the status line — we must scan backward for the last one that has content.
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}
