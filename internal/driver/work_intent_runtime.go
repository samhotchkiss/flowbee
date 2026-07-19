package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// WorkIntentSQLStore is the durable pre-epic Driver outbox. Work-intent
// delivery cannot use epic_actions because no epic exists yet; it nevertheless
// obeys the identical commit-before-mutate, action-epoch, exact-binding, grant,
// receipt, and independent-evidence laws.
type WorkIntentSQLStore struct {
	DB                     *sql.DB
	Now                    func() time.Time
	ControlOriginAvailable bool
	ControlOriginGate      func() bool
}

func (s WorkIntentSQLStore) controlOriginAvailable() bool {
	if s.ControlOriginGate != nil {
		return s.ControlOriginGate()
	}
	return s.ControlOriginAvailable
}

func (s WorkIntentSQLStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// CommitAction exists so the common Executor can persist receipts through this
// store. Runtime uses ExecuteClaimed; a second commit is deliberately rejected.
func (s WorkIntentSQLStore) CommitAction(context.Context, Action) error {
	return errors.New("work-intent Driver actions must be committed by the promotion transaction")
}

func (s WorkIntentSQLStore) PersistReceipt(ctx context.Context, a Action, r Receipt) error {
	return (SQLActionStore{DB: s.DB, Now: s.Now}).PersistReceipt(ctx, a, r)
}

const workIntentActionSelect = `SELECT
	a.id,a.project_id,a.work_intent_id,a.kind,a.action_epoch,a.dedup_key,
	a.payload_json,a.payload_sha256,a.evidence_baseline_store_seq,
	a.evidence_baseline_uncertainty_epoch,a.grant_id,a.grant_epoch,a.grant_expires_at,
	a.sender_principal_id,COALESCE(s.host_id,''),COALESCE(s.store_id,''),
	COALESCE(s.tmux_server_instance_id,''),COALESCE(s.session_id,''),COALESCE(s.agent_run_id,''),
	r.host_id,r.store_id,r.tmux_server_instance_id,r.lifecycle_key,r.target_epoch,
	r.profile_id,r.workspace_root_id,r.workspace_relative_path,r.session_id,
	r.pane_instance_id,r.agent_run_id
	FROM work_intent_actions a
	LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
	JOIN driver_session_bindings r ON r.binding_id=a.target_incarnation`

func scanWorkIntentDriverAction(row interface{ Scan(...any) error }) (Action, string, error) {
	var a Action
	var workIntentID string
	var senderHost, senderStore, senderServer string
	err := row.Scan(&a.ActionID, &a.ProjectID, &workIntentID, &a.Kind, &a.Epoch,
		&a.DedupKey, &a.Payload, &a.PayloadSHA256, &a.EvidenceBaselineStoreSeq,
		&a.EvidenceBaselineUncertaintyEpoch, &a.GrantID, &a.GrantEpoch,
		&a.GrantExpiresAt, &a.SenderPrincipalID, &senderHost, &senderStore, &senderServer,
		&a.SenderSessionID, &a.SenderAgentRunID, &a.TargetHostID, &a.TargetStoreID,
		&a.TargetServerID, &a.LifecycleKey, &a.TargetEpoch, &a.ProfileID,
		&a.WorkspaceRootID, &a.WorkspaceRelativePath, &a.RecipientSessionID,
		&a.RecipientPaneInstanceID, &a.RecipientAgentRunID)
	if err != nil {
		return Action{}, "", err
	}
	if a.SenderPrincipalID == "" && (senderHost == "" || senderStore == "" || senderServer == "") {
		return Action{}, "", ErrIdentityMismatch
	}
	a.ExecutorKind = "driver"
	a.TargetRole = "orchestrator"
	a.LeaseID = "work-intent-route:" + workIntentID
	a.LeaseEpoch = int64(max(1, a.Epoch))
	return a, workIntentID, nil
}

// FenceStaleRoutes turns a replacement-session race into visible durable state
// before any Driver call. The promotion reconciler can then materialize a new
// action against the successor binding; it never retargets this historical row.
func (s WorkIntentSQLStore) FenceStaleRoutes(ctx context.Context, now time.Time) (int64, error) {
	if s.DB == nil {
		return 0, errors.New("work-intent Driver store: nil database")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.project_id,a.work_intent_id
		FROM work_intent_actions a WHERE a.state='pending' AND (
		  (a.sender_principal_id='' AND NOT EXISTS (SELECT 1 FROM driver_session_bindings s
		    WHERE s.binding_id=a.sender_binding_id AND s.state='active' AND s.project_id=a.project_id))
		  OR
		  NOT EXISTS (SELECT 1 FROM driver_session_bindings r
		    WHERE r.binding_id=a.target_incarnation AND r.state='active' AND r.project_id=a.project_id)
		) ORDER BY a.created_at,a.id`)
	if err != nil {
		return 0, err
	}
	type stale struct{ actionID, projectID, intentID string }
	var items []stale
	for rows.Next() {
		var item stale
		if err := rows.Scan(&item.actionID, &item.projectID, &item.intentID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var fenced int64
	for _, item := range items {
		res, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET state='dead_letter',
			last_error='driver_binding_superseded',dead_lettered_at=?,updated_at=?
			WHERE id=? AND state='pending'`, stamp, stamp, item.actionID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		fenced++
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT state_version FROM work_intents WHERE id=?`,
			item.intentID).Scan(&version); err != nil {
			return 0, err
		}
		res, err = tx.ExecContext(ctx, `UPDATE work_intents SET delivery_action_id='',
			state='ready_for_orchestrator',state_version=state_version+1,
			hold_kind='',hold_reason='',route_due_at=?,updated_at=?
			WHERE id=? AND delivery_action_id=? AND state='ready_for_orchestrator' AND state_version=?`,
			stamp, stamp, item.intentID, item.actionID, version)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			_, err = tx.ExecContext(ctx, `INSERT INTO control_events
				(project_id,epic_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
				VALUES (?,'','work_intent_route_fenced','ready_for_orchestrator','ready_for_orchestrator',
				?,'driver','work_intent_runtime',json_object('work_intent_id',?,'action_id',?,'reason','driver_binding_superseded'),?)`,
				item.projectID, version+1, item.intentID, item.actionID, stamp)
			if err != nil {
				return 0, err
			}
		}
		dedup := "work_intent_delivery_route_stale:" + item.intentID
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'work_intent_delivery_route_stale',?,
			json_object('work_intent_id',?,'action_id',?),'pending',?,?)`,
			"intent-route-stale-"+item.actionID, item.projectID, dedup, item.intentID,
			item.actionID, stamp, stamp)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return fenced, nil
}

// ClaimNext atomically advances one action epoch and projects its exact A→B
// Driver grant before the executor can observe the claim.
func (s WorkIntentSQLStore) ClaimNext(ctx context.Context, owner string, now time.Time, ttl,
	ackTTL time.Duration) (Action, bool, error) {
	if !s.controlOriginAvailable() {
		return Action{}, false, nil
	}
	if s.DB == nil || owner == "" {
		return Action{}, false, errors.New("work-intent Driver claim requires database and owner")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	if ackTTL <= 0 {
		ackTTL = 10 * time.Minute
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	var a Action
	var intentID string
	a, intentID, err = scanWorkIntentDriverAction(tx.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.state='pending'
		AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		AND r.state='active' AND (a.sender_principal_id<>'' OR s.state='active')
		ORDER BY a.created_at,a.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	deadline := now.Add(ttl).UTC().Format(time.RFC3339Nano)
	ackDue := now.Add(ackTTL).UTC().Format(time.RFC3339Nano)
	expires := now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	nextEpoch := a.Epoch + 1
	nextGrantID := driverGrantUUID(a.ActionID, nextEpoch)
	res, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET state='claimed',
		action_epoch=?,grant_id=?,grant_epoch=?,grant_expires_at=?,
		claim_owner=?,claim_deadline_at=?,delivery_started_at=?,attempts=attempts+1,updated_at=?
		WHERE id=? AND state='pending' AND action_epoch=?`, nextEpoch, nextGrantID, nextEpoch,
		expires, owner, deadline,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), a.ActionID, a.Epoch)
	if err != nil {
		return Action{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Action{}, false, nil
	}
	a.Epoch = nextEpoch
	a.GrantID = nextGrantID
	a.GrantEpoch = a.Epoch
	a.GrantExpiresAt = expires
	a.LeaseEpoch = int64(max(1, a.Epoch))
	if a.SenderPrincipalID == "" && a.SenderSessionID == a.RecipientSessionID {
		return Action{}, false, ErrGrantDenied
	}
	if _, err := tx.ExecContext(ctx, `UPDATE driver_grants SET revoked_at=?
		WHERE action_id=? AND grant_epoch<? AND revoked_at=''`, now.UTC().Format(time.RFC3339Nano),
		a.ActionID, a.GrantEpoch); err != nil {
		return Action{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO driver_grants
		(grant_id,project_id,action_id,sender_session_id,sender_agent_run_id,sender_principal_id,
		 recipient_session_id,recipient_pane_instance_id,grant_epoch,
		 maximum_payload_bytes,allow_draft_stash,issued_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,65536,0,?,?)`, a.GrantID, a.ProjectID, a.ActionID,
		a.SenderSessionID, a.SenderAgentRunID, a.SenderPrincipalID, a.RecipientSessionID,
		a.RecipientPaneInstanceID, a.Epoch, now.UTC().Format(time.RFC3339Nano), expires)
	if err != nil {
		return Action{}, false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE work_intents SET route_lease_id=?,route_epoch=?,
		route_attempts=route_attempts+1,route_due_at=?,updated_at=?
		WHERE id=? AND delivery_action_id=? AND state='ready_for_orchestrator'`,
		a.LeaseID, a.Epoch, ackDue, now.UTC().Format(time.RFC3339Nano), intentID, a.ActionID)
	if err != nil {
		return Action{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Action{}, false, err
	}
	return a, true, nil
}

func (s WorkIntentSQLStore) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE work_intent_actions SET state='uncertain',
		claim_owner='',claim_deadline_at='',last_error='claim expired; reconcile receipt/evidence before retry',
		next_attempt_at=?,updated_at=? WHERE state='claimed' AND claim_deadline_at<>''
		AND julianday(claim_deadline_at)<=julianday(?)`, now.Add(time.Minute).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s WorkIntentSQLStore) ClaimNextVerifying(ctx context.Context, owner string, now time.Time,
	ttl time.Duration) (Action, bool, error) {
	// Verification is read-only recovery and remains enabled after revocation.
	if ttl <= 0 {
		ttl = time.Minute
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	a, _, err := scanWorkIntentDriverAction(tx.QueryRowContext(ctx,
		workIntentActionSelect+` WHERE a.state IN ('delivered','uncertain')
		AND a.claim_owner='' AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		ORDER BY a.updated_at,a.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET claim_owner=?,claim_deadline_at=?,updated_at=?
		WHERE id=? AND state IN ('delivered','uncertain') AND action_epoch=? AND claim_owner=''`,
		owner, now.Add(ttl).UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
		a.ActionID, a.Epoch)
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

func (s WorkIntentSQLStore) MarkDelivered(ctx context.Context, a Action, owner string, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "delivered",
		"transport submitted; awaiting independent processing evidence", now.Add(15*time.Second), now)
}

func (s WorkIntentSQLStore) MarkUncertain(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "uncertain", detail, now.Add(time.Minute), now)
}

func (s WorkIntentSQLStore) Retry(ctx context.Context, a Action, owner, detail string, next, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "pending", detail, next, now)
}

func (s WorkIntentSQLStore) DeadLetter(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET state='dead_letter',last_error=?,
		dead_lettered_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state='claimed' AND action_epoch=? AND claim_owner=?`, detail, stamp,
		stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	var intentID, projectID string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT w.id,w.project_id,w.state_version FROM work_intents w
		JOIN work_intent_actions a ON a.work_intent_id=w.id WHERE a.id=?`, a.ActionID).
		Scan(&intentID, &projectID, &version); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE work_intents SET hold_kind='orchestrator_delivery_dead_letter',
		hold_reason=?,state_version=state_version+1,route_due_at='',updated_at=?
		WHERE id=? AND state='ready_for_orchestrator' AND state_version=?`, detail, stamp,
		intentID, version); err != nil {
		return err
	}
	dedup := "work_intent_delivery_dead_letter:" + intentID
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,NULL,'work_intent_delivery_dead_letter',?,
		json_object('work_intent_id',?,'action_id',?,'error',?),'pending',?,?)`,
		"intent-delivery-dead-"+a.ActionID, projectID, dedup, intentID, a.ActionID,
		detail, stamp, stamp)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s WorkIntentSQLStore) transition(ctx context.Context, a Action, owner, from, to, detail string,
	next, now time.Time) error {
	nextText := ""
	if !next.IsZero() {
		nextText = next.UTC().Format(time.RFC3339Nano)
	}
	dead := ""
	if to == "dead_letter" {
		dead = now.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE work_intent_actions SET state=?,last_error=?,
		next_attempt_at=?,dead_lettered_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state=? AND action_epoch=? AND claim_owner=?`, to, detail, nextText, dead,
		now.UTC().Format(time.RFC3339Nano), a.ActionID, from, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s WorkIntentSQLStore) ReleaseVerification(ctx context.Context, a Action, owner, detail string,
	now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE work_intent_actions SET claim_owner='',claim_deadline_at='',
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

// SurfaceOverdue makes the post-send/no-processing seam visible without
// changing or retrying the immutable transport effect.
func (s WorkIntentSQLStore) SurfaceOverdue(ctx context.Context, now time.Time) (int64, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT w.id,w.project_id,w.state_version,a.id
		FROM work_intents w JOIN work_intent_actions a ON a.id=w.delivery_action_id
		WHERE w.state='ready_for_orchestrator' AND a.state IN ('delivered','uncertain')
		AND w.route_due_at<>'' AND julianday(w.route_due_at)<=julianday(?)
		AND w.hold_kind<>'orchestrator_ack_overdue' ORDER BY w.route_due_at,w.id`,
		now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	type overdue struct {
		intentID, projectID, actionID string
		version                       int
	}
	var items []overdue
	for rows.Next() {
		var item overdue
		if err := rows.Scan(&item.intentID, &item.projectID, &item.version, &item.actionID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	var surfaced int64
	for _, item := range items {
		detail := "Orchestrator transport was submitted but no independent processing acknowledgement arrived"
		res, err := tx.ExecContext(ctx, `UPDATE work_intents SET hold_kind='orchestrator_ack_overdue',
			hold_reason=?,state_version=state_version+1,updated_at=? WHERE id=?
			AND state='ready_for_orchestrator' AND state_version=?`, detail, stamp,
			item.intentID, item.version)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		surfaced++
		dedup := "work_intent_orchestrator_ack_overdue:" + item.intentID
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'work_intent_orchestrator_ack_overdue',?,
			json_object('work_intent_id',?,'action_id',?),'pending',?,?)`,
			"intent-ack-overdue-"+item.actionID, item.projectID, dedup, item.intentID,
			item.actionID, stamp, stamp)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return surfaced, nil
}

// Acknowledge atomically records processing evidence as both action completion
// and the work-intent's orchestrating transition. No crash can lose the latter
// after consuming the former.
func (s WorkIntentSQLStore) Acknowledge(ctx context.Context, a Action, owner string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	var intentID, projectID, state string
	var version int
	err = tx.QueryRowContext(ctx, `SELECT w.id,w.project_id,w.state,w.state_version
		FROM work_intents w JOIN work_intent_actions a ON a.work_intent_id=w.id
		WHERE a.id=? AND w.delivery_action_id=a.id`, a.ActionID).Scan(&intentID, &projectID, &state, &version)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE work_intent_actions SET state='acknowledged',
		acknowledged_at=?,claim_owner='',claim_deadline_at='',last_error='',updated_at=?
		WHERE id=? AND state IN ('delivered','uncertain') AND action_epoch=? AND claim_owner=?`,
		stamp, stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	if state == "ready_for_orchestrator" {
		res, err = tx.ExecContext(ctx, `UPDATE work_intents SET state='orchestrating',
			state_version=state_version+1,route_acknowledged_at=?,route_due_at='',hold_kind='',
			hold_reason='',updated_at=? WHERE id=? AND state='ready_for_orchestrator'
			AND state_version=? AND delivery_action_id=?`, stamp, stamp, intentID, version, a.ActionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrStaleActionEpoch
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id,epic_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
			VALUES (?,'','work_intent_orchestrator_acknowledged','ready_for_orchestrator','orchestrating',
			?,'driver',?,json_object('work_intent_id',?,'action_id',?,'action_epoch',?),?)`,
			projectID, version+1, a.RecipientAgentRunID, intentID, a.ActionID, a.Epoch, stamp)
		if err != nil {
			return err
		}
		for _, dedup := range []string{
			"work_intent_orchestrator_ack_overdue:" + intentID,
			"work_intent_promotion_stalled:" + intentID,
		} {
			if _, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
				acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`,
				stamp, stamp, dedup); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type WorkIntentRuntime struct {
	Port               DriverPort
	Store              WorkIntentSQLStore
	Evidence           StageEvidence
	Owner              string
	ClaimTTL           time.Duration
	AcknowledgementTTL time.Duration
	MaximumTries       int
}

type WorkIntentRuntimeReport struct {
	Reclaimed, Fenced, Held, Verified, Delivered, Retried, DeadLettered int
}

func (r WorkIntentRuntime) Tick(ctx context.Context, now time.Time) (WorkIntentRuntimeReport, error) {
	var out WorkIntentRuntimeReport
	if r.Port == nil || r.Store.DB == nil || r.Owner == "" {
		return out, errors.New("work-intent runtime requires port, store, and owner")
	}
	if r.ClaimTTL <= 0 {
		r.ClaimTTL = time.Minute
	}
	if r.MaximumTries <= 0 {
		r.MaximumTries = 5
	}
	if r.AcknowledgementTTL <= 0 {
		r.AcknowledgementTTL = 10 * time.Minute
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
		receipt, found, lookupErr := r.Port.ReceiptByAction(ctx, a.ExpectedReceipt())
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
				_ = r.Store.ReleaseVerification(ctx, a, r.Owner, err.Error(), now)
				return out, err
			}
		}
		if !complete {
			return out, r.Store.ReleaseVerification(ctx, a, r.Owner,
				"transport receipt exists; awaiting independent Orchestrator processing evidence", now)
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
	result, execErr := (Executor{Port: r.Port, Store: r.Store, Evidence: nil}).
		ExecuteClaimed(ctx, a.SessionTarget(), a.RouteGrant(), a)
	if execErr != nil {
		if result.Uncertain || errors.Is(execErr, ErrUncertain) {
			return out, r.Store.MarkUncertain(ctx, a, r.Owner, execErr.Error(), now)
		}
		return out, r.fail(ctx, a, execErr, now, out)
	}
	if err := r.Store.MarkDelivered(ctx, a, r.Owner, now); err != nil {
		return out, err
	}
	out.Delivered++
	return out, nil
}

func (r WorkIntentRuntime) fail(ctx context.Context, a Action, cause error, now time.Time,
	out WorkIntentRuntimeReport) error {
	var attempts int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT attempts FROM work_intent_actions WHERE id=?`,
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
	backoff := time.Minute << min(attempts-1, 3)
	if err := r.Store.Retry(ctx, a, r.Owner, cause.Error(), now.Add(backoff), now); err != nil {
		return err
	}
	out.Retried++
	return nil
}

func (s WorkIntentSQLStore) String() string { return fmt.Sprintf("work-intent-store(%p)", s.DB) }
