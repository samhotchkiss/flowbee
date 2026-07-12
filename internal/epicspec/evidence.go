package epicspec

import (
	"fmt"
	"sort"
	"strings"
)

// EvidenceResult is the epic-lane Phase 3 CONTRACT verdict: whether an epic PR's own
// claimed ## Status honors the epic's own ## Steps — the "criteria-driven review
// gate" the epic-lane design promises (trust model: a self-testing agent + verified
// evidence replaces N-reviewer redundancy, so the evidence itself must be checked,
// never taken on faith). It is deliberately NOT part of internal/content: that
// package's own doc commits it to staying PURE and stdlib-only (no clock, no
// third-party import), and this check needs epicspec's own Spec/StatusBlock types
// (which pull in gopkg.in/yaml.v3 transitively via Spec's frontmatter). The runtime
// folds this verdict into the SAME merge_handoff routing the content-integrity gate
// already uses (internal/project's contentDenyReason/RouteSelfMergeToHandoff) rather
// than duplicating that machinery — see project.go's epicDenyReason.
type EvidenceResult struct {
	// Clear is true iff every ## Steps entry is checked with non-empty evidence,
	// State: is exactly "done", and Blockers: is empty/absent. Also true, vacuously,
	// for the zero value (an epic PR that was never evaluated) — callers that care
	// about "was this even checked" track that separately (see project.go), so this
	// type never needs an Applicable flag of its own.
	Clear bool
	// Failures names EXACTLY which requirement(s) are unmet, in a form fit to embed
	// directly in a merge_handoff reason string (task brief: "a legible reason string
	// naming exactly which steps lack evidence"). Empty iff Clear.
	Failures []string
}

// CheckEvidence judges an epic's claimed ## Status (sb) against its own frozen
// ## Steps contract (spec.Steps) — task brief point 2's three requirements:
//  1. State: is exactly "done" (case-insensitive; the agent's own vocabulary per
//     epics/INSTRUCTIONS.md, matched the same way store.nextEpicState does);
//  2. every step in ## Steps has a checklist entry that is checked [x] AND carries a
//     non-empty evidence string;
//  3. Blockers: is empty or absent.
//
// PURE: a function of the two ALREADY-PARSED values, no I/O — the caller (project.go)
// is responsible for reading the epic file AS OF THE PR HEAD via the control-plane
// mirror and parsing it with ParseSpec/ParseStatus first, per the task brief's
// explicit "not from the possibly-stale epics table" requirement (a live mirror read
// is a runtime concern this package never performs itself, matching every other
// parser here — see the package doc).
func CheckEvidence(spec Spec, sb StatusBlock) EvidenceResult {
	var failures []string

	state := strings.ToLower(strings.TrimSpace(sb.State))
	if state == "" {
		failures = append(failures, `## Status has no "State:" (want "done")`)
	} else if state != "done" {
		failures = append(failures, fmt.Sprintf("State: %s (want \"done\")", sb.State))
	}

	if b := strings.TrimSpace(sb.Blockers); b != "" && !isEmptyPlaceholder(b) {
		failures = append(failures, "Blockers: "+b)
	}

	byStep := make(map[int]ChecklistItem, len(sb.Checklist))
	for _, item := range sb.Checklist {
		// last one wins on a duplicate step number — an agent that re-wrote its own
		// checklist line (a legitimate edit) should be judged on its LATEST claim,
		// not an earlier draft; ParseStatus preserves document order, so "last" here
		// means "most recently written".
		byStep[item.Step] = item
	}
	for _, step := range spec.Steps {
		item, ok := byStep[step.N]
		switch {
		case !ok:
			failures = append(failures, fmt.Sprintf("step %d (%s): missing from the ## Status checklist", step.N, truncateForReason(step.Text)))
		case !item.Checked:
			failures = append(failures, fmt.Sprintf("step %d (%s): unchecked", step.N, truncateForReason(step.Text)))
		case strings.TrimSpace(item.Evidence) == "":
			failures = append(failures, fmt.Sprintf("step %d (%s): checked but no evidence", step.N, truncateForReason(step.Text)))
		}
	}

	return EvidenceResult{Clear: len(failures) == 0, Failures: failures}
}

// isEmptyPlaceholder reports whether s is one of the conventional "nothing here"
// placeholder tokens an author template pre-fills a field with (case-insensitive) —
// the same leniency this package already extends to StatusBlock.CurrentStep's own
// "Current: not started" template literal (see StatusBlock's doc). Without this, an
// agent that dutifully wrote "Blockers: none" (the template's own suggested filler,
// not a real blocker) would be denied self-merge for a NON-EMPTY string that means
// exactly "no blockers" to a human reader.
func isEmptyPlaceholder(s string) bool {
	switch strings.ToLower(s) {
	case "none", "n/a", "na", "-", "nil", "no blockers", "not blocked":
		return true
	}
	return false
}

// truncateForReason bounds a step's TEXT when it is embedded into a merge_handoff
// reason string — the reason is user-facing (an operator's attention queue, §9.2a's
// existing DenylistHits/StaticFailures precedent) and a step's free-text description
// could in principle be long; this keeps one failing step's reason from dominating
// the whole message.
func truncateForReason(s string) string {
	const max = 60
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// CheckScope is the SCOPE-HONESTY check (task brief point 2): every file the PR
// touches must match at least one of the epic's declared `scope:` globs, verified
// with a REAL per-file glob match (MatchGlob) — deliberately NOT ScopeOverlap's
// conservative prefix-overlap heuristic, which is intentionally FALSE-POSITIVE-biased
// for the launch-time collision check and would wrongly flag disjoint-but-prefix-
// sharing paths here (e.g. "internal/foo*" vs "internal/foobar/x.go") as out of
// scope. Returns the SORTED, de-duplicated list of touched paths that matched NO
// scope glob — empty means every touched path is in scope. A nil/empty scope covers
// nothing (every non-empty touchedPaths is reported), matching content.declarationCovers'
// "an empty declaration covers nothing" default-deny posture.
func CheckScope(scope []string, touchedPaths []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range touchedPaths {
		if p == "" || seen[p] {
			continue
		}
		inScope := false
		for _, g := range scope {
			if MatchGlob(g, p) {
				inScope = true
				break
			}
		}
		if !inScope {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
