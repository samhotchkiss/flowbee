package epicspec

import (
	"regexp"
	"strconv"
	"strings"
)

// ChecklistItem is one "- [x] Step N — <criterion> (evidence: ...)" line off a
// running epic's ## Status (epics/INSTRUCTIONS.md "Status discipline").
type ChecklistItem struct {
	Step     int
	Checked  bool
	Text     string
	Evidence string // "" if the line carried no "(evidence: ...)" suffix
}

// StatusBlock is a leniently-parsed ## Status section. Every field defaults to its
// zero value on anything unparseable — see the package doc: the ingestion loop
// (cmd/flowbee) must never let a malformed status wedge every other epic, so this
// parser never returns an error, only its best partial read.
type StatusBlock struct {
	// UpdatedRaw is the "Updated:" field's raw text, unparsed — the ingestion loop
	// stores it verbatim (epics.status_updated_at) rather than this package
	// resolving it to a time.Time, since a malformed/missing timestamp should
	// still preserve whatever the agent wrote for an operator to eyeball, not
	// silently become "".
	UpdatedRaw string
	// CurrentStep/StepsTotal come from "Current: step N/M" (0/0 if absent or the
	// author-template's literal "Current: not started").
	CurrentStep int
	StepsTotal  int
	// State is the raw "State:" word (pending|building|blocked|done|... — whatever
	// the agent wrote; epics/INSTRUCTIONS.md documents the vocabulary but this
	// parser does not validate against it, matching "degrade to inert, not a bad
	// action" from the Phase 1 watchdog's own parser philosophy).
	State     string
	Checklist []ChecklistItem
	// Blockers is the free-text tail of the "Blockers:" line (through end of
	// section — a blocker explanation is often more than one clause).
	Blockers string
}

var (
	statusUpdatedRe = regexp.MustCompile(`Updated:\s*([^\s·]+)`)
	statusCurrentRe = regexp.MustCompile(`Current:\s*step\s+(\d+)\s*/\s*(\d+)`)
	statusStateRe   = regexp.MustCompile(`State:\s*(\S+)`)
	checklistLineRe = regexp.MustCompile(`(?m)^\s*-\s*\[([ xX])\]\s*Step\s+(\d+)\s*[—-]\s*(.*)$`)
	evidenceRe      = regexp.MustCompile(`\(evidence:\s*(.*?)\)\s*$`)
	blockersLineRe  = regexp.MustCompile(`(?m)^\s*Blockers:\s*(.*)$`)
)

// ParseStatus parses a ## Status section body (already extracted from the full
// file — callers typically pass epicspec.section(content, "Status")). Never
// errors; a completely garbage input parses to a zero-value StatusBlock.
func ParseStatus(body string) StatusBlock {
	var sb StatusBlock
	if m := statusUpdatedRe.FindStringSubmatch(body); m != nil {
		sb.UpdatedRaw = m[1]
	}
	if m := statusCurrentRe.FindStringSubmatch(body); m != nil {
		sb.CurrentStep, _ = strconv.Atoi(m[1])
		sb.StepsTotal, _ = strconv.Atoi(m[2])
	}
	if m := statusStateRe.FindStringSubmatch(body); m != nil {
		// trim a trailing separator glyph a greedy-ish author might leave attached
		// (e.g. "State: blocked·" with no space) — cosmetic, but cheap to guard.
		sb.State = strings.Trim(m[1], "·,;")
	}
	for _, m := range checklistLineRe.FindAllStringSubmatch(body, -1) {
		item := ChecklistItem{
			Checked: strings.EqualFold(m[1], "x"),
			Text:    strings.TrimSpace(m[3]),
		}
		item.Step, _ = strconv.Atoi(m[2])
		if em := evidenceRe.FindStringSubmatch(item.Text); em != nil {
			item.Evidence = strings.TrimSpace(em[1])
			item.Text = strings.TrimSpace(item.Text[:strings.LastIndex(item.Text, "(evidence:")])
		}
		sb.Checklist = append(sb.Checklist, item)
	}
	if m := blockersLineRe.FindStringSubmatch(body); m != nil {
		sb.Blockers = strings.TrimSpace(m[1])
	}
	return sb
}
