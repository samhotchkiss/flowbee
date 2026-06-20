package store_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestAdoptIssueConcurrentDoubleAdopt: the webhook-driven sweep and the periodic
// floor-poll sweep both call AdoptIssueAsBuild for the same labeled issue. Before the
// fix the dedup was a check-then-insert split across statements with no shared tx and
// no UNIQUE backstop, so concurrent callers both saw COUNT=0 and both seeded — two
// builds, two PRs, one issue. This drives N concurrent adopts of one issue and asserts
// EXACTLY ONE job is created (and exactly one non-empty id is returned).
func TestAdoptIssueConcurrentDoubleAdopt(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // line them up so they actually contend
			ids[i], errs[i] = st.AdoptIssueAsBuild(ctx, "", 42, "title", "body", "base", 0, now)
		}(i)
	}
	close(start)
	wg.Wait()

	nonEmpty := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("adopt %d: %v", i, errs[i])
		}
		if ids[i] != "" {
			nonEmpty++
		}
	}
	if nonEmpty != 1 {
		t.Fatalf("want exactly 1 adopting caller, got %d non-empty ids", nonEmpty)
	}

	var jobs int
	if err := st.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE issue_number = 42 AND COALESCE(repo,'') = ''`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 {
		t.Fatalf("double-adoption: %d jobs created for one issue, want 1", jobs)
	}
}
