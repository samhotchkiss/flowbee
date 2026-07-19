package driver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ObservationSQLStore owns Flowbee's durable copy of Driver transport facts.
// driver_observation_events is append-only; only driver_session_projections is
// replaced during a gap/reset. Flowbee control_events are never touched here.
type ObservationSQLStore struct {
	DB  *sql.DB
	Now func() time.Time
}

type DriverInstanceState struct {
	InstanceRef, HostID, StoreID, ProducerBootID string
	State, Cursor, LastEventID                   string
	HighStoreSeq, ResetCount                     uint64
}

type ObservationFoldResult struct {
	Inserted, Deduplicated int
	StoreReset, CursorGap  bool
	SnapshotReplaced       bool
	CaughtUp               bool
}

func (s ObservationSQLStore) now() string {
	if s.Now != nil {
		return s.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// EnsureInstance establishes the Flowbee-owned inventory key. A changed
// store_id fences the old cursor domain and drops only its derived projections.
func (s ObservationSQLStore) EnsureInstance(ctx context.Context, instanceRef string, meta DriverMetadata) (bool, error) {
	if s.DB == nil || instanceRef == "" || meta.HostID == "" || meta.StoreID == "" || meta.ProducerBootID == "" {
		return false, errors.New("driver observation store: incomplete instance identity")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var oldHost, oldStore string
	var resetCount uint64
	err = tx.QueryRowContext(ctx, `SELECT host_id,store_id,reset_count FROM driver_instances WHERE instance_ref=?`, instanceRef).
		Scan(&oldHost, &oldStore, &resetCount)
	now := s.now()
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO driver_instances
			(instance_ref,host_id,store_id,producer_boot_id,state,created_at,updated_at)
			VALUES (?,?,?,?,'resyncing',?,?)`, instanceRef, meta.HostID, meta.StoreID, meta.ProducerBootID, now, now); err != nil {
			return false, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO driver_observation_cursors
			(store_id,instance_ref,active,updated_at) VALUES (?,?,1,?)`,
			meta.StoreID, instanceRef, now); err != nil {
			return false, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO driver_instance_events
			(instance_ref,kind,new_store_id,created_at) VALUES (?,'instance_attached',?,?)`, instanceRef, meta.StoreID, now)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err != nil {
		return false, err
	}
	if oldStore == meta.StoreID {
		if oldHost != meta.HostID {
			return false, errors.New("driver observation store: host changed without store reset")
		}
		_, err = tx.ExecContext(ctx, `UPDATE driver_instances SET producer_boot_id=?,updated_at=? WHERE instance_ref=?`,
			meta.ProducerBootID, now, instanceRef)
		if err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `UPDATE driver_observation_cursors SET active=0,reset_at=?,updated_at=?
		WHERE store_id=? AND instance_ref=?`, now, now, oldStore, instanceRef); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM driver_session_projections WHERE store_id=?`, oldStore); err != nil {
		return false, err
	}
	if err := invalidateDriverEvidenceTx(ctx, tx, oldStore, "", "driver_store_reset", now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,active,updated_at) VALUES (?,?,1,?)`, meta.StoreID, instanceRef, now); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE driver_instances SET host_id=?,store_id=?,producer_boot_id=?,
		state='resyncing',reset_count=reset_count+1,last_error='',updated_at=? WHERE instance_ref=?`,
		meta.HostID, meta.StoreID, meta.ProducerBootID, now, instanceRef); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO driver_instance_events
		(instance_ref,kind,old_store_id,new_store_id,created_at) VALUES (?,'store_reset',?,?,?)`,
		instanceRef, oldStore, meta.StoreID, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (s ObservationSQLStore) Instance(ctx context.Context, instanceRef string) (DriverInstanceState, error) {
	var out DriverInstanceState
	err := s.DB.QueryRowContext(ctx, `SELECT i.instance_ref,i.host_id,i.store_id,i.producer_boot_id,
		i.state,COALESCE(c.cursor,''),COALESCE(c.last_event_id,''),COALESCE(c.high_store_seq,0),i.reset_count
		FROM driver_instances i LEFT JOIN driver_observation_cursors c ON c.store_id=i.store_id AND c.active=1
		WHERE i.instance_ref=?`, instanceRef).Scan(&out.InstanceRef, &out.HostID, &out.StoreID,
		&out.ProducerBootID, &out.State, &out.Cursor, &out.LastEventID, &out.HighStoreSeq, &out.ResetCount)
	return out, err
}

func (s ObservationSQLStore) MarkResync(ctx context.Context, instanceRef, kind, detail string) error {
	if kind == "" {
		kind = "projection_resync"
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var storeID string
	if err := tx.QueryRowContext(ctx, `SELECT store_id FROM driver_instances WHERE instance_ref=?`, instanceRef).Scan(&storeID); err != nil {
		return err
	}
	now := s.now()
	if _, err := tx.ExecContext(ctx, `UPDATE driver_instances SET state='resyncing',last_error=?,updated_at=? WHERE instance_ref=?`, detail, now, instanceRef); err != nil {
		return err
	}
	// A cursor gap means facts between the old watermark and the replacement
	// snapshot may never be replayable.  Persist a generation fence so no action
	// created before this point can later mistake post-resync facts for a complete
	// acknowledgement history.
	if _, err := tx.ExecContext(ctx, `UPDATE driver_observation_cursors
		SET uncertainty_epoch=uncertainty_epoch+1,updated_at=?
		WHERE store_id=? AND instance_ref=? AND active=1`, now, storeID, instanceRef); err != nil {
		return err
	}
	if err := invalidateDriverEvidenceTx(ctx, tx, storeID, "", kind, now); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"detail": detail})
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_instance_events
		(instance_ref,kind,old_store_id,new_store_id,payload_json,created_at) VALUES (?,?,?,?,?,?)`,
		instanceRef, kind, storeID, storeID, string(payload), now); err != nil {
		return err
	}
	return tx.Commit()
}

// ReplaceSnapshot atomically replaces only Driver-derived state for the current
// store. The append-only Driver event ledger and Flowbee control ledger survive.
func (s ObservationSQLStore) ReplaceSnapshot(ctx context.Context, instanceRef string, snapshot SessionSnapshot) error {
	if snapshot.StoreID == "" || snapshot.HostID == "" || snapshot.AsOfCursor == "" {
		return errors.New("driver observation store: incomplete session snapshot")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var hostID, storeID string
	if err := tx.QueryRowContext(ctx, `SELECT host_id,store_id FROM driver_instances WHERE instance_ref=?`, instanceRef).Scan(&hostID, &storeID); err != nil {
		return err
	}
	if hostID != snapshot.HostID || storeID != snapshot.StoreID {
		return ErrIdentityMismatch
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM driver_session_projections WHERE store_id=?`, storeID); err != nil {
		return err
	}
	now := s.now()
	for _, session := range snapshot.Sessions {
		id := session.Identity
		if id.HostID != hostID || id.StoreID != storeID || id.SessionID == "" ||
			(session.Lifecycle != "ended" && (id.PaneInstanceID == "" || id.AgentRunID == "" || id.TmuxServerInstanceID == "")) {
			return ErrIdentityMismatch
		}
		raw := session.RawState
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO driver_session_projections
			(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
			 provider,conversation_id,lifecycle,phase,binding_status,binding_epoch,state_revision,
			 as_of_cursor,started_at,ended_at,end_reason,raw_state_json,source,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, storeID, id.SessionID, hostID,
			id.PaneInstanceID, id.AgentRunID, id.TmuxServerInstanceID, id.Provider, id.ConversationID,
			session.Lifecycle, session.Phase, session.BindingStatus, session.BindingEpoch,
			session.StateRevision, snapshot.AsOfCursor, session.StartedAt, session.EndedAt,
			session.EndReason, string(raw), "snapshot", now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_observation_cursors SET cursor=?,updated_at=?
		WHERE store_id=? AND instance_ref=? AND active=1`, snapshot.AsOfCursor, now, storeID, instanceRef); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_instances SET state='live',last_error='',updated_at=? WHERE instance_ref=?`, now, instanceRef); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_instance_events
		(instance_ref,kind,new_store_id,payload_json,created_at) VALUES (?,'projection_snapshot_replaced',?,?,?)`,
		instanceRef, storeID, fmt.Sprintf(`{"session_count":%d}`, len(snapshot.Sessions)), now); err != nil {
		return err
	}
	return tx.Commit()
}

func observationEnvelope(event Observation) ([]byte, string, error) {
	if err := ValidateObservation(event); err != nil {
		return nil, "", err
	}
	raw := event.Envelope
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(struct {
			SpecVersion     string          `json:"spec_version"`
			EventID         string          `json:"event_id"`
			StoreID         string          `json:"store_id"`
			Cursor          string          `json:"cursor"`
			StoreSeq        uint64          `json:"store_seq"`
			SessionSeq      uint64          `json:"session_seq"`
			TransitionID    string          `json:"transition_id"`
			TransitionIndex int             `json:"transition_index"`
			TransitionCount int             `json:"transition_count"`
			HostID          string          `json:"host_id"`
			SessionID       string          `json:"session_id"`
			PaneInstanceID  string          `json:"pane_instance_id"`
			ProducerBootID  string          `json:"producer_boot_id"`
			Kind            string          `json:"kind"`
			ObservedAt      string          `json:"observed_at"`
			SourceAt        any             `json:"source_at"`
			Historical      bool            `json:"historical"`
			Source          json.RawMessage `json:"source"`
			Correlation     json.RawMessage `json:"correlation"`
			CausedBy        []string        `json:"caused_by"`
			Payload         json.RawMessage `json:"payload"`
		}{event.SpecVersion, event.EventID, event.Identity.StoreID, event.Cursor, event.StoreSeq,
			event.SessionSeq, event.TransitionID, event.TransitionIndex, event.TransitionCount,
			event.Identity.HostID, event.Identity.SessionID, event.Identity.PaneInstanceID,
			event.ProducerBootID, event.Kind, event.ObservedAt, nullableString(event.SourceAt),
			event.Historical, event.Source, event.Correlation, event.CausedBy, event.Payload})
		if err != nil {
			return nil, "", err
		}
	}
	var compact json.RawMessage
	if err := json.Unmarshal(raw, &compact); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(compact)
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(canonical)
	return canonical, "sha256:" + hex.EncodeToString(h[:]), nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (s ObservationSQLStore) Fold(ctx context.Context, instanceRef string, batch ObservationBatch) (ObservationFoldResult, error) {
	var result ObservationFoldResult
	if batch.NextCursor == "" || batch.DurableHighWaterCursor == "" {
		return result, errors.New("driver observation store: incomplete event page")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var hostID, storeID string
	var high, uncertaintyEpoch uint64
	if err := tx.QueryRowContext(ctx, `SELECT i.host_id,i.store_id,COALESCE(c.high_store_seq,0),
		COALESCE(c.uncertainty_epoch,0)
		FROM driver_instances i JOIN driver_observation_cursors c ON c.store_id=i.store_id AND c.active=1
		WHERE i.instance_ref=?`, instanceRef).Scan(&hostID, &storeID, &high, &uncertaintyEpoch); err != nil {
		return result, err
	}
	if batch.StoreID == "" {
		batch.StoreID = storeID
	}
	if batch.StoreID != storeID {
		return result, ErrIdentityMismatch
	}
	now := s.now()
	lastSeq := uint64(0)
	lastEventID := ""
	needsResync := false
	uncertaintyEvents := uint64(0)
	for _, event := range batch.Events {
		if event.Identity.StoreID != storeID || event.Identity.HostID != hostID {
			return result, ErrIdentityMismatch
		}
		if lastSeq > 0 && event.StoreSeq <= lastSeq {
			return result, errors.New("driver observation store: event page is not strictly store_seq ordered")
		}
		lastSeq = event.StoreSeq
		raw, digest, err := observationEnvelope(event)
		if err != nil {
			return result, err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO driver_observation_events
			(store_id,event_id,store_seq,cursor,session_seq,transition_id,transition_index,transition_count,
			 host_id,session_id,pane_instance_id,producer_boot_id,kind,observed_at,source_at,historical,
			 envelope_sha256,envelope_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			storeID, event.EventID, event.StoreSeq, event.Cursor, event.SessionSeq, event.TransitionID,
			event.TransitionIndex, event.TransitionCount, hostID, event.Identity.SessionID,
			event.Identity.PaneInstanceID, event.ProducerBootID, event.Kind, event.ObservedAt,
			event.SourceAt, event.Historical, digest, string(raw), now)
		if err != nil {
			return result, err
		}
		inserted, _ := res.RowsAffected()
		if inserted == 0 {
			var existingDigest string
			err := tx.QueryRowContext(ctx, `SELECT envelope_sha256 FROM driver_observation_events
				WHERE store_id=? AND event_id=?`, storeID, event.EventID).Scan(&existingDigest)
			if errors.Is(err, sql.ErrNoRows) {
				return result, errors.New("driver observation store: store_seq collision")
			}
			if err != nil {
				return result, err
			}
			if existingDigest != digest {
				return result, errors.New("driver observation store: event_id replay body changed")
			}
			result.Deduplicated++
			continue
		}
		if event.StoreSeq <= high {
			return result, errors.New("driver observation store: unseen event regressed below cursor watermark")
		}
		if err := foldSessionEvent(ctx, tx, event, now); err != nil {
			return result, err
		}
		result.Inserted++
		if event.StoreSeq > high {
			high = event.StoreSeq
		}
		lastEventID = event.EventID
		if event.Kind == "source.events_invalidated" || event.Kind == "source.gap" || event.Kind == "source.reset" {
			needsResync = true
			uncertaintyEvents++
			if err := invalidateDriverEvidenceTx(ctx, tx, storeID, event.Identity.SessionID,
				event.Kind, now); err != nil {
				return result, err
			}
		}
	}
	state := "catching_up"
	result.CaughtUp = batch.NextCursor == batch.DurableHighWaterCursor
	if result.CaughtUp && !needsResync {
		state = "live"
	}
	if needsResync {
		state = "resyncing"
	}
	if lastEventID == "" {
		if err := tx.QueryRowContext(ctx, `SELECT last_event_id FROM driver_observation_cursors WHERE store_id=?`, storeID).Scan(&lastEventID); err != nil {
			return result, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_observation_cursors SET cursor=?,high_store_seq=?,
		uncertainty_epoch=?,last_event_id=?,updated_at=?
		WHERE store_id=? AND instance_ref=? AND active=1`,
		batch.NextCursor, high, uncertaintyEpoch+uncertaintyEvents, lastEventID, now, storeID, instanceRef); err != nil {
		return result, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_instances SET state=?,updated_at=? WHERE instance_ref=?`, state, now, instanceRef); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

// invalidateDriverEvidenceTx conservatively reopens a transport-processing
// acknowledgement if Driver later reports that its supporting source history
// was incomplete or invalid. Workflow truth is not rewritten; the original
// evidence row is retained and marked invalidated for audit.
func invalidateDriverEvidenceTx(ctx context.Context, tx *sql.Tx, storeID, sessionID, reason, now string) error {
	if err := invalidateConversationEvidenceTx(ctx, tx, storeID, sessionID, reason, now); err != nil {
		return err
	}
	query := `UPDATE driver_action_evidence SET state='invalidated',updated_at=?
		WHERE store_id=? AND state='confirmed'`
	args := []any{now, storeID}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	query = `UPDATE epic_actions SET state='verifying',claim_owner='',claim_deadline_at='',
		last_error=?,updated_at=? WHERE state='acknowledged' AND id IN
		(SELECT action_id FROM driver_action_evidence WHERE store_id=? AND state='invalidated'`
	args = []any{"stage_evidence_invalidated:" + reason, now, storeID}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	query += `)`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	query = `UPDATE work_intent_action_evidence SET state='invalidated',updated_at=?
		WHERE store_id=? AND state='confirmed'`
	args = []any{now, storeID}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	query = `UPDATE work_intent_actions SET state='uncertain',claim_owner='',claim_deadline_at='',
		last_error=?,next_attempt_at=?,updated_at=? WHERE state='acknowledged' AND id IN
		(SELECT action_id FROM work_intent_action_evidence WHERE store_id=? AND state='invalidated'`
	args = []any{"stage_evidence_invalidated:" + reason, now, now, storeID}
	if sessionID != "" {
		query += ` AND session_id=?`
		args = append(args, sessionID)
	}
	query += `)`
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return err
	}
	// Only the still-orchestrating pre-contract seam is reopened. A later signed
	// contract/submission/admission is independent downstream evidence and is not
	// erased by loss of the original provider-log source.
	if _, err := tx.ExecContext(ctx, `UPDATE work_intents SET state='ready_for_orchestrator',
		state_version=state_version+1,route_acknowledged_at='',
		hold_kind='orchestrator_ack_evidence_invalidated',hold_reason=?,route_due_at=?,updated_at=?
		WHERE state='orchestrating' AND epic_contract_ref='' AND delivery_action_id IN
		(SELECT action_id FROM work_intent_action_evidence WHERE store_id=? AND state='invalidated')`,
		"Driver processing evidence invalidated: "+reason, now, now, storeID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		SELECT 'intent-evidence-invalidated-'||w.id,w.project_id,NULL,
		'work_intent_ack_evidence_invalidated','work_intent_ack_evidence_invalidated:'||w.id,
		json_object('work_intent_id',w.id,'action_id',w.delivery_action_id,'reason',?),
		'pending',?,? FROM work_intents w
		WHERE w.state='ready_for_orchestrator' AND w.hold_kind='orchestrator_ack_evidence_invalidated'
		AND w.updated_at=?
		AND w.delivery_action_id IN (SELECT action_id FROM work_intent_action_evidence
		WHERE store_id=? AND state='invalidated')`, reason, now, now, now, storeID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
		SELECT w.project_id,'','work_intent_ack_evidence_invalidated','orchestrating',
		'ready_for_orchestrator',w.state_version,'driver','observation_ingestor',
		json_object('work_intent_id',w.id,'action_id',w.delivery_action_id,'reason',?),?
		FROM work_intents w WHERE w.state='ready_for_orchestrator'
		AND w.hold_kind='orchestrator_ack_evidence_invalidated'
		AND w.updated_at=?
		AND w.delivery_action_id IN (SELECT action_id FROM work_intent_action_evidence
		WHERE store_id=? AND state='invalidated')`, reason, now, now, storeID)
	return err
}

// invalidateConversationEvidenceTx preserves the immutable conversation and
// evidence audit rows while withdrawing the derived "Interactor processed it"
// projection. A source reset can therefore never leave the dashboard claiming
// a mechanically unsupported acknowledgement.
func invalidateConversationEvidenceTx(ctx context.Context, tx *sql.Tx, storeID, sessionID, reason, now string) error {
	query := `SELECT a.id,a.project_id,a.thread_id,a.message_id,d.state_version
		FROM conversation_message_action_evidence e
		JOIN conversation_message_actions a ON a.id=e.action_id
		JOIN conversation_message_deliveries d ON d.message_id=a.message_id
		WHERE e.store_id=? AND e.state='confirmed' AND a.state='acknowledged'`
	args := []any{storeID}
	if sessionID != "" {
		query += ` AND e.session_id=?`
		args = append(args, sessionID)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	type item struct {
		actionID, projectID, threadID, messageID string
		deliveryVersion                          int
	}
	var items []item
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.actionID, &i.projectID, &i.threadID, &i.messageID, &i.deliveryVersion); err != nil {
			rows.Close()
			return err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, i := range items {
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_action_evidence
			SET state='invalidated',updated_at=? WHERE action_id=? AND state='confirmed'`, now, i.actionID); err != nil {
			return err
		}
		detail := "Driver processing evidence invalidated: " + reason
		res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='uncertain',
			claim_owner='',claim_deadline_at='',last_error=?,next_attempt_at=?,updated_at=?
			WHERE id=? AND state='acknowledged'`, detail, now, now, i.actionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		res, err = tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='uncertain',
			state_version=state_version+1,last_error=?,updated_at=? WHERE message_id=?
			AND action_id=? AND state='acknowledged' AND state_version=?`, detail, now,
			i.messageID, i.actionID, i.deliveryVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrStaleActionEpoch
		}
		dedup := "conversation_ack_evidence_invalidated:" + i.messageID
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'conversation_ack_evidence_invalidated',?,
			json_object('thread_id',?,'message_id',?,'action_id',?,'reason',?),'pending',?,?)`,
			"conversation-evidence-invalidated-"+i.actionID, i.projectID, dedup, i.threadID,
			i.messageID, i.actionID, reason, now, now); err != nil {
			return err
		}
		payload := fmt.Sprintf(`{"message_id":%q,"action_id":%q,"reason":%q}`,
			i.messageID, i.actionID, reason)
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,message_id,kind,payload_json,created_at)
			VALUES (?,?,?,'delivery_changed',?,?)`, i.projectID, i.threadID, i.messageID,
			payload, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id,epic_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
			VALUES (?,'','conversation_ack_evidence_invalidated','acknowledged','uncertain',?,
			'driver','observation_ingestor',?,?)`, i.projectID, i.deliveryVersion+1, payload, now); err != nil {
			return err
		}
	}
	return nil
}

func foldSessionEvent(ctx context.Context, tx *sql.Tx, event Observation, now string) error {
	id := event.Identity
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO driver_session_projections
		(store_id,session_id,host_id,pane_instance_id,agent_run_id,tmux_server_instance_id,
		 provider,conversation_id,last_store_seq,last_event_id,as_of_cursor,source,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,'events',?)`, id.StoreID, id.SessionID, id.HostID,
		id.PaneInstanceID, id.AgentRunID, id.TmuxServerInstanceID, id.Provider, id.ConversationID,
		event.StoreSeq, event.EventID, event.Cursor, now)
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return err
	}
	stringValue := func(key string) string {
		value, _ := payload[key].(string)
		return value
	}
	intValue := func(key string) int64 {
		switch value := payload[key].(type) {
		case float64:
			return int64(value)
		case int64:
			return value
		default:
			return 0
		}
	}
	agentRun, server := stringValue("agent_run_id"), stringValue("tmux_server_instance_id")
	provider, conversation := stringValue("provider"), stringValue("conversation_id")
	lifecycle, phase := "", ""
	bindingStatus, bindingEpoch := "", int64(0)
	startedAt, endedAt, endReason := "", "", ""
	switch event.Kind {
	case "session.started":
		lifecycle, phase = stringValue("lifecycle"), stringValue("phase")
		if lifecycle == "" {
			lifecycle = "observing"
		}
		if phase == "" {
			phase = "starting"
		}
		startedAt = stringValue("started_at")
	case "session.metadata_changed":
		lifecycle, phase = stringValue("lifecycle"), stringValue("phase")
	case "source.binding_changed":
		bindingStatus, bindingEpoch = stringValue("status"), intValue("binding_epoch")
	case "phase.changed":
		phase = stringValue("phase")
		if phase == "" {
			phase = stringValue("to")
		}
	case "session.ended":
		lifecycle, phase = "ended", "exited"
		endedAt, endReason = stringValue("ended_at"), stringValue("reason")
		if endReason == "" {
			endReason = stringValue("end_reason")
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE driver_session_projections SET
		host_id=?,pane_instance_id=?,agent_run_id=COALESCE(NULLIF(?,''),agent_run_id),
		tmux_server_instance_id=COALESCE(NULLIF(?,''),tmux_server_instance_id),
		provider=COALESCE(NULLIF(?,''),provider),conversation_id=COALESCE(NULLIF(?,''),conversation_id),
		lifecycle=COALESCE(NULLIF(?,''),lifecycle),phase=COALESCE(NULLIF(?,''),phase),
		binding_status=COALESCE(NULLIF(?,''),binding_status),
		binding_epoch=CASE WHEN ?>0 THEN ? ELSE binding_epoch END,
		started_at=COALESCE(NULLIF(?,''),started_at),ended_at=COALESCE(NULLIF(?,''),ended_at),
		end_reason=COALESCE(NULLIF(?,''),end_reason),last_store_seq=?,last_event_id=?,as_of_cursor=?,
		source='events',updated_at=? WHERE store_id=? AND session_id=?`, id.HostID,
		id.PaneInstanceID, agentRun, server, provider, conversation, lifecycle, phase,
		bindingStatus, bindingEpoch, bindingEpoch, startedAt, endedAt, endReason,
		event.StoreSeq, event.EventID, event.Cursor, now, id.StoreID, id.SessionID)
	return err
}

func (s ObservationSQLStore) Session(ctx context.Context, storeID, sessionID string) (SessionProjection, error) {
	var out SessionProjection
	var raw string
	err := s.DB.QueryRowContext(ctx, `SELECT host_id,store_id,session_id,pane_instance_id,agent_run_id,
		tmux_server_instance_id,provider,conversation_id,lifecycle,phase,binding_status,binding_epoch,
		state_revision,as_of_cursor,started_at,ended_at,end_reason,raw_state_json
		FROM driver_session_projections WHERE store_id=? AND session_id=?`, storeID, sessionID).
		Scan(&out.Identity.HostID, &out.Identity.StoreID, &out.Identity.SessionID,
			&out.Identity.PaneInstanceID, &out.Identity.AgentRunID, &out.Identity.TmuxServerInstanceID,
			&out.Identity.Provider, &out.Identity.ConversationID, &out.Lifecycle, &out.Phase,
			&out.BindingStatus, &out.BindingEpoch, &out.StateRevision, &out.AsOfCursor,
			&out.StartedAt, &out.EndedAt, &out.EndReason, &raw)
	out.Identity.StateCursor = out.AsOfCursor
	out.RawState = json.RawMessage(raw)
	return out, err
}

// IsCurrentIdentity is the fail-closed join used by later routing code. It never
// falls back to CWD, raw pane IDs, tmux names, PIDs, provider, or timestamps.
func (s ObservationSQLStore) IsCurrentIdentity(ctx context.Context, instanceRef string, identity Identity) (bool, error) {
	if identity.HostID == "" || identity.StoreID == "" || identity.SessionID == "" ||
		identity.PaneInstanceID == "" || identity.AgentRunID == "" || identity.TmuxServerInstanceID == "" {
		return false, nil
	}
	var count int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_instances i
		JOIN driver_session_projections s ON s.store_id=i.store_id
		WHERE i.instance_ref=? AND i.state='live' AND i.host_id=? AND i.store_id=?
		  AND s.session_id=? AND s.pane_instance_id=? AND s.agent_run_id=?
		  AND s.tmux_server_instance_id=? AND s.lifecycle<>'ended'`, instanceRef, identity.HostID,
		identity.StoreID, identity.SessionID, identity.PaneInstanceID, identity.AgentRunID,
		identity.TmuxServerInstanceID).Scan(&count)
	return count == 1, err
}

type ObservationIngestor struct {
	InstanceRef string
	Port        DriverPort
	Store       ObservationSQLStore
}

// Tick performs one bounded REST ingestion pass. It snapshots on first contact,
// retention gap, invalidation, or store reset; then resumes exclusive replay from
// the snapshot cursor. The stored cursor is scoped only to the current store_id.
func (i ObservationIngestor) Tick(ctx context.Context) (ObservationFoldResult, error) {
	var result ObservationFoldResult
	if i.InstanceRef == "" || i.Port == nil || i.Store.DB == nil {
		return result, errors.New("driver observation ingestor: incomplete dependencies")
	}
	meta, err := i.Port.Metadata(ctx)
	if err != nil {
		return result, err
	}
	reset, err := i.Store.EnsureInstance(ctx, i.InstanceRef, meta)
	if err != nil {
		return result, err
	}
	result.StoreReset = reset
	instance, err := i.Store.Instance(ctx, i.InstanceRef)
	if err != nil {
		return result, err
	}
	if instance.State == "resyncing" || instance.Cursor == "" {
		snapshot, err := i.Port.SnapshotSessions(ctx)
		if err != nil {
			return result, err
		}
		if snapshot.HostID != meta.HostID || snapshot.StoreID != meta.StoreID {
			return result, ErrIdentityMismatch
		}
		if err := i.Store.ReplaceSnapshot(ctx, i.InstanceRef, snapshot); err != nil {
			return result, err
		}
		result.SnapshotReplaced = true
		instance.Cursor = snapshot.AsOfCursor
	}
	batch, err := i.Port.Observe(ctx, instance.Cursor)
	if err != nil {
		return result, err
	}
	if batch.CursorGap {
		result.CursorGap = true
		if err := i.Store.MarkResync(ctx, i.InstanceRef, "cursor_gap_resync", "driver replay cursor expired"); err != nil {
			return result, err
		}
		snapshot, err := i.Port.SnapshotSessions(ctx)
		if err != nil {
			return result, err
		}
		if snapshot.HostID != meta.HostID || snapshot.StoreID != meta.StoreID {
			return result, ErrIdentityMismatch
		}
		if err := i.Store.ReplaceSnapshot(ctx, i.InstanceRef, snapshot); err != nil {
			return result, err
		}
		result.SnapshotReplaced = true
		return result, nil
	}
	if batch.StoreID == "" {
		batch.StoreID = meta.StoreID
	}
	folded, err := i.Store.Fold(ctx, i.InstanceRef, batch)
	if err != nil {
		return result, err
	}
	result.Inserted += folded.Inserted
	result.Deduplicated += folded.Deduplicated
	result.CaughtUp = folded.CaughtUp
	return result, nil
}
