package history

import (
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// lifecycle builds a realistic merged-build event stream: created -> leased ->
// result -> review -> bounce -> re-build -> review -> verdict -> self_merge ->
// pr_opened -> reconciled-merged-done.
func lifecycle() []ledger.Event {
	t0 := time.Date(2026, 6, 16, 10, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, "head9", "base0")
	return []ledger.Event{
		{JobID: "j1", JobSeq: 1, Kind: ledger.KindJobCreated, ToState: job.StateReady, CreatedAt: at(0),
			Payload: ledger.Payload{Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
				BaseSHA: "base0", TaskText: "Add a /healthz endpoint\nmore detail here"}},
		{JobID: "j1", JobSeq: 2, Kind: ledger.KindLeaseClaimed, ToState: job.StateLeased, CreatedAt: at(1),
			Payload: ledger.Payload{BoundIdentity: "go-developer", LeaseID: "L1"}},
		{JobID: "j1", JobSeq: 3, Kind: ledger.KindResultAccepted, ToState: job.StateReviewPending, CreatedAt: at(2)},
		{JobID: "j1", JobSeq: 4, Kind: ledger.KindReviewClaimed, ToState: job.StateCodeReview, CreatedAt: at(3),
			Payload: ledger.Payload{BoundIdentity: "senior-code-reviewer", LeaseID: "L2"}},
		{JobID: "j1", JobSeq: 5, Kind: ledger.KindReviewBounced, ToState: job.StateReady, CreatedAt: at(4),
			Payload: ledger.Payload{BouncesDelta: 1}},
		{JobID: "j1", JobSeq: 6, Kind: ledger.KindResultAccepted, ToState: job.StateReviewPending, CreatedAt: at(5)},
		{JobID: "j1", JobSeq: 7, Kind: ledger.KindReviewClaimed, ToState: job.StateCodeReview, CreatedAt: at(6),
			Payload: ledger.Payload{BoundIdentity: "senior-code-reviewer", LeaseID: "L3"}},
		{JobID: "j1", JobSeq: 8, Kind: ledger.KindVerdictMinted, ToState: job.StateMergeable, CreatedAt: at(7),
			Payload: ledger.Payload{Verdict: &v}},
		{JobID: "j1", JobSeq: 9, Kind: ledger.KindMergeStarted, ToState: job.StateMerging, CreatedAt: at(8)},
		{JobID: "j1", JobSeq: 10, Kind: ledger.KindPROpened, ToState: job.StateMerging, CreatedAt: at(9),
			Payload: ledger.Payload{PRNumber: 42}},
		{JobID: "j1", JobSeq: 11, Kind: ledger.KindUnattendedMerged, ToState: job.StateMerging, CreatedAt: at(10),
			Payload: ledger.Payload{MergeProvenance: "mergecommit123456789"}},
		{JobID: "j1", JobSeq: 12, Kind: ledger.KindJobCompleted, ToState: job.StateDone, CreatedAt: at(11)},
	}
}

func TestFoldReconstructsCard(t *testing.T) {
	c, err := Fold(lifecycle())
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if c.JobID != "j1" {
		t.Fatalf("job id: %q", c.JobID)
	}
	if c.Status != job.StateDone {
		t.Fatalf("status: %s (want done)", c.Status)
	}
	if c.Title != "Add a /healthz endpoint" {
		t.Fatalf("title: %q", c.Title)
	}
	if c.PRNumber != 42 {
		t.Fatalf("pr: %d", c.PRNumber)
	}
	if c.Bounces != 1 {
		t.Fatalf("bounces: %d", c.Bounces)
	}
	if c.MergeCommit != "mergecommit123456789" {
		t.Fatalf("merge commit: %q", c.MergeCommit)
	}
	if len(c.Verdicts) != 1 || c.Verdicts[0].Value != job.VerdictApproved {
		t.Fatalf("verdicts: %+v", c.Verdicts)
	}
	if c.Verdicts[0].Integrity == "" {
		t.Fatalf("verdict integrity hash not folded")
	}
	// the bounce lesson must be present (the precedent record).
	foundBounce := false
	for _, l := range c.Lessons {
		if strings.Contains(l, "bounced") {
			foundBounce = true
		}
	}
	if !foundBounce {
		t.Fatalf("bounce lesson missing: %+v", c.Lessons)
	}
	if c.Created.IsZero() || c.Updated.IsZero() {
		t.Fatalf("created/updated not folded: %v / %v", c.Created, c.Updated)
	}
}

func TestRenderIsDeterministicAndCurated(t *testing.T) {
	c, _ := Fold(lifecycle())
	a := Render(c)
	b := Render(c)
	if a != b {
		t.Fatalf("render not deterministic")
	}
	for _, want := range []string{
		"# Add a /healthz endpoint",
		"**Status:** done",
		"**PR:** #42",
		"## Verdicts",
		"approved",
		"## Lessons",
		"## Timeline",
		"Verdict minted",
		"Merged unattended",
		"read-model fold over the event ledger",
	} {
		if !strings.Contains(a, want) {
			t.Fatalf("rendered card missing %q\n---\n%s", want, a)
		}
	}
}

func TestRenderTOCSortedAndStable(t *testing.T) {
	entries := []TOCEntry{
		{JobID: "j-zeta", Title: "Zeta", Status: job.StateDone, PRNumber: 9},
		{JobID: "j-alpha", Title: "Alpha", Status: job.StateDone, IssueNum: 3},
	}
	out := RenderTOC(entries)
	ai := strings.Index(out, "j-alpha")
	zi := strings.Index(out, "j-zeta")
	if ai < 0 || zi < 0 || ai > zi {
		t.Fatalf("TOC not sorted by job id:\n%s", out)
	}
	if !strings.Contains(out, "[`j-alpha`](./j-alpha.md)") {
		t.Fatalf("TOC missing card link:\n%s", out)
	}
	// stable regardless of input order.
	reversed := []TOCEntry{entries[1], entries[0]}
	if RenderTOC(reversed) != out {
		t.Fatalf("TOC not stable under input reorder")
	}
}

func TestCardPathSanitizesID(t *testing.T) {
	if got := CardPath("../../etc/passwd"); got != "docs/history/------etc-passwd.md" {
		t.Fatalf("path traversal not sanitized: %q", got)
	}
	if got := CardPath("j1"); got != "docs/history/j1.md" {
		t.Fatalf("plain id: %q", got)
	}
}

func TestEmptyTOC(t *testing.T) {
	out := RenderTOC(nil)
	if !strings.Contains(out, "No history cards yet") {
		t.Fatalf("empty TOC body: %s", out)
	}
}
