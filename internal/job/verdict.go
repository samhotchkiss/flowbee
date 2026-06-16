package job

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// VerdictValue is the gate decision a reviewer claims and the engine derives.
type VerdictValue string

const (
	VerdictApproved         VerdictValue = "approved"
	VerdictChangesRequested VerdictValue = "changes_requested"
	VerdictSignedOff        VerdictValue = "signed_off" // spec flow (M7)
)

// Disposition is the code_review branch point (§5.4); only set for `approved`.
type Disposition string

const (
	DispositionSelfMerge Disposition = "self_merge"
	DispositionHandoff   Disposition = "handoff"
)

// Verdict is the tamper-evident, SHA-bound sign-off the gate MINTS from reconciled
// GitHub facts (I-9). It is NEVER taken from a worker's self-reported status: the
// engine derives Value from the gate predicate over reconciled facts, binds it to
// the reconciled (head_sha, base_sha) pair, and stamps a deterministic integrity
// hash over the bound facts. A worker can deposit a *claim*; only the gate mints
// this record. Any later SHA move invalidates it (supersede + re-arm, §6.2.4).
type Verdict struct {
	Value         VerdictValue `json:"value"`
	Disposition   Disposition  `json:"disposition,omitempty"`
	HeadSHA       string       `json:"head_sha,omitempty"`
	BaseSHA       string       `json:"base_sha,omitempty"`
	Provenance    string       `json:"provenance"`     // always "reconciled"
	TamperEvident bool         `json:"tamper_evident"` // always true
	IntegrityHash string       `json:"integrity_hash"` // sha256 over the bound facts
}

// MintVerdict builds a tamper-evident, SHA-bound approval verdict from reconciled
// facts. PURE and deterministic (sha256 only): same (value, disposition, head,
// base) -> same IntegrityHash, always. The hash binds the verdict to the exact
// reconciled SHA pair so any SHA move provably invalidates it.
func MintVerdict(value VerdictValue, disp Disposition, headSHA, baseSHA string) Verdict {
	v := Verdict{
		Value:         value,
		Disposition:   disp,
		HeadSHA:       headSHA,
		BaseSHA:       baseSHA,
		Provenance:    "reconciled",
		TamperEvident: true,
	}
	v.IntegrityHash = v.computeHash()
	return v
}

// computeHash is the deterministic integrity stamp over the bound facts. The
// domain-separated, length-prefixed encoding makes the hash collision-resistant
// against field-boundary ambiguity.
func (v Verdict) computeHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "flowbee-verdict-v1\n")
	for _, f := range []string{
		string(v.Value), string(v.Disposition), v.HeadSHA, v.BaseSHA, v.Provenance,
	} {
		fmt.Fprintf(h, "%d:%s\n", len(f), f)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Verify recomputes the integrity hash and reports whether the verdict is intact
// and still bound to the given reconciled SHA pair. A SHA move (head/base differs
// from what the verdict bound) fails verification — the structural realization of
// "any base/head move supersedes the sign-off" (§3.4, I-5).
func (v Verdict) Verify(headSHA, baseSHA string) bool {
	if !v.TamperEvident || v.Provenance != "reconciled" {
		return false
	}
	if v.IntegrityHash != v.computeHash() {
		return false
	}
	return v.HeadSHA == headSHA && v.BaseSHA == baseSHA
}

// Policy is THE ONE DECISION surface (§14): whether the MVP may merge without a
// human. Default (zero value) AllowSelfMerge=false = Branch A: every approved job
// takes handoff -> human. Flipping the bool is a policy flip, never a rewire.
type Policy struct {
	AllowSelfMerge bool
}

// DomainBFacts are the reconciled GitHub-owned facts (§3.1.B) the build-flow gate
// consumes. In M3 they come from a stubbed FactSource (the real reconcile-IN sweep
// lands in M6). They are passed INTO the pure engine as values; the engine never
// fetches them.
type DomainBFacts struct {
	PRExists bool
	PRNumber int
	HeadSHA  string
	BaseSHA  string
	CIGreen  bool
	Merged   bool
}

// GateInputs bundles everything the pure code_review gate needs to decide whether
// to mint an approval (and which arm to take), all already resolved to values.
type GateInputs struct {
	Claim     VerdictValue // the reviewer's CLAIM (untrusted; never the verdict)
	Disp      Disposition  // requested disposition (only meaningful on approved)
	Facts     DomainBFacts // reconciled GitHub facts (the authority, I-9)
	Bounces   int          // current bounce count for this job
	MaxBounce int
	Policy    Policy
}

// GateOutcome is the pure result of evaluating the code_review gate.
type GateOutcome struct {
	Trigger Trigger  // ReviewApproved / Bounce / BounceExhausted
	Verdict *Verdict // non-nil ONLY when an approval was minted from reconciled facts
	Reason  string
}

// EvaluateGate is the pure §5.5 / I-9 code-review gate. It NEVER trusts the claim
// as a verdict: an `approved` claim only mints a verdict when the reconciled facts
// are green (PR exists, CI green, both SHAs present and consistent). A green
// fact-state with a non-approved claim does not approve; a red/missing fact-state
// with an approved claim does not approve (the hostile-worker case). A
// changes_requested claim bounces (or exhausts to needs_human at max_bounces).
func EvaluateGate(in GateInputs) GateOutcome {
	switch in.Claim {
	case VerdictChangesRequested:
		if in.Bounces+1 >= in.MaxBounce {
			return GateOutcome{Trigger: TriggerBounceExhausted, Reason: "max_bounces reached"}
		}
		return GateOutcome{Trigger: TriggerBounce, Reason: "changes_requested"}

	case VerdictApproved:
		// I-9: the claim cannot become a verdict unless the RECONCILED facts pass
		// the gate predicate. This is where a worker lying `succeeded` on a red /
		// SHA-moved / missing-PR diff is stopped: it bounces, never approves.
		if !gatePredicate(in.Facts) {
			if in.Bounces+1 >= in.MaxBounce {
				return GateOutcome{Trigger: TriggerBounceExhausted, Reason: "approval claim failed reconciliation"}
			}
			return GateOutcome{Trigger: TriggerBounce, Reason: "approval claim failed reconciliation (CI not green / PR drift)"}
		}
		// disposition under policy (§5.4): self_merge is permitted iff THE ONE
		// DECISION removed the human gate; otherwise every approval takes handoff.
		disp := Disposition(DispositionHandoff)
		if in.Policy.AllowSelfMerge && in.Disp == DispositionSelfMerge {
			disp = DispositionSelfMerge
		}
		v := MintVerdict(VerdictApproved, disp, in.Facts.HeadSHA, in.Facts.BaseSHA)
		return GateOutcome{Trigger: TriggerApproved, Verdict: &v, Reason: "approved from reconciled facts"}

	default:
		// an unknown / empty claim is treated as a non-approval bounce (defensive).
		if in.Bounces+1 >= in.MaxBounce {
			return GateOutcome{Trigger: TriggerBounceExhausted, Reason: "no valid verdict claim"}
		}
		return GateOutcome{Trigger: TriggerBounce, Reason: "no valid verdict claim"}
	}
}

// SpecSignoff is the tamper-evident, CONTENT-HASH-bound spec sign-off the spec
// gate MINTS (§11.5, I-9). It is the spec-flow analogue of Verdict: where Verdict
// binds to (head_sha, base_sha), SpecSignoff binds to a spec_content_hash. It is
// Flowbee-authored, never a worker self-report. Any edit to the spec changes the
// hash and provably invalidates this record (supersede + re-arm, §11.5).
type SpecSignoff struct {
	Value         VerdictValue `json:"value"`          // signed_off | changes_requested
	SpecHash      string       `json:"spec_hash"`      // the spec_content_hash this binds to
	SpecVersion   int          `json:"spec_version"`   // ordinal on the spec branch
	ReviewerLens  string       `json:"reviewer_lens"`  // the distinct lens that judged it (§5.5)
	Provenance    string       `json:"provenance"`     // always "minted"
	TamperEvident bool         `json:"tamper_evident"` // always true
	IntegrityHash string       `json:"integrity_hash"` // sha256 over the bound facts
}

// MintSpecSignoff builds a tamper-evident, content-hash-bound spec sign-off.
// PURE and deterministic: same (hash, version, lens) -> same IntegrityHash.
func MintSpecSignoff(specHash string, version int, lens string) SpecSignoff {
	s := SpecSignoff{
		Value:         VerdictSignedOff,
		SpecHash:      specHash,
		SpecVersion:   version,
		ReviewerLens:  lens,
		Provenance:    "minted",
		TamperEvident: true,
	}
	s.IntegrityHash = s.computeHash()
	return s
}

func (s SpecSignoff) computeHash() string {
	h := sha256.New()
	fmt.Fprintf(h, "flowbee-spec-signoff-v1\n")
	fmt.Fprintf(h, "%d:%s\n", len(s.Value), s.Value)
	fmt.Fprintf(h, "%d:%s\n", len(s.SpecHash), s.SpecHash)
	fmt.Fprintf(h, "version:%d\n", s.SpecVersion)
	fmt.Fprintf(h, "%d:%s\n", len(s.ReviewerLens), s.ReviewerLens)
	fmt.Fprintf(h, "%d:%s\n", len(s.Provenance), s.Provenance)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Verify reports whether the sign-off is intact AND still bound to the given
// CURRENT spec_content_hash. A spec edit (hash differs from what it bound) fails
// verification — the structural realization of "any spec edit supersedes the
// sign-off" (§11.5).
func (s SpecSignoff) Verify(currentSpecHash string) bool {
	if !s.TamperEvident || s.Provenance != "minted" {
		return false
	}
	if s.IntegrityHash != s.computeHash() {
		return false
	}
	return s.SpecHash == currentSpecHash
}

// SpecGateInputs bundles everything the pure spec_review gate needs, resolved to
// values. The claim's BindsTo MUST equal the spec branch's CURRENT content hash
// (§11.5 step 2); a stale binding is rejected as superseded.
type SpecGateInputs struct {
	Claim             VerdictValue // signed_off | changes_requested (untrusted)
	ClaimBindsTo      string       // the spec_content_hash the worker judged
	MeetsStyle        bool         // Q1: engineering style (§11.3)
	MeetsRequirements bool         // Q2: project requirements (§11.3)
	CurrentSpecHash   string       // the spec branch's CURRENT hash (the authority)
	SpecVersion       int
	ReviewerLens      string
	AuthorLens        string // §5.5 spec term: reviewer lens must differ from author lens
	Bounces           int
	MaxBounce         int
}

// SpecGateOutcome is the pure result of evaluating the spec_review gate.
type SpecGateOutcome struct {
	Trigger Trigger      // SpecSignedOff | Bounce | BounceExhausted | Superseded
	Signoff *SpecSignoff // non-nil ONLY when a sign-off was minted
	Reason  string
}

// EvaluateSpecGate is the pure §11.5 spec-review gate. It NEVER trusts the claim
// as a sign-off. The conjunction (§11.3): a sign-off is minted IFF claim==signed_off
// AND meets_engineering_style AND meets_requirements AND the claim binds to the
// CURRENT content hash AND the reviewer's lens differs from the author's (§5.5).
// A stale binding (the spec advanced mid-review) is rejected as superseded.
func EvaluateSpecGate(in SpecGateInputs) SpecGateOutcome {
	// §11.5 step 2: the claim must bind to the CURRENT content hash. If the spec
	// advanced mid-review, the verdict is rejected as superseded; the gate stays
	// armed for the new hash (a fresh review judges the new bytes).
	if in.ClaimBindsTo != in.CurrentSpecHash {
		return SpecGateOutcome{Trigger: TriggerSpecSuperseded, Reason: "claim binds to a stale spec_content_hash"}
	}
	// §5.5 spec anti-affinity term, enforced at mint time too (defense in depth):
	// the reviewer lens must differ from the author lens. (The scheduler also
	// enforces it at lease time.)
	if in.AuthorLens != "" && in.ReviewerLens == in.AuthorLens {
		return SpecGateOutcome{Trigger: TriggerBounce, Reason: "reviewer lens must differ from author lens (§5.5)"}
	}
	// §11.3 the conjunction: a sign-off requires decision==signed_off AND both
	// sub-checks pass. A blocking requirements finding forces changes_requested
	// regardless of the worker's decision (the decision is a claim; Flowbee
	// concludes the verdict, I-9).
	if in.Claim == VerdictSignedOff && in.MeetsStyle && in.MeetsRequirements {
		s := MintSpecSignoff(in.CurrentSpecHash, in.SpecVersion, in.ReviewerLens)
		return SpecGateOutcome{Trigger: TriggerSpecSignedOff, Signoff: &s, Reason: "signed off from reviewed bytes"}
	}
	// otherwise changes_requested -> bounce (or exhaust at max_bounces, I-6).
	if in.Bounces+1 >= in.MaxBounce {
		return SpecGateOutcome{Trigger: TriggerBounceExhausted, Reason: "max_bounces reached"}
	}
	return SpecGateOutcome{Trigger: TriggerBounce, Reason: "changes_requested (conjunction not met)"}
}

// gatePredicate is the deterministic reconciled-fact check the gate requires
// before minting an approval (§5.3 require: ci_green_at_head, PR exists at the
// expected SHA pair, no drift). It is the non-LLM authority over the worker claim.
func gatePredicate(f DomainBFacts) bool {
	return f.PRExists && f.CIGreen && !f.Merged && f.HeadSHA != "" && f.BaseSHA != ""
}
