package epicexec_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicexec"
	flowgithub "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type allowMerge struct{}

func (allowMerge) AuthorizeEpicMerge(context.Context, store.EpicDomainAction) error { return nil }

type denyMerge struct{}

func (denyMerge) AuthorizeEpicMerge(context.Context, store.EpicDomainAction) error {
	return &epicexec.MergeAuthorizationDenied{Reason: "epic scope violation"}
}

type staleMerge struct{}

func (staleMerge) AuthorizeEpicMerge(context.Context, store.EpicDomainAction) error {
	return flowgithub.ErrMergeHeadModified
}

func seedApprovedEpic(t *testing.T, id string, now time.Time) (*store.Store, *flowgithub.Fake) {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	st.EnableDriverControlOrigin = true // future-capability fake route
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: id, ProjectID: "default", Repo: "acme/repo",
		Branch: "epic/" + id, FilePath: "epics/" + id + ".md"}, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: store.BuilderDriverIdentity(id), Role: store.DriverBuilderRole,
		HostID: "host-1", StoreID: "driver-store", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1",
		LifecycleOwnership: "driver_managed",
		LifecycleKey:       "builder-" + id, TargetEpoch: 1, ProfileID: "codex",
		WorkspaceRootID: "workspace-1", WorkspaceRelativePath: id,
		SessionID: "session-" + id, PaneInstanceID: "pane-" + id, AgentRunID: "run-" + id,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
		HostID: "control-host", StoreID: "control-store", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "control-server",
		LifecycleOwnership: "driver_managed",
		LifecycleKey:       "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee",
		WorkspaceRootID: "control-workspace", WorkspaceRelativePath: ".",
		SessionID: "control-session", PaneInstanceID: "control-pane", AgentRunID: "control-run",
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: id, Repo: "acme/repo",
		Branch: "epic/" + id, PRNumber: 42, PROpen: true, HeadSHA: "h1", BaseSHA: "b1",
		CIState: "green", CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
		RequiredChecks: []string{"test"}, SourceWatermark: 1}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='merge_queued',state_version=2,
		ci_state='green',head_sha='h1',base_sha='b1',verdict='approved',verdict_head_sha='h1',
		verdict_base_sha='b1',builder_affinity_state='parked',state_entered_at=?,state_due_at=?,fact_progress_at=? WHERE epic_id=?`,
		now.Format(time.RFC3339Nano), now.Add(10*time.Minute).Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano), id); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.ReconcileEpicEffectActions(ctx, now.Add(2*time.Minute), 2); err != nil || rep.Ensured != 1 {
		t.Fatalf("ensure merge action: rep=%+v err=%v", rep, err)
	}
	gh := flowgithub.NewFake()
	gh.SetPR(flowgithub.PullRequest{Number: 42, HeadRefName: "epic/" + id,
		HeadRefOid: "h1", BaseRefOid: "b1", CIRollup: flowgithub.CISuccess})
	return st, gh
}

func TestExactHeadMergeToCleanupConverges(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-happy", now)
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: allowMerge{}, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("merge execute ok=%v err=%v", ok, err)
	}
	if got := gh.MergeHead(42); got != "h1" {
		t.Fatalf("merge expected head=%q, want h1", got)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE kind='merge_dispatch' AND epic_id='effect-happy'`).Scan(&state); err != nil || state != "verifying" {
		t.Fatalf("merge action state=%q err=%v", state, err)
	}
	gh.SetPR(flowgithub.PullRequest{Number: 42, HeadRefName: "epic/effect-happy", HeadRefOid: "h1",
		BaseRefOid: "b1", Merged: true, MergeCommit: "m1"})
	if ok, err := r.VerifyNext(ctx); err != nil || !ok {
		t.Fatalf("merge verify ok=%v err=%v", ok, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-happy'`).Scan(&state); err != nil || state != "cleanup_pending" {
		t.Fatalf("after merge state=%q err=%v", state, err)
	}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("cleanup execute ok=%v err=%v", ok, err)
	}
	if got := gh.DeletedBranches(); len(got) != 1 || got[0] != "epic/effect-happy" {
		t.Fatalf("deleted branches=%v", got)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-happy'`).Scan(&state); err != nil || state != "complete" {
		t.Fatalf("final delivery state=%q err=%v", state, err)
	}
}

func TestMergeFailsClosedWithoutContentScopeEvidenceAuthorizer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 30, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-no-authorizer", now)
	r := epicexec.Runner{Store: st, GitHub: gh, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("fail-closed execute ok=%v err=%v", ok, err)
	}
	for _, call := range gh.Calls() {
		if call == "EnqueueMergeQueue(42)" {
			t.Fatalf("merge bypassed absent product gates: calls=%v", gh.Calls())
		}
	}
	var state, actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-no-authorizer'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE epic_id='effect-no-authorizer' AND kind='merge_dispatch'`).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	if state != "merge_queued" || actionState != "pending" {
		t.Fatalf("fail-closed state=%q action=%q", state, actionState)
	}
}

func TestPermanentMergeAuthorizationDenialParksVisibleWithoutMutation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 40, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-denied", now)
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: denyMerge{}, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("denied execute ok=%v err=%v", ok, err)
	}
	if got := gh.MergeHead(42); got != "" {
		t.Fatalf("unsafe merge mutated GitHub at head %q", got)
	}
	var state, hold, actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,hold_kind FROM epic_deliveries WHERE epic_id='effect-denied'`).Scan(&state, &hold); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE epic_id='effect-denied' AND kind='merge_dispatch'`).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	if state != "needs_human" || hold != "merge_authorization_denied" || actionState != "acknowledged" {
		t.Fatalf("state=%q hold=%q action=%q", state, hold, actionState)
	}
}

func TestAuthorizerHeadMoveSupersedesWithoutMutation(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 50, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-auth-stale", now)
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: staleMerge{}, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("stale execute ok=%v err=%v", ok, err)
	}
	if got := gh.MergeHead(42); got != "" {
		t.Fatalf("stale merge mutated GitHub at head %q", got)
	}
	var state, verdict string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,verdict FROM epic_deliveries WHERE epic_id='effect-auth-stale'`).Scan(&state, &verdict); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_ci" || verdict != "" {
		t.Fatalf("state=%q verdict=%q, want awaiting_ci with stale verdict cleared", state, verdict)
	}
}

func TestCrashAfterMergeEffectBeforeFactRecoversWithoutResend(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-crash", now)
	action, ok, err := st.ClaimNextEpicDomainAction(ctx, "dead-executor", now.Add(3*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := gh.EnqueueMergeQueue(ctx, action.PRNumber, action.HeadSHA); err != nil {
		t.Fatal(err)
	}
	// Process dies here: no action receipt/projection update.
	if n, err := st.ReclaimExpiredEpicDomainActions(ctx, now.Add(5*time.Minute)); err != nil || n != 1 {
		t.Fatalf("reclaim n=%d err=%v", n, err)
	}
	gh.SetPR(flowgithub.PullRequest{Number: 42, HeadRefName: "epic/effect-crash", HeadRefOid: "h1",
		BaseRefOid: "b1", Merged: true, MergeCommit: "merge-crash"})
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: allowMerge{}, Owner: "replacement", Now: func() time.Time { return now.Add(6 * time.Minute) }}
	if ok, err := r.VerifyNext(ctx); err != nil || !ok {
		t.Fatalf("verify ok=%v err=%v", ok, err)
	}
	mergeCalls := 0
	for _, call := range gh.Calls() {
		if call == "EnqueueMergeQueue(42)" {
			mergeCalls++
		}
	}
	if mergeCalls != 1 {
		t.Fatalf("uncertain merge was resent: calls=%v", gh.Calls())
	}
	var cleanup int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='effect-crash' AND kind='cleanup' AND state='pending'`).Scan(&cleanup); err != nil || cleanup != 1 {
		t.Fatalf("cleanup actions=%d err=%v", cleanup, err)
	}
}

func TestCrashAfterCleanupDeleteVerifiesAbsenceWithoutResend(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 13, 30, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "cleanup-crash", now)
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "cleanup-crash", Repo: "acme/repo",
		Branch: "epic/cleanup-crash", PRNumber: 42, Merged: true, HeadSHA: "h1", BaseSHA: "b1",
		MergeCommitSHA: "m-cleanup", SourceWatermark: 2}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	action, ok, err := st.ClaimNextEpicDomainAction(ctx, "dead-cleaner", now.Add(4*time.Minute), time.Minute)
	if err != nil || !ok || action.Kind != "cleanup" {
		t.Fatalf("cleanup claim=%+v ok=%v err=%v", action, ok, err)
	}
	if err := gh.DeleteBranch(ctx, action.Branch); err != nil {
		t.Fatal(err)
	}
	// Crash before acknowledging the exact target absence.
	if n, err := st.ReclaimExpiredEpicDomainActions(ctx, now.Add(6*time.Minute)); err != nil || n != 1 {
		t.Fatalf("reclaim cleanup n=%d err=%v", n, err)
	}
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: allowMerge{}, Owner: "replacement", Now: func() time.Time { return now.Add(7 * time.Minute) }}
	if ok, err := r.VerifyNext(ctx); err != nil || !ok {
		t.Fatalf("verify cleanup ok=%v err=%v", ok, err)
	}
	deleteCalls := 0
	for _, call := range gh.Calls() {
		if call == "DeleteBranch(epic/cleanup-crash)" {
			deleteCalls++
		}
	}
	if deleteCalls != 1 {
		t.Fatalf("uncertain cleanup was resent: calls=%v", gh.Calls())
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='cleanup-crash'`).Scan(&state); err != nil || state != "complete" {
		t.Fatalf("cleanup crash final state=%q err=%v", state, err)
	}
}

func TestHeadAdvanceCancelsOldMergeAndH1RevisitCreatesOneLiveAction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	st, _ := seedApprovedEpic(t, "effect-revisit", now)
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "effect-revisit", Repo: "acme/repo",
		Branch: "epic/effect-revisit", PRNumber: 42, PROpen: true, HeadSHA: "h2", BaseSHA: "b1",
		CIState: "pending", SourceWatermark: 2}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var deliveryState, verdict string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,verdict FROM epic_deliveries WHERE epic_id='effect-revisit'`).Scan(&deliveryState, &verdict); err != nil || deliveryState != "awaiting_ci" || verdict != "" {
		t.Fatalf("superseded delivery state=%q verdict=%q err=%v", deliveryState, verdict, err)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "effect-revisit", Repo: "acme/repo",
		Branch: "epic/effect-revisit", PRNumber: 42, PROpen: true, HeadSHA: "h1", BaseSHA: "b1",
		CIState: "green", CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
		SourceWatermark: 3}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='merge_queued',verdict='approved',
		verdict_head_sha='h1',verdict_base_sha='b1' WHERE epic_id='effect-revisit'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileEpicEffectActions(ctx, now.Add(5*time.Minute), 2); err != nil {
		t.Fatal(err)
	}
	var total, live int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*),SUM(CASE WHEN state<>'cancelled_superseded' THEN 1 ELSE 0 END)
		FROM epic_actions WHERE epic_id='effect-revisit' AND kind='merge_dispatch'`).Scan(&total, &live); err != nil {
		t.Fatal(err)
	}
	if total != 2 || live != 1 {
		t.Fatalf("H1 revisit actions total=%d live=%d, want retained cancelled + one live", total, live)
	}
}

func TestConflictResolutionNewHeadRequiresFreshCIAndReview(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "effect-conflict", now)
	gh.SetMergeConflict(42)
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: allowMerge{}, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("conflict execute ok=%v err=%v", ok, err)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-conflict'`).Scan(&state); err != nil || state != "conflict_resolution" {
		t.Fatalf("conflict state=%q err=%v", state, err)
	}
	var lifecycle store.BuilderLifecycleActionProjection
	if err := st.DB.QueryRowContext(ctx, `SELECT id,action_epoch,project_id,epic_id,kind,dedup_key,
		payload_json,payload_sha256,head_sha,base_sha,target_host_id,target_store_id,target_server_domain_id,target_server_id,
		lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch
		FROM epic_actions WHERE epic_id='effect-conflict' AND kind='conflict_resolution'`).Scan(
		&lifecycle.ActionID, &lifecycle.Epoch, &lifecycle.ProjectID, &lifecycle.EpicID,
		&lifecycle.Kind, &lifecycle.DedupKey, &lifecycle.Payload, &lifecycle.PayloadSHA256,
		&lifecycle.HeadSHA, &lifecycle.BaseSHA, &lifecycle.TargetHostID, &lifecycle.TargetStoreID,
		&lifecycle.TargetServerDomainID, &lifecycle.TargetServerID, &lifecycle.LifecycleKey, &lifecycle.TargetEpoch,
		&lifecycle.ProfileID, &lifecycle.WorkspaceRootID, &lifecycle.WorkspaceRelativePath,
		&lifecycle.LeaseID, &lifecycle.LeaseEpoch); err != nil {
		t.Fatal(err)
	}
	receipt := store.BuilderLifecycleReceiptProjection{ActionID: lifecycle.ActionID,
		ActionEpoch: lifecycle.Epoch, Operation: "ensure", LifecycleKey: lifecycle.LifecycleKey,
		TargetEpoch: lifecycle.TargetEpoch, Status: "ensured", IdentityAfter: store.BuilderLifecycleIdentity{
			HostID: lifecycle.TargetHostID, StoreID: lifecycle.TargetStoreID,
			TmuxServerDomainID: "flowbee", TmuxServerInstanceID: lifecycle.TargetServerID,
			LifecycleOwnership: "driver_managed", LifecycleKey: lifecycle.LifecycleKey,
			TargetEpoch: lifecycle.TargetEpoch, SessionID: "resolver-session",
			PaneInstanceID: "resolver-pane", AgentRunID: "resolver-run"}}
	if err := st.ProjectBuilderLifecycleResult(ctx, lifecycle, receipt, now.Add(350*time.Second)); err != nil {
		t.Fatal(err)
	}
	var affinity string
	var wakes int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,builder_affinity_state FROM epic_deliveries
		WHERE epic_id='effect-conflict'`).Scan(&state, &affinity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='effect-conflict'
		AND kind='builder_rework_wake' AND state='pending'`).Scan(&wakes); err != nil {
		t.Fatal(err)
	}
	if state != "conflict_resolution" || affinity != "active" || wakes != 1 {
		t.Fatalf("resolver projection state=%q affinity=%q wakes=%d", state, affinity, wakes)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "effect-conflict", Repo: "acme/repo",
		Branch: "epic/effect-conflict", PRNumber: 42, PROpen: true, HeadSHA: "resolved-h2", BaseSHA: "b1",
		CIState: "pending", SourceWatermark: 2}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-conflict'`).Scan(&state); err != nil || state != "awaiting_ci" {
		t.Fatalf("resolved head pre-CI state=%q err=%v", state, err)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "effect-conflict", Repo: "acme/repo",
		Branch: "epic/effect-conflict", PRNumber: 42, PROpen: true, HeadSHA: "resolved-h2", BaseSHA: "b1",
		CIState: "green", CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
		SourceWatermark: 3}, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var verdict, reviewedHead string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,verdict,verdict_head_sha FROM epic_deliveries WHERE epic_id='effect-conflict'`).Scan(&state, &verdict, &reviewedHead); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_review_dispatch" || verdict != "" || reviewedHead != "" {
		t.Fatalf("fresh-review gate state=%q verdict=%q reviewed_head=%q", state, verdict, reviewedHead)
	}
}

func TestMergedFactAlwaysCreatesCleanupActionAtomically(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	st, _ := seedApprovedEpic(t, "effect-merged", now)
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "effect-merged", Repo: "acme/repo",
		Branch: "epic/effect-merged", PRNumber: 42, Merged: true, HeadSHA: "h1", BaseSHA: "b1",
		MergeCommitSHA: "m-atomic", SourceWatermark: 2}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var state string
	var actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='effect-merged'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='effect-merged' AND kind='cleanup' AND state='pending'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if state != "cleanup_pending" || actions != 1 {
		t.Fatalf("merged fold state=%q cleanup_actions=%d", state, actions)
	}
}

func TestMissingMergeActionRecovered(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 15, 0, 0, time.UTC)
	st, _ := seedApprovedEpic(t, "missing-merge-action", now)
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM epic_actions WHERE epic_id='missing-merge-action' AND kind='merge_dispatch'`); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileEpicEffectActions(ctx, now.Add(3*time.Minute), 2)
	if err != nil || rep.Ensured != 1 {
		t.Fatalf("missing merge recovery rep=%+v err=%v", rep, err)
	}
	var actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='missing-merge-action'
		AND kind='merge_dispatch' AND state='pending'`).Scan(&actions); err != nil || actions != 1 {
		t.Fatalf("recovered merge actions=%d err=%v", actions, err)
	}
}

func TestMissingCleanupActionRecovered(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 30, 0, 0, time.UTC)
	st, _ := seedApprovedEpic(t, "missing-cleanup-action", now)
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_artifacts SET merged=1,merge_commit_sha='m-recover'
		WHERE epic_id='missing-cleanup-action'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='merged',state_version=3
		WHERE epic_id='missing-cleanup-action'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM epic_actions WHERE epic_id='missing-cleanup-action'`); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileEpicEffectActions(ctx, now.Add(3*time.Minute), 2)
	if err != nil || rep.Ensured != 1 {
		t.Fatalf("missing cleanup recovery rep=%+v err=%v", rep, err)
	}
	var state string
	var actions int
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_deliveries WHERE epic_id='missing-cleanup-action'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='missing-cleanup-action'
		AND kind='cleanup' AND state='pending'`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if state != "cleanup_pending" || actions != 1 {
		t.Fatalf("cleanup recovery state=%q actions=%d", state, actions)
	}
}

func TestConflictResolutionWithoutHeadAlerts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 45, 0, 0, time.UTC)
	st, gh := seedApprovedEpic(t, "conflict-stall", now)
	gh.SetMergeConflict(42)
	r := epicexec.Runner{Store: st, GitHub: gh, Authorizer: allowMerge{}, Owner: "effect-1", Now: func() time.Time { return now.Add(3 * time.Minute) }}
	if ok, err := r.ExecuteNext(ctx); err != nil || !ok {
		t.Fatalf("conflict execute ok=%v err=%v", ok, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state_due_at=? WHERE epic_id='conflict-stall'`,
		now.Add(4*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.ReconcileEpicDeliveryBackstops(ctx, now.Add(time.Hour)); err != nil || rep.Alerted != 1 {
		t.Fatalf("conflict backstop rep=%+v err=%v", rep, err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE epic_id='conflict-stall'
		AND kind='conflict_resolution_stalled'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("conflict alerts=%d err=%v", alerts, err)
	}
}

func TestDeadLetteredMergeAndCleanupRearmSameAction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	st, _ := seedApprovedEpic(t, "effect-rearm", now)
	var id string
	if err := st.DB.QueryRowContext(ctx, `SELECT id FROM epic_actions WHERE epic_id='effect-rearm' AND kind='merge_dispatch'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_actions SET state='dead_letter',last_error='transient 5xx' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileEpicEffectActions(ctx, now.Add(3*time.Minute), 1)
	if err != nil || rep.Rearmed != 1 {
		t.Fatalf("rearm rep=%+v err=%v", rep, err)
	}
	var gotID, actionState string
	var recovery int
	if err := st.DB.QueryRowContext(ctx, `SELECT id,state,recovery_count FROM epic_actions WHERE epic_id='effect-rearm' AND kind='merge_dispatch'`).Scan(&gotID, &actionState, &recovery); err != nil {
		t.Fatal(err)
	}
	if gotID != id || actionState != "pending" || recovery != 1 {
		t.Fatalf("rearmed id=%q state=%q recovery=%d", gotID, actionState, recovery)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_actions SET state='dead_letter' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if rep, err = st.ReconcileEpicEffectActions(ctx, now.Add(4*time.Minute), 1); err != nil || rep.Rearmed != 0 {
		t.Fatalf("budget cap rep=%+v err=%v", rep, err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE epic_id='effect-rearm' AND kind='action_dead_letter'`).Scan(&alerts); err != nil || alerts != 2 {
		t.Fatalf("dead-letter alerts=%d err=%v", alerts, err)
	}
	granted, err := st.GrantEpicActionRecoveryBudget(ctx, "effect-rearm", "h1", "merge_dispatch_stalled", now.Add(5*time.Minute))
	if err != nil || !granted {
		t.Fatalf("human recovery grant=%v err=%v", granted, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state,recovery_count FROM epic_actions WHERE id=?`, id).Scan(&actionState, &recovery); err != nil || actionState != "pending" || recovery != 0 {
		t.Fatalf("human-cleared action state=%q recovery=%d err=%v", actionState, recovery, err)
	}

	cleanupStore, _ := seedApprovedEpic(t, "cleanup-rearm", now)
	if err := cleanupStore.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{EpicID: "cleanup-rearm", Repo: "acme/repo",
		Branch: "epic/cleanup-rearm", PRNumber: 42, Merged: true, HeadSHA: "h1", BaseSHA: "b1",
		MergeCommitSHA: "cleanup-commit", SourceWatermark: 2}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var cleanupID string
	if err := cleanupStore.DB.QueryRowContext(ctx, `SELECT id FROM epic_actions WHERE epic_id='cleanup-rearm' AND kind='cleanup'`).Scan(&cleanupID); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupStore.DB.ExecContext(ctx, `UPDATE epic_actions SET state='dead_letter',last_error='transient delete failure' WHERE id=?`, cleanupID); err != nil {
		t.Fatal(err)
	}
	if rep, err := cleanupStore.ReconcileEpicEffectActions(ctx, now.Add(4*time.Minute), 1); err != nil || rep.Rearmed != 1 {
		t.Fatalf("cleanup rearm rep=%+v err=%v", rep, err)
	}
	var cleanupGotID, cleanupState string
	if err := cleanupStore.DB.QueryRowContext(ctx, `SELECT id,state FROM epic_actions WHERE epic_id='cleanup-rearm' AND kind='cleanup'`).Scan(&cleanupGotID, &cleanupState); err != nil || cleanupGotID != cleanupID || cleanupState != "pending" {
		t.Fatalf("cleanup same-row id=%q want=%q state=%q err=%v", cleanupGotID, cleanupID, cleanupState, err)
	}
	if gotID == "" {
		t.Fatal("missing durable action id")
	}
}
