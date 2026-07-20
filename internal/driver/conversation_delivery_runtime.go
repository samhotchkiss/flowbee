package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ConversationSQLStore is the Flowbee-owned durable outbox for dashboard
// human -> project Interactor messages. It contains no terminal transport code.
type ConversationSQLStore struct {
	DB                        *sql.DB
	Now                       func() time.Time
	ControlOriginAvailable    bool
	ControlOriginGate         func() bool
	EndpointControlOriginGate func(EndpointKey) bool
}

func (s ConversationSQLStore) controlOriginAvailableFor(a Action) bool {
	if s.EndpointControlOriginGate != nil {
		return s.EndpointControlOriginGate(EndpointKey{HostID: a.TargetHostID, StoreID: a.TargetStoreID,
			TmuxServerDomainID: a.TargetServerDomainID})
	}
	return s.controlOriginAvailable()
}

func (s ConversationSQLStore) controlOriginAvailable() bool {
	if s.ControlOriginGate != nil {
		return s.ControlOriginGate()
	}
	return s.ControlOriginAvailable
}

func (s ConversationSQLStore) CommitAction(context.Context, Action) error {
	return errors.New("conversation Driver actions must be committed before runtime claim")
}

func (s ConversationSQLStore) PersistReceipt(ctx context.Context, a Action, r Receipt) error {
	return (SQLActionStore{DB: s.DB, Now: s.Now}).PersistReceipt(ctx, a, r)
}

const conversationActionSelect = `SELECT
	a.id,a.project_id,a.thread_id,a.message_id,a.kind,a.action_epoch,a.dedup_key,
	a.payload_text,a.payload_sha256,a.evidence_baseline_store_seq,
	a.evidence_baseline_uncertainty_epoch,a.grant_id,a.grant_epoch,a.grant_expires_at,
	a.sender_principal_id,COALESCE(s.host_id,''),COALESCE(s.store_id,''),
	COALESCE(s.tmux_server_domain_id,''),COALESCE(s.tmux_server_instance_id,''),
	COALESCE(s.session_id,''),COALESCE(s.agent_run_id,''),
	r.host_id,r.store_id,r.tmux_server_domain_id,r.tmux_server_instance_id,r.lifecycle_ownership,
	r.lifecycle_key,r.target_epoch,r.profile_id,r.external_watch_id,r.workspace_root_id,r.workspace_relative_path,r.session_id,
	r.pane_instance_id,r.agent_run_id
	FROM conversation_message_actions a
	LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
	JOIN driver_session_bindings r ON r.binding_id=a.target_binding_id`

func scanConversationDriverAction(row interface{ Scan(...any) error }) (Action, string, string, error) {
	var a Action
	var threadID, messageID string
	var senderHost, senderStore, senderDomain, senderServer string
	err := row.Scan(&a.ActionID, &a.ProjectID, &threadID, &messageID, &a.Kind, &a.Epoch,
		&a.DedupKey, &a.Payload, &a.PayloadSHA256, &a.EvidenceBaselineStoreSeq,
		&a.EvidenceBaselineUncertaintyEpoch, &a.GrantID, &a.GrantEpoch,
		&a.GrantExpiresAt, &a.SenderPrincipalID, &senderHost, &senderStore, &senderDomain, &senderServer,
		&a.SenderSessionID, &a.SenderAgentRunID, &a.TargetHostID, &a.TargetStoreID,
		&a.TargetServerDomainID, &a.TargetServerID, &a.TargetLifecycleOwnership,
		&a.LifecycleKey, &a.TargetEpoch, &a.ProfileID, &a.ExternalWatchID,
		&a.WorkspaceRootID, &a.WorkspaceRelativePath, &a.RecipientSessionID,
		&a.RecipientPaneInstanceID, &a.RecipientAgentRunID)
	if err != nil {
		return Action{}, "", "", err
	}
	if a.SenderPrincipalID == "" && (senderHost == "" || senderStore == "" || senderDomain == "" || senderServer == "") {
		return Action{}, "", "", ErrIdentityMismatch
	}
	a.SenderHostID, a.SenderStoreID = senderHost, senderStore
	a.SenderServerDomainID, a.SenderServerID = senderDomain, senderServer
	a.ExecutorKind = "driver"
	a.TargetRole = "interactor"
	a.LeaseID = "conversation-message:" + messageID
	a.LeaseEpoch = int64(max(1, a.Epoch))
	return a, threadID, messageID, nil
}

// FenceStaleRoutes only rehomes actions known not to have reached Driver.
// Claimed/delivered/uncertain actions may have mutated the terminal and are
// deliberately left for receipt/evidence reconciliation.
func (s ConversationSQLStore) FenceStaleRoutes(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.project_id,a.thread_id,a.message_id,d.state_version
		FROM conversation_message_actions a
		JOIN conversation_message_deliveries d ON d.message_id=a.message_id
		LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
		LEFT JOIN driver_session_bindings r ON r.binding_id=a.target_binding_id
		LEFT JOIN driver_observation_cursors c ON c.store_id=r.store_id AND c.active=1
		LEFT JOIN driver_instances i ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE a.state='pending' AND ((a.sender_principal_id='' AND (s.state IS NULL OR s.state<>'active'))
		  OR r.state IS NULL OR r.state<>'active' OR c.store_id IS NULL OR i.state<>'live'
		  OR c.uncertainty_epoch<>a.evidence_baseline_uncertainty_epoch)
		ORDER BY a.created_at,a.id`)
	if err != nil {
		return 0, err
	}
	type item struct {
		actionID, projectID, threadID, messageID string
		version                                  int
	}
	var items []item
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.actionID, &i.projectID, &i.threadID, &i.messageID, &i.version); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var fenced int64
	for _, item := range items {
		res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='fenced',
			last_error='driver_binding_or_store_superseded',updated_at=? WHERE id=? AND state='pending'`,
			stamp, item.actionID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		fenced++
		res, err = tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='pending',
			state_version=state_version+1,action_id='',receipt_ref='',last_error=
			'driver binding or observation store superseded before send',updated_at=?
			WHERE message_id=? AND action_id=? AND state_version=?`, stamp, item.messageID,
			item.actionID, item.version)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			payload := fmt.Sprintf(`{"message_id":%q,"action_id":%q,"reason":"driver_binding_or_store_superseded"}`,
				item.messageID, item.actionID)
			if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
				(project_id,thread_id,message_id,kind,payload_json,created_at)
				VALUES (?,?,?,'delivery_changed',?,?)`, item.projectID, item.threadID,
				item.messageID, payload, stamp); err != nil {
				return 0, err
			}
		}
		dedup := "conversation_delivery_route_stale:" + item.messageID
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'conversation_delivery_route_stale',?,
			json_object('thread_id',?,'message_id',?,'action_id',?),'pending',?,?)`,
			"conversation-route-stale-"+item.actionID, item.projectID, dedup, item.threadID,
			item.messageID, item.actionID, stamp, stamp); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return fenced, nil
}

func (s ConversationSQLStore) ClaimNext(ctx context.Context, owner string, now time.Time, claimTTL, ackTTL time.Duration) (Action, bool, error) {
	if s.DB == nil || owner == "" {
		return Action{}, false, errors.New("conversation Driver claim requires database and owner")
	}
	if claimTTL <= 0 {
		claimTTL = time.Minute
	}
	if ackTTL <= 0 {
		ackTTL = 10 * time.Minute
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	a, _, messageID, err := scanConversationDriverAction(tx.QueryRowContext(ctx,
		conversationActionSelect+` JOIN driver_observation_cursors c ON c.store_id=r.store_id AND c.active=1
		JOIN driver_instances i ON i.instance_ref=c.instance_ref AND i.store_id=c.store_id
		WHERE a.state='pending' AND r.state='active' AND i.state='live'
		AND (a.sender_principal_id<>'' OR s.state='active')
		AND c.uncertainty_epoch=a.evidence_baseline_uncertainty_epoch
		AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		ORDER BY a.created_at,a.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	if !s.controlOriginAvailableFor(a) {
		return Action{}, false, nil
	}
	nextEpoch := a.Epoch + 1
	expires := now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	grantID := driverGrantUUID(a.ActionID, nextEpoch)
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='claimed',
		action_epoch=?,grant_id=?,grant_epoch=?,grant_expires_at=?,claim_owner=?,claim_deadline_at=?,
		delivery_started_at=?,acknowledgement_due_at=?,attempts=attempts+1,updated_at=?
		WHERE id=? AND state='pending' AND action_epoch=?`, nextEpoch, grantID, nextEpoch,
		expires, owner, now.Add(claimTTL).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), now.Add(ackTTL).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), a.ActionID, a.Epoch)
	if err != nil {
		return Action{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Action{}, false, nil
	}
	a.Epoch, a.GrantEpoch, a.GrantID, a.GrantExpiresAt = nextEpoch, nextEpoch, grantID, expires
	a.LeaseEpoch = nextEpoch
	if a.SenderPrincipalID == "" && a.SenderSessionID == a.RecipientSessionID {
		return Action{}, false, ErrGrantDenied
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_grants SET revoked_at=?
		WHERE action_id=? AND grant_epoch<? AND revoked_at=''`, now.UTC().Format(time.RFC3339Nano),
		a.ActionID, a.GrantEpoch); err != nil {
		return Action{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_grants
		(grant_id,project_id,action_id,sender_session_id,sender_agent_run_id,sender_principal_id,
		 recipient_session_id,recipient_pane_instance_id,expected_recipient_agent_run_id,grant_epoch,
		 maximum_payload_bytes,allow_draft_stash,issued_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,65536,0,?,?)`, grantID, a.ProjectID, a.ActionID,
		a.SenderSessionID, a.SenderAgentRunID, a.SenderPrincipalID, a.RecipientSessionID,
		a.RecipientPaneInstanceID, controlRecipientRunFence(a), nextEpoch,
		now.UTC().Format(time.RFC3339Nano), expires); err != nil {
		return Action{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET
		last_error='',updated_at=? WHERE message_id=? AND action_id=?`,
		now.UTC().Format(time.RFC3339Nano), messageID, a.ActionID); err != nil {
		return Action{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Action{}, false, err
	}
	return a, true, nil
}

func (s ConversationSQLStore) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT id,message_id FROM conversation_message_actions
		WHERE state='claimed' AND claim_deadline_at<>'' AND julianday(claim_deadline_at)<=julianday(?)`, stamp)
	if err != nil {
		return 0, err
	}
	type item struct{ actionID, messageID string }
	var items []item
	for rows.Next() {
		var item item
		if err := rows.Scan(&item.actionID, &item.messageID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var reclaimed int64
	for _, item := range items {
		res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='uncertain',
			claim_owner='',claim_deadline_at='',last_error='claim expired; reconcile receipt/evidence before retry',
			next_attempt_at=?,updated_at=? WHERE id=? AND state='claimed'`,
			now.Add(time.Minute).UTC().Format(time.RFC3339Nano), stamp, item.actionID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		reclaimed++
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='uncertain',
			state_version=state_version+1,last_error='delivery claim expired; original body will not be resent',updated_at=?
			WHERE message_id=? AND action_id=? AND state='routing'`, stamp, item.messageID, item.actionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reclaimed, nil
}

func (s ConversationSQLStore) ClaimNextVerifying(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	// Verification is read-only recovery and remains enabled after revocation.
	if ttl <= 0 {
		ttl = time.Minute
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	a, _, _, err := scanConversationDriverAction(tx.QueryRowContext(ctx,
		conversationActionSelect+` WHERE a.state IN ('delivered','uncertain') AND a.claim_owner=''
		AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		ORDER BY a.updated_at,a.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET claim_owner=?,claim_deadline_at=?,updated_at=?
		WHERE id=? AND state IN ('delivered','uncertain') AND action_epoch=? AND claim_owner=''`, owner,
		now.Add(ttl).UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), a.ActionID, a.Epoch)
	if err != nil {
		return Action{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Action{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return Action{}, false, err
	}
	return a, true, nil
}

func (s ConversationSQLStore) transitionClaim(ctx context.Context, a Action, owner, from, to, detail string, next, now time.Time) error {
	nextText := ""
	if !next.IsZero() {
		nextText = next.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE conversation_message_actions SET state=?,last_error=?,
		next_attempt_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state=? AND action_epoch=? AND claim_owner=?`, to, detail, nextText,
		now.UTC().Format(time.RFC3339Nano), a.ActionID, from, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s ConversationSQLStore) MarkDelivered(ctx context.Context, a Action, owner string, r Receipt, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='delivered',receipt_ref=?,
		last_error='transport submitted; awaiting independent Interactor processing evidence',
		next_attempt_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state='claimed' AND action_epoch=? AND claim_owner=?`, r.DeliveryID,
		now.Add(15*time.Second).UTC().Format(time.RFC3339Nano), stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='submitted',
		state_version=state_version+1,receipt_ref=?,last_error='',updated_at=?
		WHERE action_id=? AND state='routing'`, r.DeliveryID, stamp, a.ActionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s ConversationSQLStore) MarkUncertain(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='uncertain',last_error=?,
		next_attempt_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state='claimed' AND action_epoch=? AND claim_owner=?`, detail,
		now.Add(time.Minute).UTC().Format(time.RFC3339Nano), stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='uncertain',
		state_version=state_version+1,last_error=?,updated_at=? WHERE action_id=? AND state='routing'`,
		detail, stamp, a.ActionID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s ConversationSQLStore) Retry(ctx context.Context, a Action, owner, detail string, next, now time.Time) error {
	return s.transitionClaim(ctx, a, owner, "claimed", "pending", detail, next, now)
}

func (s ConversationSQLStore) ReleaseVerification(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE conversation_message_actions SET claim_owner='',claim_deadline_at='',
		last_error=?,next_attempt_at=?,updated_at=? WHERE id=? AND state IN ('delivered','uncertain')
		AND action_epoch=? AND claim_owner=?`, detail, now.Add(15*time.Second).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s ConversationSQLStore) DeadLetter(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='dead_letter',last_error=?,
		dead_lettered_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state='claimed' AND action_epoch=? AND claim_owner=?`, detail, stamp, stamp,
		a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	var projectID, threadID, messageID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id,thread_id,message_id FROM conversation_message_actions WHERE id=?`,
		a.ActionID).Scan(&projectID, &threadID, &messageID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='failed',
		state_version=state_version+1,last_error=?,updated_at=? WHERE action_id=? AND state='routing'`,
		detail, stamp, a.ActionID); err != nil {
		return err
	}
	dedup := "conversation_delivery_dead_letter:" + messageID
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,NULL,'conversation_delivery_dead_letter',?,
		json_object('thread_id',?,'message_id',?,'action_id',?,'error',?),'pending',?,?)`,
		"conversation-dead-"+a.ActionID, projectID, dedup, threadID, messageID, a.ActionID,
		detail, stamp, stamp); err != nil {
		return err
	}
	return tx.Commit()
}

func (s ConversationSQLStore) SurfaceOverdue(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.project_id,a.thread_id,a.message_id
		FROM conversation_message_actions a WHERE a.state IN ('delivered','uncertain')
		AND a.acknowledgement_due_at<>'' AND julianday(a.acknowledgement_due_at)<=julianday(?)
		AND NOT EXISTS (SELECT 1 FROM control_alerts c
		  WHERE c.dedup_key='conversation_interactor_ack_overdue:'||a.message_id
		    AND c.state IN ('pending','projected','delivering')) ORDER BY a.acknowledgement_due_at,a.id`, stamp)
	if err != nil {
		return 0, err
	}
	type item struct{ actionID, projectID, threadID, messageID string }
	var items []item
	for rows.Next() {
		var item item
		if err := rows.Scan(&item.actionID, &item.projectID, &item.threadID, &item.messageID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, item := range items {
		detail := "Driver transport exists but no independent Interactor processing evidence arrived"
		if _, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='uncertain',
			state_version=state_version+1,last_error=?,updated_at=? WHERE message_id=?
			AND state='submitted'`, detail, stamp, item.messageID); err != nil {
			return 0, err
		}
		dedup := "conversation_interactor_ack_overdue:" + item.messageID
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'conversation_interactor_ack_overdue',?,
			json_object('thread_id',?,'message_id',?,'action_id',?),'pending',?,?)`,
			"conversation-overdue-"+item.actionID, item.projectID, dedup, item.threadID,
			item.messageID, item.actionID, stamp, stamp); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(items)), nil
}

func (s ConversationSQLStore) Acknowledge(ctx context.Context, a Action, owner string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE conversation_message_actions SET state='acknowledged',
		acknowledged_at=?,claim_owner='',claim_deadline_at='',last_error='',updated_at=?
		WHERE id=? AND state IN ('delivered','uncertain') AND action_epoch=? AND claim_owner=?`,
		stamp, stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	var projectID, threadID, messageID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id,thread_id,message_id
		FROM conversation_message_actions WHERE id=?`, a.ActionID).Scan(&projectID, &threadID, &messageID); err != nil {
		return err
	}
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT state_version FROM conversation_message_deliveries
		WHERE message_id=?`, messageID).Scan(&version); err != nil {
		return err
	}
	res, err = tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state='acknowledged',
		state_version=state_version+1,last_error='',updated_at=? WHERE message_id=? AND action_id=?
		AND state IN ('submitted','uncertain')`, stamp, messageID, a.ActionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	payload := fmt.Sprintf(`{"message_id":%q,"action_id":%q,"action_epoch":%d}`,
		messageID, a.ActionID, a.Epoch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
		(project_id,thread_id,message_id,kind,payload_json,created_at)
		VALUES (?,?,?,'delivery_changed',?,?)`, projectID, threadID, messageID, payload, stamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,'','conversation_delivery_acknowledged','submitted','acknowledged',?,
		'driver',?,?,?)`, projectID, version+1, a.RecipientAgentRunID, payload, stamp); err != nil {
		return err
	}
	for _, dedup := range []string{"conversation_interactor_ack_overdue:" + messageID,
		"conversation_delivery_route_stale:" + messageID,
		"conversation_interactor_route_unavailable:" + messageID} {
		if _, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
			acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
			stamp, stamp, dedup); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConversationStageEvidence accepts only a post-baseline, live,
// provider-native completed user-message hash for the exact recipient
// incarnation. Driver receipt status and agent prose are never stage evidence.
type ConversationStageEvidence struct {
	DB  *sql.DB
	Now func() time.Time
}

func (s ConversationStageEvidence) AwaitStage(ctx context.Context, action Action, receipt Receipt) (bool, error) {
	if err := validateEvidenceReceipt(action, receipt); err != nil {
		return false, err
	}
	var target durableActionEvidenceTarget
	var targetBindingState string
	err := s.DB.QueryRowContext(ctx, `SELECT a.evidence_baseline_store_seq,
		a.evidence_baseline_uncertainty_epoch,a.payload_sha256,r.host_id,r.store_id,
		r.tmux_server_instance_id,r.session_id,r.pane_instance_id,r.agent_run_id,r.state
		FROM conversation_message_actions a JOIN driver_session_bindings r
		  ON r.binding_id=a.target_binding_id WHERE a.id=? AND a.action_epoch=?`,
		action.ActionID, action.Epoch).Scan(&target.BaselineStoreSeq,
		&target.BaselineUncertaintyEpoch, &target.PayloadSHA256, &target.TargetHostID,
		&target.TargetStoreID, &target.TargetServerID, &target.RecipientSessionID,
		&target.RecipientPaneInstanceID, &target.RecipientAgentRunID, &targetBindingState)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("conversation action epoch changed: %w", ErrStaleActionEpoch)
	}
	if err != nil {
		return false, err
	}
	if targetBindingState != "active" {
		return false, fmt.Errorf("Driver conversation binding was superseded: %w", ErrUncertain)
	}
	if target.PayloadSHA256 != action.PayloadSHA256 || target.TargetStoreID != action.TargetStoreID ||
		target.RecipientSessionID != action.RecipientSessionID ||
		target.RecipientPaneInstanceID != action.RecipientPaneInstanceID ||
		target.RecipientAgentRunID != action.RecipientAgentRunID {
		return false, ErrIdentityMismatch
	}
	var state string
	var uncertainty, high uint64
	err = s.DB.QueryRowContext(ctx, `SELECT i.state,c.uncertainty_epoch,c.high_store_seq
		FROM driver_instances i JOIN driver_observation_cursors c
		ON c.store_id=i.store_id AND c.instance_ref=i.instance_ref
		WHERE i.host_id=? AND i.store_id=? AND c.active=1`, target.TargetHostID,
		target.TargetStoreID).Scan(&state, &uncertainty, &high)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (state != "live" || uncertainty != target.BaselineUncertaintyEpoch) {
		return false, fmt.Errorf("Driver conversation evidence store reset or invalidated: %w", ErrUncertain)
	}
	if err != nil {
		return false, err
	}
	var current int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_projections
		WHERE host_id=? AND store_id=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?
		AND tmux_server_instance_id=? AND lifecycle<>'ended'`, target.TargetHostID,
		target.TargetStoreID, target.RecipientSessionID, target.RecipientPaneInstanceID,
		target.RecipientAgentRunID, target.TargetServerID).Scan(&current); err != nil {
		return false, err
	}
	if current != 1 {
		return false, fmt.Errorf("Driver conversation recipient incarnation is stale: %w", ErrUncertain)
	}
	if high <= target.BaselineStoreSeq {
		return false, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT event_id,store_seq,envelope_json
		FROM driver_observation_events WHERE store_id=? AND host_id=? AND session_id=?
		AND pane_instance_id=? AND store_seq>? AND historical=0 AND kind='message.completed'
		ORDER BY store_seq`, target.TargetStoreID, target.TargetHostID,
		target.RecipientSessionID, target.RecipientPaneInstanceID, target.BaselineStoreSeq)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var eventID, envelope string
		var seq uint64
		if err := rows.Scan(&eventID, &seq, &envelope); err != nil {
			return false, err
		}
		matched, err := reviewWakeMessageMatches([]byte(envelope), target.PayloadSHA256)
		if err != nil {
			return false, err
		}
		if !matched {
			continue
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		stamp := time.Now().UTC()
		if s.Now != nil {
			stamp = s.Now().UTC()
		}
		_, err = s.DB.ExecContext(ctx, `INSERT INTO conversation_message_action_evidence
			(action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
			 agent_run_id,evidence_kind,payload_sha256,state,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,'confirmed',?,?)
			ON CONFLICT(action_id,action_epoch) DO NOTHING`, action.ActionID, action.Epoch,
			target.TargetStoreID, eventID, seq, target.RecipientSessionID,
			target.RecipientPaneInstanceID, target.RecipientAgentRunID,
			"provider_user_message_hash", target.PayloadSHA256,
			stamp.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano))
		if err != nil {
			return false, err
		}
		var gotStore, gotEvent, gotSession, gotPane, gotRun, gotHash, gotState string
		var gotSeq uint64
		err = s.DB.QueryRowContext(ctx, `SELECT store_id,event_id,store_seq,session_id,
			pane_instance_id,agent_run_id,payload_sha256,state
			FROM conversation_message_action_evidence WHERE action_id=? AND action_epoch=?`,
			action.ActionID, action.Epoch).Scan(&gotStore, &gotEvent, &gotSeq, &gotSession,
			&gotPane, &gotRun, &gotHash, &gotState)
		if err != nil {
			return false, err
		}
		if gotState == "invalidated" {
			return false, fmt.Errorf("conversation processing evidence was invalidated: %w", ErrUncertain)
		}
		if gotStore != target.TargetStoreID || gotEvent != eventID || gotSeq != seq ||
			gotSession != target.RecipientSessionID || gotPane != target.RecipientPaneInstanceID ||
			gotRun != target.RecipientAgentRunID || gotHash != target.PayloadSHA256 || gotState != "confirmed" {
			return false, ErrIdempotencyBody
		}
		return true, nil
	}
	return false, rows.Err()
}

type ConversationRuntime struct {
	Port               DriverPort
	Resolver           *EndpointResolver
	Store              ConversationSQLStore
	Evidence           StageEvidence
	Owner              string
	ClaimTTL           time.Duration
	AcknowledgementTTL time.Duration
	MaximumTries       int
}

type ConversationRuntimeReport struct {
	Reclaimed, Fenced, Held, Verified, Delivered, Retried, DeadLettered int
}

func (r ConversationRuntime) Tick(ctx context.Context, now time.Time) (ConversationRuntimeReport, error) {
	var out ConversationRuntimeReport
	if (r.Resolver == nil && nilDriverPort(r.Port)) || r.Store.DB == nil || r.Owner == "" {
		return out, errors.New("conversation runtime requires port, store, and owner")
	}
	if r.ClaimTTL <= 0 {
		r.ClaimTTL = time.Minute
	}
	if r.AcknowledgementTTL <= 0 {
		r.AcknowledgementTTL = 10 * time.Minute
	}
	if r.MaximumTries <= 0 {
		r.MaximumTries = 5
	}
	n, err := r.Store.FenceStaleRoutes(ctx, now)
	if err != nil {
		return out, err
	}
	out.Fenced = int(n)
	n, err = r.Store.SurfaceOverdue(ctx, now)
	if err != nil {
		return out, err
	}
	out.Held = int(n)
	n, err = r.Store.ReclaimExpired(ctx, now)
	if err != nil {
		return out, err
	}
	out.Reclaimed = int(n)
	if a, ok, err := r.Store.ClaimNextVerifying(ctx, r.Owner, now, r.ClaimTTL); err != nil {
		return out, err
	} else if ok {
		port, resolveErr := resolveRuntimePort(r.Resolver, r.Port, a)
		if resolveErr != nil {
			return out, r.Store.ReleaseVerification(ctx, a, r.Owner, resolveErr.Error(), now)
		}
		receipt, found, lookupErr := port.ReceiptByAction(ctx, a.ExpectedReceipt())
		if lookupErr != nil || !found {
			detail := "no durable Driver receipt; awaiting mechanical evidence before retry"
			if lookupErr != nil {
				detail = lookupErr.Error()
			}
			return out, r.Store.ReleaseVerification(ctx, a, r.Owner, detail, now)
		}
		if err := a.ExpectedReceipt().Validate(receipt); err != nil {
			return out, r.Store.ReleaseVerification(ctx, a, r.Owner, err.Error(), now)
		}
		if err := r.Store.PersistReceipt(ctx, a, receipt); err != nil {
			return out, err
		}
		complete := false
		if r.Evidence != nil {
			complete, err = r.Evidence.AwaitStage(ctx, a, receipt)
			if err != nil {
				if releaseErr := r.Store.ReleaseVerification(ctx, a, r.Owner, err.Error(), now); releaseErr != nil {
					return out, releaseErr
				}
				if errors.Is(err, ErrUncertain) {
					return out, nil
				}
				return out, err
			}
		}
		if !complete {
			return out, r.Store.ReleaseVerification(ctx, a, r.Owner,
				"transport receipt exists; awaiting independent Interactor processing evidence", now)
		}
		if err := r.Store.Acknowledge(ctx, a, r.Owner, now); err != nil {
			return out, err
		}
		out.Verified++
		return out, nil
	}
	a, ok, err := r.Store.ClaimNext(ctx, r.Owner, now, r.ClaimTTL, r.AcknowledgementTTL)
	if err != nil || !ok {
		return out, err
	}
	if err := validateRuntimeRoute(a); err != nil {
		return out, r.fail(ctx, a, err, now, out)
	}
	if err := validateSessionOriginEndpoint(a); err != nil {
		return out, r.fail(ctx, a, err, now, out)
	}
	port, err := resolveRuntimeSendPort(r.Resolver, r.Port, a)
	if err != nil {
		return out, r.fail(ctx, a, err, now, out)
	}
	result, execErr := (Executor{Port: port, Store: r.Store}).
		ExecuteClaimed(ctx, a.SessionTarget(), a.RouteGrant(), a)
	if execErr != nil {
		if result.Uncertain || errors.Is(execErr, ErrUncertain) {
			return out, r.Store.MarkUncertain(ctx, a, r.Owner, execErr.Error(), now)
		}
		return out, r.fail(ctx, a, execErr, now, out)
	}
	if err := r.Store.MarkDelivered(ctx, a, r.Owner, result.Receipt, now); err != nil {
		return out, err
	}
	out.Delivered++
	return out, nil
}

func (r ConversationRuntime) fail(ctx context.Context, a Action, cause error, now time.Time, out ConversationRuntimeReport) error {
	var attempts int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT attempts FROM conversation_message_actions WHERE id=?`,
		a.ActionID).Scan(&attempts); err != nil {
		return err
	}
	if attempts >= r.MaximumTries {
		if err := r.Store.DeadLetter(ctx, a, r.Owner, cause.Error(), now); err != nil {
			return err
		}
		out.DeadLettered++
		return nil
	}
	if err := r.Store.Retry(ctx, a, r.Owner, cause.Error(), now.Add(time.Minute<<min(attempts-1, 3)), now); err != nil {
		return err
	}
	out.Retried++
	return nil
}
