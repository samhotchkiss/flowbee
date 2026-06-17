package engine

import (
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestRedispatchTargetResolvingConflict: a revoked conflict-resolution lease (a worker
// that died / lost the CP mid-resolve) must re-arm BACK to resolving_conflict so another
// conflict_resolver can retry — NOT to `ready`, which (carrying role:conflict_resolver
// caps) no eng_worker can claim and no resolver looks at: a hard wedge.
func TestRedispatchTargetResolvingConflict(t *testing.T) {
	if got := redispatchTarget(job.StateResolvingConflict); got != job.StateResolvingConflict {
		t.Fatalf("revoked resolving_conflict re-arms to %s, want resolving_conflict", got)
	}
	// the existing arms still hold.
	for from, want := range map[job.State]job.State{
		job.StateBuilding:      job.StateReady,
		job.StateLeased:        job.StateReady,
		job.StateCodeReview:    job.StateReviewPending,
		job.StateSpecReview:    job.StateSpecAuthoring,
		job.StateSpecAuthoring: job.StateSpecAuthoring,
	} {
		if got := redispatchTarget(from); got != want {
			t.Errorf("redispatchTarget(%s)=%s, want %s", from, got, want)
		}
	}
}
