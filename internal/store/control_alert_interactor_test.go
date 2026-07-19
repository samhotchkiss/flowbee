package store_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestControlAlertProjectionHoldsThenCommitsExactlyOnceToCurrentInteractor(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	insertControlAlert(t, st, "alert-route", "default", "review_dispatch_stalled", "incident:4950", `{"pr":4950}`, now)

	report, err := st.ReconcileControlAlertsToInteractors(ctx, now)
	if err != nil || report.Held != 1 || report.Projected != 0 {
		t.Fatalf("missing route report=%+v err=%v", report, err)
	}
	var state, lastError string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,last_error FROM control_alerts WHERE id='alert-route'`).
		Scan(&state, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || !strings.Contains(lastError, "no active Interactor") {
		t.Fatalf("held alert state=%q error=%q", state, lastError)
	}

	binding := seedAlertInteractorRoute(t, st, "interactor:default", 1, now.Add(time.Second))
	report, err = st.ReconcileControlAlertsToInteractors(ctx, now.Add(time.Minute))
	if err != nil || report.Projected != 1 || report.Held != 0 {
		t.Fatalf("route recovery report=%+v err=%v", report, err)
	}
	var role, actor, deliveryState, targetBinding string
	if err := st.DB.QueryRowContext(ctx, `SELECT m.role,m.actor_id,d.state,p.target_binding_id
		FROM control_alert_interactor_projections p
		JOIN conversation_messages m ON m.id=p.message_id
		JOIN conversation_message_deliveries d ON d.message_id=m.id
		WHERE p.control_alert_id='alert-route'`).Scan(&role, &actor, &deliveryState, &targetBinding); err != nil {
		t.Fatal(err)
	}
	if role != "system" || actor != store.DriverControlIdentity || deliveryState != "pending" || targetBinding != binding.BindingID {
		t.Fatalf("projection role=%q actor=%q delivery=%q binding=%q", role, actor, deliveryState, targetBinding)
	}
	if delivery, err := st.ReconcileConversationMessageActions(ctx, now.Add(90*time.Second)); err != nil ||
		delivery.RoutesHeld != 1 || delivery.ActionsCreated != 0 {
		t.Fatalf("unready endpoint did not visibly hold projected system alert: report=%+v err=%v", delivery, err)
	}
	var totalAlerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts`).Scan(&totalAlerts); err != nil || totalAlerts != 1 {
		t.Fatalf("system alert route hold recursively alerted: alerts=%d err=%v", totalAlerts, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='alert-route'`).Scan(&state); err != nil || state != "projected" {
		t.Fatalf("source state=%q err=%v", state, err)
	}
	if report, err = st.ReconcileControlAlertsToInteractors(ctx, now.Add(3*time.Minute)); err != nil || report.Projected != 0 || report.Held != 0 {
		t.Fatalf("lost-response replay report=%+v err=%v", report, err)
	}
	var threads, messages, links int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM conversation_threads WHERE conversation_key LIKE 'flowbee-alerts:v1:%'),
		(SELECT COUNT(*) FROM conversation_messages WHERE id LIKE 'conversation-alert-message-%'),
		(SELECT COUNT(*) FROM control_alert_interactor_projections)`).Scan(&threads, &messages, &links); err != nil {
		t.Fatal(err)
	}
	if threads != 1 || messages != 1 || links != 1 {
		t.Fatalf("replay duplicated projection threads=%d messages=%d links=%d", threads, messages, links)
	}
}

func TestOnlyExactInteractorEvidenceCanAcknowledgeControlAlert(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableDriverControlOrigin = true
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	binding := seedAlertInteractorRoute(t, st, "interactor:default", 1, now)
	insertControlAlert(t, st, "alert-evidence", "default", "ci_infra_incident", "ci:infra:1", `{"check":"build"}`, now)
	if report, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(time.Second)); err != nil || report.Projected != 1 {
		t.Fatalf("projection=%+v err=%v", report, err)
	}
	if report, err := st.ReconcileConversationMessageActions(ctx, now.Add(2*time.Second)); err != nil || report.ActionsCreated != 1 {
		t.Fatalf("action projection=%+v err=%v", report, err)
	}

	var actionID, messageID string
	var actionEpoch int
	if err := st.DB.QueryRowContext(ctx, `SELECT a.id,a.action_epoch,a.message_id
		FROM conversation_message_actions a
		JOIN control_alert_interactor_projections p ON p.message_id=a.message_id
		WHERE p.control_alert_id='alert-evidence'`).Scan(&actionID, &actionEpoch, &messageID); err != nil {
		t.Fatal(err)
	}
	stamp := now.Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_message_actions
		SET state='acknowledged',receipt_ref='driver-receipt-only',acknowledged_at=?,updated_at=?
		WHERE id=?`, stamp, stamp, actionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_message_deliveries
		SET state='submitted',receipt_ref='driver-receipt-only',updated_at=? WHERE message_id=?`, stamp, messageID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='alert-evidence'`).Scan(&state); err != nil || state != "projected" {
		t.Fatalf("receipt incorrectly cleared alert: state=%q err=%v", state, err)
	}
	assertProjectedAlertCountsAsOutstandingOperations(t, st, 1)
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_message_deliveries
		SET state='acknowledged',updated_at=? WHERE message_id=?`, stamp, messageID); err == nil {
		t.Fatal("delivery acknowledged without separate Interactor processing evidence")
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='alert-evidence'`).Scan(&state); err != nil || state != "projected" {
		t.Fatalf("failed evidence fence cleared alert: state=%q err=%v", state, err)
	}

	evidenceStamp := now.Add(4 * time.Second).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_events
		(store_id,event_id,store_seq,cursor,session_seq,transition_id,transition_index,
		 transition_count,host_id,session_id,pane_instance_id,producer_boot_id,kind,
		 observed_at,envelope_sha256,envelope_json)
		VALUES (?,?,9,'cursor-9',1,'transition-9',0,1,?,?,?,'boot-1','provider_user_message',?,?,'{}')`,
		binding.StoreID, "event-alert-evidence", binding.HostID, binding.SessionID,
		binding.PaneInstanceID, evidenceStamp,
		"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO conversation_message_action_evidence
		(action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
		 agent_run_id,evidence_kind,payload_sha256,state,created_at,updated_at)
		VALUES (?,?,?,?,9,?,?,?,'provider_user_message',?,'confirmed',?,?)`, actionID,
		actionEpoch, binding.StoreID, "event-alert-evidence", binding.SessionID,
		binding.PaneInstanceID, binding.AgentRunID,
		"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		evidenceStamp, evidenceStamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_message_deliveries
		SET state='acknowledged',updated_at=? WHERE message_id=?`, evidenceStamp, messageID); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='alert-evidence'`).Scan(&state); err != nil || state != "acknowledged" {
		t.Fatalf("confirmed evidence did not clear alert: state=%q err=%v", state, err)
	}
	assertProjectedAlertCountsAsOutstandingOperations(t, st, 0)
	var outstanding int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE id='alert-evidence' AND state IN ('pending','delivering','projected')`).Scan(&outstanding); err != nil || outstanding != 0 {
		t.Fatalf("acknowledged alert still outstanding=%d err=%v", outstanding, err)
	}
}

func TestControlAlertProjectionSurvivesRestartAndActorReplacementDoesNotRetarget(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flowbee.db")
	st := openControlAlertProjectionStore(t, path)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	firstBinding := seedAlertInteractorRoute(t, st, "interactor:v1", 1, now)
	insertControlAlert(t, st, "alert-v1", "default", "review_dispatch_stalled", "route:v1", `{}`, now)
	if report, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(time.Second)); err != nil || report.Projected != 1 {
		t.Fatalf("first projection=%+v err=%v", report, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = openControlAlertProjectionStore(t, path)
	defer st.Close()
	if report, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(time.Minute)); err != nil || report.Projected != 0 {
		t.Fatalf("restart replay=%+v err=%v", report, err)
	}

	secondBinding := seedAlertInteractorRoute(t, st, "interactor:v2", 2, now.Add(2*time.Minute))
	insertControlAlert(t, st, "alert-v2", "default", "ci_red", "route:v2", `{}`, now.Add(2*time.Minute))
	if report, err := st.ReconcileControlAlertsToInteractors(ctx, now.Add(3*time.Minute)); err != nil || report.Projected != 1 {
		t.Fatalf("replacement projection=%+v err=%v", report, err)
	}
	rows, err := st.DB.QueryContext(ctx, `SELECT control_alert_id,target_binding_id,thread_id
		FROM control_alert_interactor_projections ORDER BY control_alert_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string][2]string{}
	for rows.Next() {
		var alertID, bindingID, threadID string
		if err := rows.Scan(&alertID, &bindingID, &threadID); err != nil {
			t.Fatal(err)
		}
		got[alertID] = [2]string{bindingID, threadID}
	}
	if got["alert-v1"][0] != firstBinding.BindingID || got["alert-v2"][0] != secondBinding.BindingID ||
		got["alert-v1"][1] == got["alert-v2"][1] {
		t.Fatalf("actor replacement retargeted committed message: got=%v", got)
	}
	var messages int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversation_messages
		WHERE id LIKE 'conversation-alert-message-%'`).Scan(&messages); err != nil || messages != 2 {
		t.Fatalf("restart/replacement message count=%d err=%v", messages, err)
	}
}

func seedAlertInteractorRoute(t *testing.T, st *store.Store, actorID string, targetEpoch int64, now time.Time) store.DriverSessionBinding {
	t.Helper()
	ctx := context.Background()
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{
		ProjectID: "default", Role: store.DriverInteractorRole, ActorID: actorID,
	}, now); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(actorID, ":", "-")
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES (?,?,?,?,? ,?,?) ON CONFLICT(instance_ref) DO UPDATE SET state='live',updated_at=excluded.updated_at`,
		"driver-"+suffix, "host-"+suffix, "store-"+suffix, "boot-1", "live", stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,active,updated_at)
		VALUES (?,?,?,8,0,1,?) ON CONFLICT(store_id) DO UPDATE SET active=1,updated_at=excluded.updated_at`,
		"store-"+suffix, "driver-"+suffix, "cursor-8", stamp); err != nil {
		t.Fatal(err)
	}
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: actorID, Role: store.DriverInteractorRole,
		HostID: "host-" + suffix, StoreID: "store-" + suffix,
		TmuxServerDomainID: "default", TmuxServerInstanceID: "server-" + suffix,
		LifecycleOwnership: "external_observed", ExternalWatchID: "watch-" + suffix,
		LifecycleKey: "interactor-" + suffix, TargetEpoch: targetEpoch, ProfileID: "external-interactor",
		SessionID:      "session-" + suffix,
		PaneInstanceID: "pane-" + suffix, AgentRunID: "run-" + suffix,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func insertControlAlert(t *testing.T, st *store.Store, id, projectID, kind, dedup, payload string, now time.Time) {
	t.Helper()
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(context.Background(), `INSERT INTO control_alerts
		(id,project_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,?,?,?,'pending',?,?)`, id, projectID, kind, dedup, payload, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func openControlAlertProjectionStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	return st
}

func assertProjectedAlertCountsAsOutstandingOperations(t *testing.T, st *store.Store, want int) {
	t.Helper()
	requirements, err := st.CapacityPoolDemand(context.Background(), "codex", "grok", "grok")
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range requirements {
		if requirement.ProjectID == "default" && requirement.Pool == "operations" {
			if requirement.QueuedWork != want {
				t.Fatalf("operations outstanding=%d, want %d", requirement.QueuedWork, want)
			}
			return
		}
	}
	t.Fatal("default operations capacity demand missing")
}
