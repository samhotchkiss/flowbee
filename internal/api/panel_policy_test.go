package api

import (
	"testing"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// TestPolicyForRepo: a repo's required_reviewers overrides the global policy for that repo
// only; repos with no override inherit the global; AllowSelfMerge always carries through.
func TestPolicyForRepo(t *testing.T) {
	s := &Server{
		policy:          job.Policy{AllowSelfMerge: true, RequiredReviewers: 1},
		reviewersByRepo: map[string]int{"flowbee": 2},
	}
	if got := s.policyForRepo("flowbee"); got.RequiredReviewers != 2 || !got.AllowSelfMerge {
		t.Errorf("flowbee policy=%+v, want RequiredReviewers=2 + AllowSelfMerge carried", got)
	}
	if got := s.policyForRepo("russ"); got.RequiredReviewers != 1 {
		t.Errorf("russ (no override) policy=%+v, want the global RequiredReviewers=1", got)
	}
	// no overrides configured at all => the global policy everywhere.
	bare := &Server{policy: job.Policy{RequiredReviewers: 3}}
	if got := bare.policyForRepo("anything"); got.RequiredReviewers != 3 {
		t.Errorf("no overrides => global; got %+v", got)
	}
}
