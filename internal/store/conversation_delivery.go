package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var ErrConversationInteractorRouteUnavailable = errors.New("exact project Interactor Driver route is unavailable")

type ConversationDeliveryReconcileReport struct {
	ActionsCreated int
	RoutesHeld     int
}

// ReconcileConversationMessageActions materializes the durable transport
// action for each human message and linked Flowbee system alert. The message
// already is durable intent; absence of a live exact Driver route is therefore
// a visible hold, never a dropped in-memory handoff.
func (s *Store) ReconcileConversationMessageActions(ctx context.Context, now time.Time) (ConversationDeliveryReconcileReport, error) {
	var out ConversationDeliveryReconcileReport
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT m.project_id,m.thread_id,m.id
			FROM conversation_messages m
			JOIN conversation_message_deliveries d ON d.message_id=m.id
			WHERE (m.role='human' OR (m.role='system' AND EXISTS
			       (SELECT 1 FROM control_alert_interactor_projections p WHERE p.message_id=m.id)))
			  AND m.stream_state='complete'
			  AND d.state IN ('pending','failed')
			  AND NOT EXISTS (SELECT 1 FROM conversation_message_actions a
			    WHERE a.message_id=m.id AND a.state<>'fenced')
			ORDER BY m.created_at,m.id`)
		if err != nil {
			return err
		}
		type candidate struct{ projectID, threadID, messageID string }
		var candidates []candidate
		for rows.Next() {
			var item candidate
			if err := rows.Scan(&item.projectID, &item.threadID, &item.messageID); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range candidates {
			created, err := s.materializeConversationMessageActionTx(ctx, tx, item.projectID, item.threadID, item.messageID, now)
			if err == nil {
				if created {
					out.ActionsCreated++
				}
				continue
			}
			if !errors.Is(err, ErrConversationInteractorRouteUnavailable) &&
				!errors.Is(err, ErrDriverControlOriginUnavailable) {
				return err
			}
			out.RoutesHeld++
			if err := holdConversationRouteTx(ctx, tx, item.projectID, item.threadID, item.messageID, err.Error(), now); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

func (s *Store) materializeConversationMessageActionTx(ctx context.Context, tx *sql.Tx, projectID, threadID, messageID string, now time.Time) (bool, error) {
	var actorID, content, contentHash, streamState, messageRole, threadActor, threadBinding string
	err := tx.QueryRowContext(ctx, `SELECT m.actor_id,m.content_text,m.content_sha256,m.stream_state,
		m.role,t.interactor_actor_id,t.interactor_binding_id
		FROM conversation_messages m JOIN conversation_threads t ON t.id=m.thread_id
		WHERE m.project_id=? AND m.thread_id=? AND m.id=?
		  AND (m.role='human' OR (m.role='system' AND EXISTS
		      (SELECT 1 FROM control_alert_interactor_projections p WHERE p.message_id=m.id)))
		  AND t.state='active'`, projectID, threadID, messageID).
		Scan(&actorID, &content, &contentHash, &streamState, &messageRole, &threadActor, &threadBinding)
	if err != nil {
		return false, err
	}
	if streamState != "complete" || content == "" {
		return false, fmt.Errorf("%w: only complete text messages can be routed", ErrConversationInteractorRouteUnavailable)
	}
	digest := sha256.Sum256([]byte(content))
	payloadHash := "sha256:" + hex.EncodeToString(digest[:])
	if payloadHash != contentHash {
		return false, errors.New("conversation immutable message hash is corrupt")
	}
	var recipient DriverSessionBinding
	if messageRole == "system" {
		recipient, err = exactActiveDriverSessionBindingTx(ctx, tx, projectID, threadActor,
			DriverInteractorRole, threadBinding)
	} else {
		recipient, err = activeDriverSessionBindingTx(ctx, tx, projectID, threadActor, DriverInteractorRole)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: project Interactor binding missing", ErrConversationInteractorRouteUnavailable)
	}
	if err != nil {
		return false, err
	}
	if !s.HasDriverControlOriginForBinding(recipient) {
		return false, fmt.Errorf("%w: Interactor endpoint %s/%s/%s is not control-origin ready",
			ErrDriverControlOriginUnavailable, recipient.HostID, recipient.StoreID, recipient.TmuxServerDomainID)
	}
	var baselineSeq, uncertaintyEpoch uint64
	var instanceState string
	err = tx.QueryRowContext(ctx, `SELECT c.high_store_seq,c.uncertainty_epoch,i.state
		FROM driver_observation_cursors c JOIN driver_instances i
		  ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE c.store_id=? AND c.active=1 AND i.host_id=?`, recipient.StoreID, recipient.HostID).
		Scan(&baselineSeq, &uncertaintyEpoch, &instanceState)
	if errors.Is(err, sql.ErrNoRows) || err == nil && instanceState != "live" {
		return false, fmt.Errorf("%w: Interactor observation store is not live", ErrConversationInteractorRouteUnavailable)
	}
	if err != nil {
		return false, err
	}
	dedup := fmt.Sprintf("conversation-message:%s:deliver:%s:%s", messageID, DriverControlIdentity, recipient.BindingID)
	actionID := "conversation-action-" + stableID(dedup)
	grantID := stableUUID("driver-conversation-message-grant/v1", dedup)
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversation_message_actions
		(id,project_id,thread_id,message_id,kind,state,action_epoch,dedup_key,payload_text,
		 payload_sha256,target_actor_id,sender_principal_id,sender_binding_id,target_binding_id,
		 evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,grant_id,created_at,updated_at)
		VALUES (?,?,?,?,'deliver_to_interactor','pending',0,?,?,?,?,?,NULL,?,?,?,?,?,?)`, actionID,
		projectID, threadID, messageID, dedup, content, payloadHash, threadActor,
		DriverControlIdentity, recipient.BindingID, baselineSeq, uncertaintyEpoch, grantID, stamp, stamp)
	if err != nil {
		return false, err
	}
	created, _ := res.RowsAffected()
	var gotHash, gotPrincipal, gotSender, gotTarget string
	if err := tx.QueryRowContext(ctx, `SELECT payload_sha256,sender_principal_id,
		COALESCE(sender_binding_id,''),target_binding_id
		FROM conversation_message_actions WHERE dedup_key=?`, dedup).
		Scan(&gotHash, &gotPrincipal, &gotSender, &gotTarget); err != nil {
		return false, err
	}
	if gotHash != payloadHash || gotPrincipal != DriverControlIdentity || gotSender != "" || gotTarget != recipient.BindingID {
		return false, ErrConversationIdempotencyConflict
	}
	if created == 1 {
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT state_version FROM conversation_message_deliveries
			WHERE message_id=?`, messageID).Scan(&version); err != nil {
			return false, err
		}
		res, err = tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='routing',
			state_version=state_version+1,action_id=?,receipt_ref='',last_error='',updated_at=?
			WHERE message_id=? AND state IN ('pending','failed') AND state_version=?`, actionID, stamp, messageID, version)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, ErrConversationStale
		}
		payload := mustConversationJSON(map[string]any{"message_id": messageID, "action_id": actionID,
			"payload_sha256": payloadHash, "sender_principal_id": DriverControlIdentity,
			"target_binding_id": recipient.BindingID})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,message_id,kind,payload_json,created_at)
			VALUES (?,?,?,'delivery_changed',?,?)`, projectID, threadID, messageID, payload, stamp); err != nil {
			return false, err
		}
		if err := appendConversationControlEventTx(ctx, tx, projectID,
			"conversation_delivery_action_committed", "pending", "routing", version+1,
			"conversation_delivery", payload, now); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
		acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
		stamp, stamp, "conversation_interactor_route_unavailable:"+messageID)
	return created == 1, err
}

func holdConversationRouteTx(ctx context.Context, tx *sql.Tx, projectID, threadID, messageID, reason string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	dedup := "conversation_interactor_route_unavailable:" + messageID
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		SELECT ?,?,NULL,'conversation_interactor_route_unavailable',?,
		json_object('thread_id',?,'message_id',?,'reason',?),'pending',?,?
		WHERE NOT EXISTS (SELECT 1 FROM control_alert_interactor_projections p WHERE p.message_id=?)`,
		"conversation-route-"+stableID(dedup), projectID, dedup, threadID, messageID, reason, stamp, stamp,
		messageID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET last_error=?,updated_at=?
		WHERE message_id=? AND state IN ('pending','failed')`, reason, stamp, messageID)
	return err
}

func exactActiveDriverSessionBindingTx(ctx context.Context, tx *sql.Tx, projectID, workerIdentity, role, bindingID string) (DriverSessionBinding, error) {
	return activeDriverSessionBindingRow(tx.QueryRowContext(ctx, `SELECT
		binding_id,project_id,worker_identity,role,seat_id,binding_epoch,host_id,store_id,
		tmux_server_domain_id,tmux_server_instance_id,lifecycle_ownership,external_watch_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		conversation_id,observed_at
		FROM driver_session_bindings WHERE project_id=? AND worker_identity=? AND role=?
		  AND binding_id=? AND state='active'`, projectID, workerIdentity, role, bindingID))
}
