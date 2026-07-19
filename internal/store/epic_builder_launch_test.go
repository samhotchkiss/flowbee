package store_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/driverbridge"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type builderLaunchHarness struct {
	st          *store.Store
	seat        store.Seat
	now         time.Time
	instanceRef string
}

func newBuilderLaunchHarness(t *testing.T, maximum int) builderLaunchHarness {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	st.EnableDriverControlOrigin = true // future-capability fake route
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	seat := store.Seat{Box: "host-build", AgentFamily: "codex", CodexHome: "/codex/build",
		Health: store.SeatReady, MaxConcurrent: maximum}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: seat.ID,
		HostID: seat.Box, AccountKey: "account-build", CredentialLineage: "lineage-build",
		ReservePct: 10, AccountMaximum: maximum}, now); err != nil {
		t.Fatal(err)
	}
	obs := store.CapacitySeatObservation{ObservationID: "builder-observation", SeatID: seat.ID,
		HostID: seat.Box, Provider: "codex", AccountKey: "account-build",
		CredentialLineage: "lineage-build", CollectorID: "collector-build",
		Source: "live_app_server", TrustState: "verified", IntegrityState: "verified",
		Windows:   []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}},
		FetchedAt: now, RawSHA256: "sha256:builder", AdapterVersion: "fixture/v1"}
	reviewer := store.Seat{Box: "host-review", AgentFamily: "grok", ConfigDir: "/grok/review",
		Health: store.SeatReady, MaxConcurrent: 1}
	if err := st.AddSeat(ctx, reviewer, now); err != nil {
		t.Fatal(err)
	}
	reviewer.ID = reviewer.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: reviewer.ID,
		HostID: reviewer.Box, AccountKey: "account-review", CredentialLineage: "lineage-review",
		ReservePct: 10, AccountMaximum: 1}, now); err != nil {
		t.Fatal(err)
	}
	reviewObs := store.CapacitySeatObservation{ObservationID: "review-observation", SeatID: reviewer.ID,
		HostID: reviewer.Box, Provider: "grok", AccountKey: "account-review",
		CredentialLineage: "lineage-review", CollectorID: "collector-review",
		Source: "live_billing", TrustState: "verified", IntegrityState: "verified",
		BillingPeriodActive: true,
		Windows:             []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 20}},
		FetchedAt:           now, RawSHA256: "sha256:review", AdapterVersion: "fixture/v1"}
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "builder-generation",
		StartedAt: now, ExpectedSeatIDs: []string{seat.ID, reviewer.ID},
		Observations: []store.CapacitySeatObservation{obs, reviewObs}}, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	instanceRef := "builder-driver"
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES (?,?,?,'boot-builder','live',?,?)`, instanceRef, seat.Box, "store-build", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-build',?,'tdc2.baseline',5,0,'baseline',1,?)`, instanceRef, stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "default",
		SeatID: seat.ID, InstanceRef: instanceRef, TmuxServerInstanceID: "server-build",
		ProfileID: "codex-builder", WorkspaceRootID: "workspace-build",
		WorkspaceRelativeBase: "repos", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
		HostID: seat.Box, StoreID: "store-build", TmuxServerInstanceID: "server-build",
		LifecycleKey: "flowbee-control", TargetEpoch: 1, ProfileID: "flowbee-control",
		WorkspaceRootID: "workspace-build", WorkspaceRelativePath: "control",
		SessionID: "flowbee-control-session", PaneInstanceID: "flowbee-control-pane",
		AgentRunID: "flowbee-control-run", Provider: "flowbee", ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	return builderLaunchHarness{st: st, seat: seat, now: now, instanceRef: instanceRef}
}

func (h builderLaunchHarness) addEpic(t *testing.T, id string) {
	t.Helper()
	if err := h.st.AddEpicRun(context.Background(), store.EpicRun{ID: id, Repo: "russ",
		Branch: "epic/" + id, Slug: id, Title: "Build " + id,
		FilePath: "epics/" + id + ".md", Scope: []string{id + "/**"}}, 1, h.now); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionLaunchCrashRedispatchesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "launch-crash")
	rep, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute), 5*time.Minute, "codex", 5)
	if err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("launch reconcile=%+v err=%v", rep, err)
	}
	actionStore := driver.SQLActionStore{DB: h.st.DB, ControlOriginAvailable: true}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, "dead-launcher", h.now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok || action.Kind != "builder_launch" {
		t.Fatalf("claim=%+v ok=%v err=%v", action, ok, err)
	}
	projector := driverbridge.Projector{Store: h.st, CapacityFreshFor: 5 * time.Minute}
	gate, err := projector.PrepareLifecycleAction(ctx, action, h.now.Add(2*time.Minute))
	if err != nil || !gate.Allowed {
		t.Fatalf("launch gate=%+v err=%v", gate, err)
	}
	fake := driver.NewFake()
	if _, err := fake.EnsureLifecycleSession(ctx, action.SessionTarget(), action); err != nil {
		t.Fatal(err)
	}
	// Process dies after Driver's durable Ensure receipt, before Flowbee persists
	// the receipt, projects the binding, or acknowledges the action.
	restarted := driver.LifecycleRuntime{Port: fake, Store: actionStore, Projector: projector,
		Owner: "replacement-launcher", ClaimTTL: time.Minute, MaximumTries: 5}
	lifecycleRep, err := restarted.Tick(ctx, h.now.Add(4*time.Minute))
	if err != nil || lifecycleRep.Reclaimed != 1 || lifecycleRep.Verified != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("lifecycle recovery=%+v ensure_calls=%d err=%v", lifecycleRep, fake.EnsureCalls, err)
	}
	var launchActions, contractActions int
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='launch-crash'
		AND kind='builder_launch'`).Scan(&launchActions)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id='launch-crash'
		AND kind='builder_launch_contract'`).Scan(&contractActions)
	if launchActions != 1 || contractActions != 1 {
		t.Fatalf("launch actions=%d contract actions=%d", launchActions, contractActions)
	}
	beforeAck, err := h.st.GetEpicRun(ctx, "launch-crash")
	if err != nil || beforeAck.State != "launching" || beforeAck.DeliveryState != "admitted" {
		t.Fatalf("transport Ensure incorrectly proved building: epic=%+v err=%v", beforeAck, err)
	}

	var contract driver.Action
	contract, ok, err = actionStore.ClaimNextAction(ctx, "contract-inspector", h.now.Add(5*time.Minute), time.Minute)
	if err != nil || !ok || contract.Kind != "builder_launch_contract" {
		t.Fatalf("claim contract=%+v ok=%v err=%v", contract, ok, err)
	}
	if err := actionStore.RetryAction(ctx, contract.ActionID, "contract-inspector", contract.Epoch,
		"test releases to production runtime", h.now.Add(5*time.Minute), h.now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,'observing','working',5,'tdc2.baseline','snapshot',?)`,
		contract.TargetStoreID, contract.RecipientSessionID, contract.TargetHostID,
		contract.RecipientPaneInstanceID, contract.RecipientAgentRunID, contract.TargetServerID,
		h.now.Add(5*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	runtime := driver.Runtime{Port: fake, Store: actionStore,
		Evidence: driver.SQLStageEvidence{DB: h.st.DB}, Owner: "contract-runtime", ClaimTTL: time.Minute}
	transportRep, err := runtime.Tick(ctx, h.now.Add(6*time.Minute))
	if err != nil || transportRep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("contract delivery=%+v sends=%d err=%v", transportRep, fake.SendCalls, err)
	}
	insertBuilderContractEvidence(t, h, contract, 6)
	transportRep, err = runtime.Tick(ctx, h.now.Add(7*time.Minute))
	if err != nil || transportRep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("contract evidence=%+v sends=%d err=%v", transportRep, fake.SendCalls, err)
	}
	rep, err = h.st.ReconcileBuilderLaunches(ctx, h.now.Add(8*time.Minute), 5*time.Minute, "codex", 5)
	if err != nil || rep.Acknowledged != 1 {
		t.Fatalf("launch ack=%+v err=%v", rep, err)
	}
	epic, err := h.st.GetEpicRun(ctx, "launch-crash")
	if err != nil || epic.State != "running" || epic.DeliveryState != "building" {
		t.Fatalf("epic=%+v err=%v", epic, err)
	}
}

func TestBuilderLaunchSyntheticControlBindingCannotMaterializeContract(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "launch-control-gap")
	if rep, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("launch reconcile=%+v err=%v", rep, err)
	}
	// The harness inserted the historical synthetic flowbee-control binding.
	// Turning off the negotiated capability models production after startup.
	h.st.EnableDriverControlOrigin = false
	fake := driver.NewFake()
	runtime := driver.LifecycleRuntime{Port: fake,
		Store:     driver.SQLActionStore{DB: h.st.DB},
		Projector: driverbridge.Projector{Store: h.st}, Owner: "control-gap-launcher",
		ClaimTTL: time.Minute, MaximumTries: 5}
	if rep, err := runtime.Tick(ctx, h.now.Add(2*time.Minute)); err != nil || rep.Executed != 1 {
		t.Fatalf("lifecycle=%+v err=%v", rep, err)
	}
	var messageActions int
	var hold string
	if err := h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='launch-control-gap' AND executor_kind='driver'`).Scan(&messageActions); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries
		WHERE epic_id='launch-control-gap'`).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if messageActions != 0 || hold != "driver_control_origin_unavailable" || fake.SendCalls != 0 {
		t.Fatalf("message actions=%d hold=%q sends=%d", messageActions, hold, fake.SendCalls)
	}
}

func TestBuilderLaunchStoreResetFencesCommittedTargetBeforeEnsure(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "launch-reset")
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	actionStore := driver.SQLActionStore{DB: h.st.DB, ControlOriginAvailable: true}
	action, ok, err := actionStore.ClaimNextLifecycleAction(ctx, "reset-launcher",
		h.now.Add(2*time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE driver_instances SET store_id='store-replacement'
		WHERE instance_ref=?`, h.instanceRef); err != nil {
		t.Fatal(err)
	}
	gate, err := (driverbridge.Projector{Store: h.st, CapacityFreshFor: 5 * time.Minute}).
		PrepareLifecycleAction(ctx, action, h.now.Add(2*time.Minute))
	if err == nil || gate.Allowed {
		t.Fatalf("store-reset target was not fenced: gate=%+v err=%v", gate, err)
	}
	fake := driver.NewFake()
	if fake.EnsureCalls != 0 {
		t.Fatalf("stale target caused %d Driver Ensure calls", fake.EnsureCalls)
	}
}

func TestBuilderLaunchDeadLetterBecomesVisibleDurableHold(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "launch-dead")
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE epic_actions SET state='dead_letter',attempts=5,
		dead_lettered_at=?,updated_at=? WHERE epic_id='launch-dead' AND kind='builder_launch'`,
		h.now.Add(2*time.Minute).UTC().Format(time.RFC3339Nano),
		h.now.Add(2*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	rep, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(3*time.Minute),
		5*time.Minute, "codex", 5)
	if err != nil || rep.Stalled != 1 {
		t.Fatalf("stalled reconcile=%+v err=%v", rep, err)
	}
	var hold string
	var attention, alert int
	_ = h.st.DB.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries
		WHERE epic_id='launch-dead'`).Scan(&hold)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE epic_id='launch-dead' AND kind='builder_launch_stalled' AND state='open'`).Scan(&attention)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE epic_id='launch-dead' AND kind='builder_launch_stalled'`).Scan(&alert)
	if hold != "builder_launch_stalled" || attention != 1 || alert != 1 {
		t.Fatalf("hold=%q attention=%d alert=%d", hold, attention, alert)
	}
}

func insertBuilderContractEvidence(t *testing.T, h builderLaunchHarness, action driver.Action, seq uint64) {
	t.Helper()
	ctx := context.Background()
	bindingEpoch := int64(1)
	source := json.RawMessage(`{"kind":"provider_log","source_id":"codex-jsonl","logical_record_id":"sha256:logical","ingest_occurrence_id":"sha256:occurrence","binding_epoch":1,"fidelity":"replayable","position":{},"adapter_profile":"codex-rollout/1","rule_id":null}`)
	payload, _ := json.Marshal(map[string]any{"message_id": "builder-contract-message", "role": "user",
		"channel": "visible", "status": "completed", "content_sha256": action.PayloadSHA256,
		"content": map[string]any{"mode": "inline", "encoding": "message_parts", "parts": []any{},
			"logical_text_bytes": len(action.Payload), "sha256": action.PayloadSHA256}})
	event := driver.Observation{SpecVersion: "tmux-driver.events/v2", EventID: "builder-contract-event",
		Cursor: "tdc2.builder-contract", StoreSeq: seq, SessionSeq: seq,
		TransitionID: "transition-builder-contract", TransitionIndex: 0, TransitionCount: 1,
		ProducerBootID: "boot-builder", Kind: "message.completed",
		ObservedAt: h.now.Add(7 * time.Minute).UTC().Format(time.RFC3339Nano),
		Identity: driver.Identity{HostID: action.TargetHostID, StoreID: action.TargetStoreID,
			TmuxServerInstanceID: action.TargetServerID, SessionID: action.RecipientSessionID,
			PaneInstanceID: action.RecipientPaneInstanceID, AgentRunID: action.RecipientAgentRunID,
			StateCursor: "tdc2.builder-contract"}, Source: source,
		Correlation: json.RawMessage(`{"turn_id":"turn-builder","message_id":"builder-contract-message","tool_call_id":null,"attention_id":null}`),
		CausedBy:    []string{}, Payload: payload}
	_ = bindingEpoch
	result, err := (driver.ObservationSQLStore{DB: h.st.DB}).Fold(ctx, h.instanceRef,
		driver.ObservationBatch{StoreID: action.TargetStoreID, NextCursor: event.Cursor,
			DurableHighWaterCursor: event.Cursor, HistoryComplete: true, Events: []driver.Observation{event}})
	if err != nil || result.Inserted != 1 {
		t.Fatalf("fold contract evidence=%+v err=%v", result, err)
	}
}

func TestConcurrentAdmissionsCannotDoubleBookBuilderCapacity(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "launch-one")
	h.addEpic(t, "launch-two")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
				5*time.Minute, "codex", 5)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var assigned, actions, held int
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE seat_id=?`, h.seat.ID).Scan(&assigned)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE kind='builder_launch'`).Scan(&actions)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries
		WHERE hold_kind='builder_capacity_unavailable'`).Scan(&held)
	if assigned != 1 || actions != 1 || held != 1 {
		t.Fatalf("assigned=%d actions=%d capacity-held=%d", assigned, actions, held)
	}
}
