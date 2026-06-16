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

// gatePredicate is the deterministic reconciled-fact check the gate requires
// before minting an approval (§5.3 require: ci_green_at_head, PR exists at the
// expected SHA pair, no drift). It is the non-LLM authority over the worker claim.
func gatePredicate(f DomainBFacts) bool {
	return f.PRExists && f.CIGreen && !f.Merged && f.HeadSHA != "" && f.BaseSHA != ""
}
