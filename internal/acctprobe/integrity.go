package acctprobe

import "fmt"

// MarkIntegrity applies fleet-wide integrity checks across a set of probe results,
// mutating them in place and returning human-readable warnings. Today it detects
// DUPLICATE IDENTITY: two config dirs that resolve to the SAME account (same provider
// + fingerprint) are BOTH forced to TrustHeld/ReasonDuplicateIdentity and made
// non-routable — dispatching two "different" slots that are really one login would
// double-count its capacity and race its limits. Results with no fingerprint (identity
// unbindable) are skipped, not collided. Ordering of `results` is preserved.
func MarkIntegrity(results []*Result) []string {
	var warnings []string
	first := map[string]*Result{} // provider|fingerprint -> first result seen
	for _, r := range results {
		if r == nil || r.Identity.Fingerprint == "" {
			continue
		}
		key := string(r.Identity.Provider) + "|" + r.Identity.Fingerprint
		prev, ok := first[key]
		if !ok {
			first[key] = r
			continue
		}
		for _, dup := range []*Result{prev, r} {
			dup.TrustState = TrustHeld
			dup.Hold = ReasonDuplicateIdentity
		}
		warnings = append(warnings, fmt.Sprintf(
			"duplicate %s identity: config dirs %q and %q resolve to the same account (%s); both held",
			r.Identity.Provider, prev.Identity.ConfigDir, r.Identity.ConfigDir, r.Identity.Fingerprint))
	}
	return warnings
}
