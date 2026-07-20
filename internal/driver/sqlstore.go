package driver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SQLActionStore is the Flowbee-owned durable side of the Driver boundary. It
// commits immutable actions before any Driver mutation and records transport
// receipts independently from workflow-stage evidence.
type SQLActionStore struct {
	DB                     *sql.DB
	Now                    func() time.Time
	ControlOriginAvailable bool
	// ControlOriginGate is the live, token-bound capability proof. When set it
	// overrides ControlOriginAvailable so revocation closes pending claims while
	// the process continues reconciling uncertain/verifying effects.
	ControlOriginGate func() bool
	// EndpointControlOriginGate is the production multi-endpoint fence. When
	// present it is authoritative for message claims: a capability proven on one
	// host/store/server domain can never authorize an action for another.
	EndpointControlOriginGate func(EndpointKey) bool
}

func (s SQLActionStore) controlOriginAvailable() bool {
	if s.ControlOriginGate != nil {
		return s.ControlOriginGate()
	}
	return s.ControlOriginAvailable
}

func (s SQLActionStore) controlOriginAvailableFor(a Action) bool {
	if s.EndpointControlOriginGate != nil {
		return s.EndpointControlOriginGate(EndpointKey{HostID: a.TargetHostID, StoreID: a.TargetStoreID,
			TmuxServerDomainID: a.TargetServerDomainID})
	}
	return s.controlOriginAvailable()
}

type actionScanner interface{ Scan(...any) error }

const actionSelectColumns = `id,project_id,epic_id,kind,action_epoch,dedup_key,
	payload_json,payload_sha256,evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,
	head_sha,base_sha,executor_kind,target_role,
	target_host_id,target_store_id,target_server_domain_id,target_server_id,lifecycle_key,target_epoch,profile_id,external_watch_id,
	workspace_root_id,workspace_relative_path,lease_id,lease_epoch,sender_session_id,
	sender_host_id,sender_store_id,sender_server_domain_id,sender_server_id,
	sender_agent_run_id,sender_principal_id,recipient_session_id,recipient_pane_instance_id,
	recipient_agent_run_id,grant_id,grant_epoch,grant_expires_at`

func scanAction(row actionScanner, a *Action) error {
	return row.Scan(&a.ActionID, &a.ProjectID, &a.EpicID, &a.Kind, &a.Epoch, &a.DedupKey,
		&a.Payload, &a.PayloadSHA256, &a.EvidenceBaselineStoreSeq,
		&a.EvidenceBaselineUncertaintyEpoch, &a.HeadSHA, &a.BaseSHA, &a.ExecutorKind, &a.TargetRole,
		&a.TargetHostID, &a.TargetStoreID, &a.TargetServerDomainID, &a.TargetServerID, &a.LifecycleKey, &a.TargetEpoch,
		&a.ProfileID, &a.ExternalWatchID, &a.WorkspaceRootID, &a.WorkspaceRelativePath, &a.LeaseID, &a.LeaseEpoch,
		&a.SenderSessionID, &a.SenderHostID, &a.SenderStoreID, &a.SenderServerDomainID, &a.SenderServerID,
		&a.SenderAgentRunID, &a.SenderPrincipalID, &a.RecipientSessionID,
		&a.RecipientPaneInstanceID, &a.RecipientAgentRunID, &a.GrantID, &a.GrantEpoch,
		&a.GrantExpiresAt)
}

// ClaimNextAction atomically moves one due outbox item into delivering and
// increments its epoch. Only the returned epoch may mutate the action again.
func (s SQLActionStore) ClaimNextAction(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	return s.claimNextAction(ctx, owner, "driver", true, now, ttl)
}

func (s SQLActionStore) ClaimNextLifecycleAction(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	return s.claimNextAction(ctx, owner, "driver_lifecycle", false, now, ttl)
}

func (s SQLActionStore) claimNextAction(ctx context.Context, owner, executorKind string, projectGrant bool, now time.Time, ttl time.Duration) (Action, bool, error) {
	if s.DB == nil || owner == "" {
		return Action{}, false, errors.New("driver sql store: claim requires database and owner")
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	nowText, deadline := now.UTC().Format(time.RFC3339Nano), now.UTC().Add(ttl).Format(time.RFC3339Nano)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return Action{}, false, err
	}
	defer tx.Rollback()
	var a Action
	err = scanAction(tx.QueryRowContext(ctx, `
		SELECT `+actionSelectColumns+`
		  FROM epic_actions
		 WHERE state='pending' AND executor_kind=?
		   AND (next_attempt_at='' OR julianday(next_attempt_at) <= julianday(?))
		 ORDER BY created_at, id LIMIT 1`, executorKind, nowText), &a)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	if projectGrant && !s.controlOriginAvailableFor(a) {
		return Action{}, false, nil
	}
	nextEpoch := a.Epoch + 1
	nextGrantID := driverGrantUUID(a.ActionID, nextEpoch)
	res, err := tx.ExecContext(ctx, `
		UPDATE epic_actions
		   SET state='delivering', action_epoch=?, claim_owner=?, grant_id=?,
		       grant_epoch=?, claim_deadline_at=?, delivery_started_at=?, attempts=attempts+1, updated_at=?
		 WHERE id=? AND state='pending' AND action_epoch=?`,
		nextEpoch, owner, nextGrantID, nextEpoch, deadline, nowText, nowText, a.ActionID, a.Epoch)
	if err != nil {
		return Action{}, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Action{}, false, nil
	}
	a.Epoch = nextEpoch
	a.GrantID = nextGrantID
	a.GrantEpoch = a.Epoch
	if !projectGrant {
		if err := tx.Commit(); err != nil {
			return Action{}, false, err
		}
		return a, true, nil
	}
	// The exact directional grant projection is durable before Driver sees it.
	// Driver remains the enforcement point, while this history lets Flowbee
	// recover/audit every route epoch without inferring it from a receipt.
	if _, err := tx.ExecContext(ctx, `UPDATE driver_grants SET revoked_at=?
		WHERE action_id=? AND grant_epoch<? AND revoked_at=''`, nowText, a.ActionID, a.GrantEpoch); err != nil {
		return Action{}, false, fmt.Errorf("fence prior Driver grant projection: %w", err)
	}
	recipientRunFence := ""
	if a.SenderPrincipalID != "" {
		recipientRunFence = a.RecipientAgentRunID
	}
	if a.SenderPrincipalID != "" && recipientRunFence == "" {
		return Action{}, false, errors.New("persist Driver control grant: missing expected recipient agent run")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_grants
		(grant_id,project_id,action_id,sender_session_id,sender_agent_run_id,sender_principal_id,
		 recipient_session_id,recipient_pane_instance_id,expected_recipient_agent_run_id,grant_epoch,
		 maximum_payload_bytes,allow_draft_stash,issued_at,expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,65536,0,?,?)`, a.GrantID, a.ProjectID, a.ActionID,
		a.SenderSessionID, a.SenderAgentRunID, a.SenderPrincipalID, a.RecipientSessionID,
		a.RecipientPaneInstanceID, recipientRunFence, a.GrantEpoch, nowText, a.GrantExpiresAt); err != nil {
		return Action{}, false, fmt.Errorf("persist Driver grant projection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Action{}, false, err
	}
	return a, true, nil
}

// ReclaimExpiredActions never makes an uncertain external effect pending again.
// A process may have died after Driver mutation; recovery must verify the original
// action/epoch rather than blindly resend it.
func (s SQLActionStore) ReclaimExpiredActions(ctx context.Context, now time.Time) (int64, error) {
	nowText := now.UTC().Format(time.RFC3339Nano)
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state='verifying',claim_owner='',
		claim_deadline_at='',last_error='executor_claim_expired',updated_at=?
		WHERE state='delivering' AND claim_deadline_at<>'' AND julianday(claim_deadline_at)<=julianday(?)`, nowText, nowText)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s SQLActionStore) ClaimNextVerifying(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	// A capability downgrade fences new mutations, but must never strand an
	// already uncertain external effect. Receipt/evidence reconciliation is
	// read-only and remains live while new delivery claims are held.
	return s.claimNextVerifying(ctx, owner, "driver", now, ttl)
}

func (s SQLActionStore) ClaimNextLifecycleVerifying(ctx context.Context, owner string, now time.Time, ttl time.Duration) (Action, bool, error) {
	return s.claimNextVerifying(ctx, owner, "driver_lifecycle", now, ttl)
}

// AdvanceLifecycleVerificationEpoch authorizes Driver's inspection-only Verify
// call. It runs only after an uncertain canonical receipt was observed. The
// immutable lifecycle action ID/target stay fixed; the higher action epoch
// fences a prior verifier without authorizing another lifecycle mutation.
func (s SQLActionStore) AdvanceLifecycleVerificationEpoch(ctx context.Context, a Action,
	owner string, now time.Time) (Action, error) {
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET action_epoch=action_epoch+1,
		updated_at=? WHERE id=? AND state='verifying' AND claim_owner=? AND action_epoch=?`,
		now.UTC().Format(time.RFC3339Nano), a.ActionID, owner, a.Epoch)
	if err != nil {
		return Action{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return Action{}, ErrStaleActionEpoch
	}
	a.Epoch++
	return a, nil
}

// ResumeLifecycleAfterAbsentProof moves an inspection claim back to delivery
// without changing the immutable claimant epoch. Callers may use it only after
// Driver returned no by-action receipt and authoritative lifecycle presence made
// repeating this exact effect mechanically safe.
func (s SQLActionStore) ResumeLifecycleAfterAbsentProof(ctx context.Context, a Action,
	owner string, now time.Time, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = time.Minute
	}
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state='delivering',
		claim_deadline_at=?,last_error='recovered_after_exact_presence_check',updated_at=?
		WHERE id=? AND state='verifying' AND claim_owner=? AND action_epoch=?`,
		now.Add(ttl).UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano),
		a.ActionID, owner, a.Epoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s SQLActionStore) claimNextVerifying(ctx context.Context, owner, executorKind string, now time.Time, ttl time.Duration) (Action, bool, error) {
	if s.DB == nil || owner == "" {
		return Action{}, false, errors.New("driver sql store: verify claim requires database and owner")
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	var a Action
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, false, err
	}
	defer tx.Rollback()
	err = scanAction(tx.QueryRowContext(ctx, `SELECT `+actionSelectColumns+` FROM epic_actions
		WHERE state='verifying' AND executor_kind=? AND claim_owner=''
		ORDER BY updated_at,id LIMIT 1`, executorKind), &a)
	if errors.Is(err, sql.ErrNoRows) {
		return Action{}, false, nil
	}
	if err != nil {
		return Action{}, false, err
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `UPDATE epic_actions SET claim_owner=?,claim_deadline_at=?,updated_at=?
		WHERE id=? AND state='verifying' AND action_epoch=? AND claim_owner=''`, owner,
		now.Add(ttl).UTC().Format(time.RFC3339Nano), nowText, a.ActionID, a.Epoch)
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

func (s SQLActionStore) AcknowledgeVerifying(ctx context.Context, id, owner string, epoch int64, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.transitionClaimed(ctx, id, owner, epoch, "verifying", "acknowledged", now,
		"acknowledged_at=?, claim_deadline_at=''", stamp)
}

func (s SQLActionStore) ReleaseVerifying(ctx context.Context, id, owner string, epoch int64, detail string, now time.Time) error {
	return s.transitionClaimed(ctx, id, owner, epoch, "verifying", "verifying", now,
		"last_error=?, claim_owner='', claim_deadline_at=''", detail)
}

func (s SQLActionStore) DeadLetterVerifying(ctx context.Context, id, owner string, epoch int64, detail string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.transitionClaimed(ctx, id, owner, epoch, "verifying", "dead_letter", now,
		"last_error=?, dead_lettered_at=?, claim_deadline_at=''", detail, stamp)
}

func (s SQLActionStore) transitionClaimed(ctx context.Context, id, owner string, epoch int64, from, to string, now time.Time, extra string, args ...any) error {
	params := []any{to}
	params = append(params, args...)
	params = append(params, now.UTC().Format(time.RFC3339Nano), id, from, owner, epoch)
	res, err := s.DB.ExecContext(ctx, `UPDATE epic_actions SET state=?, `+extra+`, updated_at=?
		WHERE id=? AND state=? AND claim_owner=? AND action_epoch=?`, params...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s SQLActionStore) MarkActionVerifying(ctx context.Context, id, owner string, epoch int64, detail string, now time.Time) error {
	return s.transitionClaimed(ctx, id, owner, epoch, "delivering", "verifying", now,
		"last_error=?, claim_owner='', claim_deadline_at=''", detail)
}

func (s SQLActionStore) AcknowledgeAction(ctx context.Context, id, owner string, epoch int64, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.transitionClaimed(ctx, id, owner, epoch, "delivering", "acknowledged", now,
		"acknowledged_at=?, claim_deadline_at=''", stamp)
}

func (s SQLActionStore) RetryAction(ctx context.Context, id, owner string, epoch int64, detail string, next time.Time, now time.Time) error {
	return s.transitionClaimed(ctx, id, owner, epoch, "delivering", "pending", now,
		"last_error=?, next_attempt_at=?, claim_owner='', claim_deadline_at=''", detail, next.UTC().Format(time.RFC3339Nano))
}

func (s SQLActionStore) DeadLetterAction(ctx context.Context, id, owner string, epoch int64, detail string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.transitionClaimed(ctx, id, owner, epoch, "delivering", "dead_letter", now,
		"last_error=?, dead_lettered_at=?, claim_deadline_at=''", detail, stamp)
}

// RearmDeadLetter updates the historical row in place. It never creates a new
// dedup key and caps automatic recovery per effect.
func (s SQLActionStore) RearmDeadLetter(ctx context.Context, id string, maximumRecoveries int, now time.Time) error {
	if maximumRecoveries < 1 {
		return errors.New("driver sql store: recovery budget must be positive")
	}
	res, err := s.DB.ExecContext(ctx, `
		UPDATE epic_actions
		   SET state='pending', recovery_count=recovery_count+1, next_attempt_at=?,
		       claim_owner='', claim_deadline_at='', last_error='', updated_at=?
		 WHERE id=? AND state='dead_letter' AND recovery_count < ?`,
		now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), id, maximumRecoveries)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrStaleActionEpoch
	}
	return nil
}

func (s SQLActionStore) now() string {
	if s.Now != nil {
		return s.Now().UTC().Format(time.RFC3339Nano)
	}
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func (s SQLActionStore) CommitAction(ctx context.Context, a Action) error {
	if s.DB == nil {
		return errors.New("driver sql store: nil database")
	}
	if a.ActionID == "" || a.EpicID == "" || a.Kind == "" || a.DedupKey == "" || a.PayloadSHA256 == "" {
		return errors.New("driver sql store: incomplete action identity")
	}
	projectID := a.ProjectID
	if projectID == "" {
		projectID = "default"
	}
	executorKind := a.ExecutorKind
	if executorKind == "" {
		executorKind = "driver"
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Exact replays retain the original creation watermark even when more Driver
	// events have arrived since.  For a new action, capture both the ledger high
	// water and its uncertainty generation under this insert transaction.
	var existing Action
	qerr := scanAction(tx.QueryRowContext(ctx, `SELECT `+actionSelectColumns+`
		FROM epic_actions WHERE id=? OR (dedup_key=? AND state<>'cancelled_superseded') LIMIT 1`,
		a.ActionID, a.DedupKey), &existing)
	if qerr == nil {
		a.ProjectID, a.ExecutorKind = projectID, executorKind
		a.EvidenceBaselineStoreSeq = existing.EvidenceBaselineStoreSeq
		a.EvidenceBaselineUncertaintyEpoch = existing.EvidenceBaselineUncertaintyEpoch
		if existing != a {
			return fmt.Errorf("commit driver action %s: %w", a.ActionID, ErrIdempotencyBody)
		}
		return tx.Commit()
	}
	if !errors.Is(qerr, sql.ErrNoRows) {
		return qerr
	}
	if a.TargetStoreID != "" && a.EvidenceBaselineStoreSeq == 0 && a.EvidenceBaselineUncertaintyEpoch == 0 {
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(high_store_seq),0),
			COALESCE(MAX(uncertainty_epoch),0) FROM driver_observation_cursors
			WHERE store_id=?`, a.TargetStoreID).Scan(&a.EvidenceBaselineStoreSeq,
			&a.EvidenceBaselineUncertaintyEpoch); err != nil {
			return fmt.Errorf("capture Driver evidence baseline: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO epic_actions
		 (id, project_id, epic_id, kind, state, action_epoch, dedup_key,
		  payload_json, payload_sha256,evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,
		  head_sha, base_sha, executor_kind,target_role,
		  target_host_id,target_store_id,target_server_domain_id,target_server_id,lifecycle_key,target_epoch,profile_id,external_watch_id,
		  workspace_root_id,workspace_relative_path,lease_id,lease_epoch,sender_session_id,
		  sender_host_id,sender_store_id,sender_server_domain_id,sender_server_id,
		  sender_agent_run_id,sender_principal_id,recipient_session_id,recipient_pane_instance_id,
		  recipient_agent_run_id,grant_id,grant_epoch,grant_expires_at,created_at,updated_at)
		VALUES (?, ?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ActionID, projectID, a.EpicID, a.Kind, a.Epoch, a.DedupKey,
		a.Payload, a.PayloadSHA256, a.EvidenceBaselineStoreSeq,
		a.EvidenceBaselineUncertaintyEpoch, a.HeadSHA, a.BaseSHA, executorKind, a.TargetRole,
		a.TargetHostID, a.TargetStoreID, a.TargetServerDomainID, a.TargetServerID, a.LifecycleKey, a.TargetEpoch,
		a.ProfileID, a.ExternalWatchID, a.WorkspaceRootID, a.WorkspaceRelativePath, a.LeaseID, a.LeaseEpoch,
		a.SenderSessionID, a.SenderHostID, a.SenderStoreID, a.SenderServerDomainID, a.SenderServerID,
		a.SenderAgentRunID, a.SenderPrincipalID, a.RecipientSessionID, a.RecipientPaneInstanceID,
		a.RecipientAgentRunID, a.GrantID, a.GrantEpoch, a.GrantExpiresAt, now, now)
	if err == nil {
		return tx.Commit()
	}
	// Both action ID and semantic dedup collisions are valid replays only when
	// every immutable field matches. A changed payload/epoch/artifact is never
	// allowed to mutate the prior effect.
	var got Action
	qerr = scanAction(tx.QueryRowContext(ctx, `
		SELECT `+actionSelectColumns+`
		  FROM epic_actions
		 WHERE id = ? OR (dedup_key = ? AND state <> 'cancelled_superseded')
		 LIMIT 1`, a.ActionID, a.DedupKey), &got)
	if qerr != nil {
		return fmt.Errorf("commit driver action: %w", err)
	}
	a.ProjectID, a.ExecutorKind = projectID, executorKind
	a.EvidenceBaselineStoreSeq = got.EvidenceBaselineStoreSeq
	a.EvidenceBaselineUncertaintyEpoch = got.EvidenceBaselineUncertaintyEpoch
	if got != a {
		return fmt.Errorf("commit driver action %s: %w", a.ActionID, ErrIdempotencyBody)
	}
	return tx.Commit()
}

func (s SQLActionStore) PersistReceipt(ctx context.Context, a Action, r Receipt) error {
	if s.DB == nil {
		return errors.New("driver sql store: nil database")
	}
	if err := a.ExpectedReceipt().Validate(r); err != nil {
		return fmt.Errorf("persist driver receipt %s: %w", a.ActionID, err)
	}
	if !deliveryReceiptStatusKnown(r.Status) {
		return fmt.Errorf("persist driver receipt %s has invalid status %q: %w", a.ActionID, r.Status, ErrIdempotencyBody)
	}
	now := s.now()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT OR IGNORE INTO driver_receipts
		 (delivery_id, action_id, grant_id, grant_epoch, sender_session_id,sender_principal_id,
		  recipient_session_id, recipient_pane_instance_id,expected_recipient_agent_run_id,payload_sha256,
		  status, compatibility_code, diagnostic_code, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.DeliveryID, r.ActionID, r.GrantID, r.GrantEpoch, r.Sender.SessionID,
		r.SenderPrincipalID, r.Recipient.SessionID, r.Recipient.PaneInstanceID,
		r.ExpectedRecipientAgentRunID, r.PayloadSHA256,
		string(r.Status), r.CompatibilityCode, r.DiagnosticCode, now)
	if err != nil {
		return fmt.Errorf("persist driver receipt: %w", err)
	}
	if inserted, insertErr := result.RowsAffected(); insertErr != nil {
		return insertErr
	} else if inserted == 1 {
		return tx.Commit()
	}
	var deliveryID, grantID, senderSession, senderPrincipal, recipientSession, recipientPane, recipientRun, payloadHash, status, diagnostic string
	var epoch int64
	var compatibility int
	qerr := tx.QueryRowContext(ctx, `
		SELECT delivery_id, grant_id, grant_epoch, sender_session_id, sender_principal_id,
		       recipient_session_id, recipient_pane_instance_id,expected_recipient_agent_run_id,payload_sha256,
		       status, compatibility_code, diagnostic_code
		  FROM driver_receipts WHERE action_id = ? AND grant_epoch = ?`,
		r.ActionID, r.GrantEpoch).Scan(&deliveryID, &grantID, &epoch, &senderSession,
		&senderPrincipal, &recipientSession, &recipientPane, &recipientRun, &payloadHash, &status,
		&compatibility, &diagnostic)
	if qerr != nil {
		return fmt.Errorf("persist driver receipt: immutable receipt collision: %w", ErrIdempotencyBody)
	}
	if deliveryID != r.DeliveryID || grantID != r.GrantID || epoch != r.GrantEpoch ||
		senderSession != r.Sender.SessionID || senderPrincipal != r.SenderPrincipalID ||
		recipientSession != r.Recipient.SessionID || recipientPane != r.Recipient.PaneInstanceID ||
		recipientRun != r.ExpectedRecipientAgentRunID || payloadHash != r.PayloadSHA256 {
		return fmt.Errorf("persist driver receipt %s: %w", r.ActionID, ErrIdempotencyBody)
	}
	if !deliveryReceiptTransitionAllowed(ReceiptStatus(status), r.Status) {
		return fmt.Errorf("persist driver receipt %s status %s -> %s: %w",
			r.ActionID, status, r.Status, ErrIdempotencyBody)
	}
	if status == string(r.Status) {
		if compatibility != r.CompatibilityCode || diagnostic != r.DiagnosticCode {
			return fmt.Errorf("persist driver receipt %s changed canonical result: %w", r.ActionID, ErrIdempotencyBody)
		}
		return tx.Commit()
	}
	updated, err := tx.ExecContext(ctx, `UPDATE driver_receipts
		SET status=?, compatibility_code=?, diagnostic_code=?
		WHERE action_id=? AND grant_epoch=? AND status=?`, string(r.Status),
		r.CompatibilityCode, r.DiagnosticCode, r.ActionID, r.GrantEpoch, status)
	if err != nil {
		return fmt.Errorf("advance driver receipt: %w", err)
	}
	if n, err := updated.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return fmt.Errorf("advance driver receipt: %w", err)
		}
		return fmt.Errorf("advance driver receipt lost status race: %w", ErrIdempotencyBody)
	}
	return tx.Commit()
}

// deliveryReceiptTransitionAllowed mirrors Driver's documented append-only
// state machine. Reads may skip intermediate events, so accepted may be
// observed next as delivering or any terminal result. No terminal status ever
// changes, and uncertain never resolves by replay (it requires separate
// mechanical evidence or a separately authorized action).
func deliveryReceiptTransitionAllowed(from, to ReceiptStatus) bool {
	if from == to {
		return true
	}
	terminal := func(status ReceiptStatus) bool {
		switch status {
		case ReceiptSubmitted, ReceiptTyped, ReceiptUnverified, ReceiptRefused,
			ReceiptTargetMismatch, ReceiptFailed, ReceiptUncertain:
			return true
		default:
			return false
		}
	}
	switch from {
	case ReceiptAccepted:
		return to == ReceiptDelivering || terminal(to)
	case ReceiptDelivering:
		return terminal(to)
	default:
		return false
	}
}

func deliveryReceiptStatusKnown(status ReceiptStatus) bool {
	switch status {
	case ReceiptAccepted, ReceiptDelivering, ReceiptSubmitted, ReceiptTyped,
		ReceiptUnverified, ReceiptRefused, ReceiptTargetMismatch, ReceiptFailed,
		ReceiptUncertain:
		return true
	default:
		return false
	}
}

func (s SQLActionStore) PersistLifecycleReceipt(ctx context.Context, r LifecycleReceipt) error {
	if s.DB == nil || r.LifecycleReceiptID == "" || r.ActionID == "" || r.ActionEpoch < 1 ||
		r.Operation == "" || r.LifecycleKey == "" || r.TmuxServerDomainID == "" || r.TargetEpoch < 1 || r.Status == "" ||
		((r.Operation == "adopt" || r.Operation == "release") && r.ExternalWatchID == "") ||
		(r.Operation != "adopt" && r.Operation != "release" && r.Operation != "reattach" && r.ExternalWatchID != "") {
		return errors.New("driver sql store: incomplete lifecycle receipt")
	}
	before, err := json.Marshal(r.IdentityBefore)
	if err != nil {
		return err
	}
	after, err := json.Marshal(r.IdentityAfter)
	if err != nil {
		return err
	}
	now := s.now()
	_, err = s.DB.ExecContext(ctx, `INSERT INTO driver_lifecycle_receipts
		(lifecycle_receipt_id,action_id,action_epoch,operation,lifecycle_key,target_epoch,
		 lease_id,lease_epoch,tmux_server_domain_id,external_watch_id,status,identity_before_json,identity_after_json,
		 absence_observed_at,diagnostic_code,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.LifecycleReceiptID, r.ActionID,
		r.ActionEpoch, r.Operation, r.LifecycleKey, r.TargetEpoch, r.LeaseID, r.LeaseEpoch,
		r.TmuxServerDomainID, r.ExternalWatchID, r.Status, string(before), string(after), r.AbsenceObservedAt, r.DiagnosticCode, now, now)
	if err == nil {
		return nil
	}
	var gotID, operation, key, leaseID, domainID, watchID, status, gotBefore, gotAfter, absent, diagnostic string
	var actionEpoch, targetEpoch, leaseEpoch int64
	qerr := s.DB.QueryRowContext(ctx, `SELECT lifecycle_receipt_id,action_epoch,operation,
		lifecycle_key,target_epoch,lease_id,lease_epoch,tmux_server_domain_id,external_watch_id,status,identity_before_json,
		identity_after_json,absence_observed_at,diagnostic_code
		FROM driver_lifecycle_receipts WHERE action_id=?`,
		r.ActionID).Scan(&gotID, &actionEpoch, &operation, &key, &targetEpoch,
		&leaseID, &leaseEpoch, &domainID, &watchID, &status, &gotBefore, &gotAfter, &absent, &diagnostic)
	if qerr != nil {
		return fmt.Errorf("persist Driver lifecycle receipt: %w", err)
	}
	if gotID != r.LifecycleReceiptID || actionEpoch > r.ActionEpoch || operation != r.Operation ||
		key != r.LifecycleKey || targetEpoch != r.TargetEpoch || leaseID != r.LeaseID ||
		leaseEpoch > r.LeaseEpoch || domainID != r.TmuxServerDomainID || watchID != r.ExternalWatchID {
		return fmt.Errorf("persist Driver lifecycle receipt %s: %w", r.ActionID, ErrIdempotencyBody)
	}
	var existingBefore, existingAfter Identity
	if json.Unmarshal([]byte(gotBefore), &existingBefore) != nil || json.Unmarshal([]byte(gotAfter), &existingAfter) != nil {
		return fmt.Errorf("persist Driver lifecycle receipt %s: corrupt stored identity", r.ActionID)
	}
	// Driver lifecycle receipts are canonical records whose transport status can
	// move from uncertain to a mechanically verified terminal result. Immutable
	// request identity never changes, and a populated observation identity can
	// only be repeated—not replaced.
	if existingBefore != (Identity{}) && existingBefore != r.IdentityBefore ||
		existingAfter != (Identity{}) && existingAfter != r.IdentityAfter ||
		actionEpoch != r.ActionEpoch && !(LifecycleReceipt{Status: status}).Uncertain() ||
		!lifecycleReceiptTransitionAllowed(status, r.Status) {
		return fmt.Errorf("persist Driver lifecycle receipt %s: %w", r.ActionID, ErrIdempotencyBody)
	}
	if r.IdentityBefore == (Identity{}) {
		before = []byte(gotBefore)
	}
	if r.IdentityAfter == (Identity{}) {
		after = []byte(gotAfter)
	}
	if r.AbsenceObservedAt == "" {
		r.AbsenceObservedAt = absent
	}
	if r.DiagnosticCode == "" {
		r.DiagnosticCode = diagnostic
	}
	_, err = s.DB.ExecContext(ctx, `UPDATE driver_lifecycle_receipts SET action_epoch=?,lease_epoch=?,status=?,
		identity_before_json=?,identity_after_json=?,absence_observed_at=?,diagnostic_code=?,updated_at=?
		WHERE action_id=?`, r.ActionEpoch, r.LeaseEpoch, r.Status, string(before), string(after),
		r.AbsenceObservedAt, r.DiagnosticCode, now, r.ActionID)
	return err
}

func lifecycleReceiptTransitionAllowed(from, to string) bool {
	if from == to {
		return true
	}
	old := LifecycleReceipt{Status: from}
	next := LifecycleReceipt{Status: to}
	return old.Uncertain() && (next.Uncertain() || next.Resolved())
}
