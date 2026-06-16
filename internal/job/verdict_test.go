package job

import (
	"testing"

	"github.com/samhotchkiss/flowbee/internal/content"
)

// cleanContent is a content-integrity Result that passes all three §9.2 conditions
// (the M9 prerequisite for self_merge eligibility).
func cleanContent() *content.Result {
	return &content.Result{DenylistClear: true, BlastRadiusConsistent: true, StaticChecksPass: true}
}

func greenFacts() DomainBFacts {
	return DomainBFacts{PRExists: true, PRNumber: 7, HeadSHA: "head1", BaseSHA: "base1", CIGreen: true}
}

// TestMintVerdictIsTamperEvidentAndSHABound: a minted approval binds the exact
// reconciled SHA pair and a deterministic integrity hash; Verify holds for that
// pair and FAILS for any SHA move or hash tamper (I-9 / I-5).
func TestMintVerdictIsTamperEvidentAndSHABound(t *testing.T) {
	v := MintVerdict(VerdictApproved, DispositionHandoff, "head1", "base1")
	if v.IntegrityHash == "" || v.Provenance != "reconciled" || !v.TamperEvident {
		t.Fatalf("verdict not stamped: %+v", v)
	}
	if !v.Verify("head1", "base1") {
		t.Fatal("verdict should verify against its bound SHA pair")
	}
	// determinism: minting the same facts twice yields the same hash.
	v2 := MintVerdict(VerdictApproved, DispositionHandoff, "head1", "base1")
	if v.IntegrityHash != v2.IntegrityHash {
		t.Fatalf("mint not deterministic: %s != %s", v.IntegrityHash, v2.IntegrityHash)
	}
	// a SHA move invalidates the sign-off.
	if v.Verify("head2", "base1") || v.Verify("head1", "base2") {
		t.Fatal("verdict must NOT verify after a SHA move (supersede, I-5)")
	}
	// tampering the bound value breaks the hash.
	tampered := v
	tampered.Value = VerdictChangesRequested
	if tampered.Verify("head1", "base1") {
		t.Fatal("tampered verdict must not verify")
	}
}

// TestGateMintsApprovalOnlyFromReconciledFacts is the I-9 keystone at the unit
// level: an `approved` claim mints a verdict ONLY when reconciled facts are green;
// a hostile `approved` over red/missing facts bounces and mints nothing.
func TestGateMintsApprovalOnlyFromReconciledFacts(t *testing.T) {
	// honest approval, green facts -> mint, handoff (default policy).
	out := EvaluateGate(GateInputs{Claim: VerdictApproved, Disp: DispositionSelfMerge, Facts: greenFacts(), MaxBounce: 3})
	if out.Trigger != TriggerApproved || out.Verdict == nil {
		t.Fatalf("green approval should mint: %+v", out)
	}
	if out.Verdict.Disposition != DispositionHandoff {
		t.Fatalf("default policy must force handoff even when self_merge requested, got %s", out.Verdict.Disposition)
	}

	// hostile approval over RED CI -> no mint, bounces.
	red := greenFacts()
	red.CIGreen = false
	out = EvaluateGate(GateInputs{Claim: VerdictApproved, Facts: red, MaxBounce: 3})
	if out.Trigger == TriggerApproved || out.Verdict != nil {
		t.Fatalf("approval over red CI must NOT mint (I-9): %+v", out)
	}

	// hostile approval with NO reconciled PR -> no mint.
	out = EvaluateGate(GateInputs{Claim: VerdictApproved, Facts: DomainBFacts{}, MaxBounce: 3})
	if out.Trigger == TriggerApproved || out.Verdict != nil {
		t.Fatalf("approval over missing facts must NOT mint: %+v", out)
	}

	// approval over a MERGED PR -> no mint (terminal-SHA guard surface).
	merged := greenFacts()
	merged.Merged = true
	out = EvaluateGate(GateInputs{Claim: VerdictApproved, Facts: merged, MaxBounce: 3})
	if out.Trigger == TriggerApproved {
		t.Fatalf("approval over merged PR must NOT mint: %+v", out)
	}
}

// TestGateSelfMergeUnderPolicy: self_merge disposition is honored only when policy
// allows it (Branch B) AND the M9 content-integrity gate is clear; default (Branch
// A) or a failed content gate forces handoff.
func TestGateSelfMergeUnderPolicy(t *testing.T) {
	// policy ON + clean content -> self_merge honored.
	out := EvaluateGate(GateInputs{
		Claim: VerdictApproved, Disp: DispositionSelfMerge, Facts: greenFacts(), MaxBounce: 3,
		Policy: Policy{AllowSelfMerge: true}, Content: cleanContent(),
	})
	if out.Verdict == nil || out.Verdict.Disposition != DispositionSelfMerge {
		t.Fatalf("policy-enabled self_merge over a clean diff should be honored: %+v", out)
	}

	// policy OFF (Branch A) -> handoff regardless of the clean content.
	out = EvaluateGate(GateInputs{
		Claim: VerdictApproved, Disp: DispositionSelfMerge, Facts: greenFacts(), MaxBounce: 3,
		Policy: Policy{AllowSelfMerge: false}, Content: cleanContent(),
	})
	if out.Verdict == nil || out.Verdict.Disposition != DispositionHandoff {
		t.Fatalf("Branch A must force handoff even over a clean diff: %+v", out)
	}

	// policy ON but NO content gate ran (nil) -> handoff (absence of proof is denial).
	out = EvaluateGate(GateInputs{
		Claim: VerdictApproved, Disp: DispositionSelfMerge, Facts: greenFacts(), MaxBounce: 3,
		Policy: Policy{AllowSelfMerge: true}, Content: nil,
	})
	if out.Verdict == nil || out.Verdict.Disposition != DispositionHandoff {
		t.Fatalf("a nil content Result must force handoff: %+v", out)
	}
}

// TestGateContentFailureForcesHandoff is the M9 §5.4 conditions-2–4 truth: an
// approved + self_merge request over a diff that hits the denylist, exceeds its
// declared blast-radius, or fails static checks STILL mints the approval but is
// forced to handoff (I-11). The verdict is never blocked — only its disposition.
func TestGateContentFailureForcesHandoff(t *testing.T) {
	cases := []struct {
		name string
		chk  *content.Result
	}{
		{"denylist_hit", &content.Result{DenylistClear: false, BlastRadiusConsistent: true, StaticChecksPass: true}},
		{"blast_radius_exceeded", &content.Result{DenylistClear: true, BlastRadiusConsistent: false, StaticChecksPass: true}},
		{"static_checks_failed", &content.Result{DenylistClear: true, BlastRadiusConsistent: true, StaticChecksPass: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := EvaluateGate(GateInputs{
				Claim: VerdictApproved, Disp: DispositionSelfMerge, Facts: greenFacts(), MaxBounce: 3,
				Policy: Policy{AllowSelfMerge: true}, Content: tc.chk,
			})
			if out.Trigger != TriggerApproved || out.Verdict == nil {
				t.Fatalf("a green-CI approval must still mint the verdict (I-9): %+v", out)
			}
			if out.Verdict.Disposition != DispositionHandoff {
				t.Fatalf("%s must force handoff despite the self_merge request: %+v", tc.name, out)
			}
		})
	}
}

// TestSelfMergeEligiblePredicate exercises the SINGLE canonical §5.4 predicate
// directly across all six conditions.
func TestSelfMergeEligiblePredicate(t *testing.T) {
	gh := greenFacts()
	v := MintVerdict(VerdictApproved, DispositionSelfMerge, gh.HeadSHA, gh.BaseSHA)

	if !SelfMergeEligible(v, gh, cleanContent(), Policy{AllowSelfMerge: true}) {
		t.Fatal("all conditions met must be eligible")
	}
	// condition 1: policy off.
	if SelfMergeEligible(v, gh, cleanContent(), Policy{AllowSelfMerge: false}) {
		t.Fatal("policy off must deny")
	}
	// conditions 2–4: any content failure denies.
	if SelfMergeEligible(v, gh, &content.Result{DenylistClear: false, BlastRadiusConsistent: true, StaticChecksPass: true}, Policy{AllowSelfMerge: true}) {
		t.Fatal("denylist hit must deny")
	}
	// nil content denies.
	if SelfMergeEligible(v, gh, nil, Policy{AllowSelfMerge: true}) {
		t.Fatal("nil content must deny")
	}
	// condition 5: a SHA move since the mint supersedes the binding.
	moved := gh
	moved.HeadSHA = "head-moved"
	if SelfMergeEligible(v, moved, cleanContent(), Policy{AllowSelfMerge: true}) {
		t.Fatal("a moved head SHA must deny (binding superseded)")
	}
}

// TestGateBounceAndExhaust: changes_requested bounces below the ceiling and
// exhausts to needs_human at max_bounces (counted on bounces, not attempts).
func TestGateBounceAndExhaust(t *testing.T) {
	// bounces=0, max=3: a changes_requested bounces (->building).
	out := EvaluateGate(GateInputs{Claim: VerdictChangesRequested, Bounces: 0, MaxBounce: 3})
	if out.Trigger != TriggerBounce {
		t.Fatalf("first changes_requested should bounce: %+v", out)
	}
	// bounces=2, max=3: the third changes_requested exhausts.
	out = EvaluateGate(GateInputs{Claim: VerdictChangesRequested, Bounces: 2, MaxBounce: 3})
	if out.Trigger != TriggerBounceExhausted {
		t.Fatalf("bounce at ceiling should exhaust: %+v", out)
	}
}
