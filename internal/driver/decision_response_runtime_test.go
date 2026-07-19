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

const decisionRuntimeSHA = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

func seedDecisionResponseRuntime(t *testing.T) (*store.Store, Action, string, *FakePort, time.Time) {
	t.Helper()
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableDriverControlOrigin = true // future-capability fake transport
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('local-driver','host-1','store-1','boot-1','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-1','local-driver','tdc2.baseline',10,0,'baseline-event',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	interactor, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1",
		LifecycleOwnership: "driver_managed",
		LifecycleKey:       "interactor-default", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
		SessionID: "interactor-session", PaneInstanceID: "interactor-pane", AgentRunID: "interactor-run",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	insertDecisionProjection(t, st, interactor, 10, stamp)
	req, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "runtime-decision", ProjectID: "default", Kind: workintent.DecisionQuestion,
		Title: "Choose target", Prompt: "Which target?", ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseAnswer},
		RequestedBy: "interactor:default", RouteTo: "human:sam", SubjectArtifactRef: "artifact://decision/runtime",
		SubjectVersion: 1, SubjectSHA256: decisionRuntimeSHA,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: req.ID, RequestVersion: 1, SubjectVersion: 1, SubjectSHA256: decisionRuntimeSHA,
		Kind: workintent.ResponseAnswer, StructuredValue: []byte(`{"target":"stable"}`),
		Comment: "Use the stable target", ActorID: "human:sam", AuthorizationScope: "project:default",
		IdempotencyKey: "answer-runtime",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	action, responseID, err := scanDecisionResponseDriverAction(st.DB.QueryRowContext(ctx,
		decisionResponseActionSelect+` WHERE a.response_id=?`, resp.ID))
	if err != nil || responseID != resp.ID || action.EvidenceBaselineStoreSeq != 10 ||
		action.SenderPrincipalID != store.DriverControlIdentity || action.SenderSessionID != "" {
		t.Fatalf("action=%+v response=%s err=%v", action, responseID, err)
	}
	fake := NewFake()
	fake.Sessions[action.RecipientSessionID] = action.SessionTarget().Identity
	return st, action, resp.ID, fake, now
}

func insertDecisionProjection(t *testing.T, st *store.Store, binding store.DriverSessionBinding, seq int, stamp string) {
	t.Helper()
	if _, err := st.DB.Exec(`INSERT OR REPLACE INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,'observing','idle',?,'tdc2.baseline','snapshot',?)`,
		binding.StoreID, binding.SessionID, binding.HostID, binding.PaneInstanceID,
		binding.AgentRunID, binding.TmuxServerInstanceID, seq, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestDecisionResponseCrashAfterCommitIsRecoveredThroughDriverOnce(t *testing.T) {
	ctx := context.Background()
	st, baseline, responseID, fake, now := seedDecisionResponseRuntime(t)
	// The response transaction committed the immutable action before a runtime
	// existed. This is the production crash seam: restart must drain, not rely on
	// the dashboard request handler's process memory.
	var actionState, payloadHash string
	if err := st.DB.QueryRow(`SELECT state,payload_sha256 FROM decision_response_actions WHERE response_id=?`, responseID).
		Scan(&actionState, &payloadHash); err != nil {
		t.Fatal(err)
	}
	if actionState != "pending" || payloadHash != baseline.PayloadSHA256 {
		t.Fatalf("committed action=%s hash=%s want=%s", actionState, payloadHash, baseline.PayloadSHA256)
	}
	restarted := DecisionResponseRuntime{Port: fake, Store: DecisionResponseSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: DecisionResponseStageEvidence{DB: st.DB}, Owner: "after-crash"}
	rep, err := restarted.Tick(ctx, now.Add(2*time.Minute))
	if err != nil || rep.Delivered != 1 || fake.SendCalls != 1 || len(fake.Grants) != 1 {
		t.Fatalf("delivery=%+v sends=%d grants=%d err=%v", rep, fake.SendCalls, len(fake.Grants), err)
	}
	// A submitted receipt is only terminal insertion evidence, not processing.
	rep, err = restarted.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Verified != 0 || fake.SendCalls != 1 {
		t.Fatalf("pre-evidence verification=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	claimed, _, err := scanDecisionResponseDriverAction(st.DB.QueryRowContext(ctx,
		decisionResponseActionSelect+` WHERE a.response_id=?`, responseID))
	if err != nil {
		t.Fatal(err)
	}
	event := providerMessageEvent(claimed, "decision-processing-evidence", claimed.RecipientSessionID,
		claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 11)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, event)
	rep, err = restarted.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("ack=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	if err := st.DB.QueryRow(`SELECT state FROM decision_response_actions WHERE response_id=?`, responseID).Scan(&actionState); err != nil || actionState != "acknowledged" {
		t.Fatalf("action state=%s err=%v", actionState, err)
	}
}

func TestDecisionResponseUncertainDeliveryNeverBlindlyResends(t *testing.T) {
	ctx := context.Background()
	st, _, _, fake, now := seedDecisionResponseRuntime(t)
	fake.NextError = errors.New("crash while Driver was delivering")
	runtime := DecisionResponseRuntime{Port: fake, Store: DecisionResponseSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: DecisionResponseStageEvidence{DB: st.DB}, Owner: "runtime"}
	if _, err := runtime.Tick(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("initial sends=%d", fake.SendCalls)
	}
	fake.NextError = nil
	if _, err := runtime.Tick(ctx, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("uncertain action was blindly resent: sends=%d", fake.SendCalls)
	}
	var state string
	if err := st.DB.QueryRow(`SELECT state FROM decision_response_actions`).Scan(&state); err != nil || state != "uncertain" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestDecisionResponseReplacementBindingFencesOldRouteBeforeSend(t *testing.T) {
	ctx := context.Background()
	st, old, responseID, fake, now := seedDecisionResponseRuntime(t)
	replacement, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1",
		LifecycleOwnership: "driver_managed",
		LifecycleKey:       "interactor-default", TargetEpoch: 2, ProfileID: "interactor",
		WorkspaceRootID: "workspace-root", WorkspaceRelativePath: "repo",
		SessionID: "interactor-session-2", PaneInstanceID: "interactor-pane-2", AgentRunID: "interactor-run-2",
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	insertDecisionProjection(t, st, replacement, 10, now.Add(2*time.Minute).Format(time.RFC3339Nano))
	fake.Sessions = map[string]Identity{replacement.SessionID: {
		HostID: replacement.HostID, StoreID: replacement.StoreID,
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: replacement.TmuxServerInstanceID, LifecycleKey: replacement.LifecycleKey,
		TargetEpoch: replacement.TargetEpoch, SessionID: replacement.SessionID,
		PaneInstanceID: replacement.PaneInstanceID, AgentRunID: replacement.AgentRunID,
	}}
	runtime := DecisionResponseRuntime{Port: fake, Store: DecisionResponseSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Domain: st, Evidence: DecisionResponseStageEvidence{DB: st.DB}, Owner: "runtime"}
	rep, err := runtime.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Fenced != 1 || rep.Materialized != 1 || rep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("replacement=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var fenced, pending int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FILTER (WHERE state='fenced'),
		COUNT(*) FILTER (WHERE state='delivered') FROM decision_response_actions WHERE response_id=?`, responseID).
		Scan(&fenced, &pending); err != nil {
		t.Fatal(err)
	}
	if fenced != 1 || pending != 1 {
		t.Fatalf("fenced/delivered=%d/%d", fenced, pending)
	}
	if _, ok := fake.Receipts[old.ActionID]; ok {
		t.Fatal("old exact route reached Driver")
	}
}

func TestDecisionResponseMissingAckSurfacesDurableAlert(t *testing.T) {
	ctx := context.Background()
	st, _, responseID, fake, now := seedDecisionResponseRuntime(t)
	runtime := DecisionResponseRuntime{Port: fake, Store: DecisionResponseSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: DecisionResponseStageEvidence{DB: st.DB}, Owner: "runtime", AcknowledgementTTL: time.Minute}
	if _, err := runtime.Tick(ctx, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rep, err := runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Held != 1 {
		t.Fatalf("overdue=%+v err=%v", rep, err)
	}
	var alerts int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts
		WHERE dedup_key=? AND state='pending'`, "decision_response_ack_overdue:"+responseID).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("alerts=%d err=%v", alerts, err)
	}
}
