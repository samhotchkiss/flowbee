package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedNativeReviewObligation(t *testing.T, reviewer string) (*store.Store, string, time.Time) {
	t.Helper()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	st.EnableDriverControlOrigin = true // future-capability fake route
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "epic-driver-review", "driver-head", now.Add(-10*time.Minute))
	if got, err := st.ReconcileEpicReviewHandoffs(context.Background(), now, 5*time.Minute); err != nil || got.Dispatched != 1 {
		t.Fatalf("materialize native review: got=%+v err=%v", got, err)
	}
	candidates, err := st.ReviewPendingCandidates(context.Background())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("review candidates=%+v err=%v", candidates, err)
	}
	return st, candidates[0].JobID, now
}

func TestNativeReviewMaterializationCreatesNoPreClaimDriverAction(t *testing.T) {
	st, _, _ := seedNativeReviewObligation(t, "reviewer")
	var jobs, actions int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM jobs WHERE workflow_domain='epic_v2'`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM epic_actions WHERE kind IN ('review_dispatch','review_wake')`).Scan(&actions); err != nil {
		t.Fatal(err)
	}
	if jobs != 1 || actions != 0 {
		t.Fatalf("native jobs=%d pre-claim send actions=%d, want 1/0", jobs, actions)
	}
}

func TestNativeReviewClaimWithoutExactBindingCommitsVisibleHold(t *testing.T) {
	ctx := context.Background()
	st, jobID, now := seedNativeReviewObligation(t, "unbound-reviewer")
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "unbound-lease", Identity: "unbound-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"},
		TTL: 5 * time.Minute, Now: now.Add(time.Minute),
	})
	if ls != nil || !errors.Is(err, store.ErrDriverSessionBindingMissing) {
		t.Fatalf("claim lease=%+v err=%v, want durable binding hold", ls, err)
	}
	var jobState, deliveryState, holdKind, holdReason string
	var actions, attention, alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&jobState); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state,hold_kind,hold_reason FROM epic_deliveries
		WHERE epic_id='epic-driver-review'`).Scan(&deliveryState, &holdKind, &holdReason); err != nil {
		t.Fatal(err)
	}
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-driver-review'`).Scan(&actions)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items WHERE epic_id='epic-driver-review' AND kind='review_claim_stalled' AND state='open'`).Scan(&attention)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE epic_id='epic-driver-review' AND kind='review_claim_stalled'`).Scan(&alerts)
	if jobState != "review_pending" || deliveryState != "review_queued" ||
		holdKind != "review_session_unbound" || holdReason == "" || actions != 0 || attention != 1 || alerts != 1 {
		t.Fatalf("job=%s delivery=%s hold=%s/%q actions=%d attention=%d alerts=%d",
			jobState, deliveryState, holdKind, holdReason, actions, attention, alerts)
	}
	if candidates, err := st.ReviewPendingCandidates(ctx); err != nil || len(candidates) != 0 {
		t.Fatalf("held review remained claimable: candidates=%+v err=%v", candidates, err)
	}
}

func TestNativeReviewSyntheticControlBindingInsertedAfterStartupCannotClaim(t *testing.T) {
	ctx := context.Background()
	st, jobID, now := seedNativeReviewObligation(t, "synthetic-reviewer")
	bindReviewDriverRoute(t, st, "synthetic-reviewer", now)
	// The helper models the future fake capability; switch the runtime gate back
	// off after inserting the exact (but synthetic) binding, as production does.
	st.EnableDriverControlOrigin = false
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "must-not-claim", Identity: "synthetic-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"},
		TTL: 5 * time.Minute, Now: now.Add(time.Minute),
	})
	if ls != nil || !errors.Is(err, store.ErrDriverSessionBindingMissing) {
		t.Fatalf("claim lease=%+v err=%v", ls, err)
	}
	var actions, leases int
	var hold string
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-driver-review'`).Scan(&actions)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases WHERE lease_id='must-not-claim'`).Scan(&leases)
	_ = st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries
		WHERE epic_id='epic-driver-review'`).Scan(&hold)
	if actions != 0 || leases != 0 || hold != "review_session_unbound" {
		t.Fatalf("synthetic binding mutated workflow: actions=%d leases=%d hold=%q", actions, leases, hold)
	}
}

func TestNativeReviewClaimCreatesOneImmutableFullyBoundDriverWake(t *testing.T) {
	ctx := context.Background()
	st, jobID, now := seedNativeReviewObligation(t, "bound-reviewer")
	bindReviewDriverRoute(t, st, "bound-reviewer", now)
	ls, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "bound-lease", Identity: "bound-reviewer", Lens: "correctness",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"},
		TTL: 5 * time.Minute, Now: now.Add(time.Minute),
	})
	if err != nil || ls == nil {
		t.Fatalf("claim lease=%+v err=%v", ls, err)
	}
	var count int
	var kind, executor, senderSession, senderRun, senderPrincipal, recipientSession, recipientPane, recipientRun string
	var host, storeID, serverID, lifecycle, profile, workspaceRoot, workspacePath string
	var leaseID, payload, payloadHash, grantID, head, base string
	var leaseEpoch, targetEpoch int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*),kind,executor_kind,sender_session_id,
		sender_agent_run_id,sender_principal_id,recipient_session_id,recipient_pane_instance_id,recipient_agent_run_id,
		target_host_id,target_store_id,target_server_id,lifecycle_key,profile_id,workspace_root_id,
		workspace_relative_path,lease_id,lease_epoch,target_epoch,payload_json,payload_sha256,
		grant_id,head_sha,base_sha FROM epic_actions WHERE epic_id='epic-driver-review'`).Scan(
		&count, &kind, &executor, &senderSession, &senderRun, &senderPrincipal, &recipientSession, &recipientPane,
		&recipientRun, &host, &storeID, &serverID, &lifecycle, &profile, &workspaceRoot,
		&workspacePath, &leaseID, &leaseEpoch, &targetEpoch, &payload, &payloadHash, &grantID,
		&head, &base); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(payload))
	if count != 1 || kind != "review_wake" || executor != "driver" ||
		senderSession != "" || senderRun != "" || senderPrincipal != store.DriverControlIdentity ||
		recipientSession != "session-bound-reviewer" || recipientPane != "pane-bound-reviewer" ||
		recipientRun != "run-bound-reviewer" || host != "host-review" || storeID != "store-review" ||
		serverID != "server-review" || lifecycle != "reviewer-bound-reviewer" ||
		profile != "code-reviewer" || workspaceRoot != "workspace-root" || workspacePath != "repo" ||
		leaseID != "bound-lease" || leaseEpoch != ls.Epoch || targetEpoch != 1 || grantID == "" ||
		head != "driver-head" || base != "base" || payloadHash != "sha256:"+hex.EncodeToString(h[:]) {
		t.Fatalf("wake action was not fully and immutably bound: count=%d kind=%s executor=%s sender=%s/%s principal=%s recipient=%s/%s/%s target=%s/%s/%s/%s profile=%s workspace=%s/%s lease=%s/%d target_epoch=%d grant=%s head/base=%s/%s hash=%s",
			count, kind, executor, senderSession, senderRun, senderPrincipal, recipientSession, recipientPane,
			recipientRun, host, storeID, serverID, lifecycle, profile, workspaceRoot,
			workspacePath, leaseID, leaseEpoch, targetEpoch, grantID, head, base, payloadHash)
	}
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "lost-ack-retry", Identity: "bound-reviewer", Lens: "correctness",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: 5 * time.Minute,
		Now: now.Add(2 * time.Minute),
	}); !errors.Is(err, lease.ErrLostRace) {
		t.Fatalf("lost-ack replay err=%v, want already-claimed fence", err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='epic-driver-review'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("claim replay actions=%d err=%v", count, err)
	}

	claimed, ok, err := (driver.SQLActionStore{DB: st.DB, ControlOriginAvailable: true}).ClaimNextAction(ctx, "driver-executor", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || claimed.Epoch != 1 || claimed.GrantEpoch != 1 {
		t.Fatalf("action claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if claimed.SenderPrincipalID != store.DriverControlIdentity || claimed.SenderSessionID != "" {
		t.Fatalf("claimed origin principal=%q session=%q", claimed.SenderPrincipalID, claimed.SenderSessionID)
	}
	var grants int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_grants WHERE action_id=? AND grant_epoch=1`, claimed.ActionID).Scan(&grants); err != nil || grants != 1 {
		t.Fatalf("durable grant projections=%d err=%v", grants, err)
	}
}

type staleIdentityPort struct {
	*driver.FakePort
	identity driver.Identity
}

func (p *staleIdentityPort) EnsureSession(context.Context, driver.SessionTarget, driver.Action) (driver.Identity, error) {
	return p.identity, nil
}

func claimedReviewWake(t *testing.T) (*store.Store, driver.Action, time.Time) {
	t.Helper()
	ctx := context.Background()
	st, jobID, now := seedNativeReviewObligation(t, "route-reviewer")
	bindReviewDriverRoute(t, st, "route-reviewer", now)
	if _, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: jobID, LeaseID: "route-lease", Identity: "route-reviewer", ModelFamily: "grok",
		Attested: []string{"role:code_reviewer"}, TTL: 5 * time.Minute, Now: now.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	a, ok, err := (driver.SQLActionStore{DB: st.DB, ControlOriginAvailable: true}).ClaimNextAction(ctx, "executor", now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim wake ok=%v err=%v", ok, err)
	}
	return st, a, now
}

func TestReviewWakeStaleIncarnationIsFencedBeforeGrantOrSend(t *testing.T) {
	st, action, now := claimedReviewWake(t)
	// A successor binding is durable, while the immutable action remains tied to
	// the old pane/run. Driver returning the current incarnation must fence it.
	newBinding := store.DriverSessionBinding{
		WorkerIdentity: "route-reviewer", Role: store.DriverReviewerRole,
		HostID: "host-review", StoreID: "store-review", TmuxServerInstanceID: "server-review",
		LifecycleKey: "reviewer-route-reviewer", TargetEpoch: 2, ProfileID: "code-reviewer",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
		SessionID: "session-route-reviewer", PaneInstanceID: "pane-route-reviewer-v2", AgentRunID: "run-route-reviewer-v2",
	}
	if _, err := st.UpsertDriverSessionBinding(context.Background(), newBinding, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fake := driver.NewFake()
	port := &staleIdentityPort{FakePort: fake, identity: driver.Identity{
		HostID: newBinding.HostID, StoreID: newBinding.StoreID,
		TmuxServerInstanceID: newBinding.TmuxServerInstanceID, LifecycleKey: newBinding.LifecycleKey,
		TargetEpoch: newBinding.TargetEpoch, SessionID: newBinding.SessionID,
		PaneInstanceID: newBinding.PaneInstanceID, AgentRunID: newBinding.AgentRunID,
	}}
	_, err := (driver.Executor{Port: port, Store: driver.SQLActionStore{DB: st.DB}}).
		ExecuteClaimed(context.Background(), action.SessionTarget(), action.RouteGrant(), action)
	if !errors.Is(err, driver.ErrIdentityMismatch) || fake.SendCalls != 0 || len(fake.Grants) != 0 {
		t.Fatalf("stale incarnation err=%v sends=%d grants=%d", err, fake.SendCalls, len(fake.Grants))
	}
}

func TestReviewWakeForbidsLateralRouteWithZeroDriverMutation(t *testing.T) {
	st, action, _ := claimedReviewWake(t)
	fake := driver.NewFake()
	tampered := action.RouteGrant()
	tampered.RecipientSessionID = "unrelated-session"
	tampered.RecipientPaneInstanceID = "unrelated-pane"
	_, err := (driver.Executor{Port: fake, Store: driver.SQLActionStore{DB: st.DB}}).
		ExecuteClaimed(context.Background(), action.SessionTarget(), tampered, action)
	if !errors.Is(err, driver.ErrGrantDenied) || fake.SendCalls != 0 || len(fake.Grants) != 0 || len(fake.Sessions) != 0 {
		t.Fatalf("lateral route err=%v sends=%d grants=%d sessions=%d", err, fake.SendCalls, len(fake.Grants), len(fake.Sessions))
	}
}

func TestDriverSessionBindingReplayAndIncarnationHistory(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	b := store.DriverSessionBinding{
		WorkerIdentity: "reviewer", Role: store.DriverReviewerRole, HostID: "host", StoreID: "store",
		TmuxServerInstanceID: "server", LifecycleKey: "life", TargetEpoch: 1, ProfileID: "reviewer",
		WorkspaceRootID: "root", WorkspaceRelativePath: "repo", SessionID: "session",
		PaneInstanceID: "pane-1", AgentRunID: "run-1",
	}
	first, err := st.UpsertDriverSessionBinding(ctx, b, now)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := st.UpsertDriverSessionBinding(ctx, b, now.Add(time.Minute))
	if err != nil || replay.BindingID != first.BindingID || replay.BindingEpoch != 1 {
		t.Fatalf("exact replay=%+v err=%v first=%+v", replay, err, first)
	}
	b.PaneInstanceID, b.AgentRunID, b.TargetEpoch = "pane-2", "run-2", 2
	second, err := st.UpsertDriverSessionBinding(ctx, b, now.Add(2*time.Minute))
	if err != nil || second.BindingID == first.BindingID || second.BindingEpoch != 2 {
		t.Fatalf("successor=%+v err=%v first=%+v", second, err, first)
	}
	active, err := st.ActiveDriverSessionBinding(ctx, "default", "reviewer", store.DriverReviewerRole)
	if err != nil || active.BindingID != second.BindingID || active.PaneInstanceID != "pane-2" {
		t.Fatalf("active=%+v err=%v", active, err)
	}
	var activeCount, supersededCount int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings WHERE state='active'`).Scan(&activeCount)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings WHERE state='superseded'`).Scan(&supersededCount)
	if activeCount != 1 || supersededCount != 1 {
		t.Fatalf("binding history active=%d superseded=%d", activeCount, supersededCount)
	}
}
