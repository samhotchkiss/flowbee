package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

const intentRuntimeSHA = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

func seedWorkIntentRuntime(t *testing.T) (*store.Store, Action, *FakePort, time.Time) {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableDriverControlOrigin = true // future-capability fake transport
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('local-driver','host-1','store-1','boot-1','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-1','local-driver','tdc2.baseline',5,0,'baseline-event',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	orchestrator, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "project-orchestrator", Role: store.DriverOrchestratorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerInstanceID: "server-1",
		LifecycleKey: "orchestrator", TargetEpoch: 1, ProfileID: "orchestrator",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
		SessionID: "orchestrator-session", PaneInstanceID: "orchestrator-pane",
		AgentRunID: "orchestrator-run",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES ('store-1',?,?,?,?,?,'observing','idle',5,'tdc2.baseline','snapshot',?)`,
		orchestrator.SessionID, orchestrator.HostID, orchestrator.PaneInstanceID,
		orchestrator.AgentRunID, orchestrator.TmuxServerInstanceID, stamp); err != nil {
		t.Fatal(err)
	}
	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "default", SourceConversationID: "conversation-1", SourceMessageID: "message-1",
		SourceMessageVersion: 1, InteractorIncarnationID: "interactor-run", Title: "Build intent",
		ArtifactRef: "artifact://intent/runtime", ArtifactSHA256: intentRuntimeSHA, IntentVersion: 1,
		DefinitionComplete: true, OwnerActorID: "interactor",
		OrchestratorRegistration: "project-orchestrator",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("materialize=%+v err=%v", rep, err)
	}
	var action Action
	var workIntentID string
	action, workIntentID, err = scanWorkIntentDriverAction(st.DB.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.work_intent_id=?`, intent.ID))
	if err != nil || workIntentID != intent.ID || action.EvidenceBaselineStoreSeq != 5 ||
		action.SenderPrincipalID != store.DriverControlIdentity || action.SenderSessionID != "" {
		t.Fatalf("action=%+v intent=%s err=%v", action, workIntentID, err)
	}
	fake := NewFake()
	fake.Sessions[action.RecipientSessionID] = action.SessionTarget().Identity
	return st, action, fake, now
}

func TestWorkIntentRuntimeRoutesOnceAndAcknowledgesOnlyIndependentEvidence(t *testing.T) {
	ctx := context.Background()
	st, baseline, fake, now := seedWorkIntentRuntime(t)
	runtime := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "intent-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("delivery=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM work_intents WHERE id=(
		SELECT work_intent_id FROM work_intent_actions WHERE id=?)`, baseline.ActionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != string(workintent.StateReadyForOrchestrator) {
		t.Fatalf("transport receipt advanced stage to %s", state)
	}
	claimed, _, err := scanWorkIntentDriverAction(st.DB.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.id=?`, baseline.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	matching := providerMessageEvent(claimed, "intent-processing-evidence", claimed.RecipientSessionID,
		claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 6)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, matching)
	rep, err = runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("verification=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var actionState string
	if err := st.DB.QueryRowContext(ctx, `SELECT w.state,a.state FROM work_intents w
		JOIN work_intent_actions a ON a.work_intent_id=w.id WHERE a.id=?`, baseline.ActionID).
		Scan(&state, &actionState); err != nil {
		t.Fatal(err)
	}
	if state != string(workintent.StateOrchestrating) || actionState != "acknowledged" {
		t.Fatalf("intent/action=%s/%s", state, actionState)
	}
}

func TestWorkIntentAcknowledgementInvalidationReopensOnlyPreContractIntent(t *testing.T) {
	ctx := context.Background()
	st, baseline, fake, now := seedWorkIntentRuntime(t)
	runtime := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "intent-runtime"}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	action, _, err := scanWorkIntentDriverAction(st.DB.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.id=?`, baseline.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	matching := providerMessageEvent(action, "intent-evidence-to-invalidate", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 6)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, matching)
	if _, err := runtime.Tick(ctx, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	invalidation := observation("store-1", "intent-invalidation", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "source.events_invalidated", 7,
		`{"binding_epoch":7,"session_seq_ranges":[[6,6]],"closure_event_ids":["intent-evidence-to-invalidate"]}`)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, invalidation)
	var intentState, actionState, hold string
	if err := st.DB.QueryRow(`SELECT w.state,a.state,w.hold_kind FROM work_intents w
		JOIN work_intent_actions a ON a.work_intent_id=w.id`).Scan(&intentState, &actionState, &hold); err != nil {
		t.Fatal(err)
	}
	var alerts int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts
		WHERE kind='work_intent_ack_evidence_invalidated' AND state='pending'`).Scan(&alerts)
	if intentState != "ready_for_orchestrator" || actionState != "uncertain" ||
		hold != "orchestrator_ack_evidence_invalidated" || alerts != 1 {
		t.Fatalf("intent=%s action=%s hold=%s alerts=%d", intentState, actionState, hold, alerts)
	}
}

func TestWorkIntentRuntimeCrashUncertainNeverBlindlyResends(t *testing.T) {
	ctx := context.Background()
	st, _, fake, now := seedWorkIntentRuntime(t)
	fake.NextError = errors.New("crash after possible insertion")
	runtime := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "before-crash"}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fake.NextError = nil
	restarted := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "after-crash"}
	if _, err := restarted.Tick(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("uncertain action resent %d times", fake.SendCalls)
	}
	var state string
	if err := st.DB.QueryRow(`SELECT state FROM work_intent_actions`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "uncertain" {
		t.Fatalf("uncertain action state=%s", state)
	}
}

func TestWorkIntentRuntimeSurfacesMissingProcessingAcknowledgement(t *testing.T) {
	ctx := context.Background()
	st, _, fake, now := seedWorkIntentRuntime(t)
	runtime := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "intent-runtime",
		AcknowledgementTTL: 30 * time.Second}
	if rep, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil || rep.Delivered != 1 {
		t.Fatalf("deliver=%+v err=%v", rep, err)
	}
	rep, err := runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Held != 1 {
		t.Fatalf("overdue=%+v err=%v", rep, err)
	}
	var hold string
	var alerts int
	_ = st.DB.QueryRow(`SELECT hold_kind FROM work_intents`).Scan(&hold)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts
		WHERE kind='work_intent_orchestrator_ack_overdue' AND state='pending'`).Scan(&alerts)
	if hold != "orchestrator_ack_overdue" || alerts != 1 || fake.SendCalls != 1 {
		t.Fatalf("hold=%s alerts=%d sends=%d", hold, alerts, fake.SendCalls)
	}
}

type failingEnsurePort struct {
	*FakePort
	err error
}

func (p failingEnsurePort) EnsureSession(context.Context, SessionTarget, Action) (Identity, error) {
	return Identity{}, p.err
}

func TestWorkIntentRuntimeDeadLetterIsVisible(t *testing.T) {
	ctx := context.Background()
	st, _, fake, now := seedWorkIntentRuntime(t)
	runtime := WorkIntentRuntime{Port: failingEnsurePort{FakePort: fake, err: errors.New("driver unavailable")},
		Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true}, Evidence: SQLStageEvidence{DB: st.DB},
		Owner: "intent-runtime", MaximumTries: 1}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var actionState, hold string
	var alerts int
	_ = st.DB.QueryRow(`SELECT state FROM work_intent_actions`).Scan(&actionState)
	_ = st.DB.QueryRow(`SELECT hold_kind FROM work_intents`).Scan(&hold)
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts
		WHERE kind='work_intent_delivery_dead_letter' AND state='pending'`).Scan(&alerts)
	if actionState != "dead_letter" || hold != "orchestrator_delivery_dead_letter" || alerts != 1 || fake.SendCalls != 0 {
		t.Fatalf("action=%s hold=%s alerts=%d sends=%d", actionState, hold, alerts, fake.SendCalls)
	}
}

func TestWorkIntentRuntimeFencesReplacementBindingBeforeDriverMutation(t *testing.T) {
	ctx := context.Background()
	st, action, fake, now := seedWorkIntentRuntime(t)
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "project-orchestrator", Role: store.DriverOrchestratorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerInstanceID: "server-1",
		LifecycleKey: "orchestrator", TargetEpoch: 2, ProfileID: "orchestrator",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
		SessionID: "orchestrator-session-v2", PaneInstanceID: "orchestrator-pane-v2",
		AgentRunID: "orchestrator-run-v2",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := WorkIntentRuntime{Port: fake, Store: WorkIntentSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: SQLStageEvidence{DB: st.DB}, Owner: "intent-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Fenced != 1 || fake.SendCalls != 0 || len(fake.Grants) != 0 {
		t.Fatalf("fence=%+v sends=%d grants=%d err=%v", rep, fake.SendCalls, len(fake.Grants), err)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM work_intent_actions WHERE id=?`,
		action.ActionID).Scan(&state); err != nil || state != "dead_letter" {
		t.Fatalf("historical action state=%s err=%v", state, err)
	}
	if rep, err := st.ReconcileWorkIntents(ctx, now.Add(5*time.Minute), 10*time.Minute); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("replacement route reconcile=%+v err=%v", rep, err)
	}
	var total, pending int
	_ = st.DB.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN state='pending' THEN 1 ELSE 0 END)
		FROM work_intent_actions`).Scan(&total, &pending)
	if total != 2 || pending != 1 {
		t.Fatalf("replacement actions total=%d pending=%d", total, pending)
	}
}
