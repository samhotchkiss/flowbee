package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
)

// TestSpecAuthorClaimGuardsLiveLease pins the multi-worker churn fix: the spec_author
// stage stays spec_authoring while worked, so a SECOND spec_author worker must NOT be
// able to re-claim an in-flight job and fence the first (whose submit then 409s — the
// live "11 claims for one spec" stall). An unexpired lease blocks the steal; an expired
// one (dead worker) is reclaimable so recovery still works.
func TestSpecAuthorClaimGuardsLiveLease(t *testing.T) {
	st := newLiveStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)
	if _, err := st.SeedSpecJob(ctx, SeedSpecParams{ID: "sp", ChatRef: "c", Now: now}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	attest := []string{"role:spec_author", "model_family:opus"}

	// worker A claims with a 1h lease.
	if _, err := st.ClaimSpecAuthor(ctx, ClaimSpecAuthorParams{
		JobID: "sp", LeaseID: "lA", Identity: "A", ModelFamily: "opus", Attested: attest,
		TTL: time.Hour, Now: now,
	}); err != nil {
		t.Fatalf("A claim: %v", err)
	}

	// worker B tries to claim the SAME in-flight job a minute later: must lose the race
	// (A's lease is live) — not steal it and fence A.
	_, err := st.ClaimSpecAuthor(ctx, ClaimSpecAuthorParams{
		JobID: "sp", LeaseID: "lB", Identity: "B", ModelFamily: "opus", Attested: attest,
		TTL: time.Hour, Now: now.Add(time.Minute),
	})
	if !errors.Is(err, lease.ErrLostRace) {
		t.Fatalf("B claim during A's live lease: err=%v, want ErrLostRace (no steal)", err)
	}

	// after A's lease deadline passes (A presumed dead), B may reclaim — recovery path.
	if _, err := st.ClaimSpecAuthor(ctx, ClaimSpecAuthorParams{
		JobID: "sp", LeaseID: "lB2", Identity: "B", ModelFamily: "opus", Attested: attest,
		TTL: time.Hour, Now: now.Add(2 * time.Hour),
	}); err != nil {
		t.Fatalf("B reclaim after A's lease expired: %v", err)
	}
}
