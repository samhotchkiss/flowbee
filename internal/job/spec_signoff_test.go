package job

import "testing"

// TestSpecSignoffTamperEvidentAndBound: a minted spec sign-off verifies against the
// bound hash, fails against any other hash (the §11.5 supersession), and fails if
// the integrity stamp is tampered (I-9).
func TestSpecSignoffTamperEvidentAndBound(t *testing.T) {
	s := MintSpecSignoff("blake3:H1", 1, "staff_engineer")
	if s.Value != VerdictSignedOff || !s.TamperEvident || s.Provenance != "minted" {
		t.Fatalf("unexpected sign-off shape: %+v", s)
	}
	if !s.Verify("blake3:H1") {
		t.Fatal("must verify against the bound hash")
	}
	if s.Verify("blake3:H2") {
		t.Fatal("must NOT verify against a different hash (supersession, §11.5)")
	}
	// a tampered integrity hash fails verification.
	tampered := s
	tampered.IntegrityHash = "sha256:deadbeef"
	if tampered.Verify("blake3:H1") {
		t.Fatal("a tampered sign-off must not verify")
	}
	// a forged "minted" provenance with no real hash fails.
	forged := SpecSignoff{Value: VerdictSignedOff, SpecHash: "blake3:H1", Provenance: "minted", TamperEvident: true}
	if forged.Verify("blake3:H1") {
		t.Fatal("a sign-off without a valid integrity stamp must not verify")
	}
}

// TestSpecGatePure is a focused truth-table check of EvaluateSpecGate.
func TestSpecGatePure(t *testing.T) {
	base := SpecGateInputs{
		CurrentSpecHash: "blake3:H", SpecVersion: 1,
		ReviewerLens: "staff_engineer", AuthorLens: "product_speccer", MaxBounce: 3,
	}
	// signed_off + both pass + current hash + distinct lens -> mint.
	in := base
	in.Claim, in.ClaimBindsTo, in.MeetsStyle, in.MeetsRequirements = VerdictSignedOff, "blake3:H", true, true
	if out := EvaluateSpecGate(in); out.Trigger != TriggerSpecSignedOff || out.Signoff == nil {
		t.Fatalf("expected sign-off, got %+v", out)
	}
	// stale binding -> superseded, no mint.
	in.ClaimBindsTo = "blake3:STALE"
	if out := EvaluateSpecGate(in); out.Trigger != TriggerSpecSuperseded || out.Signoff != nil {
		t.Fatalf("expected superseded, got %+v", out)
	}
	// changes_requested -> bounce.
	in = base
	in.Claim, in.ClaimBindsTo = VerdictChangesRequested, "blake3:H"
	if out := EvaluateSpecGate(in); out.Trigger != TriggerBounce {
		t.Fatalf("expected bounce, got %+v", out)
	}
	// at max_bounces -> exhaust.
	in.Bounces = 2
	if out := EvaluateSpecGate(in); out.Trigger != TriggerBounceExhausted {
		t.Fatalf("expected bounce exhausted, got %+v", out)
	}
}
