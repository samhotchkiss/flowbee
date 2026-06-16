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

// TestSpecGateAmendInPlace: the F4 issue-review amend path. A sub-standard spec
// (style/requirements failing) is AMENDED in place — the reviewer supplies amended
// bytes, the gate mints a sign-off bound to the AMENDED hash, and the outcome is
// SpecAmended (-> done). It NEVER bounces to the author.
func TestSpecGateAmendInPlace(t *testing.T) {
	in := SpecGateInputs{
		Claim: VerdictAmended, ClaimBindsTo: "blake3:H",
		CurrentSpecHash: "blake3:H", SpecVersion: 1,
		ReviewerLens: "engineering_manager", AuthorLens: "product_speccer", MaxBounce: 3,
		// the original sub-checks FAILED (the spec was sub-standard) — amend ignores
		// them; the reviewer FIXED the spec rather than bouncing it.
		MeetsStyle: false, MeetsRequirements: false,
		AmendedHash: "blake3:AMENDED", AmendedVersion: 2,
	}
	out := EvaluateSpecGate(in)
	if out.Trigger != TriggerSpecAmended {
		t.Fatalf("a sub-standard spec must AMEND in place, got trigger %s (%s)", out.Trigger, out.Reason)
	}
	if out.Signoff == nil {
		t.Fatal("an amend must mint a sign-off")
	}
	// the sign-off binds to the AMENDED bytes, not the reviewed (sub-standard) bytes.
	if !out.Signoff.Verify("blake3:AMENDED") {
		t.Fatal("the sign-off must bind to the AMENDED content hash")
	}
	if out.Signoff.Verify("blake3:H") {
		t.Fatal("the sign-off must NOT bind to the original (pre-amend) hash")
	}
	if out.AmendedHash != "blake3:AMENDED" || out.AmendedVersion != 2 {
		t.Fatalf("the amend outcome must carry the new content address: %+v", out)
	}

	// a no-op "amendment" (amended hash == reviewed hash) is NOT an amend — it falls
	// through to the normal conjunction (and bounces here, since sub-checks fail).
	noop := in
	noop.AmendedHash = "blake3:H"
	if out := EvaluateSpecGate(noop); out.Trigger == TriggerSpecAmended {
		t.Fatalf("a no-op amendment must not count as an amend, got %+v", out)
	}

	// an author-lens reviewer may NOT amend either (§5.5 distinct-lens holds across
	// every arm).
	sameLens := in
	sameLens.ReviewerLens = "product_speccer"
	if out := EvaluateSpecGate(sameLens); out.Trigger == TriggerSpecAmended {
		t.Fatalf("an author-lens reviewer must not amend, got %+v", out)
	}
}

// TestSpecGateNeedsDesign: the F4 design-fork escalation. A needs_design claim
// routes to SpecNeedsDesign (no sign-off, no bounce) — even when the sub-checks pass.
func TestSpecGateNeedsDesign(t *testing.T) {
	in := SpecGateInputs{
		Claim: VerdictNeedsDesign, ClaimBindsTo: "blake3:H",
		CurrentSpecHash: "blake3:H", SpecVersion: 1,
		ReviewerLens: "engineering_manager", AuthorLens: "product_speccer", MaxBounce: 3,
		MeetsStyle: true, MeetsRequirements: true,
	}
	out := EvaluateSpecGate(in)
	if out.Trigger != TriggerSpecNeedsDesign {
		t.Fatalf("a design fork must escalate to needs_design, got %s", out.Trigger)
	}
	if out.Signoff != nil {
		t.Fatal("a design fork must NOT mint a sign-off")
	}
	// a stale binding still supersedes before the needs_design arm is reached.
	stale := in
	stale.ClaimBindsTo = "blake3:STALE"
	if out := EvaluateSpecGate(stale); out.Trigger != TriggerSpecSuperseded {
		t.Fatalf("a stale binding must supersede even on a needs_design claim, got %s", out.Trigger)
	}
}
