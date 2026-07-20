package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

const controlAlertInteractorRetryDelay = 30 * time.Second

type ControlAlertInteractorProjectionReport struct {
	Projected int
	Held      int
}

type controlAlertInteractorCandidate struct {
	id, projectID, epicID, kind, dedupKey, payload string
}

// ReconcileControlAlertsToInteractors turns each durable pending control alert
// into one immutable system message addressed to the project's exact current
// Interactor route. The source alert moves to projected, not acknowledged. The
// migration's evidence trigger advances it to acknowledged only after the
// existing conversation delivery projector records separate processing
// evidence for the exact action epoch.
func (s *Store) ReconcileControlAlertsToInteractors(ctx context.Context, now time.Time) (ControlAlertInteractorProjectionReport, error) {
	var report ControlAlertInteractorProjectionReport
	err := s.tx(ctx, func(tx *sql.Tx) error {
		stamp := now.UTC().Format(rfc3339)
		rows, err := tx.QueryContext(ctx, `SELECT a.id,a.project_id,COALESCE(a.epic_id,''),
			a.kind,a.dedup_key,a.payload_json
			FROM control_alerts a
			WHERE a.state='pending'
			  AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
			  AND NOT EXISTS (SELECT 1 FROM control_alert_interactor_projections p
			                  WHERE p.control_alert_id=a.id)
			ORDER BY a.created_at,a.id`, stamp)
		if err != nil {
			return err
		}
		var candidates []controlAlertInteractorCandidate
		for rows.Next() {
			var candidate controlAlertInteractorCandidate
			if err := rows.Scan(&candidate.id, &candidate.projectID, &candidate.epicID,
				&candidate.kind, &candidate.dedupKey, &candidate.payload); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, candidate := range candidates {
			projected, err := s.projectControlAlertToInteractorTx(ctx, tx, candidate, now)
			if err == nil {
				if projected {
					report.Projected++
				}
				continue
			}
			if !errors.Is(err, ErrConversationInteractorRouteUnavailable) &&
				!errors.Is(err, ErrDriverControlOriginUnavailable) {
				return err
			}
			report.Held++
			if _, holdErr := tx.ExecContext(ctx, `UPDATE control_alerts
				SET last_error=?,next_attempt_at=?,updated_at=?
				WHERE id=? AND state='pending'`, err.Error(),
				now.Add(controlAlertInteractorRetryDelay).UTC().Format(rfc3339), stamp, candidate.id); holdErr != nil {
				return holdErr
			}
		}
		return nil
	})
	return report, err
}

func (s *Store) projectControlAlertToInteractorTx(ctx context.Context, tx *sql.Tx,
	candidate controlAlertInteractorCandidate, now time.Time) (bool, error) {
	var actorID, actorState string
	var routeVersion int
	err := tx.QueryRowContext(ctx, `SELECT actor_id,state,state_version FROM project_actor_routes
		WHERE project_id=? AND role=?`, candidate.projectID, DriverInteractorRole).
		Scan(&actorID, &actorState, &routeVersion)
	if errors.Is(err, sql.ErrNoRows) || err == nil && actorState != "active" {
		return false, fmt.Errorf("%w: project has no active Interactor actor route", ErrConversationInteractorRouteUnavailable)
	}
	if err != nil {
		return false, err
	}
	recipient, err := activeDriverSessionBindingTx(ctx, tx, candidate.projectID, actorID, DriverInteractorRole)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("%w: current project Interactor binding missing", ErrConversationInteractorRouteUnavailable)
	}
	if err != nil {
		return false, err
	}

	threadKey := "flowbee-alerts:v1:" + stableID(fmt.Sprintf("%s\x00%s\x00%d\x00%s",
		candidate.projectID, actorID, routeVersion, recipient.BindingID))
	threadID := "conversation-alert-thread-" + stableID(threadKey)
	messageID := "conversation-alert-message-" + stableID(candidate.id)
	stamp := now.UTC().Format(rfc3339)

	var existingThreadID, existingActor, existingBinding, existingRun string
	err = tx.QueryRowContext(ctx, `SELECT id,interactor_actor_id,interactor_binding_id,
		interactor_incarnation_id FROM conversation_threads
		WHERE project_id=? AND conversation_key=?`, candidate.projectID, threadKey).
		Scan(&existingThreadID, &existingActor, &existingBinding, &existingRun)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_threads
			(id,project_id,conversation_key,title,interactor_actor_id,interactor_binding_id,
			 interactor_incarnation_id,state,state_version,focus_kind,focus_ref,
			 focus_artifact_sha256,last_message_seq,creation_idempotency_key,created_at,updated_at)
			VALUES (?,?,?,'Flowbee control alerts',?,?,?,'active',1,'project',?,'',0,?,?,?)`,
			threadID, candidate.projectID, threadKey, actorID, recipient.BindingID,
			recipient.AgentRunID, candidate.projectID, threadKey, stamp, stamp)
		if err != nil {
			return false, err
		}
		payload := mustConversationJSON(map[string]any{"thread_id": threadID,
			"conversation_key": threadKey, "focus_kind": ConversationFocusProject,
			"focus_ref": candidate.projectID, "system_alerts": true})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,kind,payload_json,created_at)
			VALUES (?,?,'thread_created',?,?)`, candidate.projectID, threadID, payload, stamp); err != nil {
			return false, err
		}
		if err := appendConversationControlEventTx(ctx, tx, candidate.projectID,
			"conversation_thread_created", "", "active", 1, DriverControlIdentity, payload, now); err != nil {
			return false, err
		}
	case err != nil:
		return false, err
	default:
		if existingActor != actorID || existingBinding != recipient.BindingID || existingRun != recipient.AgentRunID {
			return false, ErrConversationIdempotencyConflict
		}
		threadID = existingThreadID
	}

	content := formatControlAlertInteractorMessage(candidate)
	contentHash := conversationTextSHA256(content)
	var lastSeq int64
	if err := tx.QueryRowContext(ctx, `SELECT last_message_seq FROM conversation_threads
		WHERE id=? AND project_id=? AND state='active'`, threadID, candidate.projectID).Scan(&lastSeq); err != nil {
		return false, err
	}
	messageSeq := lastSeq + 1
	messageKey := "control-alert:" + candidate.id
	res, err := tx.ExecContext(ctx, `INSERT INTO conversation_messages
		(id,project_id,thread_id,thread_seq,role,actor_id,agent_incarnation_id,
		 reply_to_message_id,content_text,content_artifact_ref,content_sha256,
		 stream_state,idempotency_key,created_at)
		VALUES (?,?,?,?, 'system',?,'',NULL,?,'',?,'complete',?,?)`, messageID,
		candidate.projectID, threadID, messageSeq, DriverControlIdentity, content, contentHash,
		messageKey, stamp)
	if err != nil {
		return false, err
	}
	created, _ := res.RowsAffected()
	if created != 1 {
		return false, errors.New("control alert message insert did not create exactly one row")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_message_deliveries
		(message_id,project_id,thread_id,state,state_version,updated_at)
		VALUES (?,?,?,'pending',1,?)`, messageID, candidate.projectID, threadID, stamp); err != nil {
		return false, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE conversation_threads SET last_message_seq=?,updated_at=?
		WHERE id=? AND project_id=? AND last_message_seq=?`, messageSeq, stamp,
		threadID, candidate.projectID, lastSeq)
	if err != nil {
		return false, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return false, ErrConversationStale
	}
	payload := mustConversationJSON(map[string]any{"thread_id": threadID, "message_id": messageID,
		"thread_seq": messageSeq, "role": "system", "content_sha256": contentHash,
		"stream_state": "complete", "delivery_state": "pending", "control_alert_id": candidate.id})
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
		(project_id,thread_id,message_id,kind,payload_json,created_at)
		VALUES (?,?,?,'message_appended',?,?)`, candidate.projectID, threadID, messageID, payload, stamp); err != nil {
		return false, err
	}
	if err := appendConversationControlEventTx(ctx, tx, candidate.projectID,
		"conversation_message_appended", "", "pending", int(messageSeq), DriverControlIdentity, payload, now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_alert_interactor_projections
		(control_alert_id,project_id,project_actor_id,project_actor_route_version,
		 target_binding_id,target_binding_epoch,thread_id,message_id,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, candidate.id, candidate.projectID, actorID, routeVersion,
		recipient.BindingID, recipient.BindingEpoch, threadID, messageID, stamp); err != nil {
		return false, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE control_alerts SET state='projected',
		claim_owner='',claim_deadline_at='',next_attempt_at='',last_error='',updated_at=?
		WHERE id=? AND state='pending'`, stamp, candidate.id)
	if err != nil {
		return false, err
	}
	if changed, _ := res.RowsAffected(); changed != 1 {
		return false, errors.New("control alert projection lost source state fence")
	}
	return true, nil
}

func formatControlAlertInteractorMessage(candidate controlAlertInteractorCandidate) string {
	content := fmt.Sprintf("FLOWBEE ALERT — %s\nProject: %s\nAlert: %s\nDedup: %s\nDetails: %s",
		candidate.kind, candidate.projectID, candidate.id, candidate.dedupKey, candidate.payload)
	if len(content) <= maxConversationContent {
		return content
	}
	suffix := "\n[details truncated to the conversation message limit]"
	limit := maxConversationContent - len(suffix)
	content = content[:limit]
	for !utf8.ValidString(content) {
		content = content[:len(content)-1]
	}
	return content + suffix
}

func conversationTextSHA256(content string) string {
	digest := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(digest[:])
}
