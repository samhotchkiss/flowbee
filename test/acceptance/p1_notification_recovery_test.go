package acceptance

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alertingress"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestP1DeadmanNotificationCrashRecoveryToExactInteractorEvidence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flowbee.db")
	st := openPhase2Store(t, path)
	now := time.Date(2026, 7, 20, 2, 30, 0, 0, time.UTC)
	const secret = "p1-deadman-ingress-secret"
	const key = "deadman:default:p1-recovery"
	body, err := json.Marshal(alertingress.Envelope{
		FormatVersion: alertingress.FormatVersion,
		ID:            "external-deadman-p1-recovery",
		DedupKey:      key,
		ProjectID:     "default",
		Kind:          "external_deadman",
		Payload:       json.RawMessage(`{"reason":"process_unreachable","detail":"Flowbee health endpoint was unavailable"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	bodyDigest := sha256.Sum256(body)
	bodySHA := hex.EncodeToString(bodyDigest[:])

	handler := p1IngressHandler(t, st, "default", secret, now)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, p1SignedIngressRequest(body, key, secret))
	if response.Code != http.StatusAccepted {
		t.Fatalf("initial ingress status=%d body=%s", response.Code, response.Body.String())
	}
	if err := alertingress.ValidateAcknowledgement(response.Body.Bytes(),
		response.Header().Get("X-Flowbee-Signature"), secret, key, bodySHA); err != nil {
		t.Fatalf("initial durable acknowledgement: %v", err)
	}

	// No Interactor route exists yet. The accepted alert remains durable and
	// visible; projection does not fabricate a session or silently drop it.
	projection, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(time.Second))
	if err != nil || projection.Held != 1 || projection.Projected != 0 {
		t.Fatalf("missing-route projection=%+v err=%v", projection, err)
	}
	var alertState, alertError string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,last_error FROM control_alerts WHERE dedup_key=?`, key).
		Scan(&alertState, &alertError); err != nil {
		t.Fatal(err)
	}
	if alertState != "pending" || alertError == "" {
		t.Fatalf("route-unavailable alert state=%q error=%q", alertState, alertError)
	}

	// Simulate a process crash after the signed caller received its durable ack.
	// A restarted handler must prove the exact body from SQLite and return the
	// same acknowledgement without creating another alert.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openPhase2Store(t, path)
	defer st.Close()
	handler = p1IngressHandler(t, st, "default", secret, now.Add(time.Minute))
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, p1SignedIngressRequest(body, key, secret))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("restart replay status=%d body=%s", replay.Code, replay.Body.String())
	}
	p1AssertNotificationCounts(t, st, key, 1, 0, 0)

	// Recovery establishes the exact external/default Interactor route. No raw
	// tmux identity is inferred: the durable binding carries watch, domain,
	// session, pane, and agent-run fences.
	binding := p1SeedExternalInteractor(t, st, now.Add(2*time.Minute))
	st.EnableDriverControlOrigin = true // negotiated-capability fake for this no-daemon acceptance test
	projection, err = st.ReconcileControlAlertsToInteractors(ctx, now.Add(3*time.Minute))
	if err != nil || projection.Projected != 1 || projection.Held != 0 {
		t.Fatalf("route recovery projection=%+v err=%v", projection, err)
	}
	messageProjection, err := st.ReconcileConversationMessageActions(ctx, now.Add(4*time.Minute))
	if err != nil || messageProjection.ActionsCreated != 1 || messageProjection.RoutesHeld != 0 {
		t.Fatalf("message action projection=%+v err=%v", messageProjection, err)
	}
	p1AssertNotificationCounts(t, st, key, 1, 1, 1)
	var preAction, preBinding, preInstance string
	var preActive int
	var preCursorUncertainty, preActionUncertainty uint64
	if err := st.DB.QueryRowContext(ctx, `SELECT a.state,r.state,i.state,c.active,
		c.uncertainty_epoch,a.evidence_baseline_uncertainty_epoch
		FROM conversation_message_actions a
		JOIN driver_session_bindings r ON r.binding_id=a.target_binding_id
		JOIN driver_observation_cursors c ON c.store_id=r.store_id
		JOIN driver_instances i ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id`).
		Scan(&preAction, &preBinding, &preInstance, &preActive, &preCursorUncertainty, &preActionUncertainty); err != nil {
		t.Fatal(err)
	}
	if preAction != "pending" || preBinding != "active" || preInstance != "live" || preActive != 1 ||
		preCursorUncertainty != preActionUncertainty {
		t.Fatalf("pre-delivery route action=%s binding=%s instance=%s active=%d uncertainty=%d/%d",
			preAction, preBinding, preInstance, preActive, preCursorUncertainty, preActionUncertainty)
	}
	var eligible int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_message_actions a
		JOIN driver_session_bindings r ON r.binding_id=a.target_binding_id
		LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
		JOIN driver_observation_cursors c ON c.store_id=r.store_id AND c.active=1
		JOIN driver_instances i ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE a.state='pending' AND r.state='active' AND i.state='live'
		AND (a.sender_principal_id<>'' OR s.state='active')
		AND c.uncertainty_epoch=a.evidence_baseline_uncertainty_epoch
		AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))`,
		now.Add(5*time.Minute).UTC().Format(time.RFC3339Nano)).Scan(&eligible); err != nil || eligible != 1 {
		t.Fatalf("eligible actions=%d err=%v", eligible, err)
	}

	fake := driver.NewFake()
	fake.Sessions[binding.SessionID] = driver.Identity{
		HostID: binding.HostID, StoreID: binding.StoreID,
		TmuxServerDomainID: binding.TmuxServerDomainID, TmuxServerInstanceID: binding.TmuxServerInstanceID,
		Ownership: binding.LifecycleOwnership, LifecycleKey: binding.LifecycleKey,
		TargetEpoch: binding.TargetEpoch, SessionID: binding.SessionID,
		PaneInstanceID: binding.PaneInstanceID, AgentRunID: binding.AgentRunID,
	}
	runtime := p1ConversationRuntime(st, fake)
	claimed, ok, err := runtime.Store.ClaimNext(ctx, runtime.Owner, now.Add(5*time.Minute), time.Minute, 10*time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim materialized notification action ok=%v err=%v", ok, err)
	}
	execution, err := (driver.Executor{Port: fake, Store: runtime.Store}).ExecuteClaimed(
		ctx, claimed.SessionTarget(), claimed.RouteGrant(), claimed)
	if err != nil {
		t.Fatalf("fake Driver execute: %v", err)
	}
	if err := runtime.Store.MarkDelivered(ctx, claimed, runtime.Owner, execution.Receipt, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("persist fake Driver receipt: %v", err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("fake Driver sends=%d, want 1", fake.SendCalls)
	}
	p1AssertAlertAndDeliveryState(t, st, key, "projected", "submitted", "delivered")

	// Restart after Driver's terminal-insertion receipt. The durable receipt is
	// recovered from the fake Driver, but without processing evidence the source
	// alert remains outstanding and there is no blind resend.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openPhase2Store(t, path)
	defer st.Close()
	st.EnableDriverControlOrigin = true
	runtime = p1ConversationRuntime(st, fake)
	verification, err := runtime.Tick(ctx, now.Add(6*time.Minute))
	if err != nil || verification.Verified != 0 || fake.SendCalls != 1 {
		t.Fatalf("receipt-only restart verification=%+v sends=%d err=%v", verification, fake.SendCalls, err)
	}
	p1AssertAlertAndDeliveryState(t, st, key, "projected", "submitted", "delivered")

	var payloadSHA string
	if err := st.DB.QueryRowContext(ctx, `SELECT a.payload_sha256 FROM conversation_message_actions a
		JOIN control_alert_interactor_projections p ON p.message_id=a.message_id
		JOIN control_alerts c ON c.id=p.control_alert_id WHERE c.dedup_key=?`, key).Scan(&payloadSHA); err != nil {
		t.Fatal(err)
	}
	evidence := p1InteractorProcessingObservation(binding, payloadSHA, 9)
	folded, err := (driver.ObservationSQLStore{DB: st.DB, Now: func() time.Time { return now.Add(7 * time.Minute) }}).
		Fold(ctx, "p1-external-driver", driver.ObservationBatch{
			StoreID: binding.StoreID, NextCursor: evidence.Cursor,
			DurableHighWaterCursor: evidence.Cursor, HistoryComplete: true,
			Events: []driver.Observation{evidence},
		})
	if err != nil || folded.Inserted != 1 {
		t.Fatalf("processing evidence fold=%+v err=%v", folded, err)
	}
	verification, err = runtime.Tick(ctx, now.Add(8*time.Minute))
	if err != nil || verification.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("evidence verification=%+v sends=%d err=%v", verification, fake.SendCalls, err)
	}
	p1AssertAlertAndDeliveryState(t, st, key, "acknowledged", "acknowledged", "acknowledged")

	// A final signed retry after full stage acknowledgement still resolves the
	// original ingress binding and cannot regenerate notification work.
	handler = p1IngressHandler(t, st, "default", secret, now.Add(9*time.Minute))
	finalReplay := httptest.NewRecorder()
	handler.ServeHTTP(finalReplay, p1SignedIngressRequest(body, key, secret))
	if finalReplay.Code != http.StatusAccepted {
		t.Fatalf("final replay status=%d body=%s", finalReplay.Code, finalReplay.Body.String())
	}
	if projected, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(10*time.Minute)); err != nil || projected.Projected != 0 || projected.Held != 0 {
		t.Fatalf("acknowledged replay regenerated projection=%+v err=%v", projected, err)
	}
	p1AssertNotificationCounts(t, st, key, 1, 1, 1)
}

func p1IngressHandler(t *testing.T, st *store.Store, projectID, secret string, now time.Time) http.Handler {
	t.Helper()
	handler, err := alertingress.New(alertingress.Config{Secret: secret, Acceptor: alertingress.StoreAcceptor{
		Store: st, AuthorizedProjectID: projectID, Now: func() time.Time { return now },
	}})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func p1SignedIngressRequest(body []byte, key, secret string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, alertingress.ControlAlertIngressPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func p1SeedExternalInteractor(t *testing.T, st *store.Store, now time.Time) store.DriverSessionBinding {
	t.Helper()
	ctx := context.Background()
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "default", Role: store.DriverInteractorRole, ActorID: "russ-claude",
	}, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,tmux_server_domain_id,
		 tmux_server_ownership,created_at,updated_at)
		VALUES ('p1-external-driver','host-p1','store-p1','boot-p1','live','default',
		'external_default',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-p1','p1-external-driver','tdc2.baseline',8,0,'baseline-event',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: "russ-claude", Role: store.DriverInteractorRole,
		HostID: "host-p1", StoreID: "store-p1", TmuxServerDomainID: "default",
		TmuxServerInstanceID: "server-p1", LifecycleOwnership: "external_observed",
		ExternalWatchID: "watch-russ-claude", LifecycleKey: "project:default:interactor",
		TargetEpoch: 1, ProfileID: "external-interactor", SessionID: "session-russ-claude",
		PaneInstanceID: "pane-russ-claude", AgentRunID: "run-russ-claude",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 tmux_server_domain_id,lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,?,'observing','idle',8,'tdc2.baseline','snapshot',?)`,
		binding.StoreID, binding.SessionID, binding.HostID, binding.PaneInstanceID,
		binding.AgentRunID, binding.TmuxServerInstanceID, binding.TmuxServerDomainID, stamp); err != nil {
		t.Fatal(err)
	}
	return binding
}

func p1ConversationRuntime(st *store.Store, fake *driver.FakePort) driver.ConversationRuntime {
	return driver.ConversationRuntime{
		Port:     fake,
		Store:    driver.ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: driver.ConversationStageEvidence{DB: st.DB},
		Owner:    "p1-notification-runtime",
	}
}

func p1InteractorProcessingObservation(binding store.DriverSessionBinding, payloadSHA string, seq uint64) driver.Observation {
	bindingEpoch := int64(1)
	source, _ := json.Marshal(map[string]any{
		"kind": "provider_log", "source_id": "claude-jsonl",
		"logical_record_id": "sha256:p1-logical", "ingest_occurrence_id": "sha256:p1-occurrence",
		"binding_epoch": bindingEpoch, "fidelity": "replayable", "position": map[string]any{},
		"adapter_profile": "claude-transcript/1", "rule_id": nil,
	})
	payload, _ := json.Marshal(map[string]any{
		"message_id": "provider-message-p1-evidence", "role": "user", "channel": "visible",
		"status": "completed", "content_sha256": payloadSHA,
		"content": map[string]any{"mode": "inline", "encoding": "message_parts", "parts": []any{},
			"logical_text_bytes": 0, "sha256": payloadSHA},
	})
	return driver.Observation{
		SpecVersion: "tmux-driver.events/v2", EventID: "p1-interactor-processing-evidence",
		Cursor: "tdc2.p1-evidence", StoreSeq: seq, SessionSeq: seq,
		TransitionID: "transition-p1-evidence", TransitionIndex: 0, TransitionCount: 1,
		ProducerBootID: "boot-p1", Kind: "message.completed",
		ObservedAt: "2026-07-20T02:37:00.000Z",
		Identity: driver.Identity{HostID: binding.HostID, StoreID: binding.StoreID,
			TmuxServerDomainID: binding.TmuxServerDomainID, TmuxServerInstanceID: binding.TmuxServerInstanceID,
			SessionID: binding.SessionID, PaneInstanceID: binding.PaneInstanceID,
			AgentRunID: binding.AgentRunID, StateCursor: "tdc2.p1-evidence"},
		Source: source, Correlation: json.RawMessage(`{"turn_id":"turn-p1","message_id":"message-p1","tool_call_id":null,"attention_id":null}`),
		CausedBy: []string{}, Payload: payload,
	}
}

func p1AssertNotificationCounts(t *testing.T, st *store.Store, key string, ingress, messages, actions int) {
	t.Helper()
	var gotIngress, gotAlerts, gotMessages, gotActions int
	if err := st.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM control_alert_ingress_submissions WHERE idempotency_key=?),
		(SELECT COUNT(*) FROM control_alerts WHERE dedup_key=?),
		(SELECT COUNT(*) FROM conversation_messages m JOIN control_alert_interactor_projections p ON p.message_id=m.id
		 JOIN control_alerts c ON c.id=p.control_alert_id WHERE c.dedup_key=?),
		(SELECT COUNT(*) FROM conversation_message_actions a JOIN control_alert_interactor_projections p ON p.message_id=a.message_id
		 JOIN control_alerts c ON c.id=p.control_alert_id WHERE c.dedup_key=?)`, key, key, key, key).
		Scan(&gotIngress, &gotAlerts, &gotMessages, &gotActions); err != nil {
		t.Fatal(err)
	}
	if gotIngress != ingress || gotAlerts != 1 || gotMessages != messages || gotActions != actions {
		t.Fatalf("notification counts ingress=%d alerts=%d messages=%d actions=%d; want %d/1/%d/%d",
			gotIngress, gotAlerts, gotMessages, gotActions, ingress, messages, actions)
	}
}

func p1AssertAlertAndDeliveryState(t *testing.T, st *store.Store, key, wantAlert, wantDelivery, wantAction string) {
	t.Helper()
	var alertState, deliveryState, actionState string
	if err := st.DB.QueryRow(`SELECT c.state,d.state,a.state FROM control_alerts c
		JOIN control_alert_interactor_projections p ON p.control_alert_id=c.id
		JOIN conversation_message_deliveries d ON d.message_id=p.message_id
		JOIN conversation_message_actions a ON a.message_id=p.message_id WHERE c.dedup_key=?`, key).
		Scan(&alertState, &deliveryState, &actionState); err != nil {
		t.Fatal(err)
	}
	if alertState != wantAlert || deliveryState != wantDelivery || actionState != wantAction {
		t.Fatalf("notification states alert=%s delivery=%s action=%s; want %s/%s/%s",
			alertState, deliveryState, actionState, wantAlert, wantDelivery, wantAction)
	}
}
