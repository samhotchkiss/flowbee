package reconcile_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type v2IntakeMirror struct{}

func (v2IntakeMirror) HeadSHA(string) (string, error) { return "main-head", nil }

func TestV2SweepNeverAdoptsOneOffLabeledIssuesOrPRs(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	fake := gh.NewFake()
	fake.SetIssue(gh.Issue{Number: 4950, Title: "one off", Body: "must not adopt", Labels: []string{"flowbee:build"}})
	fake.SetPR(gh.PullRequest{Number: 4951, HeadRefName: "dev/russ", BaseRefOid: "base", HeadRefOid: "head",
		Labels: []string{"needs-claude"}, CIRollup: gh.CISuccess, CIHasRealSuccess: true, UpdatedAt: now})
	fake.SetPRDiff(4951, "diff --git a/a b/a\n")
	rec := reconcile.NewForRepo("russ", st, fake, clock.NewFake(now), nil).WithIntake(v2IntakeMirror{}, "main")
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	var jobs int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE issue_number=4950 OR pr_number=4951`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 0 {
		t.Fatalf("v2 created %d generic issue/PR jobs", jobs)
	}
	for _, call := range fake.Calls() {
		if call == "PullRequest:4951" || call == "PullRequestDiff:4951" {
			t.Fatalf("v2 label path performed targeted adoption call %q", call)
		}
	}
}
