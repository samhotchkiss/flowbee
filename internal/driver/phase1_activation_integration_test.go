package driver

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

// TestPhase1ActivationFromCapturedIntentToBuilderLaunch covers the narrowest
// complete Phase 1 activation path. In particular, it crosses the package
// seams between the durable pre-epic Driver outbox, independent processing
// evidence, the Orchestrator contract, exactly-once epic admission, and the
// admitted-delivery builder scheduler.
func TestPhase1ActivationFromCapturedIntentToBuilderLaunch(t *testing.T) {
	ctx := context.Background()
	st, initialAction, fake, now := seedWorkIntentRuntime(t)
	st.EnableEpicReviewHandoffV2 = true
	seedPhase1BuilderRoute(t, st, now)

	beforeRestart := WorkIntentRuntime{
		Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "phase1-before-restart",
	}
	delivery, err := beforeRestart.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || delivery.Delivered != 1 || fake.SendCalls != 1 || len(fake.Grants) != 1 {
		t.Fatalf("Driver delivery=%+v sends=%d grants=%d err=%v",
			delivery, fake.SendCalls, len(fake.Grants), err)
	}
	var receipts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_receipts
		WHERE action_id=? AND payload_sha256=?`, initialAction.ActionID,
		initialAction.PayloadSHA256).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatalf("durable Driver receipts=%d err=%v", receipts, err)
	}

	// Simulate a control-plane process restart after the immutable action,
	// directional grant, and receipt are durable but before stage evidence.
	// The replacement runtime must not send the same message a second time and
	// must not treat the transport receipt as Orchestrator processing.
	afterRestart := WorkIntentRuntime{
		Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "phase1-after-restart",
	}
	if report, err := afterRestart.Tick(ctx, now.Add(4*time.Minute)); err != nil ||
		report.Verified != 0 || fake.SendCalls != 1 {
		t.Fatalf("pre-evidence restart=%+v sends=%d err=%v", report, fake.SendCalls, err)
	}
	var intentState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM work_intents WHERE delivery_action_id=?`,
		initialAction.ActionID).Scan(&intentState); err != nil ||
		intentState != string(workintent.StateReadyForOrchestrator) {
		t.Fatalf("receipt advanced intent state=%q err=%v", intentState, err)
	}

	claimed, intentID, err := scanWorkIntentDriverAction(st.DB.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.id=?`, initialAction.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, providerMessageEvent(
		claimed, "phase1-orchestrator-processing", claimed.RecipientSessionID,
		claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 6))
	if report, err := afterRestart.Tick(ctx, now.Add(5*time.Minute)); err != nil ||
		report.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("evidence verification=%+v sends=%d err=%v", report, fake.SendCalls, err)
	}

	intent, err := st.GetWorkIntent(ctx, "default", intentID)
	if err != nil || intent.State != workintent.StateOrchestrating {
		t.Fatalf("orchestrating intent=%+v err=%v", intent, err)
	}
	binding, err := st.ActiveDriverSessionBinding(ctx, "default", "project-orchestrator",
		store.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	contract := store.WorkIntentEpicContract{
		Slug: "phase1-activation", Title: "Activate the durable Flowbee v2 path",
		Repositories: []string{"russ"}, DeliveryRepo: "russ",
		SpecPath: "epics/phase1-activation.md", Scope: []string{"internal/flowbee/**"},
		IssueRefs:  []string{"#4950", "#4951"},
		Acceptance: []string{"the durable handoff reaches a builder exactly once"},
	}
	contractHash, err := store.WorkIntentEpicContractSHA256(contract)
	if err != nil {
		t.Fatal(err)
	}
	admissionKey, err := workintent.AdmissionKey(workintent.Intent{
		ID: intent.ID, ProjectID: intent.ProjectID, Version: intent.IntentVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := store.RecordWorkIntentEpicContractInput{
		ProjectID: intent.ProjectID, WorkIntentID: intent.ID, IntentVersion: intent.IntentVersion,
		ExpectedStateVersion: intent.StateVersion, SourceArtifactSHA256: intent.ArtifactSHA256,
		ContractVersion: 1, ContractRef: "artifact://epic-contract/phase1-activation",
		ContractSHA256: contractHash, Contract: contract,
		OrchestratorBindingID: binding.BindingID, SubmissionKey: admissionKey,
	}
	prepared, err := st.RecordWorkIntentEpicContract(ctx, input, now.Add(6*time.Minute))
	if err != nil || prepared.State != "prepared" {
		t.Fatalf("prepared contract=%+v err=%v", prepared, err)
	}
	// Lost acknowledgement at the Orchestrator boundary is an exact replay,
	// never a second contract or epic.
	replayed, err := st.RecordWorkIntentEpicContract(ctx, input, now.Add(7*time.Minute))
	if err != nil || replayed.ID != prepared.ID {
		t.Fatalf("contract replay=%+v err=%v", replayed, err)
	}
	admission, err := st.ReconcileWorkIntentAdmissions(ctx, now.Add(8*time.Minute))
	if err != nil || admission.Admitted != 1 {
		t.Fatalf("admission=%+v err=%v", admission, err)
	}
	intent, err = st.GetWorkIntent(ctx, "default", intent.ID)
	if err != nil || intent.AdmittedEpicID == "" || intent.State != workintent.StateAdmitted {
		t.Fatalf("admitted intent=%+v err=%v", intent, err)
	}

	launch, err := st.ReconcileBuilderLaunches(ctx, now.Add(9*time.Minute),
		30*time.Minute, "codex", 5)
	if err != nil || launch.ActionsCreated != 1 {
		t.Fatalf("builder launch=%+v err=%v", launch, err)
	}
	// Every reconciler is safe to replay after a restart: no duplicate contract,
	// epic, delivery, or builder lifecycle action is materialized.
	if again, err := st.ReconcileWorkIntentAdmissions(ctx, now.Add(10*time.Minute)); err != nil ||
		again.Admitted != 0 {
		t.Fatalf("replayed admission=%+v err=%v", again, err)
	}
	if again, err := st.ReconcileBuilderLaunches(ctx, now.Add(10*time.Minute),
		30*time.Minute, "codex", 5); err != nil || again.ActionsCreated != 0 {
		t.Fatalf("replayed builder launch=%+v err=%v", again, err)
	}
	var contracts, epics, deliveries, launches int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_intent_epic_contracts
		WHERE work_intent_id=?`, intent.ID).Scan(&contracts); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics
		WHERE work_intent_id=?`, intent.ID).Scan(&epics); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries
		WHERE epic_id=?`, intent.AdmittedEpicID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id=? AND kind='builder_launch'`, intent.AdmittedEpicID).Scan(&launches); err != nil {
		t.Fatal(err)
	}
	if contracts != 1 || epics != 1 || deliveries != 1 || launches != 1 {
		t.Fatalf("contracts=%d epics=%d deliveries=%d launches=%d",
			contracts, epics, deliveries, launches)
	}
}

func seedPhase1BuilderRoute(t *testing.T, st *store.Store, now time.Time) {
	t.Helper()
	ctx := context.Background()
	st.EnableCapacityV2 = true
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "default", "russ", now); err != nil {
		t.Fatal(err)
	}
	seat := store.Seat{Box: "phase1-builder-host", AgentFamily: "codex",
		CodexHome: "/codex/phase1", Health: store.SeatReady, MaxConcurrent: 1}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{
		SeatID: seat.ID, HostID: seat.Box, AccountKey: "phase1-builder-account",
		CredentialLineage: "phase1-lineage", ReservePct: 10, AccountMaximum: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	reviewer := store.Seat{Box: "phase1-review-host", AgentFamily: "grok",
		ConfigDir: "/grok/phase1", Health: store.SeatReady, MaxConcurrent: 1}
	if err := st.AddSeat(ctx, reviewer, now); err != nil {
		t.Fatal(err)
	}
	reviewer.ID = reviewer.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{
		SeatID: reviewer.ID, HostID: reviewer.Box, AccountKey: "phase1-review-account",
		CredentialLineage: "phase1-review-lineage", ReservePct: 10, AccountMaximum: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	capacityAt := now.Add(6 * time.Minute)
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "phase1-builder-generation", StartedAt: capacityAt, ExpectedSeatIDs: []string{seat.ID, reviewer.ID},
		Observations: []store.CapacitySeatObservation{{
			ObservationID: "phase1-builder-observation", SeatID: seat.ID, HostID: seat.Box,
			Provider: "codex", AccountKey: "phase1-builder-account",
			CredentialLineage: "phase1-lineage", CollectorID: "phase1-fixture",
			Source: "live_app_server", TrustState: "verified", IntegrityState: "verified",
			Windows:   []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}},
			FetchedAt: capacityAt, RawSHA256: "sha256:phase1-builder", AdapterVersion: "fixture/v1",
		}, {
			ObservationID: "phase1-review-observation", SeatID: reviewer.ID, HostID: reviewer.Box,
			Provider: "grok", AccountKey: "phase1-review-account",
			CredentialLineage: "phase1-review-lineage", CollectorID: "phase1-fixture",
			Source: "live_billing", TrustState: "verified", IntegrityState: "verified",
			BillingPeriodActive: true,
			Windows:             []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 20}},
			FetchedAt:           capacityAt, RawSHA256: "sha256:phase1-review", AdapterVersion: "fixture/v1",
		}},
	}, capacityAt); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: "phase1-reviewer", Role: store.DriverReviewerRole,
		SeatID: reviewer.ID, HostID: reviewer.Box, StoreID: "phase1-review-store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "phase1-review-server",
		LifecycleOwnership: "driver_managed", LifecycleKey: "phase1-reviewer",
		TargetEpoch: 1, ProfileID: "grok_reviewer", WorkspaceRootID: "phase1-workspace",
		WorkspaceRelativePath: "review", SessionID: "phase1-review-session",
		PaneInstanceID: "phase1-review-pane", AgentRunID: "phase1-review-run",
		Provider: "grok", ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,tmux_server_domain_id,tmux_server_ownership,state,created_at,updated_at)
		VALUES ('phase1-builder-driver',?, 'phase1-builder-store','phase1-boot','flowbee','managed_dedicated','live',?,?)`,
		seat.Box, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('phase1-builder-store','phase1-builder-driver','tdc2.phase1',1,0,'phase1-baseline',1,?)`,
		stamp); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{
		ProjectID: "default", SeatID: seat.ID, InstanceRef: "phase1-builder-driver",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "phase1-builder-server", ProfileID: "codex-builder",
		WorkspaceRootID: "phase1-workspace", WorkspaceRelativeBase: "repos", Enabled: true,
	}, now); err != nil {
		t.Fatal(err)
	}
}
