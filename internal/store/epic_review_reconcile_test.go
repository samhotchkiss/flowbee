package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedAwaitingReview(t *testing.T, st *store.Store, epic, head string, greenAt time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: epic, Repo: "repo", Branch: "epic/" + epic}, 1, greenAt); err != nil {
		t.Fatalf("seed epic: %v", err)
	}
	err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{
		EpicID: epic, Repo: "repo", Branch: "epic/" + epic, PRNumber: 4950,
		PROpen: true, HeadSHA: head, BaseSHA: "base", CIState: "green",
		CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
	}, greenAt)
	if err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
}

func bindReviewDriverRoute(t *testing.T, st *store.Store, reviewer string, now time.Time) {
	t.Helper()
	st.EnableDriverControlOrigin = true // future-capability fake route
	ctx := context.Background()
	bindings := []store.DriverSessionBinding{
		{
			WorkerIdentity: reviewer, Role: store.DriverReviewerRole,
			HostID: "host-review", StoreID: "store-review", TmuxServerInstanceID: "server-review",
			LifecycleKey: "reviewer-" + reviewer, TargetEpoch: 1, ProfileID: "code-reviewer",
			WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
			SessionID: "session-" + reviewer, PaneInstanceID: "pane-" + reviewer, AgentRunID: "run-" + reviewer,
		},
	}
	for _, binding := range bindings {
		if _, err := st.UpsertDriverSessionBinding(ctx, binding, now); err != nil {
			t.Fatalf("bind Driver route: %v", err)
		}
	}
}

func TestEpicReviewReconcilerRecoversInterruptedHandoffIdempotently(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	green := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "epic-4950", "head-4950", green)
	now := green.Add(10 * time.Minute)
	first, err := st.ReconcileEpicReviewHandoffs(ctx, now, 5*time.Minute)
	if err != nil || first.Dispatched != 1 {
		t.Fatalf("first pass=%+v err=%v, want one durable dispatch", first, err)
	}
	// Simulate dispatcher death after the durable job commit: rerun must
	// rediscover the same obligation without creating a send action or duplicate alert.
	second, err := st.ReconcileEpicReviewHandoffs(ctx, now.Add(time.Minute), 5*time.Minute)
	if err != nil || second.Dispatched != 0 {
		t.Fatalf("recovery pass=%+v err=%v, want no duplicate repair", second, err)
	}
	var actions, alerts, controlAlerts, jobs int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-4950'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE epic_id='epic-4950' AND kind='review_dispatch_stalled'`).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE epic_id='epic-4950' AND kind='review_dispatch_stalled'`).Scan(&controlAlerts); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE epic_delivery_id='epic-4950' AND workflow_domain='epic_v2'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if actions != 0 || alerts != 1 || controlAlerts != 1 || jobs != 1 {
		t.Fatalf("actions=%d attention=%d control_alerts=%d jobs=%d, want no pre-claim action and one obligation/alert", actions, alerts, controlAlerts, jobs)
	}
}

func TestEpicReviewReconcilerIsFlagGated(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	green := time.Now().UTC().Add(-10 * time.Minute)
	seedAwaitingReview(t, st, "disabled", "head-disabled", green)
	got, err := st.ReconcileEpicReviewHandoffs(ctx, time.Now().UTC(), time.Minute)
	if err != nil || got.Dispatched != 0 {
		t.Fatalf("disabled pass=%+v err=%v", got, err)
	}
}

func TestEpicReviewReconcilerSurfacesNonAdoptedPRJobConflict(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	green := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "epic-conflict", "conflict-head", green)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO jobs
		(id,kind,flow,stage,state,role,repo,pr_number,base_sha,head_sha,
		 blocked_by,required_capabilities,enqueued_at,adopted,opted_in)
		VALUES ('originated-conflict','build','build','review','review_pending','code_reviewer',
		        'repo',4950,'base','conflict-head','[]','[]',?,0,1)`, green.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileEpicReviewHandoffs(ctx, green.Add(10*time.Minute), 5*time.Minute)
	if err != nil || rep.Dispatched != 0 {
		t.Fatalf("conflict reconcile=%+v err=%v", rep, err)
	}
	var kind, reason string
	if err := st.DB.QueryRowContext(ctx, `SELECT hold_kind,hold_reason FROM epic_deliveries
		WHERE epic_id='epic-conflict'`).Scan(&kind, &reason); err != nil {
		t.Fatal(err)
	}
	if kind != "review_job_conflict" || reason == "" {
		t.Fatalf("conflict hold kind=%q reason=%q", kind, reason)
	}
	var native int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs
		WHERE workflow_domain='epic_v2' AND epic_delivery_id='epic-conflict'`).Scan(&native); err != nil {
		t.Fatal(err)
	}
	if native != 0 {
		t.Fatalf("conflict created %d duplicate native reviews", native)
	}
}

func TestEpicReviewHandoffRecoversInFreshProcessAndBecomesClaimable(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/flowbee.db"
	first, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(ctx, first.DB); err != nil {
		t.Fatal(err)
	}
	greenAt := time.Date(2026, 7, 19, 3, 0, 0, 0, time.UTC)
	if err := first.AddEpicRun(ctx, store.EpicRun{ID: "epic-crash", Repo: "repo", Branch: "epic/crash", BuilderModelFamily: "codex"}, 1, greenAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := first.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{
		EpicID: "epic-crash", Repo: "repo", Branch: "epic/crash", PRNumber: 4951,
		PROpen: true, HeadSHA: "crash-head", BaseSHA: "crash-base", CIState: "green",
		CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
	}, greenAt); err != nil {
		t.Fatal(err)
	}
	// Crash boundary: the authoritative green fact and durable review obligation
	// committed, but no review materialization call ran in this process.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := store.MigrateUp(ctx, second.DB); err != nil {
		t.Fatal(err)
	}
	second.EnableEpicReviewHandoffV2 = true
	rep, err := second.ReconcileEpicReviewHandoffs(ctx, greenAt.Add(10*time.Minute), 5*time.Minute)
	if err != nil || rep.Dispatched != 1 {
		t.Fatalf("fresh-process recovery=%+v err=%v", rep, err)
	}
	candidates, err := second.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	bindReviewDriverRoute(t, second, "grok-reviewer", greenAt.Add(10*time.Minute))
	lease, err := second.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: "review-lease", Identity: "grok-reviewer",
		ModelFamily: "grok", Model: "grok", Lens: "correctness",
		Attested: []string{"role:code_reviewer"}, TTL: 5 * time.Minute,
		Now: greenAt.Add(11 * time.Minute),
	})
	if err != nil || lease == nil {
		t.Fatalf("claim recovered review: lease=%+v err=%v", lease, err)
	}
	var state, reviewer string
	if err := second.DB.QueryRowContext(ctx, `SELECT state,reviewer_identity FROM epic_deliveries WHERE epic_id='epic-crash'`).Scan(&state, &reviewer); err != nil {
		t.Fatal(err)
	}
	if state != "in_review" || reviewer != "grok-reviewer" {
		t.Fatalf("delivery state=%s reviewer=%s", state, reviewer)
	}
	resp, err := second.ReviewResult(ctx, store.DBFactSource{DB: second.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: candidates[0].JobID, Epoch: lease.Epoch, Claim: job.VerdictApproved,
		Disposition: job.DispositionHandoff, IdempotencyKey: "review-result-1",
		Now: greenAt.Add(12 * time.Minute),
	})
	if err != nil || !resp.Minted {
		t.Fatalf("record recovered review result: resp=%+v err=%v", resp, err)
	}
	var verdict, verdictHead string
	if err := second.DB.QueryRowContext(ctx, `SELECT state,verdict,verdict_head_sha FROM epic_deliveries WHERE epic_id='epic-crash'`).Scan(&state, &verdict, &verdictHead); err != nil {
		t.Fatal(err)
	}
	if state != "merge_queued" || verdict != "approved" || verdictHead != "crash-head" {
		t.Fatalf("approved delivery state=%s verdict=%s head=%s", state, verdict, verdictHead)
	}
	var mergeActions int
	if err := second.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-crash' AND kind='merge_dispatch'`).Scan(&mergeActions); err != nil {
		t.Fatal(err)
	}
	if mergeActions != 1 {
		t.Fatalf("merge actions=%d, want one durable next effect", mergeActions)
	}
}

func TestEpicReviewRejectionDurablyQueuesBuilderRework(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	greenAt := time.Date(2026, 7, 19, 5, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-reject", Repo: "repo", Branch: "epic/reject", BuilderModelFamily: "codex"}, 1, greenAt.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{
		EpicID: "epic-reject", Repo: "repo", Branch: "epic/reject", PRNumber: 4952,
		PROpen: true, HeadSHA: "reject-head", BaseSHA: "reject-base", CIState: "green",
		CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
	}, greenAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileEpicReviewHandoffs(ctx, greenAt.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET builder_affinity_state='parked'
		WHERE epic_id='epic-reject'`); err != nil {
		t.Fatal(err)
	}
	bindBuilderDriver(t, st, "epic-reject", greenAt.Add(10*time.Minute))
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	bindReviewDriverRoute(t, st, "grok-reviewer", greenAt.Add(10*time.Minute))
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: "reject-review-lease", Identity: "grok-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: time.Minute,
		Now: greenAt.Add(11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := st.ReviewResult(ctx, store.DBFactSource{DB: st.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: candidates[0].JobID, Epoch: ls.Epoch, Claim: job.VerdictChangesRequested,
		Notes: "fix the recovery fence", IdempotencyKey: "reject-result-1",
		Now: greenAt.Add(12 * time.Minute),
	})
	if err != nil || resp.JobState != string(job.StateReady) {
		t.Fatalf("review result=%+v err=%v", resp, err)
	}
	var state, verdict string
	var round, actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,verdict,review_round FROM epic_deliveries WHERE epic_id='epic-reject'`).Scan(&state, &verdict, &round); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-reject' AND kind='builder_rework' AND state='pending'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if state != "changes_requested" || verdict != "changes_requested" || round != 1 || actions != 1 {
		t.Fatalf("delivery state=%s verdict=%s round=%d actions=%d", state, verdict, round, actions)
	}
	var affinity, executor string
	if err := st.DB.QueryRowContext(ctx, `SELECT builder_affinity_state FROM epic_deliveries
		WHERE epic_id='epic-reject'`).Scan(&affinity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT executor_kind FROM epic_actions
		WHERE epic_id='epic-reject' AND kind='builder_rework'`).Scan(&executor); err != nil {
		t.Fatal(err)
	}
	if affinity != "relaunching" || executor != "driver_lifecycle" {
		t.Fatalf("rework affinity/executor=%s/%s", affinity, executor)
	}
}

func TestEpicReviewerReleaseReturnsDeliveryToClaimableQueue(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	greenAt := time.Date(2026, 7, 19, 6, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "epic-release", "release-head", greenAt)
	if _, err := st.ReconcileEpicReviewHandoffs(ctx, greenAt.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	bindReviewDriverRoute(t, st, "reviewer-one", greenAt.Add(10*time.Minute))
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: "release-lease", Identity: "reviewer-one",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: time.Minute,
		Now: greenAt.Add(11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Release(ctx, store.ReleaseParams{JobID: candidates[0].JobID, Epoch: ls.Epoch, NoPenalty: true, Now: greenAt.Add(12 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var state, reviewer string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,reviewer_identity FROM epic_deliveries WHERE epic_id='epic-release'`).Scan(&state, &reviewer); err != nil {
		t.Fatal(err)
	}
	if state != "review_queued" || reviewer != "" {
		t.Fatalf("released delivery state=%s reviewer=%q", state, reviewer)
	}
	candidates, err = st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("released review not claimable: candidates=%+v err=%v", candidates, err)
	}
}

func TestHungRenewingReviewerIsFencedAndRequeued(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	greenAt := time.Date(2026, 7, 19, 7, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "epic-hung-review", "hung-head", greenAt)
	if _, err := st.ReconcileEpicReviewHandoffs(ctx, greenAt.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("candidates=%+v err=%v", candidates, err)
	}
	bindReviewDriverRoute(t, st, "hung-reviewer", greenAt.Add(10*time.Minute))
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: "hung-live-lease", Identity: "hung-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: time.Hour,
		Now: greenAt.Add(11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The lease is still live at +40m, but no reviewer fact or verdict has
	// progressed since +11m. Liveness alone must not hold the pipeline forever.
	rep, err := st.ReconcileEpicReviewVerdictStalls(ctx, greenAt.Add(40*time.Minute), 20*time.Minute, 3)
	if err != nil || rep.Requeued != 1 || rep.Escalated != 0 {
		t.Fatalf("verdict stall reconcile=%+v err=%v", rep, err)
	}
	var state, reviewer string
	var alertPending, attentions int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,reviewer_identity,alert_pending FROM epic_deliveries WHERE epic_id='epic-hung-review'`).Scan(&state, &reviewer, &alertPending); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE epic_id='epic-hung-review' AND kind='review_verdict_overdue'`).Scan(&attentions); err != nil {
		t.Fatal(err)
	}
	if state != "review_queued" || reviewer != "" || alertPending != 1 || attentions != 1 {
		t.Fatalf("delivery state=%s reviewer=%q alert=%d attention=%d", state, reviewer, alertPending, attentions)
	}
	_, err = st.ReviewResult(ctx, store.DBFactSource{DB: st.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: candidates[0].JobID, Epoch: ls.Epoch, Claim: job.VerdictApproved,
		Disposition: job.DispositionHandoff, Now: greenAt.Add(41 * time.Minute),
	})
	if !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("fenced reviewer result err=%v, want stale epoch", err)
	}
	candidates, err = st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("requeued review not claimable: candidates=%+v err=%v", candidates, err)
	}
}
