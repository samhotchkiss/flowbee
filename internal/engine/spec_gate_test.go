package engine

import (
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

func specReviewState(epoch int, hash, authorLens, reviewerLens string) EngineState {
	return EngineState{
		Job: job.Job{
			ID: "s1", Kind: job.KindSpec, State: job.StateSpecReview,
			Role: job.RoleSpecReviewer, MaxBounces: 3, LeaseEpoch: epoch,
		},
		Now:   time.Unix(3000, 0),
		Epoch: epoch,
		Spec:  SpecState{CurrentHash: hash, Version: 2, AuthorLens: authorLens, ReviewerLens: reviewerLens},
	}
}

// TestSpecGateMintsFromBytesNotClaim: the spec gate mints a content-hash-bound
// sign-off ONLY when the claim binds to the CURRENT hash AND both sub-checks pass
// AND the reviewer lens differs from the author lens (I-9, §11.5).
func TestSpecGateMintsFromBytesNotClaim(t *testing.T) {
	s := specReviewState(4, "blake3:H2", "product_speccer", "staff_engineer")

	dec := Decide(s, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictSignedOff, ClaimBindsTo: "blake3:H2",
		MeetsStyle: true, MeetsRequirements: true,
	})
	if dec.Reject != nil {
		t.Fatalf("valid claim rejected: %+v", dec.Reject)
	}
	if dec.SpecMint == nil {
		t.Fatal("a current-hash signed-off claim must mint a sign-off")
	}
	if !dec.SpecMint.Verify("blake3:H2") {
		t.Fatal("the minted sign-off must verify against the bound hash")
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateDone ||
		dec.Transitions[0].Kind != ledger.KindSpecSignoffMinted {
		t.Fatalf("sign-off must transition spec_review->done via spec_signoff_minted: %+v", dec.Transitions)
	}
}

// TestSpecGateStaleHashSupersedes: a claim bound to a STALE hash is rejected as
// superseded — a sign-off over old bytes never mints (§11.5).
func TestSpecGateStaleHashSupersedes(t *testing.T) {
	s := specReviewState(4, "blake3:H2", "product_speccer", "staff_engineer")
	dec := Decide(s, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictSignedOff, ClaimBindsTo: "blake3:H1-STALE",
		MeetsStyle: true, MeetsRequirements: true,
	})
	if dec.SpecMint != nil {
		t.Fatal("a stale-hash claim must NOT mint")
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].Kind != ledger.KindSpecSuperseded {
		t.Fatalf("a stale-hash claim must be superseded: %+v", dec.Transitions)
	}
}

// TestSpecGateConjunctionAndLens: a blocking sub-check OR an author-lens reviewer
// never mints; the decision field is a claim, Flowbee concludes the verdict (I-9).
func TestSpecGateConjunctionAndLens(t *testing.T) {
	// a signed_off claim with a FAILED requirements sub-check bounces.
	s := specReviewState(4, "blake3:H2", "product_speccer", "staff_engineer")
	dec := Decide(s, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictSignedOff, ClaimBindsTo: "blake3:H2",
		MeetsStyle: true, MeetsRequirements: false,
	})
	if dec.SpecMint != nil {
		t.Fatal("a failed requirements sub-check must NOT mint (conjunction, §11.3)")
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateSpecAuthoring {
		t.Fatalf("a failed conjunction must bounce to spec_authoring: %+v", dec.Transitions)
	}

	// a reviewer whose lens equals the author lens never mints (§5.5, defense in depth).
	sameLens := specReviewState(4, "blake3:H2", "product_speccer", "product_speccer")
	dec2 := Decide(sameLens, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictSignedOff, ClaimBindsTo: "blake3:H2",
		MeetsStyle: true, MeetsRequirements: true,
	})
	if dec2.SpecMint != nil {
		t.Fatal("an author-lens reviewer must NOT mint (§5.5 distinct-lens)")
	}
}

// TestSpecGateAmendInPlace_Engine: the F4 amend arm. An `amended` claim with new
// bytes mints a sign-off bound to the AMENDED hash and transitions spec_review->done
// via spec_amended — NEVER spec_authoring (no author bounce).
func TestSpecGateAmendInPlace_Engine(t *testing.T) {
	s := specReviewState(4, "blake3:H2", "product_speccer", "engineering_manager")
	dec := Decide(s, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictAmended, ClaimBindsTo: "blake3:H2",
		MeetsStyle: false, MeetsRequirements: false, // the spec was sub-standard
		AmendedHash: "blake3:AMENDED", AmendedVersion: 3,
	})
	if dec.Reject != nil {
		t.Fatalf("valid amend rejected: %+v", dec.Reject)
	}
	if dec.SpecMint == nil || !dec.SpecMint.Verify("blake3:AMENDED") {
		t.Fatalf("amend must mint a sign-off bound to the amended hash: %+v", dec.SpecMint)
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateDone ||
		dec.Transitions[0].Kind != ledger.KindSpecAmended {
		t.Fatalf("amend must transition spec_review->done via spec_amended (no author bounce): %+v", dec.Transitions)
	}
	for _, tr := range dec.Transitions {
		if tr.To == job.StateSpecAuthoring {
			t.Fatal("amend must NEVER bounce to spec_authoring")
		}
	}
}

// TestSpecGateNeedsDesign_Engine: the F4 design-fork arm escalates to needs_design.
func TestSpecGateNeedsDesign_Engine(t *testing.T) {
	s := specReviewState(4, "blake3:H2", "product_speccer", "engineering_manager")
	dec := Decide(s, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictNeedsDesign, ClaimBindsTo: "blake3:H2",
		MeetsStyle: true, MeetsRequirements: true,
	})
	if dec.SpecMint != nil {
		t.Fatal("a design fork must not mint")
	}
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateNeedsDesign ||
		dec.Transitions[0].Kind != ledger.KindSpecNeedsDesign {
		t.Fatalf("a design fork must transition spec_review->needs_design: %+v", dec.Transitions)
	}
}

// TestSpecGateStaleEpoch: a stale lease epoch is rejected (409), never evaluated.
// TestSpecGateEpicBounceEscalates: a changes_requested on an EPIC BARRIER has no author
// to bounce to (the decomposition is provided, not authored), so it must escalate to
// needs_human — not dead-end in spec_authoring. A non-epic spec still bounces to author.
func TestSpecGateEpicBounceEscalates(t *testing.T) {
	epic := specReviewState(4, "blake3:H2", "product_speccer", "engineering_manager")
	epic.Job.IsEpic = true
	dec := Decide(epic, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictChangesRequested, ClaimBindsTo: "blake3:H2",
	})
	if len(dec.Transitions) != 1 || dec.Transitions[0].To != job.StateNeedsHuman {
		t.Fatalf("epic barrier changes_requested must escalate to needs_human, got %+v", dec.Transitions)
	}

	// a NON-epic spec still bounces back to the author for a re-draft.
	plain := specReviewState(4, "blake3:H2", "product_speccer", "engineering_manager")
	pdec := Decide(plain, SpecReviewClaim{
		Epoch: 4, Claim: job.VerdictChangesRequested, ClaimBindsTo: "blake3:H2",
	})
	if len(pdec.Transitions) != 1 || pdec.Transitions[0].To != job.StateSpecAuthoring {
		t.Fatalf("non-epic spec changes_requested must bounce to spec_authoring, got %+v", pdec.Transitions)
	}
}

// TestSpecGateStaleEpoch: a stale lease epoch is rejected (409), never evaluated.
func TestSpecGateStaleEpoch(t *testing.T) {
	s := specReviewState(7, "blake3:H2", "product_speccer", "staff_engineer")
	dec := Decide(s, SpecReviewClaim{Epoch: 6, Claim: job.VerdictSignedOff, ClaimBindsTo: "blake3:H2", MeetsStyle: true, MeetsRequirements: true})
	if dec.Reject == nil {
		t.Fatal("a stale epoch must be rejected")
	}
	if dec.SpecMint != nil || len(dec.Transitions) != 0 {
		t.Fatal("a rejected claim must produce no mint/transition")
	}
}
