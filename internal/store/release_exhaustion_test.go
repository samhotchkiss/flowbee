package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// claimOnce claims the ready build and returns its live lease epoch.
func claimOnce(t *testing.T, st *store.Store, id, leaseID string, now time.Time) int {
	t.Helper()
	if _, err := st.ClaimReadyJob(context.Background(), store.ClaimParams{
		JobID: id, LeaseID: leaseID, Identity: "w", ModelFamily: "codex",
		Role: job.RoleEngWorker, Attested: []string{"role:eng_worker"},
		TTL: time.Minute, Now: now,
	}); err != nil {
		t.Fatalf("claim %s: %v", leaseID, err)
	}
	j, _ := st.GetJob(context.Background(), id)
	return j.LeaseEpoch
}

// claimedBuildAt seeds a ready build and drives `attempts` REAL penalty claim→release
// cycles (each burns one attempt via a genuine ledger event, so the fold stays
// consistent — unlike a raw UPDATE), then leaves it claimed. Returns the live lease
// epoch for the caller's final Release. Requires attempts < max_attempts (5) so every
// warm-up cycle re-arms to ready.
func claimedBuildAt(t *testing.T, st *store.Store, id string, attempts int, now time.Time) int {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < attempts; i++ {
		ep := claimOnce(t, st, id, "warm-"+id+string(rune('a'+i)), now)
		if err := st.Release(ctx, store.ReleaseParams{JobID: id, Epoch: ep, Now: now}); err != nil {
			t.Fatalf("warm-up release %d: %v", i, err)
		}
	}
	return claimOnce(t, st, id, "final-"+id, now)
}

func assertFoldMatchesProjection(t *testing.T, st *store.Store, id string) {
	t.Helper()
	proj, _ := st.GetJob(context.Background(), id)
	events, _ := st.LoadEvents(context.Background(), id)
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.State != proj.State {
		t.Fatalf("fold state=%s != projection %s (determinism invariant)", folded.State, proj.State)
	}
	if folded.Attempts != proj.Attempts {
		t.Fatalf("fold attempts=%d != projection %d", folded.Attempts, proj.Attempts)
	}
	// escalation_reason and over_budget are projection fields that several escalation
	// paths set via a direct UPDATE; the fold must reproduce them or a rebuild-from-ledger
	// silently corrupts the §12.6.1 triage signal (and strands over_budget=true forever).
	if folded.EscalationReason != proj.EscalationReason {
		t.Fatalf("fold escalation_reason=%q != projection %q (rebuild-from-ledger would lose it)", folded.EscalationReason, proj.EscalationReason)
	}
	if folded.OverBudget != proj.OverBudget {
		t.Fatalf("fold over_budget=%v != projection %v", folded.OverBudget, proj.OverBudget)
	}
	if folded.LastReviewNotes != proj.LastReviewNotes {
		t.Fatalf("fold last_review_notes=%q != projection %q (review-findings carry-forward must fold)", folded.LastReviewNotes, proj.LastReviewNotes)
	}
	if folded.LeaseEpoch != proj.LeaseEpoch {
		t.Fatalf("fold lease_epoch=%d != projection %d (epoch bumps must fold)", folded.LeaseEpoch, proj.LeaseEpoch)
	}
	// head_sha is read by reconcile's flowbeePlaced guard (an external-push-vs-our-own
	// classification that gates supersession). Head-establishing re-arms (rebased,
	// conflict_resolved, the panel accumulate) set it via a direct UPDATE, so the fold must
	// carry the head on those events or a rebuild-from-ledger would blank/stale it and
	// reconcile would misclassify the next sweep.
	if folded.HeadSHA != proj.HeadSHA {
		t.Fatalf("fold head_sha=%q != projection %q (head-establishing re-arms must fold the head)", folded.HeadSHA, proj.HeadSHA)
	}
	// base_sha is the cut point a re-armed build builds against; several re-arm paths
	// (supersede, rebase, resolve) set it via a direct UPDATE, and an empty incoming base
	// must be COALESCEd to the prior — the fold guards must mirror that or a rebuild-from-
	// ledger strands the build on a wrong/empty base.
	if folded.BaseSHA != proj.BaseSHA {
		t.Fatalf("fold base_sha=%q != projection %q (re-arm paths must fold the base)", folded.BaseSHA, proj.BaseSHA)
	}
	// role + required_capabilities decide WHO can lease the job. Several re-arm-to-ready
	// paths (operator requeue, stall revoke, fast-cancel) set them via a direct UPDATE, NOT
	// a folded field, so a rebuild-from-ledger could keep STALE review caps on a re-armed
	// build (role:code_reviewer) — unleaseable by every builder, and the resync + normalize
	// watchdogs then churn it forever. The role must fold exactly; for caps we assert the
	// FUNCTIONAL invariant — an eng_worker can lease the build iff the projection says so —
	// which catches the stale-review-cap strand while tolerating the benign empty-vs-
	// [eng_worker] difference a fresh adopt (empty) vs the normalize watchdog ([eng_worker])
	// produces, both of which a builder can claim.
	if folded.Role != proj.Role {
		t.Fatalf("fold role=%q != projection %q (re-arm paths must fold the role)", folded.Role, proj.Role)
	}
	// caps: assert the FUNCTIONAL invariant for the job's ACTUAL role (not just eng_worker) —
	// an agent holding the projection's role can lease the folded job iff it can lease the
	// projection. This generalization catches a spec_review job that folds stale code_reviewer
	// caps (the ClaimSpecReview class) or a backlog/spec job that folds empty caps, while
	// tolerating the benign empty-vs-[role:X] difference (both leaseable by an X-agent).
	if proj.Role != "" {
		roleCap := []string{"role:" + string(proj.Role)}
		if job.CapabilitiesSatisfy(roleCap, folded.RequiredCapabilities) != job.CapabilitiesSatisfy(roleCap, proj.RequiredCapabilities) {
			t.Fatalf("fold caps=%v vs projection %v diverge on %q leaseability (stale/empty caps strand the correct agent)",
				folded.RequiredCapabilities, proj.RequiredCapabilities, proj.Role)
		}
	}
	// kind/priority are set at creation/backlog and drive routing + scheduling order; a
	// rebuild-from-ledger that dropped them (the KindBacklogged class) yields kind="" — an
	// un-promotable, unschedulable stub.
	if folded.Kind != proj.Kind {
		t.Fatalf("fold kind=%q != projection %q (creation/backlog must fold the kind)", folded.Kind, proj.Kind)
	}
	if folded.Priority != proj.Priority {
		t.Fatalf("fold priority=%d != projection %d", folded.Priority, proj.Priority)
	}
	// epic_reviewed is the drain gate for fanning out an epic's children; spec_text is the
	// content the I-9 sign-off binds to + the build consumes. Both are set via direct UPDATEs
	// on event paths (KindEpicReviewed, KindSpecAuthored/amend) the fold must reproduce.
	if folded.EpicReviewed != proj.EpicReviewed {
		t.Fatalf("fold epic_reviewed=%v != projection %v (a re-locked barrier strands every child)", folded.EpicReviewed, proj.EpicReviewed)
	}
	if folded.SpecText != proj.SpecText {
		t.Fatalf("fold spec_text=%q != projection %q (spec authoring/amend must fold the materialized text)", folded.SpecText, proj.SpecText)
	}
	// verdict: at minimum its presence must agree — the requeue re-arm clears it (verdict=NULL)
	// and the gate mints it; a fold that kept a stale verdict on a re-armed job (or dropped a
	// minted one) diverges on the I-9 sign-off the merge gate reads.
	if (folded.Verdict == nil) != (proj.Verdict == nil) {
		t.Fatalf("fold verdict-present=%v != projection %v (requeue clears / mint sets the verdict)", folded.Verdict != nil, proj.Verdict != nil)
	}
}

// TestReleaseEscalatesOnAttemptsExhaustion: a penalty release that burns the LAST
// attempt escalates the build to needs_human instead of re-arming `ready` — the
// backstop against an always-no-output agent churning forever (the claim path has no
// attempts guard and the churn keeps updated_at fresh, hiding the stall from the
// watchdog). max_attempts defaults to 5.
func TestReleaseEscalatesOnAttemptsExhaustion(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	// attempts=4, max=5: the release burns the 5th (final) attempt.
	epoch := claimedBuildAt(t, st, "exh", 4, now)
	if err := st.Release(ctx, store.ReleaseParams{JobID: "exh", Epoch: epoch, Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j, _ := st.GetJob(ctx, "exh")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("exhausted release state=%s, want needs_human (must not re-arm a doomed build)", j.State)
	}
	if j.Attempts != 5 {
		t.Fatalf("attempts=%d, want 5 (the burned final attempt)", j.Attempts)
	}
	if j.EscalationReason != string(job.EscalationAttempts) {
		t.Fatalf("escalation_reason=%q, want %q", j.EscalationReason, job.EscalationAttempts)
	}
	assertFoldMatchesProjection(t, st, "exh")
}

// TestReleaseReArmsWhenAttemptsRemain: a penalty release with attempts left re-arms
// to `ready` so the build can legitimately retry.
func TestReleaseReArmsWhenAttemptsRemain(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	epoch := claimedBuildAt(t, st, "retry", 1, now) // attempts 1 -> 2 of 5
	if err := st.Release(ctx, store.ReleaseParams{JobID: "retry", Epoch: epoch, Now: now.Add(time.Second)}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j, _ := st.GetJob(ctx, "retry")
	if j.State != job.StateReady {
		t.Fatalf("state=%s, want ready (attempts remain)", j.State)
	}
	if j.Attempts != 2 {
		t.Fatalf("attempts=%d, want 2", j.Attempts)
	}
	assertFoldMatchesProjection(t, st, "retry")
}

// TestReleaseNoPenaltyNeverEscalates: a NON-failure abandon (fast-forward race) keeps
// the attempt budget — it must never escalate even at the ceiling, or re-validation
// churn would escalate a good change.
func TestReleaseNoPenaltyNeverEscalates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	epoch := claimedBuildAt(t, st, "keep", 4, now) // at the ceiling, but...
	if err := st.Release(ctx, store.ReleaseParams{JobID: "keep", Epoch: epoch, Now: now.Add(time.Second), NoPenalty: true}); err != nil {
		t.Fatalf("release: %v", err)
	}
	j, _ := st.GetJob(ctx, "keep")
	if j.State != job.StateReady {
		t.Fatalf("no-penalty release state=%s, want ready (must not escalate a non-failure)", j.State)
	}
	if j.Attempts != 4 {
		t.Fatalf("attempts=%d, want 4 (no-penalty burns nothing)", j.Attempts)
	}
	assertFoldMatchesProjection(t, st, "keep")
}
