package watchdog

import (
	"regexp"
	"strings"
	"time"
)

// blockKind is the closed classification of a 'blocked' session, decided from its
// tmux scrollback (not just the last status line — the reason text scrolled by
// earlier in the pane, per the task brief's -S -60 scrollback capture).
type blockKind int

const (
	// blockAutoResume: nothing in scrollback explains the block (or codex just
	// wants a plain `/goal resume`, e.g. after a transient CLI hiccup) — safe to
	// self-serve the resume.
	blockAutoResume blockKind = iota
	// blockUsageLimit: codex reported hitting a usage limit. If a reset time was
	// parseable and is still in the future, the watcher must NOT resume before
	// then (hammering `/goal resume` against a live cap wastes the attempt budget
	// and won't work anyway).
	blockUsageLimit
	// blockInfra: an infrastructure problem (missing `gh` auth, disk full, ...) —
	// the exact incident that motivated this watchdog (a day-long silent stall on
	// box `buncher`). NEVER auto-resumed: retrying `/goal resume` against a broken
	// environment just burns the attempt budget and hides the real problem from
	// the operator.
	blockInfra
)

// blockedClassification is classifyBlocked's verdict.
type blockedClassification struct {
	Kind    blockKind
	ResetAt time.Time // valid only when Kind == blockUsageLimit and a time was parsed
	Weekly  bool      // reset names a day/date rather than a same-day clock time
	Reason  string    // short human-readable snippet for logs / state_detail
}

// infraKeywords are substrings (case-insensitive) that flag an environment problem
// a `/goal resume` cannot fix — the operator must intervene. Deliberately narrow
// and literal (per the task's exact examples) rather than a broad heuristic: a
// false positive here silently strands a session that COULD have self-resumed,
// which is its own failure mode.
var infraKeywords = []string{
	"gh auth",
	"not logged into any github hosts",
	"gh: command not found",
	"no space left",
	"disk full",
	"enospc",
	"permission denied",
	"could not read from remote repository",
	"authentication failed",
}

// usageLimitRe detects codex's usage-limit message, e.g.:
//
//	"You've hit your usage limit ... try again at 10:47 AM"
//
// and the weekly variant, which names a day/date instead of a same-day time.
var usageLimitRe = regexp.MustCompile(`(?i)usage limit`)

// tryAgainRe captures the clause after "try again" up to end-of-line or a period,
// e.g. "at 10:47 AM" or "Monday" or "on July 15". Captured raw; parseResetTime does
// the actual time-shape parsing so the two concerns stay separate and independently
// testable.
var tryAgainRe = regexp.MustCompile(`(?i)try again\s+([^.\n]+)`)

// classifyBlocked inspects scrollback text (ideally captured with `-S -60`, but the
// caller may fall back to just the last pane on a scrollback-capture failure) and
// returns the classification the watcher acts on.
func classifyBlocked(scrollback string, now time.Time) blockedClassification {
	lower := strings.ToLower(scrollback)

	for _, kw := range infraKeywords {
		if strings.Contains(lower, kw) {
			return blockedClassification{Kind: blockInfra, Reason: kw}
		}
	}

	if usageLimitRe.MatchString(scrollback) {
		if m := tryAgainRe.FindStringSubmatch(scrollback); m != nil {
			clause := strings.TrimSpace(m[1])
			if resetAt, weekly, ok := parseResetTime(clause, now); ok {
				return blockedClassification{
					Kind: blockUsageLimit, ResetAt: resetAt, Weekly: weekly,
					Reason: "usage limit, resets " + clause,
				}
			}
			// usage-limit text present but the reset clause didn't parse: this
			// must NOT fall through to auto-resume (that would hammer /goal
			// resume against a real, live cap — worse than doing nothing and
			// re-parsing next tick). Conservative fixed cool-down instead; the
			// next pass re-reads scrollback and may parse it correctly, or the
			// operator sees the state_detail and the raw clause in the logs.
			return blockedClassification{
				Kind: blockUsageLimit, ResetAt: now.Add(usageLimitFallbackCooldown),
				Reason: "usage limit, unparsed reset clause: " + clause,
			}
		}
		// usage-limit text present with no "try again ..." clause we could find at
		// all — same conservative fallback as above.
		return blockedClassification{
			Kind: blockUsageLimit, ResetAt: now.Add(usageLimitFallbackCooldown),
			Reason: "usage limit, no reset clause found",
		}
	}

	return blockedClassification{Kind: blockAutoResume}
}

// usageLimitFallbackCooldown is the conservative wait applied when usage-limit text
// is detected but its reset time can't be parsed — long enough that a real cap
// window has a decent chance of having rolled over by the next check, short enough
// that a mis-classification doesn't strand a session for hours.
const usageLimitFallbackCooldown = 30 * time.Minute

// clockTimeFormats are the same-day "try again at ..." shapes we've seen /
// anticipate (12h with meridiem is the one in the task's captured sample).
var clockTimeFormats = []string{"3:04 PM", "3:04PM", "15:04"}

var weekdayNames = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
	"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday,
	"saturday": time.Saturday,
}

// parseResetTime turns the "try again <clause>" tail into a concrete deadline.
// Two shapes:
//
//   - a same-day clock time ("at 10:47 AM") — the DAILY usage-limit variant. If
//     that time has already passed today, it must mean tomorrow (a reset time in
//     the past isn't a pending gate at all — but codex only shows this message
//     while the cap is still active, so "already passed today" reads as "tomorrow").
//   - a weekday name ("Monday", "on Monday") — the WEEKLY usage-limit variant
//     (§ task: "reset time named a day, not a same-day time"). Resolved to the
//     NEXT occurrence of that weekday; if that's today, weekly caps don't reset
//     same-day, so it's bumped a further 7 days.
//
// NOTE (uncertainty flagged in the report): only the daily "at <time>" shape was
// given as an exact captured sample; the weekly shape's exact wording was not, so
// this weekday-name parse is a best-effort heuristic pending a real sample.
func parseResetTime(clause string, now time.Time) (t time.Time, weekly bool, ok bool) {
	clause = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(clause), "at "))
	clause = strings.TrimSuffix(clause, ".")

	for _, layout := range clockTimeFormats {
		if parsed, err := time.Parse(layout, clause); err == nil {
			candidate := time.Date(now.Year(), now.Month(), now.Day(),
				parsed.Hour(), parsed.Minute(), 0, 0, now.Location())
			if !candidate.After(now) {
				candidate = candidate.AddDate(0, 0, 1)
			}
			return candidate, false, true
		}
	}

	// weekday form, optionally prefixed "on " (e.g. "Monday", "on Monday"); take
	// just the first word so trailing punctuation/clauses don't break the match.
	name := strings.ToLower(strings.TrimPrefix(strings.ToLower(clause), "on "))
	fields := strings.Fields(name)
	if len(fields) == 0 {
		return time.Time{}, false, false
	}
	if wd, isDay := weekdayNames[fields[0]]; isDay {
		days := (int(wd) - int(now.Weekday()) + 7) % 7
		if days == 0 {
			days = 7 // a weekly cap can't reset "today" — same weekday means next week
		}
		candidate := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, days)
		return candidate, true, true
	}

	return time.Time{}, false, false
}
