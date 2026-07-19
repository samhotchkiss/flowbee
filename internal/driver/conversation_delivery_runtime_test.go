package driver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func openConversationRuntimeStore(t *testing.T, dsn string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	st.EnableDriverControlOrigin = true // future-capability fake transport
	return st
}

func seedConversationRuntime(t *testing.T, dsn string) (*store.Store, Action, *FakePort, time.Time, string) {
	t.Helper()
	ctx := context.Background()
	st := openConversationRuntimeStore(t, dsn)
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
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
	interactor, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerInstanceID: "server-1",
		LifecycleKey: "project-interactor", TargetEpoch: 1, ProfileID: "interactor",
		WorkspaceRootID: "root", WorkspaceRelativePath: "project",
		SessionID: "interactor-session", PaneInstanceID: "interactor-pane", AgentRunID: "interactor-run",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES ('store-1',?,?,?,?,?,'observing','idle',5,'tdc2.baseline','snapshot',?)`,
		interactor.SessionID, interactor.HostID, interactor.PaneInstanceID,
		interactor.AgentRunID, interactor.TmuxServerInstanceID, stamp); err != nil {
		t.Fatal(err)
	}
	thread, err := st.CreateConversationThread(ctx, store.CreateConversationThreadInput{
		ID: "conversation-runtime", ProjectID: "default", ConversationKey: "primary",
		Title: "Default project", InteractorActorID: "interactor:default",
		InteractorBindingID: interactor.BindingID, InteractorIncarnationID: interactor.AgentRunID,
		IdempotencyKey: "conversation-runtime-create",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	message, err := st.AppendConversationMessage(ctx, store.AppendConversationMessageInput{
		ID: "conversation-message-runtime", ProjectID: "default", ThreadID: thread.ID,
		Role: "human", ActorID: "human:sam", ContentText: "Keep Flowbee moving",
		IdempotencyKey: "conversation-message-runtime-create",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileConversationMessageActions(ctx, now.Add(2*time.Minute))
	if err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("materialize=%+v err=%v", rep, err)
	}
	action, _, gotMessage, err := scanConversationDriverAction(st.DB.QueryRowContext(ctx,
		conversationActionSelect+` WHERE a.message_id=?`, message.ID))
	if err != nil || gotMessage != message.ID || action.PayloadSHA256 != message.ContentSHA256 ||
		action.EvidenceBaselineStoreSeq != 5 || action.SenderPrincipalID != store.DriverControlIdentity ||
		action.SenderSessionID != "" {
		t.Fatalf("action=%+v message=%s err=%v", action, gotMessage, err)
	}
	fake := NewFake()
	fake.Sessions[action.RecipientSessionID] = action.SessionTarget().Identity
	return st, action, fake, now, message.ID
}

func TestConversationRuntimeRoutesOnceAndRequiresSeparateProcessingEvidence(t *testing.T) {
	ctx := context.Background()
	st, baseline, fake, now, messageID := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "conversation-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("delivery=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var deliveryState, actionState string
	if err := st.DB.QueryRow(`SELECT d.state,a.state FROM conversation_message_deliveries d
		JOIN conversation_message_actions a ON a.id=d.action_id WHERE d.message_id=?`, messageID).
		Scan(&deliveryState, &actionState); err != nil {
		t.Fatal(err)
	}
	if deliveryState != "submitted" || actionState != "delivered" {
		t.Fatalf("transport incorrectly completed stage: delivery=%s action=%s", deliveryState, actionState)
	}
	claimed, _, _, err := scanConversationDriverAction(st.DB.QueryRowContext(ctx,
		conversationActionSelect+` WHERE a.id=?`, baseline.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	matching := providerMessageEvent(claimed, "conversation-processing-evidence",
		claimed.RecipientSessionID, claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 6)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, matching)
	rep, err = runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("verification=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	if err := st.DB.QueryRow(`SELECT d.state,a.state FROM conversation_message_deliveries d
		JOIN conversation_message_actions a ON a.id=d.action_id WHERE d.message_id=?`, messageID).
		Scan(&deliveryState, &actionState); err != nil {
		t.Fatal(err)
	}
	if deliveryState != "acknowledged" || actionState != "acknowledged" {
		t.Fatalf("ack states=%s/%s", deliveryState, actionState)
	}
}

func TestConversationRuntimeRestartAfterClaimNeverBlindlySends(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flowbee.db")
	st, _, fake, now, _ := seedConversationRuntime(t, path)
	storeBefore := ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true}
	if _, ok, err := storeBefore.ClaimNext(ctx, "process-before-crash", now.Add(3*time.Minute),
		30*time.Second, 10*time.Minute); err != nil || !ok {
		t.Fatalf("pre-crash claim ok=%v err=%v", ok, err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openConversationRuntimeStore(t, path)
	defer st.Close()
	restarted := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "process-after-crash"}
	rep, err := restarted.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Reclaimed != 1 {
		t.Fatalf("restart=%+v err=%v", rep, err)
	}
	if fake.SendCalls != 0 || len(fake.Grants) != 0 {
		t.Fatalf("expired uncertain claim blindly mutated Driver sends=%d grants=%d", fake.SendCalls, len(fake.Grants))
	}
	var actionState, deliveryState string
	if err := st.DB.QueryRow(`SELECT a.state,d.state FROM conversation_message_actions a
		JOIN conversation_message_deliveries d ON d.action_id=a.id`).Scan(&actionState, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if actionState != "uncertain" || deliveryState != "uncertain" {
		t.Fatalf("restart states=%s/%s", actionState, deliveryState)
	}
}

func TestConversationRuntimeSendUncertaintyNeverResends(t *testing.T) {
	ctx := context.Background()
	st, _, fake, now, _ := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	fake.NextError = errors.New("crash after possible terminal insertion")
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "before-crash"}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	fake.NextError = nil
	restarted := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "after-crash"}
	if _, err := restarted.Tick(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("uncertain action resent %d times", fake.SendCalls)
	}
}

func TestConversationRuntimeFencesReplacementBeforeDriverMutation(t *testing.T) {
	ctx := context.Background()
	st, old, fake, now, messageID := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerInstanceID: "server-1",
		LifecycleKey: "project-interactor", TargetEpoch: 2, ProfileID: "interactor",
		WorkspaceRootID: "root", WorkspaceRelativePath: "project",
		SessionID: "interactor-session-v2", PaneInstanceID: "interactor-pane-v2",
		AgentRunID: "interactor-run-v2",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "conversation-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(4*time.Minute))
	if err != nil || rep.Fenced != 1 || fake.SendCalls != 0 || len(fake.Grants) != 0 {
		t.Fatalf("fence=%+v sends=%d grants=%d err=%v", rep, fake.SendCalls, len(fake.Grants), err)
	}
	var oldState string
	if err := st.DB.QueryRow(`SELECT state FROM conversation_message_actions WHERE id=?`, old.ActionID).Scan(&oldState); err != nil {
		t.Fatal(err)
	}
	if oldState != "fenced" {
		t.Fatalf("old action state=%s", oldState)
	}
	if rep, err := st.ReconcileConversationMessageActions(ctx, now.Add(5*time.Minute)); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("replacement materialization=%+v err=%v", rep, err)
	}
	var total, pending int
	if err := st.DB.QueryRow(`SELECT COUNT(*),SUM(CASE WHEN state='pending' THEN 1 ELSE 0 END)
		FROM conversation_message_actions WHERE message_id=?`, messageID).Scan(&total, &pending); err != nil {
		t.Fatal(err)
	}
	if total != 2 || pending != 1 {
		t.Fatalf("replacement actions total=%d pending=%d", total, pending)
	}
}

func TestConversationRuntimeStoreResetFencesUnsentAction(t *testing.T) {
	ctx := context.Background()
	st, _, fake, now, _ := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	if _, err := st.DB.Exec(`UPDATE driver_observation_cursors SET uncertainty_epoch=1
		WHERE store_id='store-1'`); err != nil {
		t.Fatal(err)
	}
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "conversation-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(3*time.Minute))
	if err != nil || rep.Fenced != 1 || fake.SendCalls != 0 {
		t.Fatalf("reset fence=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
}

func TestConversationRuntimeNeverAcceptsOldInteractorEvidenceAfterReplacement(t *testing.T) {
	ctx := context.Background()
	st, baseline, fake, now, messageID := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "conversation-runtime"}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, _, _, err := scanConversationDriverAction(st.DB.QueryRowContext(ctx,
		conversationActionSelect+` WHERE a.id=?`, baseline.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		WorkerIdentity: "interactor:default", Role: store.DriverInteractorRole,
		HostID: "host-1", StoreID: "store-1", TmuxServerInstanceID: "server-1",
		LifecycleKey: "project-interactor", TargetEpoch: 2, ProfileID: "interactor",
		WorkspaceRootID: "root", WorkspaceRelativePath: "project",
		SessionID: "interactor-session-v2", PaneInstanceID: "interactor-pane-v2",
		AgentRunID: "interactor-run-v2",
	}, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	oldEvidence := providerMessageEvent(claimed, "old-interactor-evidence",
		claimed.RecipientSessionID, claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 6)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, oldEvidence)
	if _, err := runtime.Tick(ctx, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("expected fail-closed hold, got err=%v", err)
	}
	var actionState, deliveryState string
	if err := st.DB.QueryRow(`SELECT a.state,d.state FROM conversation_message_actions a
		JOIN conversation_message_deliveries d ON d.action_id=a.id WHERE a.message_id=?`, messageID).
		Scan(&actionState, &deliveryState); err != nil {
		t.Fatal(err)
	}
	if actionState != "delivered" || deliveryState != "submitted" || fake.SendCalls != 1 {
		t.Fatalf("old authority accepted or resent: action=%s delivery=%s sends=%d",
			actionState, deliveryState, fake.SendCalls)
	}
}

func TestConversationLateEvidenceInvalidationWithdrawsOnlyDerivedAcknowledgement(t *testing.T) {
	ctx := context.Background()
	st, baseline, fake, now, messageID := seedConversationRuntime(t, ":memory:")
	defer st.Close()
	runtime := ConversationRuntime{Port: fake, Store: ConversationSQLStore{DB: st.DB, ControlOriginAvailable: true},
		Evidence: ConversationStageEvidence{DB: st.DB}, Owner: "conversation-runtime"}
	if _, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, _, _, err := scanConversationDriverAction(st.DB.QueryRowContext(ctx,
		conversationActionSelect+` WHERE a.id=?`, baseline.ActionID))
	if err != nil {
		t.Fatal(err)
	}
	matching := providerMessageEvent(claimed, "conversation-evidence-invalidated",
		claimed.RecipientSessionID, claimed.RecipientPaneInstanceID, "user", claimed.PayloadSHA256, 6)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, matching)
	if _, err := runtime.Tick(ctx, now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	invalidation := observation("store-1", "conversation-source-invalidation",
		claimed.RecipientSessionID, claimed.RecipientPaneInstanceID, "source.events_invalidated", 7,
		`{"binding_epoch":7,"session_seq_ranges":[[6,6]],"closure_event_ids":["conversation-evidence-invalidated"]}`)
	foldEvidenceEvents(t, ObservationSQLStore{DB: st.DB}, invalidation)
	var actionState, deliveryState, evidenceState string
	if err := st.DB.QueryRow(`SELECT a.state,d.state,e.state FROM conversation_message_actions a
		JOIN conversation_message_deliveries d ON d.action_id=a.id
		JOIN conversation_message_action_evidence e ON e.action_id=a.id
		WHERE a.message_id=?`, messageID).Scan(&actionState, &deliveryState, &evidenceState); err != nil {
		t.Fatal(err)
	}
	var alerts int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts
		WHERE kind='conversation_ack_evidence_invalidated' AND state='pending'`).Scan(&alerts)
	if actionState != "uncertain" || deliveryState != "uncertain" || evidenceState != "invalidated" || alerts != 1 {
		t.Fatalf("invalidation action=%s delivery=%s evidence=%s alerts=%d",
			actionState, deliveryState, evidenceState, alerts)
	}
	var messageCount int
	_ = st.DB.QueryRow(`SELECT COUNT(*) FROM conversation_messages WHERE id=?`, messageID).Scan(&messageCount)
	if messageCount != 1 {
		t.Fatalf("immutable message truth was erased: %d", messageCount)
	}
}
