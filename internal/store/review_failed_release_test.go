package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestFailedReleaseBurnsAttemptOnReviewGate: a reviewer that produces no parseable verdict
// releases with Failed=true, which BURNS an attempt even though a code_review release is
// normally penalty-free — so a persistently-broken reviewer escalates after max_attempts
// instead of churning claim↔release forever (the review-path analogue of the build's
// no-output abandon). A plain (penalty-free) gate release must still NOT burn an attempt.
func TestFailedReleaseBurnsAttemptOnReviewGate(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(3000, 0)

	// a plain gate release does NOT burn an attempt (the existing, deliberate behavior).
	driveToCodeReview(t, st, "ok", "h0", "b0")
	okBefore, _ := st.GetJob(ctx, "ok")
	if err := st.Release(ctx, store.ReleaseParams{JobID: "ok", Epoch: okBefore.LeaseEpoch, Now: now}); err != nil {
		t.Fatalf("plain release: %v", err)
	}
	okAfter, _ := st.GetJob(ctx, "ok")
	if okAfter.State != job.StateReviewPending || okAfter.Attempts != okBefore.Attempts {
		t.Fatalf("plain gate release must re-arm review_pending w/o burning an attempt; state=%s attempts %d->%d",
			okAfter.State, okBefore.Attempts, okAfter.Attempts)
	}

	// a FAILED release burns an attempt and re-arms to review_pending.
	driveToCodeReview(t, st, "rj", "h1", "b1")
	before, _ := st.GetJob(ctx, "rj")
	if err := st.Release(ctx, store.ReleaseParams{JobID: "rj", Epoch: before.LeaseEpoch, Failed: true, Now: now}); err != nil {
		t.Fatalf("failed release: %v", err)
	}
	after, _ := st.GetJob(ctx, "rj")
	if after.State != job.StateReviewPending {
		t.Fatalf("failed release state=%s, want review_pending", after.State)
	}
	if after.Attempts != before.Attempts+1 {
		t.Fatalf("failed release attempts=%d, want %d (an attempt must burn)", after.Attempts, before.Attempts+1)
	}
	assertFoldMatchesProjection(t, st, "rj")
}

// TestFailedReleaseEscalatesAtMaxAttempts: repeated Failed review releases (a reviewer that
// never produces a parseable verdict) exhaust the attempt budget and escalate the job to
// needs_human — a human reviews it — rather than churning claim↔release forever. Driven via
// REAL events only (no raw UPDATEs), so the escalated projection still equals Fold.
func TestFailedReleaseEscalatesAtMaxAttempts(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Unix(3000, 0)

	driveToCodeReview(t, st, "ex", "h0", "b0") // starts in code_review (1st reviewer bound)
	j0, _ := st.GetJob(ctx, "ex")
	maxA := j0.MaxAttempts // folded default

	// each cycle: a reviewer holds the gate and fails to produce a verdict -> Failed release
	// (burns an attempt). After maxA failures the job must escalate to needs_human.
	for i := 0; i < maxA+2; i++ {
		cur, _ := st.GetJob(ctx, "ex")
		if cur.State == job.StateNeedsHuman {
			break
		}
		if cur.State == job.StateReviewPending {
			// a fresh reviewer re-claims the re-armed gate (anti-affinity excludes the builder).
			if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
				JobID: "ex", LeaseID: "rl-ex-" + itoa(i), Identity: "reviewer-ex-" + itoa(i),
				ModelFamily: "opus", Attested: []string{"role:code_reviewer", "model_family:opus"},
				TTL: time.Minute, Now: now,
			}); err != nil {
				t.Fatalf("re-claim review (cycle %d): %v", i, err)
			}
			cur, _ = st.GetJob(ctx, "ex")
		}
		if err := st.Release(ctx, store.ReleaseParams{JobID: "ex", Epoch: cur.LeaseEpoch, Failed: true, Now: now}); err != nil {
			t.Fatalf("failed release (cycle %d): %v", i, err)
		}
	}
	got, _ := st.GetJob(ctx, "ex")
	if got.State != job.StateNeedsHuman {
		t.Fatalf("after maxA failed review releases, want needs_human, got %s (attempts=%d/%d)",
			got.State, got.Attempts, maxA)
	}
	assertFoldMatchesProjection(t, st, "ex")
}
