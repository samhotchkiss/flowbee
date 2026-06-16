package job

// F10: CI as a PLUGGABLE fact. The merge gate (§5.3) requires ci_green@head before
// it mints an approval. That fact is NOT tied to one producer: it may come from
//   (a) reconcile-from-GitHub-Actions (the reconciled DomainBFacts.CIGreen), OR
//   (b) a Flowbee `test` job that ran the build's tests itself and reported green.
// Either provenance is authoritative for the gate; this file is the pure fold that
// makes "ci_green@sha" a single boolean the gate consumes regardless of producer.
//
// PURE deterministic-core code: no clock, no I/O. The runtime resolves the raw
// facts (reconciled bool, the recorded test-job fact) and passes them in as values.

// CIProvenance records WHERE a ci_green@sha fact came from (audit + the F10
// "pluggable" guarantee: the gate accepts EITHER producer).
type CIProvenance string

const (
	// CIProvReconciledActions is a ci_green fact reconciled from GitHub Actions
	// (the reconcile-IN sweep's statusCheckRollup == SUCCESS).
	CIProvReconciledActions CIProvenance = "reconciled_actions"
	// CIProvFlowbeeTest is a ci_green fact produced by a Flowbee `test` job that ran
	// the build's tests on a capability-matched worker and reported green.
	CIProvFlowbeeTest CIProvenance = "flowbee_test"
)

// CIFact is a single ci_green@sha observation: it asserts CI was green for a
// specific HEAD sha, carrying its provenance. SHA-binding is what keeps a stale
// green from satisfying a moved head (the same I-5 binding the verdict uses).
type CIFact struct {
	HeadSHA    string       `json:"head_sha"`
	Green      bool         `json:"green"`
	Provenance CIProvenance `json:"provenance"`
}

// CIGreenAtHead is the pure §F10 pluggable-CI fold: ci_green@head holds iff EITHER
// the reconciled Actions fact is green for the head OR any recorded Flowbee test-job
// fact is green AND bound to the SAME head. A test-job fact bound to a DIFFERENT
// head (a stale green from before a push) does not count — the SHA binding is the
// supersession guard (I-5). reconciledGreen is the DomainBFacts.CIGreen the gate
// already consumed; testFacts are the recorded Flowbee-test CI facts for the job.
//
// headSHA is the reconciled head the gate is judging (DomainBFacts.HeadSHA). When
// it is empty (no PR head reconciled yet) a test-job fact cannot bind, so only the
// reconciled bool can satisfy the gate (the safe default).
func CIGreenAtHead(reconciledGreen bool, headSHA string, testFacts []CIFact) bool {
	if reconciledGreen {
		return true
	}
	if headSHA == "" {
		return false
	}
	for _, f := range testFacts {
		if f.Green && f.Provenance == CIProvFlowbeeTest && f.HeadSHA == headSHA {
			return true
		}
	}
	return false
}
