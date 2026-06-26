package reconcile

import (
	"testing"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// TestToReconciledRequiredChecksGate locks the CI gate's behavior: with the repo's
// REQUIRED checks known, a PR is green iff every required check passed (even when the
// AGGREGATE rollup is UNSTABLE/FAILURE from a NON-required cosmetic check), and failed
// iff a REQUIRED check failed. With required checks unknown it falls back to the prior
// conservative aggregate-only rule, so the change is never looser than before.
func TestToReconciledRequiredChecksGate(t *testing.T) {
	const reqd = "Migration version guard"
	required := []string{reqd}

	cases := []struct {
		name       string
		pr         gh.PullRequest
		required   []string
		wantGreen  bool
		wantFailed bool
	}{
		{
			name:      "required passed, aggregate UNSTABLE from cosmetic failure -> green, not failed",
			pr:        gh.PullRequest{CIRollup: gh.CIFailure, CIHasRealSuccess: true, PassedChecks: []string{reqd, "Backend tests"}, FailingChecks: []string{"Post merged PR summary"}},
			required:  required,
			wantGreen: true, wantFailed: false,
		},
		{
			name:      "required check failing -> not green, failed",
			pr:        gh.PullRequest{CIRollup: gh.CIFailure, CIHasRealSuccess: true, PassedChecks: []string{"Backend tests"}, FailingChecks: []string{reqd}},
			required:  required,
			wantGreen: false, wantFailed: true,
		},
		{
			name:      "required check still pending (absent) -> not green, not failed",
			pr:        gh.PullRequest{CIRollup: gh.CIPending, CIHasRealSuccess: true, PassedChecks: []string{"Backend tests"}},
			required:  required,
			wantGreen: false, wantFailed: false,
		},
		{
			name:      "required passed but NO real success (all-skipped) -> not green",
			pr:        gh.PullRequest{CIRollup: gh.CISuccess, CIHasRealSuccess: false, PassedChecks: []string{reqd}},
			required:  required,
			wantGreen: false, wantFailed: false,
		},
		{
			name:      "required unknown + aggregate SUCCESS -> green (unchanged fallback)",
			pr:        gh.PullRequest{CIRollup: gh.CISuccess, CIHasRealSuccess: true, PassedChecks: []string{"whatever"}},
			required:  nil,
			wantGreen: true, wantFailed: false,
		},
		{
			name:      "required unknown + aggregate FAILURE -> failed (unchanged fallback)",
			pr:        gh.PullRequest{CIRollup: gh.CIFailure, CIHasRealSuccess: true},
			required:  nil,
			wantGreen: false, wantFailed: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := toReconciled(c.pr, false, c.required)
			if got.CIGreen != c.wantGreen {
				t.Errorf("CIGreen=%v, want %v", got.CIGreen, c.wantGreen)
			}
			if got.CIFailed != c.wantFailed {
				t.Errorf("CIFailed=%v, want %v", got.CIFailed, c.wantFailed)
			}
		})
	}
}
