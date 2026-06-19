// Package history is the issue-archive markdown projection (build-list §F): a
// read-model FOLD over the event ledger that produces, on merge, a curated
// `docs/history/<id>.md` card (status, attempts, verdicts, linked PR, lessons)
// plus a generated table of contents. The ledger stays canonical; the markdown is
// a derived, regenerable view — Flowbee is the SOLE writer, and a dedicated
// post-merge commit lands it (never entangled with the feature PR).
//
// Like the deterministic core it FOLDS FROM (internal/ledger), this package is
// PURE: it reads no clock, no randomness, no GitHub, no LLM. Card == fold(events)
// and Render(Card) is a total function of the card, so the same ledger always
// reconstructs byte-identical markdown. That property is what makes the archive a
// trustworthy read-model: a precedent-gate hook can grep it knowing it is exactly
// what the canonical events say.
package history

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// Card is the curated read-model of one job's full lifecycle, folded from its
// events. It is the structured precursor to the rendered markdown — a precedent
// gate can consume the Card directly (or grep the rendered file). Every field is a
// pure function of the events; nothing here is read from a clock.
type Card struct {
	JobID   string
	Kind    job.Kind
	Flow    string
	Title   string // the human intent (task text first line), for the TOC + header
	Status  job.State
	Created time.Time // the job_created event's recorded time
	Updated time.Time // the last event's recorded time (the merge/done instant)

	IssueNum int
	PRNumber int

	// counters folded from the ledger (attempts/bounces/stall revocations).
	Attempts         int
	Bounces          int
	StallRevocations int

	// MergeCommit is the reconciled merge-commit provenance recorded on an
	// unattended self_merge (KindUnattendedMerged); empty for a handoff merge.
	MergeCommit string
	BaseSHA     string
	HeadSHA     string

	// Verdicts are the tamper-evident sign-offs minted over the job's life, in
	// order (the build gate's approval + any re-armed re-approval).
	Verdicts []VerdictRecord

	// Attempts/Bounces are summarized; Timeline is the ordered, human-readable
	// audit of every projection-moving event (the institutional record).
	Timeline []TimelineEntry

	// Lessons are the curated, grep-able takeaways a precedent gate routes on:
	// bounces (what a reviewer rejected), escalations (why a human was pulled in),
	// supersessions (a SHA moved underneath), conflicts. Derived from the events.
	Lessons []string
}

// VerdictRecord is one minted verdict folded onto the card (value + disposition +
// the SHA pair it bound to + its integrity hash).
type VerdictRecord struct {
	Value       job.VerdictValue
	Disposition job.Disposition
	HeadSHA     string
	BaseSHA     string
	Integrity   string
	At          time.Time
}

// TimelineEntry is one ordered, rendered line of the job's lifecycle.
type TimelineEntry struct {
	Seq  int
	Kind ledger.EventKind
	At   time.Time
	Note string
}

// Fold builds the Card from a job's full event list (ascending job_seq). PURE: no
// clock, no I/O — Card == Fold(events). It reuses the canonical ledger.Fold for the
// terminal projection (status/counters/SHAs) and layers the curated, archive-only
// summary (verdicts, lessons, timeline) on top.
func Fold(events []ledger.Event) (Card, error) {
	j, err := ledger.Fold(events)
	if err != nil {
		return Card{}, err
	}
	c := Card{
		JobID:            j.ID,
		Kind:             j.Kind,
		Flow:             j.Flow,
		Status:           j.State,
		IssueNum:         j.IssueNum,
		PRNumber:         j.PRNumber,
		Attempts:         j.Attempts,
		Bounces:          j.Bounces,
		StallRevocations: j.StallRevocations,
		MergeCommit:      j.MergeProvenance,
		BaseSHA:          j.BaseSHA,
		HeadSHA:          j.HeadSHA,
		Title:            title(j),
	}
	for _, e := range events {
		if c.Created.IsZero() && e.Kind == ledger.KindJobCreated {
			c.Created = e.CreatedAt
		}
		if !e.CreatedAt.IsZero() {
			c.Updated = e.CreatedAt
		}
		switch e.Kind {
		case ledger.KindVerdictMinted:
			if e.Payload.Verdict != nil {
				v := e.Payload.Verdict
				c.Verdicts = append(c.Verdicts, VerdictRecord{
					Value: v.Value, Disposition: v.Disposition,
					HeadSHA: v.HeadSHA, BaseSHA: v.BaseSHA,
					Integrity: v.IntegrityHash, At: e.CreatedAt,
				})
			}
		case ledger.KindReviewBounced:
			c.Lessons = append(c.Lessons,
				fmt.Sprintf("Build bounced at code review (attempt %d); the reviewer requested changes.", c.bounceCount(events, e.JobSeq)))
		case ledger.KindBounceExhausted:
			c.Lessons = append(c.Lessons,
				"Bounce budget exhausted — routed to a human (max review bounces reached).")
		case ledger.KindSpecNeedsDesign:
			c.Lessons = append(c.Lessons,
				"Issue-review flagged a design fork — needed human design input before building.")
		case ledger.KindSuperseded, ledger.KindRebased:
			c.Lessons = append(c.Lessons,
				"A base/head SHA moved underneath the verdict — re-armed review + CI at the new head (I-5).")
		case ledger.KindConflictDetected:
			c.Lessons = append(c.Lessons,
				"A real merge conflict (overlapping edits) routed to a conflict_resolver job.")
		case ledger.KindCostEscalated:
			c.Lessons = append(c.Lessons,
				"Cost ceiling crossed — escalated to a human (over budget).")
		case ledger.KindStallEscalated:
			c.Lessons = append(c.Lessons,
				"Repeated stalls hit the governor ceiling — escalated to a human (anti-thrash).")
		case ledger.KindUnattendedMerged:
			if e.Payload.MergeProvenance != "" {
				c.MergeCommit = e.Payload.MergeProvenance
			}
		}
		if note := timelineNote(e); note != "" {
			c.Timeline = append(c.Timeline, TimelineEntry{
				Seq: e.JobSeq, Kind: e.Kind, At: e.CreatedAt, Note: note,
			})
		}
	}
	return c, nil
}

// bounceCount returns the number of bounce events at-or-before seq (so each
// "bounced" lesson is numbered deterministically).
func (c Card) bounceCount(events []ledger.Event, uptoSeq int) int {
	n := 0
	for _, e := range events {
		if e.JobSeq > uptoSeq {
			break
		}
		if e.Kind == ledger.KindReviewBounced {
			n++
		}
	}
	return n
}

// title derives the card's human-readable title from the folded job: the first
// line of the task text, falling back to the spec text, then the job id.
func title(j job.Job) string {
	for _, s := range []string{j.TaskText, j.SpecText} {
		if line := firstLine(s); line != "" {
			return line
		}
	}
	return j.ID
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// timelineNote maps an event kind to a short human-readable note (empty => the
// event is folded silently, e.g. heartbeats, and is omitted from the timeline).
func timelineNote(e ledger.Event) string {
	switch e.Kind {
	case ledger.KindJobCreated:
		return "Job created."
	case ledger.KindLeaseClaimed:
		return "Lease claimed by " + boundWorker(e, "a worker") + "."
	case ledger.KindResultAccepted:
		return "Build result accepted -> review pending."
	case ledger.KindReviewClaimed:
		return "Code review claimed by " + boundWorker(e, "a reviewer") + "."
	case ledger.KindVerdictMinted:
		if e.Payload.Verdict != nil {
			return "Verdict minted: " + string(e.Payload.Verdict.Value) + " (" + string(e.Payload.Verdict.Disposition) + ")."
		}
		return "Verdict minted."
	case ledger.KindReviewApproved:
		return "Consensus approval recorded -> re-armed review_pending for the next reviewer (F5 panel, below quorum)."
	case ledger.KindReviewBounced:
		return "Review bounced -> build re-armed (changes requested)."
	case ledger.KindBounceExhausted:
		return "Bounce budget exhausted -> needs human."
	case ledger.KindMergeStarted:
		return "Self-merge dispatched (Branch B autonomous merge)."
	case ledger.KindMergeHandoff:
		return "Merge handed off."
	case ledger.KindUnattendedMerged:
		return "Merged unattended; provenance " + shortSHA(e.Payload.MergeProvenance) + "."
	case ledger.KindJobCompleted:
		return "Reconciled merged -> done."
	case ledger.KindPROpened:
		return fmt.Sprintf("PR #%d opened.", e.Payload.PRNumber)
	case ledger.KindIssueMaterialized:
		return fmt.Sprintf("Issue #%d materialized.", e.Payload.IssueNumber)
	case ledger.KindSpecAuthored:
		return "Spec authored."
	case ledger.KindSpecSignoffMinted:
		return "Spec signed off."
	case ledger.KindSpecAmended:
		return "Spec amended in place (issue-review)."
	case ledger.KindSpecBounced:
		return "Spec bounced to author."
	case ledger.KindSpecNeedsDesign:
		return "Spec flagged needs-design."
	case ledger.KindSuperseded:
		return "Superseded by a SHA move -> re-armed."
	case ledger.KindRebased:
		return "Auto-rebased onto current main -> re-validate."
	case ledger.KindConflictDetected:
		return "Merge conflict -> conflict_resolver job."
	case ledger.KindConflictResolved:
		return "Conflict resolved -> re-review + re-CI."
	case ledger.KindLeaseRevoked:
		return "Lease revoked (" + nonEmpty(e.Payload.RevokeReason, "stall") + ") -> re-dispatched."
	case ledger.KindStallEscalated:
		return "Stall governor ceiling -> needs human."
	case ledger.KindCostEscalated:
		return "Cost ceiling crossed -> needs human."
	case ledger.KindCostMetered:
		return "" // metering is silent in the timeline (rolled up elsewhere)
	case ledger.KindTestCIRecorded:
		return "Flowbee test job recorded a CI result."
	case ledger.KindAdopted:
		return "Adopted (imported quiescent)."
	default:
		return ""
	}
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// boundWorker renders the node's worker plus the model that actually did the work, e.g.
// "feller-builder-2 (codex)" — so a card shows WHICH model built/reviewed each node. It
// prefers BoundModel (the real backend, recorded since the codex migration); for older
// events that predate it, it falls back to BoundModelFamily (the anti-affinity tag, which
// for a pre-codex job WAS the real model). No model on either field => just the identity.
func boundWorker(e ledger.Event, fallbackIdentity string) string {
	who := nonEmpty(e.Payload.BoundIdentity, fallbackIdentity)
	model := strings.TrimSpace(e.Payload.BoundModel)
	if model == "" {
		model = strings.TrimSpace(e.Payload.BoundModelFamily)
	}
	if model != "" {
		return who + " (" + model + ")"
	}
	return who
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(none)"
	}
	return s
}

// CardPath is the canonical archive path for a job's history card, relative to the
// repo root. The id is sanitized so a hostile/odd job id can never escape the
// docs/history directory (path traversal / separators are stripped).
func CardPath(jobID string) string {
	return "docs/history/" + sanitizeID(jobID) + ".md"
}

// sanitizeID reduces a job id to a filesystem-safe slug (alnum, dash, underscore).
// Anything else becomes a dash, so a card path is always inside docs/history.
func sanitizeID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		return "unknown"
	}
	return s
}

// Render produces the curated markdown card for a job (the docs/history/<id>.md
// body). PURE + total: the same Card always renders byte-identical markdown, so the
// archive is fully regenerable from the ledger. Times are rendered in UTC RFC3339
// so the output is deterministic regardless of the writer's locale.
func Render(c Card) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", mdEscape(c.Title))
	fmt.Fprintf(&b, "- **Job:** `%s`\n", c.JobID)
	fmt.Fprintf(&b, "- **Status:** %s\n", c.Status)
	if c.Kind != "" {
		fmt.Fprintf(&b, "- **Kind / flow:** %s / %s\n", c.Kind, nonEmpty(c.Flow, "—"))
	}
	if c.IssueNum > 0 {
		fmt.Fprintf(&b, "- **Issue:** #%d\n", c.IssueNum)
	}
	if c.PRNumber > 0 {
		fmt.Fprintf(&b, "- **PR:** #%d\n", c.PRNumber)
	}
	if c.MergeCommit != "" {
		fmt.Fprintf(&b, "- **Merge commit:** `%s`\n", c.MergeCommit)
	}
	if c.BaseSHA != "" || c.HeadSHA != "" {
		fmt.Fprintf(&b, "- **SHAs:** base `%s` head `%s`\n", shortSHA(c.BaseSHA), shortSHA(c.HeadSHA))
	}
	fmt.Fprintf(&b, "- **Attempts:** %d · **Bounces:** %d · **Stall revocations:** %d\n",
		c.Attempts, c.Bounces, c.StallRevocations)
	if !c.Created.IsZero() {
		fmt.Fprintf(&b, "- **Created:** %s\n", c.Created.UTC().Format(time.RFC3339))
	}
	if !c.Updated.IsZero() {
		fmt.Fprintf(&b, "- **Last event:** %s\n", c.Updated.UTC().Format(time.RFC3339))
	}
	b.WriteString("\n")

	if len(c.Verdicts) > 0 {
		b.WriteString("## Verdicts\n\n")
		for _, v := range c.Verdicts {
			fmt.Fprintf(&b, "- `%s`", v.Value)
			if v.Disposition != "" {
				fmt.Fprintf(&b, " (%s)", v.Disposition)
			}
			fmt.Fprintf(&b, " — head `%s` base `%s`", shortSHA(v.HeadSHA), shortSHA(v.BaseSHA))
			if v.Integrity != "" {
				fmt.Fprintf(&b, " — `%s`", v.Integrity)
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if len(c.Lessons) > 0 {
		b.WriteString("## Lessons\n\n")
		for _, l := range dedupeStable(c.Lessons) {
			fmt.Fprintf(&b, "- %s\n", l)
		}
		b.WriteString("\n")
	}

	if len(c.Timeline) > 0 {
		b.WriteString("## Timeline\n\n")
		for _, t := range c.Timeline {
			ts := ""
			if !t.At.IsZero() {
				ts = t.At.UTC().Format(time.RFC3339) + " — "
			}
			fmt.Fprintf(&b, "%d. %s%s\n", t.Seq, ts, t.Note)
		}
		b.WriteString("\n")
	}

	b.WriteString("---\n\n")
	b.WriteString("_Generated by Flowbee as a read-model fold over the event ledger. The ledger is canonical; this file is regenerable and Flowbee is its sole writer._\n")
	return b.String()
}

// dedupeStable removes duplicate lessons while preserving first-seen order (a job
// that bounces twice records two numbered bounce lessons, but identical escalation
// lessons collapse to one).
func dedupeStable(ss []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range ss {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// TOCEntry is one row of the generated table of contents — the minimal index a
// reader (or a precedent gate) scans before opening a card.
type TOCEntry struct {
	JobID    string
	Title    string
	Status   job.State
	IssueNum int
	PRNumber int
	Updated  time.Time
}

// EntryFromCard projects a Card to its TOC row.
func EntryFromCard(c Card) TOCEntry {
	return TOCEntry{
		JobID: c.JobID, Title: c.Title, Status: c.Status,
		IssueNum: c.IssueNum, PRNumber: c.PRNumber, Updated: c.Updated,
	}
}

// RenderTOC renders docs/history/README.md — the generated index over every card.
// Entries are sorted deterministically (by job id) so the file is stable across
// regenerations regardless of input order. Each row links to the card's relative
// path so the index is navigable in any markdown viewer.
func RenderTOC(entries []TOCEntry) string {
	sorted := make([]TOCEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].JobID < sorted[j].JobID })

	var b strings.Builder
	b.WriteString("# Issue history\n\n")
	b.WriteString("A read-model projection of Flowbee's event ledger: one card per completed job. ")
	b.WriteString("The ledger is canonical; these files are regenerable and Flowbee is their sole writer.\n\n")
	if len(sorted) == 0 {
		b.WriteString("_No history cards yet._\n")
		return b.String()
	}
	b.WriteString("| Job | Title | Status | Issue | PR | Last event |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, e := range sorted {
		issue := "—"
		if e.IssueNum > 0 {
			issue = fmt.Sprintf("#%d", e.IssueNum)
		}
		pr := "—"
		if e.PRNumber > 0 {
			pr = fmt.Sprintf("#%d", e.PRNumber)
		}
		updated := "—"
		if !e.Updated.IsZero() {
			updated = e.Updated.UTC().Format(time.RFC3339)
		}
		link := fmt.Sprintf("[`%s`](./%s.md)", e.JobID, sanitizeID(e.JobID))
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			link, mdEscapeCell(e.Title), e.Status, issue, pr, updated)
	}
	return b.String()
}

// mdEscape neutralizes a leading markdown control sequence in a title used in a
// heading (best-effort; the title is human intent, not adversarial markup).
func mdEscape(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(untitled job)"
	}
	return s
}

// mdEscapeCell escapes pipe characters so a title never breaks a TOC table row.
func mdEscapeCell(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		s = "(untitled)"
	}
	return strings.ReplaceAll(s, "|", "\\|")
}

// TOCPath is the canonical path of the generated table of contents.
const TOCPath = "docs/history/README.md"
