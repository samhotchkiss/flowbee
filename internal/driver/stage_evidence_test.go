package driver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func seedStageEvidenceHarness(t *testing.T) (SQLActionStore, ObservationSQLStore, Action, Receipt, time.Time) {
	t.Helper()
	ctx := context.Background()
	actionStore, action := seedSQLStoreEpic(t)
	action.Epoch = 0
	action.Kind = "review_wake"
	action = routedAction(action)
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := actionStore.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
		VALUES ('local-driver','host-1','store-1','boot-1','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := actionStore.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('store-1','local-driver','tdc2.baseline',5,0,'baseline-event',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := actionStore.DB.ExecContext(ctx, `INSERT INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 lifecycle,phase,last_store_seq,as_of_cursor,source,updated_at)
		VALUES ('store-1','reviewer','host-1','pane-1','run-1','server-1',
		 'observing','idle',5,'tdc2.baseline','snapshot',?)`, stamp); err != nil {
		t.Fatal(err)
	}
	if err := actionStore.CommitAction(ctx, action); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := actionStore.ClaimNextAction(ctx, "executor", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim action ok=%v err=%v", ok, err)
	}
	if claimed.EvidenceBaselineStoreSeq != 5 || claimed.EvidenceBaselineUncertaintyEpoch != 0 {
		t.Fatalf("baseline=%d/%d, want 5/0", claimed.EvidenceBaselineStoreSeq,
			claimed.EvidenceBaselineUncertaintyEpoch)
	}
	receipt := Receipt{DeliveryID: "delivery-1", ActionID: claimed.ActionID,
		GrantID: claimed.GrantID, GrantEpoch: claimed.Epoch, SenderPrincipalID: claimed.SenderPrincipalID,
		Recipient: Identity{SessionID: claimed.RecipientSessionID,
			PaneInstanceID: claimed.RecipientPaneInstanceID},
		PayloadSHA256: claimed.PayloadSHA256, Status: ReceiptSubmitted}
	return actionStore, ObservationSQLStore{DB: actionStore.DB, Now: func() time.Time { return now }}, claimed, receipt, now
}

func providerMessageEvent(action Action, eventID, sessionID, paneID, role, hash string, seq uint64) Observation {
	source := json.RawMessage(`{"kind":"provider_log","source_id":"codex-jsonl","logical_record_id":"sha256:logical","ingest_occurrence_id":"sha256:occurrence","binding_epoch":7,"fidelity":"replayable","position":{},"adapter_profile":"codex-rollout/1","rule_id":null}`)
	payload, _ := json.Marshal(map[string]any{
		"message_id": "provider-message-" + eventID,
		"role":       role, "channel": "visible", "status": "completed",
		"content_sha256": hash,
		"content": map[string]any{"mode": "inline", "encoding": "message_parts",
			"parts": []any{}, "logical_text_bytes": len(action.Payload), "sha256": hash},
	})
	return Observation{SpecVersion: "tmux-driver.events/v2", EventID: eventID,
		Cursor: "tdc2." + eventID, StoreSeq: seq, SessionSeq: seq,
		TransitionID: "transition-" + eventID, TransitionIndex: 0, TransitionCount: 1,
		ProducerBootID: "boot-1", Kind: "message.completed",
		ObservedAt: "2026-07-19T14:01:00.000Z",
		Identity: Identity{HostID: action.TargetHostID, StoreID: action.TargetStoreID,
			SessionID: sessionID, PaneInstanceID: paneID, StateCursor: "tdc2." + eventID},
		Source: source, Correlation: json.RawMessage(`{"turn_id":"turn-1","message_id":"message-1","tool_call_id":null,"attention_id":null}`),
		CausedBy: []string{}, Payload: payload}
}

func foldEvidenceEvents(t *testing.T, store ObservationSQLStore, events ...Observation) {
	t.Helper()
	last := events[len(events)-1]
	result, err := store.Fold(context.Background(), "local-driver", ObservationBatch{
		StoreID: "store-1", NextCursor: last.Cursor, DurableHighWaterCursor: last.Cursor,
		HistoryComplete: true, Events: events})
	if err != nil {
		t.Fatalf("fold evidence: %v", err)
	}
	if result.Inserted != len(events) {
		t.Fatalf("folded=%+v want inserted=%d", result, len(events))
	}
}

func TestSQLStageEvidenceRequiresExactLiveProviderUserMessageAfterBaseline(t *testing.T) {
	actionStore, observations, action, receipt, now := seedStageEvidenceHarness(t)
	evidence := SQLStageEvidence{DB: actionStore.DB, Now: func() time.Time { return now.Add(time.Minute) }}

	// Agent prose with the exact same hash is not a user-message acknowledgement.
	assistant := providerMessageEvent(action, "assistant-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "assistant", action.PayloadSHA256, 6)
	// An unrelated exact user message must remain isolated by stable session/pane.
	unrelated := providerMessageEvent(action, "unrelated-event", "other-session",
		"other-pane", "user", action.PayloadSHA256, 7)
	foldEvidenceEvents(t, observations, assistant, unrelated)
	if complete, err := evidence.AwaitStage(context.Background(), action, receipt); err != nil || complete {
		t.Fatalf("unrelated/prose evidence complete=%v err=%v", complete, err)
	}

	matching := providerMessageEvent(action, "matching-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 8)
	foldEvidenceEvents(t, observations, matching)
	if complete, err := evidence.AwaitStage(context.Background(), action, receipt); err != nil || !complete {
		t.Fatalf("matching evidence complete=%v err=%v", complete, err)
	}
	// A process crash/replay reconstructs from SQL and does not duplicate or
	// mutate the evidence link.
	restarted := SQLStageEvidence{DB: actionStore.DB, Now: func() time.Time { return now.Add(2 * time.Minute) }}
	if complete, err := restarted.AwaitStage(context.Background(), action, receipt); err != nil || !complete {
		t.Fatalf("replayed evidence complete=%v err=%v", complete, err)
	}
	var evidenceRows int
	if err := actionStore.DB.QueryRow(`SELECT COUNT(*) FROM driver_action_evidence
		WHERE action_id=? AND event_id='matching-event' AND state='confirmed'`, action.ActionID).Scan(&evidenceRows); err != nil || evidenceRows != 1 {
		t.Fatalf("evidence rows=%d err=%v", evidenceRows, err)
	}
}

func TestSQLStageEvidenceFencesStaleRecipientIncarnation(t *testing.T) {
	actionStore, observations, action, receipt, now := seedStageEvidenceHarness(t)
	matching := providerMessageEvent(action, "matching-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 6)
	foldEvidenceEvents(t, observations, matching)
	if _, err := actionStore.DB.Exec(`UPDATE driver_session_projections
		SET agent_run_id='replacement-run' WHERE store_id='store-1' AND session_id='reviewer'`); err != nil {
		t.Fatal(err)
	}
	complete, err := (SQLStageEvidence{DB: actionStore.DB, Now: func() time.Time { return now }}).
		AwaitStage(context.Background(), action, receipt)
	if complete || !errors.Is(err, ErrUncertain) {
		t.Fatalf("stale incarnation complete=%v err=%v", complete, err)
	}
	var evidenceRows int
	_ = actionStore.DB.QueryRow(`SELECT COUNT(*) FROM driver_action_evidence`).Scan(&evidenceRows)
	if evidenceRows != 0 {
		t.Fatalf("stale incarnation persisted evidence rows=%d", evidenceRows)
	}
}

func TestSQLStageEvidenceTreatsGapResetOrInvalidationAsUncertain(t *testing.T) {
	actionStore, observations, action, receipt, now := seedStageEvidenceHarness(t)
	matching := providerMessageEvent(action, "matching-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 6)
	foldEvidenceEvents(t, observations, matching)
	if _, err := actionStore.DB.Exec(`UPDATE driver_observation_cursors
		SET uncertainty_epoch=uncertainty_epoch+1 WHERE store_id='store-1'`); err != nil {
		t.Fatal(err)
	}
	complete, err := (SQLStageEvidence{DB: actionStore.DB, Now: func() time.Time { return now }}).
		AwaitStage(context.Background(), action, receipt)
	if complete || !errors.Is(err, ErrUncertain) {
		t.Fatalf("gapped evidence complete=%v err=%v", complete, err)
	}
}

func TestLateDriverInvalidationReopensAcknowledgedActionWithoutErasingAudit(t *testing.T) {
	actionStore, observations, action, receipt, now := seedStageEvidenceHarness(t)
	matching := providerMessageEvent(action, "matching-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 6)
	foldEvidenceEvents(t, observations, matching)
	evidence := SQLStageEvidence{DB: actionStore.DB, Now: func() time.Time { return now }}
	if complete, err := evidence.AwaitStage(context.Background(), action, receipt); err != nil || !complete {
		t.Fatalf("initial evidence complete=%v err=%v", complete, err)
	}
	if _, err := actionStore.DB.Exec(`UPDATE epic_actions SET state='acknowledged',
		claim_owner='',claim_deadline_at='' WHERE id=?`, action.ActionID); err != nil {
		t.Fatal(err)
	}
	invalidation := observation("store-1", "invalidation-event", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "source.events_invalidated", 7,
		`{"binding_epoch":7,"session_seq_ranges":[[6,6]],"closure_event_ids":["matching-event"]}`)
	foldEvidenceEvents(t, observations, invalidation)
	var actionState, evidenceState, lastError string
	if err := actionStore.DB.QueryRow(`SELECT state,last_error FROM epic_actions WHERE id=?`,
		action.ActionID).Scan(&actionState, &lastError); err != nil {
		t.Fatal(err)
	}
	if err := actionStore.DB.QueryRow(`SELECT state FROM driver_action_evidence WHERE action_id=?`,
		action.ActionID).Scan(&evidenceState); err != nil {
		t.Fatal(err)
	}
	if actionState != "verifying" || evidenceState != "invalidated" || lastError == "" {
		t.Fatalf("late invalidation action=%s evidence=%s error=%q", actionState, evidenceState, lastError)
	}
	complete, err := evidence.AwaitStage(context.Background(), action, receipt)
	if complete || !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalidated evidence complete=%v err=%v", complete, err)
	}
}

func TestRuntimeRestartAcknowledgesOnlyPersistedIndependentEvidence(t *testing.T) {
	actionStore, observations, action, _, now := seedStageEvidenceHarness(t)
	// Return the claimed action to pending to model normal runtime ownership.
	if _, err := actionStore.DB.Exec(`UPDATE epic_actions SET state='pending',action_epoch=0,
		claim_owner='',claim_deadline_at='',attempts=0,grant_epoch=0 WHERE id=?`, action.ActionID); err != nil {
		t.Fatal(err)
	}
	if _, err := actionStore.DB.Exec(`DELETE FROM driver_grants WHERE action_id=?`, action.ActionID); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	runtime := Runtime{Port: fake, Store: actionStore, Owner: "runtime-before-crash",
		Evidence: SQLStageEvidence{DB: actionStore.DB}}
	if rep, err := runtime.Tick(context.Background(), now.Add(time.Minute)); err != nil || rep.Delivered != 1 {
		t.Fatalf("initial delivery=%+v err=%v", rep, err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("initial sends=%d", fake.SendCalls)
	}
	matching := providerMessageEvent(action, "post-crash-message", action.RecipientSessionID,
		action.RecipientPaneInstanceID, "user", action.PayloadSHA256, 6)
	foldEvidenceEvents(t, observations, matching)
	restarted := Runtime{Port: fake, Store: actionStore, Owner: "runtime-after-crash",
		Evidence: SQLStageEvidence{DB: actionStore.DB}}
	rep, err := restarted.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || rep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("restart verification=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
}
