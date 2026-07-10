package engine

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

func codeReviewState(epoch int) EngineState {
	return EngineState{
		Job: job.Job{
			ID: "j1", Kind: job.KindBuild, State: job.StateCodeReview,
			Role: job.RoleCodeReviewer, MaxBounces: 3, LeaseEpoch: epoch,
		},
		Now:   time.Unix(2000, 0),
		Epoch: epoch,
	}
}

// TestDecideReviewMintsFromFactsNotClaim: engine.Decide mints a verdict on an
// approved claim ONLY when EngineState.GitHub is green; a stale epoch is rejected;
// a hostile approve over red facts bounces with no mint (I-9).
func TestDecideReviewMintsFromFactsNotClaim(t *testing.T) {
	s := codeReviewState(5)
	s.GitHub = job.DomainBFacts{PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: true}

	dec := Decide(s, ReviewClaim{Epoch: 5, Value: job.VerdictApproved, Disposition: job.DispositionHandoff})
	if dec.Reject != nil {
		t.Fatalf("valid epoch rejected: %+v", dec.Reject)
	}
	if dec.VerdictMint == nil {
		t.Fatal("green approval should mint a verdict")
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateMergeable ||
		dec.Transitions[0].Kind != ledger.KindVerdictMinted {
		t.Fatalf("approval should transition code_review->mergeable via verdict_minted: %+v", dec.Transitions)
	}

	// stale epoch -> reject, no transition.
	if d := Decide(s, ReviewClaim{Epoch: 4, Value: job.VerdictApproved}); d.Reject == nil {
		t.Fatal("stale-epoch review claim must be rejected (409)")
	}

	// hostile approve over RED facts -> bounce, no mint.
	red := codeReviewState(5)
	red.GitHub = job.DomainBFacts{PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: false}
	d := Decide(red, ReviewClaim{Epoch: 5, Value: job.VerdictApproved})
	if d.VerdictMint != nil {
		t.Fatal("approve over red facts must NOT mint (I-9)")
	}
	// a bounce re-arms the build stage (`ready`), not `building` (§6.2.2).
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateReady {
		t.Fatalf("approve over red facts should bounce to ready: %+v", d.Transitions)
	}
}

// TestDecideReviewBounceExhausts: a changes_requested at the bounce ceiling sends
// the job to needs_human.
func TestDecideReviewBounceExhausts(t *testing.T) {
	s := codeReviewState(2)
	s.Job.Bounces = 2 // next bounce hits max_bounces=3
	d := Decide(s, ReviewClaim{Epoch: 2, Value: job.VerdictChangesRequested})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateNeedsHuman {
		t.Fatalf("exhausted bounce should -> needs_human: %+v", d.Transitions)
	}
}

// TestDecideMergeDispatchBranch: a mergeable job defaults to merge_handoff; with
// policy ON and a self_merge verdict it goes to merging.
func TestDecideMergeDispatchBranch(t *testing.T) {
	base := EngineState{Job: job.Job{State: job.StateMergeable}, Now: time.Unix(3000, 0)}

	d := Decide(base, MergeDispatch{})
	if len(d.Transitions) != 0 {
		t.Fatalf("dispatch without a current SHA-bound verdict and green facts must wait: %+v", d.Transitions)
	}

	handoff := base
	hv := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h", "b")
	handoff.Job.Verdict = &hv
	handoff.GitHub = job.DomainBFacts{PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: true}
	d = Decide(handoff, MergeDispatch{})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateMergeHandoff {
		t.Fatalf("green approved handoff dispatch should be handoff: %+v", d.Transitions)
	}

	sm := base
	sm.Policy = job.Policy{AllowSelfMerge: true}
	v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, "h", "b")
	sm.Job.Verdict = &v
	// M9 (§5.4 conditions 2–5): self_merge also requires a clean content-integrity
	// Result AND the verdict still bound to the reconciled SHA pair.
	sm.GitHub = job.DomainBFacts{PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: true}
	sm.Content = &content.Result{DenylistClear: true, BlastRadiusConsistent: true, StaticChecksPass: true}
	d = Decide(sm, MergeDispatch{})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateMerging {
		t.Fatalf("policy-on self_merge dispatch should be merging: %+v", d.Transitions)
	}

	// the SAME verdict but a FAILING content Result (denylist hit) falls back to
	// handoff even with policy on (M9, I-11).
	tampered := sm
	tampered.Content = &content.Result{DenylistClear: false, BlastRadiusConsistent: true, StaticChecksPass: true}
	d = Decide(tampered, MergeDispatch{})
	if len(d.Transitions) != 1 || d.Transitions[0].To != job.StateMergeHandoff {
		t.Fatalf("self_merge over a denylisted diff must fall back to handoff: %+v", d.Transitions)
	}
}

func TestDecideMergeDispatchWaitsForRequiredChecks(t *testing.T) {
	v := job.MintVerdict(job.VerdictApproved, job.DispositionSelfMerge, "h", "b")
	base := EngineState{
		Job:     job.Job{State: job.StateMergeable, Verdict: &v},
		Now:     time.Unix(3000, 0),
		Policy:  job.Policy{AllowSelfMerge: true},
		Content: &content.Result{DenylistClear: true, BlastRadiusConsistent: true, StaticChecksPass: true},
	}

	for name, facts := range map[string]job.DomainBFacts{
		"pending_ci":    {PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: false},
		"missing_pr":    {HeadSHA: "h", BaseSHA: "b", CIGreen: true},
		"unknown_sha":   {PRExists: true, CIGreen: true},
		"stale_binding": {PRExists: true, HeadSHA: "new", BaseSHA: "b", CIGreen: true},
		"unknown_mergeable_state": {
			PRExists: true, HeadSHA: "h", BaseSHA: "b", CIGreen: true, MergeableState: "UNKNOWN",
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := base
			s.GitHub = facts
			if d := Decide(s, MergeDispatch{}); len(d.Transitions) != 0 {
				t.Fatalf("non-terminal/current-success CI facts must not transition to merge: %+v", d.Transitions)
			}
		})
	}
}
