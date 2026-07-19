package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// DecisionResponseSQLStore is the durable Driver outbox for typed human
// responses. The append-only decision response is workflow truth; this table is
// the independently recoverable transport projection to its exact Interactor.
type DecisionResponseSQLStore struct {
	DB                        *sql.DB
	Now                       func() time.Time
	ControlOriginAvailable    bool
	ControlOriginGate         func() bool
	EndpointControlOriginGate func(EndpointKey) bool
}

func (s DecisionResponseSQLStore) controlOriginAvailableFor(a Action) bool {
	if s.EndpointControlOriginGate != nil {
		return s.EndpointControlOriginGate(EndpointKey{HostID: a.TargetHostID, StoreID: a.TargetStoreID,
			TmuxServerDomainID: a.TargetServerDomainID})
	}
	return s.controlOriginAvailable()
}

func (s DecisionResponseSQLStore) controlOriginAvailable() bool {
	if s.ControlOriginGate != nil {
		return s.ControlOriginGate()
	}
	return s.ControlOriginAvailable
}

func (s DecisionResponseSQLStore) CommitAction(context.Context, Action) error {
	return errors.New("decision response actions are committed by the response/reconcile transaction")
}

func (s DecisionResponseSQLStore) PersistReceipt(ctx context.Context, a Action, r Receipt) error {
	return (SQLActionStore{DB: s.DB, Now: s.Now}).PersistReceipt(ctx, a, r)
}

const decisionResponseActionSelect = `SELECT
	a.id,a.project_id,a.response_id,a.kind,a.action_epoch,a.dedup_key,a.payload_json,
	a.payload_sha256,a.evidence_baseline_store_seq,a.evidence_baseline_uncertainty_epoch,
	a.grant_id,a.grant_epoch,a.grant_expires_at,
	a.sender_principal_id,COALESCE(s.host_id,''),COALESCE(s.store_id,''),
	COALESCE(s.tmux_server_domain_id,''),COALESCE(s.tmux_server_instance_id,''),
	COALESCE(s.session_id,''),COALESCE(s.agent_run_id,''),
	r.host_id,r.store_id,r.tmux_server_domain_id,r.tmux_server_instance_id,r.lifecycle_ownership,
	r.lifecycle_key,r.target_epoch,r.profile_id,r.external_watch_id,r.workspace_root_id,r.workspace_relative_path,r.session_id,
	r.pane_instance_id,r.agent_run_id
	FROM decision_response_actions a
	LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
	JOIN driver_session_bindings r ON r.binding_id=a.target_binding_id`

func scanDecisionResponseDriverAction(row interface{ Scan(...any) error }) (Action, string, error) {
	var a Action
	var responseID, senderHost, senderStore, senderDomain, senderServer string
	err := row.Scan(&a.ActionID, &a.ProjectID, &responseID, &a.Kind, &a.Epoch,
		&a.DedupKey, &a.Payload, &a.PayloadSHA256, &a.EvidenceBaselineStoreSeq,
		&a.EvidenceBaselineUncertaintyEpoch, &a.GrantID, &a.GrantEpoch,
		&a.GrantExpiresAt, &a.SenderPrincipalID, &senderHost, &senderStore, &senderDomain, &senderServer,
		&a.SenderSessionID, &a.SenderAgentRunID, &a.TargetHostID, &a.TargetStoreID,
		&a.TargetServerDomainID, &a.TargetServerID, &a.TargetLifecycleOwnership,
		&a.LifecycleKey, &a.TargetEpoch, &a.ProfileID, &a.ExternalWatchID,
		&a.WorkspaceRootID, &a.WorkspaceRelativePath, &a.RecipientSessionID,
		&a.RecipientPaneInstanceID, &a.RecipientAgentRunID)
	if err != nil {
		return Action{}, "", err
	}
	if a.SenderPrincipalID == "" && (senderHost == "" || senderStore == "" || senderDomain == "" || senderServer == "") {
		return Action{}, "", ErrIdentityMismatch
	}
	a.SenderHostID, a.SenderStoreID = senderHost, senderStore
	a.SenderServerDomainID, a.SenderServerID = senderDomain, senderServer
	a.ExecutorKind = "driver"
	a.TargetRole = "interactor"
	a.LeaseID = "decision-response-route:" + responseID
	a.LeaseEpoch = int64(max(1, a.Epoch))
	return a, responseID, nil
}

// FenceStaleRoutes never retargets a committed action. It closes the old route
// before the domain reconciler can mint a successor for the replacement binding.
func (s DecisionResponseSQLStore) FenceStaleRoutes(ctx context.Context, now time.Time) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.project_id,a.response_id
		FROM decision_response_actions a
		LEFT JOIN driver_session_bindings s ON s.binding_id=a.sender_binding_id
		WHERE a.state IN ('pending','claimed','delivered','uncertain') AND (
		 (a.sender_principal_id='' AND (s.state IS NULL OR s.state<>'active')) OR
		 NOT EXISTS (SELECT 1 FROM driver_session_bindings b
		  WHERE b.binding_id=a.target_binding_id AND b.state='active' AND b.project_id=a.project_id))
		ORDER BY a.created_at,a.id`)
	if err != nil {
		return 0, err
	}
	type item struct{ actionID, projectID, responseID string }
	var items []item
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.actionID, &i.projectID, &i.responseID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var count int64
	for _, i := range items {
		res, err := tx.ExecContext(ctx, `UPDATE decision_response_actions SET state='fenced',
			claim_owner='',claim_deadline_at='',last_error='driver_binding_superseded',updated_at=?
			WHERE id=? AND state IN ('pending','claimed','delivered','uncertain')`, stamp, i.actionID)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		count++
		dedup := "decision_response_route_fenced:" + i.responseID
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'decision_response_route_fenced',?,
			json_object('decision_response_id',?,'action_id',?),'pending',?,?)`,
			"decision-route-fenced-"+i.actionID, i.projectID, dedup, i.responseID,
			i.actionID, stamp, stamp)
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s DecisionResponseSQLStore) ClaimNext(ctx context.Context, owner string, now time.Time, claimTTL, ackTTL time.Duration) (Action, bool, error) {
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
	a, _, err := scanDecisionResponseDriverAction(tx.QueryRowContext(ctx,
		decisionResponseActionSelect+` WHERE a.state='pending'
		AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		AND r.state='active' AND (a.sender_principal_id<>'' OR s.state='active')
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
	grantID := driverGrantUUID(a.ActionID, nextEpoch)
	stamp := now.UTC().Format(time.RFC3339Nano)
	expires := now.Add(10 * time.Minute).UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE decision_response_actions SET state='claimed',
		action_epoch=?,grant_id=?,grant_epoch=?,grant_expires_at=?,claim_owner=?,
		claim_deadline_at=?,delivery_started_at=?,acknowledgement_due_at=?,
		attempts=attempts+1,updated_at=? WHERE id=? AND state='pending' AND action_epoch=?`,
		nextEpoch, grantID, nextEpoch, expires, owner,
		now.Add(claimTTL).UTC().Format(time.RFC3339Nano), stamp,
		now.Add(ackTTL).UTC().Format(time.RFC3339Nano), stamp, a.ActionID, a.Epoch)
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
		WHERE action_id=? AND grant_epoch<? AND revoked_at=''`, stamp, a.ActionID, nextEpoch); err != nil {
		return Action{}, false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO driver_grants
		(grant_id,project_id,action_id,sender_session_id,sender_agent_run_id,sender_principal_id,
		 recipient_session_id,recipient_pane_instance_id,expected_recipient_agent_run_id,grant_epoch,
		 maximum_payload_bytes,allow_draft_stash,issued_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,65536,0,?,?)`, grantID, a.ProjectID, a.ActionID,
		a.SenderSessionID, a.SenderAgentRunID, a.SenderPrincipalID, a.RecipientSessionID,
		a.RecipientPaneInstanceID, controlRecipientRunFence(a), nextEpoch, stamp, expires)
	if err != nil {
		return Action{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Action{}, false, err
	}
	return a, true, nil
}

func (s DecisionResponseSQLStore) ReclaimExpired(ctx context.Context, now time.Time) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(ctx, `UPDATE decision_response_actions SET state='uncertain',
		claim_owner='',claim_deadline_at='',last_error='claim expired; reconcile receipt/evidence before retry',
		next_attempt_at=?,updated_at=? WHERE state='claimed' AND claim_deadline_at<>''
		AND julianday(claim_deadline_at)<=julianday(?)`, now.Add(time.Minute).UTC().Format(time.RFC3339Nano), stamp, stamp)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s DecisionResponseSQLStore) ClaimNextVerifying(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	// Verification is read-only recovery and remains enabled after revocation.
	if ttl <= 0 {
		ttl = time.Minute
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	a, _, err := scanDecisionResponseDriverAction(tx.QueryRowContext(ctx,
		decisionResponseActionSelect+` WHERE a.state IN ('delivered','uncertain')
		AND a.claim_owner='' AND (a.next_attempt_at='' OR julianday(a.next_attempt_at)<=julianday(?))
		ORDER BY a.updated_at,a.id LIMIT 1`, now.UTC().Format(time.RFC3339Nano)))
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE decision_response_actions SET claim_owner=?,
		claim_deadline_at=?,updated_at=? WHERE id=? AND state IN ('delivered','uncertain')
		AND action_epoch=? AND claim_owner=''`, owner, now.Add(ttl).UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), a.ActionID, a.Epoch)
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

func (s DecisionResponseSQLStore) transition(ctx context.Context, a Action, owner, from, to, detail string, next, now time.Time) error {
	nextText := ""
	if !next.IsZero() {
		nextText = next.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE decision_response_actions SET state=?,last_error=?,
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

func (s DecisionResponseSQLStore) MarkDelivered(ctx context.Context, a Action, owner string, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "delivered", "transport submitted; awaiting Interactor processing evidence", now.Add(15*time.Second), now)
}

func (s DecisionResponseSQLStore) MarkUncertain(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "uncertain", detail, now.Add(time.Minute), now)
}

func (s DecisionResponseSQLStore) Retry(ctx context.Context, a Action, owner, detail string, next, now time.Time) error {
	return s.transition(ctx, a, owner, "claimed", "pending", detail, next, now)
}

func (s DecisionResponseSQLStore) ReleaseVerification(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	res, err := s.DB.ExecContext(ctx, `UPDATE decision_response_actions SET claim_owner='',claim_deadline_at='',
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

func (s DecisionResponseSQLStore) Acknowledge(ctx context.Context, a Action, owner string, now time.Time) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	var responseID, projectID string
	if err := tx.QueryRowContext(ctx, `SELECT response_id,project_id FROM decision_response_actions WHERE id=?`, a.ActionID).
		Scan(&responseID, &projectID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE decision_response_actions SET state='acknowledged',
		acknowledged_at=?,acknowledgement_due_at='',claim_owner='',claim_deadline_at='',
		last_error='',updated_at=? WHERE id=? AND state IN ('delivered','uncertain')
		AND action_epoch=? AND claim_owner=?`, stamp, stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,'','decision_response_interactor_acknowledged','driver',?,
		json_object('decision_response_id',?,'action_id',?,'action_epoch',?),?)`, projectID,
		a.RecipientAgentRunID, responseID, a.ActionID, a.Epoch, stamp)
	if err != nil {
		return err
	}
	for _, dedup := range []string{"decision_response_ack_overdue:" + responseID, "decision_response_route_fenced:" + responseID} {
		if _, err := tx.ExecContext(ctx, `UPDATE control_alerts SET state='acknowledged',
			acknowledged_at=?,updated_at=? WHERE dedup_key=? AND state IN ('pending','delivering')`, stamp, stamp, dedup); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s DecisionResponseSQLStore) SurfaceOverdue(ctx context.Context, now time.Time) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,project_id,response_id FROM decision_response_actions
		WHERE state IN ('delivered','uncertain') AND acknowledgement_due_at<>''
		AND julianday(acknowledgement_due_at)<=julianday(?) ORDER BY acknowledgement_due_at,id`, stamp)
	if err != nil {
		return 0, err
	}
	type item struct{ actionID, projectID, responseID string }
	var items []item
	for rows.Next() {
		var i item
		if err := rows.Scan(&i.actionID, &i.projectID, &i.responseID); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, i)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var count int64
	for _, i := range items {
		dedup := "decision_response_ack_overdue:" + i.responseID
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
			(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
			VALUES (?,?,NULL,'decision_response_ack_overdue',?,
			json_object('decision_response_id',?,'action_id',?),'pending',?,?)`,
			"decision-ack-overdue-"+i.actionID, i.projectID, dedup, i.responseID, i.actionID, stamp, stamp)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			count++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (s DecisionResponseSQLStore) DeadLetter(ctx context.Context, a Action, owner, detail string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE decision_response_actions SET state='dead_letter',
		last_error=?,dead_lettered_at=?,claim_owner='',claim_deadline_at='',updated_at=?
		WHERE id=? AND state='claimed' AND action_epoch=? AND claim_owner=?`, detail, stamp,
		stamp, a.ActionID, a.Epoch, owner)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	var responseID, projectID string
	if err := tx.QueryRowContext(ctx, `SELECT response_id,project_id FROM decision_response_actions WHERE id=?`, a.ActionID).
		Scan(&responseID, &projectID); err != nil {
		return err
	}
	dedup := "decision_response_delivery_dead_letter:" + responseID
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO control_alerts
		(id,project_id,epic_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES (?,?,NULL,'decision_response_delivery_dead_letter',?,
		json_object('decision_response_id',?,'action_id',?,'error',?),'pending',?,?)`,
		"decision-delivery-dead-"+a.ActionID, projectID, dedup, responseID, a.ActionID,
		detail, stamp, stamp)
	if err != nil {
		return err
	}
	return tx.Commit()
}

type DecisionResponseRuntime struct {
	Port               DriverPort
	Resolver           *EndpointResolver
	Store              DecisionResponseSQLStore
	Domain             *store.Store
	Evidence           StageEvidence
	Owner              string
	ClaimTTL           time.Duration
	AcknowledgementTTL time.Duration
	MaximumTries       int
}

type DecisionResponseRuntimeReport struct {
	Materialized, Reclaimed, Fenced, Held, Verified, Delivered, Retried, DeadLettered int
}

func (r DecisionResponseRuntime) Tick(ctx context.Context, now time.Time) (DecisionResponseRuntimeReport, error) {
	var out DecisionResponseRuntimeReport
	if (r.Resolver == nil && nilDriverPort(r.Port)) || r.Store.DB == nil || r.Owner == "" {
		return out, errors.New("decision response runtime requires port, store, and owner")
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
	domain := r.Domain
	if domain == nil {
		domain = &store.Store{DB: r.Store.DB}
	}
	materialized, err := domain.ReconcileDecisionResponseActions(ctx, now)
	if err != nil {
		return out, err
	}
	out.Materialized, out.Held = materialized.ActionsCreated, materialized.RoutesHeld
	n, err = r.Store.SurfaceOverdue(ctx, now)
	if err != nil {
		return out, err
	}
	out.Held += int(n)
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
				_ = r.Store.ReleaseVerification(ctx, a, r.Owner, err.Error(), now)
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
	result, execErr := (Executor{Port: port, Store: r.Store}).ExecuteClaimed(ctx, a.SessionTarget(), a.RouteGrant(), a)
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

func (r DecisionResponseRuntime) fail(ctx context.Context, a Action, cause error, now time.Time, out DecisionResponseRuntimeReport) error {
	var attempts int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT attempts FROM decision_response_actions WHERE id=?`, a.ActionID).Scan(&attempts); err != nil {
		return err
	}
	if attempts >= r.MaximumTries {
		return r.Store.DeadLetter(ctx, a, r.Owner, cause.Error(), now)
	}
	backoff := time.Minute << min(attempts-1, 3)
	return r.Store.Retry(ctx, a, r.Owner, cause.Error(), now.Add(backoff), now)
}

func (s DecisionResponseSQLStore) String() string {
	return fmt.Sprintf("decision-response-store(%p)", s.DB)
}
