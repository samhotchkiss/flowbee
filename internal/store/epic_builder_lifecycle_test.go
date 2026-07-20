package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/driverbridge"
	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func bindBuilderDriver(t *testing.T, st *store.Store, epicID string, now time.Time) store.DriverSessionBinding {
	t.Helper()
	st.EnableDriverControlOrigin = true // future-capability fake route
	b := store.DriverSessionBinding{
		WorkerIdentity: store.BuilderDriverIdentity(epicID), Role: store.DriverBuilderRole,
		HostID: "host-builder", StoreID: "store-builder", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-builder", LifecycleOwnership: "driver_managed",
		LifecycleKey: "builder-" + epicID, TargetEpoch: 1, ProfileID: "codex-builder",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo/" + epicID,
		SessionID: "session-" + epicID, PaneInstanceID: "pane-" + epicID,
		AgentRunID: "run-" + epicID, Provider: "codex", ConversationID: "conversation-" + epicID,
	}
	out, err := st.UpsertDriverSessionBinding(context.Background(), b, now)
	if err != nil {
		t.Fatalf("bind builder: %v", err)
	}
	return out
}

func TestBuilderDoneParksOnlyAfterExactDriverStopAndKeepsScopeReserved(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "park-epic", Repo: "repo",
		Host: "host-builder", Branch: "epic/park", Scope: []string{"internal/payments/**"}}, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkEpicLaunched(ctx, "park-epic", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	bindBuilderDriver(t, st, "park-epic", now.Add(time.Minute))
	if err := st.UpsertEpicStatus(ctx, "park-epic", epicspec.StatusBlock{State: "done"},
		now.Add(2*time.Minute)); err != nil {
		t.Fatalf("commit park intent: %v", err)
	}
	epic, _ := st.GetEpicRun(ctx, "park-epic")
	if epic.State != "running" {
		t.Fatalf("builder claim released compute before Stop proof: state=%s", epic.State)
	}
	var affinity, actionState, executor string
	if err := st.DB.QueryRow(`SELECT builder_affinity_state FROM epic_deliveries WHERE epic_id='park-epic'`).Scan(&affinity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRow(`SELECT state,executor_kind FROM epic_actions
		WHERE epic_id='park-epic' AND kind='builder_park'`).Scan(&actionState, &executor); err != nil {
		t.Fatal(err)
	}
	if affinity != "active" || actionState != "pending" || executor != "driver_lifecycle" {
		t.Fatalf("pre-stop affinity=%s action=%s/%s", affinity, actionState, executor)
	}
	if _, held, err := st.HostActiveEpic(ctx, "host-builder"); err != nil || !held {
		t.Fatalf("physical seat released before stop held=%v err=%v", held, err)
	}

	fake := driver.NewFake()
	runtime := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: st.DB},
		Projector: driverbridge.Projector{Store: st}, Owner: "lifecycle", ClaimTTL: time.Minute}
	rep, err := runtime.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Executed != 1 || fake.StopCalls != 1 {
		t.Fatalf("park runtime=%+v stop_calls=%d err=%v", rep, fake.StopCalls, err)
	}
	epic, _ = st.GetEpicRun(ctx, "park-epic")
	if epic.State != "done" || epic.FinishedAt == "" {
		t.Fatalf("positive stop did not finish legacy builder row: %+v", epic)
	}
	if err := st.DB.QueryRow(`SELECT builder_affinity_state FROM epic_deliveries WHERE epic_id='park-epic'`).Scan(&affinity); err != nil || affinity != "parked" {
		t.Fatalf("affinity=%s err=%v", affinity, err)
	}
	if _, held, err := st.HostActiveEpic(ctx, "host-builder"); err != nil || held {
		t.Fatalf("parked physical seat remained occupied held=%v err=%v", held, err)
	}
	// Compute is free, but the unmerged worktree/branch scope remains reserved.
	err = st.AddEpicRun(ctx, store.EpicRun{ID: "overlap", Repo: "repo", Host: "other-host",
		Scope: []string{"internal/**"}}, 1, now.Add(4*time.Minute))
	if !errors.Is(err, store.ErrEpicScopeOverlap) {
		t.Fatalf("parked scope was released before merge/abandon: %v", err)
	}
}

func TestBuilderParkCrashAfterStopRecoversReceiptWithoutSecondStop(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "park-crash", Repo: "repo",
		Host: "host-builder", Branch: "epic/crash"}, 1, now); err != nil {
		t.Fatal(err)
	}
	_ = st.MarkEpicLaunched(ctx, "park-crash", now)
	bindBuilderDriver(t, st, "park-crash", now)
	if err := st.UpsertEpicStatus(ctx, "park-crash", epicspec.StatusBlock{State: "done"}, now); err != nil {
		t.Fatal(err)
	}
	actionStore := driver.SQLActionStore{DB: st.DB}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, "dead-runtime", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim park ok=%v err=%v", ok, err)
	}
	fake := driver.NewFake()
	if _, err := fake.StopSession(ctx, action.SessionTarget(), action); err != nil {
		t.Fatal(err)
	}
	// Process dies here: no Flowbee receipt/projection/action acknowledgement.
	restarted := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: st}, Owner: "replacement-runtime",
		ClaimTTL: time.Minute}
	rep, err := restarted.Tick(ctx, now.Add(2*time.Minute))
	if err != nil || rep.Reclaimed != 1 || rep.Verified != 1 || fake.StopCalls != 1 {
		t.Fatalf("recovery=%+v stop_calls=%d err=%v", rep, fake.StopCalls, err)
	}
	var affinity, actionState string
	_ = st.DB.QueryRow(`SELECT builder_affinity_state FROM epic_deliveries WHERE epic_id='park-crash'`).Scan(&affinity)
	_ = st.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, action.ActionID).Scan(&actionState)
	if affinity != "parked" || actionState != "acknowledged" {
		t.Fatalf("recovered affinity/action=%s/%s", affinity, actionState)
	}
}

func TestBuilderParkUncertainStopUsesInspectionOnlyVerifyWithoutSecondStop(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 17, 30, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "park-uncertain", Repo: "repo",
		Host: "host-builder", Branch: "epic/uncertain"}, 1, now); err != nil {
		t.Fatal(err)
	}
	_ = st.MarkEpicLaunched(ctx, "park-uncertain", now)
	bindBuilderDriver(t, st, "park-uncertain", now)
	if err := st.UpsertEpicStatus(ctx, "park-uncertain", epicspec.StatusBlock{State: "done"}, now); err != nil {
		t.Fatal(err)
	}
	actionStore := driver.SQLActionStore{DB: st.DB}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, "dead-runtime", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim park ok=%v err=%v", ok, err)
	}
	fake := driver.NewFake()
	receipt, err := fake.StopSession(ctx, action.SessionTarget(), action)
	if err != nil {
		t.Fatal(err)
	}
	receipt.Status = "uncertain"
	fake.LifecycleReceipts[action.ActionID] = receipt
	fake.NextLifecycleStatus = "stopped"
	restarted := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: st}, Owner: "verification-runtime",
		ClaimTTL: time.Minute}
	rep, err := restarted.Tick(ctx, now.Add(2*time.Minute))
	if err != nil || rep.Verified != 1 || fake.StopCalls != 1 {
		t.Fatalf("uncertain recovery=%+v stop_calls=%d err=%v", rep, fake.StopCalls, err)
	}
	var actionEpoch, receiptEpoch int64
	var actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,action_epoch FROM epic_actions WHERE id=?`,
		action.ActionID).Scan(&actionState, &actionEpoch); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT action_epoch FROM driver_lifecycle_receipts
		WHERE action_id=?`, action.ActionID).Scan(&receiptEpoch); err != nil {
		t.Fatal(err)
	}
	if actionState != "acknowledged" || actionEpoch != action.Epoch+1 || receiptEpoch != actionEpoch {
		t.Fatalf("verified action=%s action_epoch=%d receipt_epoch=%d", actionState, actionEpoch, receiptEpoch)
	}
}

func TestBuilderParkStaleBindingCannotReleaseReplacementSeat(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 17, 45, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "park-stale", Repo: "repo",
		Host: "host-builder", Branch: "epic/stale"}, 1, now); err != nil {
		t.Fatal(err)
	}
	_ = st.MarkEpicLaunched(ctx, "park-stale", now)
	prior := bindBuilderDriver(t, st, "park-stale", now)
	if err := st.UpsertEpicStatus(ctx, "park-stale", epicspec.StatusBlock{State: "done"}, now); err != nil {
		t.Fatal(err)
	}
	// A newer incarnation becomes authoritative before the old Stop receipt is
	// projected. Positive absence for the old pane must not free the new seat.
	replacement := prior
	replacement.BindingID = ""
	replacement.BindingEpoch = 0
	replacement.TargetEpoch++
	replacement.SessionID = "session-park-stale-2"
	replacement.PaneInstanceID = "pane-park-stale-2"
	replacement.AgentRunID = "run-park-stale-2"
	if _, err := st.UpsertDriverSessionBinding(ctx, replacement, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	fake := driver.NewFake()
	runtime := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: st.DB},
		Projector: driverbridge.Projector{Store: st}, Owner: "stale-runtime", ClaimTTL: time.Minute}
	if _, err := runtime.Tick(ctx, now.Add(2*time.Minute)); err == nil {
		t.Fatal("stale Stop projection unexpectedly released replacement binding")
	}
	var affinity, epicState, actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT builder_affinity_state FROM epic_deliveries
		WHERE epic_id='park-stale'`).Scan(&affinity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epics WHERE id='park-stale'`).Scan(&epicState); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions
		WHERE epic_id='park-stale' AND kind='builder_park'`).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	if affinity != "active" || epicState != "running" || actionState != "verifying" || fake.StopCalls != 1 {
		t.Fatalf("stale projection affinity=%s epic=%s action=%s stop_calls=%d",
			affinity, epicState, actionState, fake.StopCalls)
	}
}

func TestRejectedReviewRelaunchCrashRecoversNewIncarnationAndOneWake(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	seedAwaitingReview(t, st, "rework-crash", "rework-head", now)
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries
		SET builder_affinity_state='parked' WHERE epic_id='rework-crash'`); err != nil {
		t.Fatal(err)
	}
	prior := bindBuilderDriver(t, st, "rework-crash", now)
	if _, err := st.ReconcileEpicReviewHandoffs(ctx, now.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	bindReviewDriverRoute(t, st, "rework-reviewer", now.Add(10*time.Minute))
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("review candidates=%+v err=%v", candidates, err)
	}
	lease, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: "rework-review-lease", Identity: "rework-reviewer",
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: time.Minute,
		Now: now.Add(11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReviewResult(ctx, store.DBFactSource{DB: st.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: candidates[0].JobID, Epoch: lease.Epoch, Claim: job.VerdictChangesRequested,
		Notes: "fix the epoch fence", IdempotencyKey: "rework-crash-verdict",
		Now: now.Add(12 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	actionStore := driver.SQLActionStore{DB: st.DB}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, "dead-relauncher",
		now.Add(13*time.Minute), time.Minute)
	if err != nil || !ok || action.Kind != "builder_rework" {
		t.Fatalf("claim relaunch=%+v ok=%v err=%v", action, ok, err)
	}
	fake := driver.NewFake()
	receipt, err := fake.EnsureLifecycleSession(ctx, action.SessionTarget(), action)
	if err != nil || receipt.IdentityAfter.AgentRunID == prior.AgentRunID {
		t.Fatalf("ensure new incarnation receipt=%+v err=%v", receipt, err)
	}
	// Process dies after Driver commits Ensure but before Flowbee stores the
	// receipt, projects the new binding, or queues the routed wake.
	restarted := driver.LifecycleRuntime{Port: fake, Store: actionStore,
		Projector: driverbridge.Projector{Store: st}, Owner: "replacement-relauncher",
		ClaimTTL: time.Minute}
	rep, err := restarted.Tick(ctx, now.Add(15*time.Minute))
	if err != nil || rep.Reclaimed != 1 || rep.Verified != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("relaunch recovery=%+v ensure_calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	var state, affinity, actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,builder_affinity_state
		FROM epic_deliveries WHERE epic_id='rework-crash'`).Scan(&state, &affinity); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE id=?`,
		action.ActionID).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	var newRun string
	var targetEpoch int64
	if err := st.DB.QueryRowContext(ctx, `SELECT agent_run_id,target_epoch FROM driver_session_bindings
		WHERE project_id='default' AND worker_identity=? AND role=? AND state='active'`,
		store.BuilderDriverIdentity("rework-crash"), store.DriverBuilderRole).Scan(&newRun, &targetEpoch); err != nil {
		t.Fatal(err)
	}
	var wakes int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='rework-crash' AND kind='builder_rework_wake' AND state='pending'`).Scan(&wakes); err != nil {
		t.Fatal(err)
	}
	if state != "rebuild_in_flight" || affinity != "active" || actionState != "acknowledged" ||
		newRun == prior.AgentRunID || targetEpoch != prior.TargetEpoch+1 || wakes != 1 {
		t.Fatalf("relaunch state=%s affinity=%s action=%s run=%s epoch=%d wakes=%d",
			state, affinity, actionState, newRun, targetEpoch, wakes)
	}
	// Projection replay may only recover/retain the same routed wake.
	if err := (driverbridge.Projector{Store: st}).ProjectLifecycleResult(ctx, action, receipt,
		now.Add(16*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='rework-crash' AND kind='builder_rework_wake'`).Scan(&wakes); err != nil || wakes != 1 {
		t.Fatalf("replay wake count=%d err=%v", wakes, err)
	}
}

func seedRejectedBuilderLifecycle(t *testing.T, st *store.Store, epicID, seatID, account string,
	prNumber int, now time.Time) driver.Action {
	t.Helper()
	ctx := context.Background()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: epicID, Repo: "repo", Branch: "epic/" + epicID}, 1, now); err != nil {
		t.Fatal(err)
	}
	if err := st.ObserveEpicArtifactFact(ctx, store.EpicArtifactFact{
		EpicID: epicID, Repo: "repo", Branch: "epic/" + epicID, PRNumber: prNumber,
		PROpen: true, HeadSHA: epicID + "-head", BaseSHA: "base", CIState: "green",
		CIHasRealSuccess: true, RequiredChecksPresentPassed: true,
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epics SET state='done',seat_id=?,account_key=?,host='host-builder'
		WHERE id=?`, seatID, account, epicID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET builder_affinity_state='parked'
		WHERE epic_id=?`, epicID); err != nil {
		t.Fatal(err)
	}
	bindBuilderDriver(t, st, epicID, now)
	if _, err := st.ReconcileEpicReviewHandoffs(ctx, now.Add(10*time.Minute), 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	reviewer := epicID + "-reviewer"
	bindReviewDriverRoute(t, st, reviewer, now.Add(10*time.Minute))
	candidates, err := st.ReviewPendingCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("review candidates=%+v err=%v", candidates, err)
	}
	lease, err := st.ClaimReviewJob(ctx, store.ClaimReviewParams{
		JobID: candidates[0].JobID, LeaseID: epicID + "-review-lease", Identity: reviewer,
		ModelFamily: "grok", Attested: []string{"role:code_reviewer"}, TTL: time.Minute,
		Now: now.Add(11 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReviewResult(ctx, store.DBFactSource{DB: st.DB}, job.Policy{}, store.ReviewResultParams{
		JobID: candidates[0].JobID, Epoch: lease.Epoch, Claim: job.VerdictChangesRequested,
		Notes: "address the review findings", IdempotencyKey: epicID + "-verdict",
		Now: now.Add(12 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	actionStore := driver.SQLActionStore{DB: st.DB}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, epicID+"-claim",
		now.Add(13*time.Minute), time.Minute)
	if err != nil || !ok || action.Kind != "builder_rework" {
		t.Fatalf("builder rework claim=%+v ok=%v err=%v", action, ok, err)
	}
	return action
}

func TestBuilderRelaunchCapacityFailsClosedThenAcquiresAtomically(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	seat := addBoundCapacitySeat(t, st, now, "/codex/builder-capacity", "host-builder",
		"builder-account", "builder-lineage")
	action := seedRejectedBuilderLifecycle(t, st, "rework-capacity", seat.ID,
		"builder-account", 5101, now)
	// Return the setup claim to pending. The production runtime owns the real
	// claim and runs the capacity gate before touching Driver.
	if err := (driver.SQLActionStore{DB: st.DB}).RetryAction(ctx, action.ActionID,
		"rework-capacity-claim", action.Epoch, "test setup", now.Add(14*time.Minute),
		now.Add(13*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.EnableCapacityV2 = true
	fake := driver.NewFake()
	projector := driverbridge.Projector{Store: st, CapacityFreshFor: 5 * time.Minute}
	runtime := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: st.DB},
		Projector: projector, Gate: projector, Owner: "capacity-runtime", ClaimTTL: time.Minute}
	rep, err := runtime.Tick(ctx, now.Add(14*time.Minute))
	if err != nil || rep.Held != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("capacity hold=%+v ensure_calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	var epicState, hold, actionState string
	_ = st.DB.QueryRowContext(ctx, `SELECT state FROM epics WHERE id='rework-capacity'`).Scan(&epicState)
	_ = st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries WHERE epic_id='rework-capacity'`).Scan(&hold)
	_ = st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE id=?`, action.ActionID).Scan(&actionState)
	if epicState != "done" || hold != "builder_capacity_unavailable" || actionState != "pending" {
		t.Fatalf("held epic=%s hold=%s action=%s", epicState, hold, actionState)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE epic_id='rework-capacity' AND kind='capacity_pool_exhausted' AND state='open'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("capacity attention=%d err=%v", alerts, err)
	}
	observation := liveCapacityObservation("obs-builder-rework", seat, now.Add(15*time.Minute),
		"builder-account", "builder-lineage")
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "generation-builder-rework", StartedAt: now.Add(15 * time.Minute),
		ExpectedSeatIDs: []string{seat.ID}, Observations: []store.CapacitySeatObservation{observation},
	}, now.Add(15*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rep, err = runtime.Tick(ctx, now.Add(15*time.Minute))
	if err != nil || rep.Executed != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("capacity recovery=%+v ensure_calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	var deliveryState, affinity string
	_ = st.DB.QueryRowContext(ctx, `SELECT state FROM epics WHERE id='rework-capacity'`).Scan(&epicState)
	_ = st.DB.QueryRowContext(ctx, `SELECT state,builder_affinity_state FROM epic_deliveries
		WHERE epic_id='rework-capacity'`).Scan(&deliveryState, &affinity)
	if epicState != "running" || deliveryState != "rebuild_in_flight" || affinity != "active" {
		t.Fatalf("reacquired epic=%s delivery=%s affinity=%s", epicState, deliveryState, affinity)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE epic_id='rework-capacity' AND kind='capacity_pool_exhausted' AND state='resolved'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("resolved capacity attention=%d err=%v", alerts, err)
	}
}

func TestBuilderRelaunchCrashAfterCapacityLeaseBeforeEnsureRecoversExactlyOnce(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	action := seedRejectedBuilderLifecycle(t, st, "rework-pre-ensure-crash", "", "", 5102, now)
	projector := driverbridge.Projector{Store: st}
	gate, err := projector.PrepareLifecycleAction(ctx, action, now.Add(13*time.Minute))
	if err != nil || !gate.Allowed {
		t.Fatalf("capacity lease gate=%+v err=%v", gate, err)
	}
	var epicState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epics
		WHERE id='rework-pre-ensure-crash'`).Scan(&epicState); err != nil || epicState != "launching" {
		t.Fatalf("durable compute lease state=%s err=%v", epicState, err)
	}
	// Process dies before calling Ensure. Driver has no receipt and reports exact
	// target absence; recovery resumes the same action epoch, never a blind send.
	fake := driver.NewFake()
	runtime := driver.LifecycleRuntime{Port: fake, Store: driver.SQLActionStore{DB: st.DB},
		Projector: projector, Gate: projector, Owner: "pre-ensure-recovery", ClaimTTL: time.Minute}
	rep, err := runtime.Tick(ctx, now.Add(15*time.Minute))
	if err != nil || rep.Reclaimed != 1 || rep.Executed != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("pre-ensure recovery=%+v ensure_calls=%d err=%v", rep, fake.EnsureCalls, err)
	}
	var recoveredEpoch int64
	var actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,action_epoch FROM epic_actions WHERE id=?`,
		action.ActionID).Scan(&actionState, &recoveredEpoch); err != nil {
		t.Fatal(err)
	}
	if actionState != "acknowledged" || recoveredEpoch != action.Epoch {
		t.Fatalf("recovered action=%s epoch=%d want=%d", actionState, recoveredEpoch, action.Epoch)
	}
}

func TestConcurrentBuilderRelaunchesCannotConsumeOneLastSeatTwice(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	seat := addBoundCapacitySeat(t, st, now, "/codex/one-slot", "host-builder",
		"one-slot-account", "one-slot-lineage")
	if _, err := st.DB.ExecContext(ctx, `UPDATE seats SET max_concurrent=1 WHERE id=?`, seat.ID); err != nil {
		t.Fatal(err)
	}
	a := seedRejectedBuilderLifecycle(t, st, "rework-race-a", seat.ID, "one-slot-account", 5103, now)
	b := seedRejectedBuilderLifecycle(t, st, "rework-race-b", seat.ID, "one-slot-account", 5104, now.Add(time.Hour))
	observation := liveCapacityObservation("obs-one-slot", seat, now.Add(2*time.Hour),
		"one-slot-account", "one-slot-lineage")
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "generation-one-slot", StartedAt: now.Add(2 * time.Hour),
		ExpectedSeatIDs: []string{seat.ID}, Observations: []store.CapacitySeatObservation{observation},
	}, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	st.EnableCapacityV2 = true
	projector := driverbridge.Projector{Store: st, CapacityFreshFor: 5 * time.Minute}
	type result struct {
		gate driver.LifecycleGateResult
		err  error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, action := range []driver.Action{a, b} {
		go func(action driver.Action) {
			<-start
			gate, err := projector.PrepareLifecycleAction(ctx, action, now.Add(2*time.Hour))
			results <- result{gate, err}
		}(action)
	}
	close(start)
	allowed, held := 0, 0
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.gate.Allowed {
			allowed++
		} else {
			held++
		}
	}
	var launching, capacityHolds int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE seat_id=? AND state='launching'`, seat.ID).Scan(&launching)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries
		WHERE hold_kind='builder_capacity_unavailable'`).Scan(&capacityHolds)
	if allowed != 1 || held != 1 || launching != 1 || capacityHolds != 1 {
		t.Fatalf("allowed=%d held=%d launching=%d holds=%d", allowed, held, launching, capacityHolds)
	}
}
