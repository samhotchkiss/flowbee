package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrBootstrapActionConflict = errors.New("bootstrap action idempotency conflict")
	ErrBootstrapActionStale    = errors.New("bootstrap action claim is stale")
)

type BootstrapActionInput struct {
	ID, BootstrapID, ProjectID, Kind, PayloadJSON, PayloadSHA256 string
}

type BootstrapActionRecord struct {
	ID, BootstrapID, ProjectID, Kind, PayloadJSON, PayloadSHA256 string
	State                                                        string
	ActionEpoch, ClaimEpoch                                      int64
	ClaimOwner                                                   string
	ClaimDeadlineAt                                              time.Time
	Attempts, RecoveryCount                                      int
	NextAttemptAt                                                time.Time
	ReceiptID, ReceiptState, LastError                           string
	AlertPending                                                 bool
	CreatedAt, UpdatedAt                                         time.Time
}

func normalizeBootstrapAction(in BootstrapActionInput) (BootstrapActionInput, error) {
	in.ID, in.BootstrapID, in.ProjectID, in.Kind = strings.TrimSpace(in.ID), strings.TrimSpace(in.BootstrapID), strings.TrimSpace(in.ProjectID), strings.TrimSpace(in.Kind)
	if in.ID == "" || in.BootstrapID == "" || !projectIDPattern.MatchString(in.ProjectID) || len(in.ID) > 200 || len(in.BootstrapID) > 200 {
		return in, ErrBootstrapActionConflict
	}
	switch in.Kind {
	case "project_upsert", "repository_attach", "actor_route", "actor_lifecycle", "seat_bind", "managed_topology":
	default:
		return in, ErrBootstrapActionConflict
	}
	var payload any
	if json.Unmarshal([]byte(in.PayloadJSON), &payload) != nil || payload == nil {
		return in, ErrBootstrapActionConflict
	}
	sum := sha256.Sum256([]byte(in.PayloadJSON))
	if in.PayloadSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return in, ErrBootstrapActionConflict
	}
	return in, nil
}

// CommitBootstrapAction records immutable desired work before any product or
// Driver mutation. Exact replay returns the existing row; changed body, owner,
// or kind under the action id fails closed.
func (s *Store) CommitBootstrapAction(ctx context.Context, in BootstrapActionInput, now time.Time) (BootstrapActionRecord, error) {
	in, err := normalizeBootstrapAction(in)
	if err != nil {
		return BootstrapActionRecord{}, err
	}
	stamp := now.UTC().Format(rfc3339)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		existing, getErr := bootstrapActionTx(ctx, tx, in.ID)
		if getErr == nil {
			if existing.BootstrapID != in.BootstrapID || existing.ProjectID != in.ProjectID || existing.Kind != in.Kind ||
				existing.PayloadJSON != in.PayloadJSON || existing.PayloadSHA256 != in.PayloadSHA256 {
				return ErrBootstrapActionConflict
			}
			return nil
		}
		if !errors.Is(getErr, sql.ErrNoRows) {
			return getErr
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO bootstrap_actions
			(id,bootstrap_id,project_id,kind,payload_json,payload_sha256,state,next_attempt_at,created_at,updated_at)
			VALUES (?,?,?,?,?,?,'pending',?,?,?)`, in.ID, in.BootstrapID, in.ProjectID, in.Kind,
			in.PayloadJSON, in.PayloadSHA256, stamp, stamp, stamp); err != nil {
			return err
		}
		return appendBootstrapControlEventTx(ctx, tx, in.ProjectID, in.ID, "bootstrap_action_committed", "", "pending", 0, "{}", now)
	})
	if err != nil {
		return BootstrapActionRecord{}, err
	}
	return s.GetBootstrapAction(ctx, in.ID)
}

func (s *Store) GetBootstrapAction(ctx context.Context, id string) (BootstrapActionRecord, error) {
	return scanBootstrapAction(s.DB.QueryRowContext(ctx, `SELECT `+bootstrapActionColumns+` FROM bootstrap_actions WHERE id=?`, id))
}

// ClaimNextBootstrapAction gives one serve-owned runner an epoch-fenced lease.
// Uncertain actions are intentionally not automatically re-run: a separate
// mechanical fact or explicit recovery budget must resolve them.
func (s *Store) ClaimNextBootstrapAction(ctx context.Context, owner string, now time.Time, ttl time.Duration) (BootstrapActionRecord, error) {
	if strings.TrimSpace(owner) == "" || ttl <= 0 {
		return BootstrapActionRecord{}, ErrBootstrapActionStale
	}
	var out BootstrapActionRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM bootstrap_actions
			WHERE state='pending' AND (next_attempt_at='' OR julianday(next_attempt_at)<=julianday(?))
			ORDER BY next_attempt_at,created_at,id LIMIT 1`, now.UTC().Format(rfc3339)).Scan(&id); err != nil {
			return err
		}
		deadline := now.Add(ttl).UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state='claimed',action_epoch=action_epoch+1,
			claim_epoch=claim_epoch+1,claim_owner=?,claim_deadline_at=?,attempts=attempts+1,updated_at=?
			WHERE id=? AND state='pending'`, owner, deadline, now.UTC().Format(rfc3339), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrBootstrapActionStale
		}
		out, err = bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return appendBootstrapControlEventTx(ctx, tx, out.ProjectID, out.ID, "bootstrap_action_claimed", "pending", "claimed", out.ActionEpoch, "{}", now)
	})
	return out, err
}

func (s *Store) ClaimBootstrapAction(ctx context.Context, id, owner string, now time.Time, ttl time.Duration) (BootstrapActionRecord, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(owner) == "" || ttl <= 0 {
		return BootstrapActionRecord{}, ErrBootstrapActionStale
	}
	var out BootstrapActionRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		before, err := bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if before.State != "pending" || (!before.NextAttemptAt.IsZero() && before.NextAttemptAt.After(now)) {
			return ErrBootstrapActionStale
		}
		res, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state='claimed',action_epoch=action_epoch+1,
			claim_epoch=claim_epoch+1,claim_owner=?,claim_deadline_at=?,attempts=attempts+1,updated_at=?
			WHERE id=? AND state='pending' AND claim_epoch=?`, owner, now.Add(ttl).UTC().Format(rfc3339),
			now.UTC().Format(rfc3339), id, before.ClaimEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrBootstrapActionStale
		}
		out, err = bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		return appendBootstrapControlEventTx(ctx, tx, out.ProjectID, out.ID, "bootstrap_action_claimed", "pending", "claimed", out.ActionEpoch, "{}", now)
	})
	return out, err
}

func (s *Store) ListBootstrapActionsForVerification(ctx context.Context, limit int) ([]BootstrapActionRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT `+bootstrapActionColumns+` FROM bootstrap_actions
		WHERE state IN ('verifying','uncertain') ORDER BY updated_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BootstrapActionRecord
	for rows.Next() {
		row, err := scanBootstrapAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) RecordBootstrapActionReceipt(ctx context.Context, id, owner string, epoch int64,
	receiptID, receiptState string, uncertain bool, now time.Time) (BootstrapActionRecord, error) {
	if id == "" || owner == "" || epoch < 1 || receiptID == "" || receiptState == "" {
		return BootstrapActionRecord{}, ErrBootstrapActionStale
	}
	to := "verifying"
	if uncertain {
		to = "uncertain"
	}
	var out BootstrapActionRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		before, err := bootstrapActionTx(ctx, tx, id)
		if err != nil || before.State != "claimed" || before.ClaimOwner != owner || before.ClaimEpoch != epoch {
			return ErrBootstrapActionStale
		}
		alert := 0
		if uncertain {
			alert = 1
		}
		res, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state=?,receipt_id=?,receipt_state=?,
			claim_owner='',claim_deadline_at='',alert_pending=?,updated_at=?
			WHERE id=? AND state='claimed' AND claim_owner=? AND claim_epoch=?`, to, receiptID, receiptState,
			alert, now.UTC().Format(rfc3339), id, owner, epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrBootstrapActionStale
		}
		out, err = bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if uncertain {
			if err := upsertBootstrapAttentionTx(ctx, tx, out, "bootstrap_action_uncertain", now); err != nil {
				return err
			}
		}
		payload, _ := json.Marshal(map[string]string{"receipt_id": receiptID, "receipt_state": receiptState})
		return appendBootstrapControlEventTx(ctx, tx, out.ProjectID, id, "bootstrap_action_receipt_recorded", "claimed", to, out.ActionEpoch, string(payload), now)
	})
	return out, err
}

func (s *Store) CompleteBootstrapAction(ctx context.Context, id string, expectedEpoch int64, evidence string, now time.Time) (BootstrapActionRecord, error) {
	var out BootstrapActionRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		before, err := bootstrapActionTx(ctx, tx, id)
		if err != nil || before.ActionEpoch != expectedEpoch || (before.State != "verifying" && before.State != "uncertain") {
			return ErrBootstrapActionStale
		}
		res, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state='succeeded',alert_pending=0,
			last_error='',updated_at=? WHERE id=? AND action_epoch=? AND state IN ('verifying','uncertain')`,
			now.UTC().Format(rfc3339), id, expectedEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrBootstrapActionStale
		}
		out, err = bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='mechanical_fact',
			resolved_at=?,updated_at=? WHERE project_id=? AND dedup_key=? AND state<>'resolved'`,
			now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), out.ProjectID, "bootstrap-action:"+out.ID); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"evidence": evidence})
		return appendBootstrapControlEventTx(ctx, tx, out.ProjectID, id, "bootstrap_action_completed", before.State, "succeeded", out.ActionEpoch, string(payload), now)
	})
	return out, err
}

func (s *Store) HoldBootstrapAction(ctx context.Context, id, owner string, epoch int64, reason string, retryAt time.Time, terminal bool, now time.Time) (BootstrapActionRecord, error) {
	to := "held"
	if terminal {
		to = "dead_letter"
	}
	var out BootstrapActionRecord
	err := s.tx(ctx, func(tx *sql.Tx) error {
		before, err := bootstrapActionTx(ctx, tx, id)
		if err != nil || before.ClaimOwner != owner || before.ClaimEpoch != epoch || before.State != "claimed" {
			return ErrBootstrapActionStale
		}
		res, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state=?,claim_owner='',claim_deadline_at='',
			last_error=?,next_attempt_at=?,alert_pending=1,updated_at=? WHERE id=? AND state='claimed' AND claim_epoch=?`,
			to, strings.TrimSpace(reason), formatActorTime(retryAt), now.UTC().Format(rfc3339), id, epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrBootstrapActionStale
		}
		out, err = bootstrapActionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := upsertBootstrapAttentionTx(ctx, tx, out, reason, now); err != nil {
			return err
		}
		return appendBootstrapControlEventTx(ctx, tx, out.ProjectID, id, "bootstrap_action_"+to, "claimed", to, out.ActionEpoch, "{}", now)
	})
	return out, err
}

func (s *Store) RecoverExpiredBootstrapClaims(ctx context.Context, now time.Time) (int, error) {
	count := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM bootstrap_actions WHERE state='claimed'
			AND claim_deadline_at<>'' AND julianday(claim_deadline_at)<=julianday(?) ORDER BY id`, now.UTC().Format(rfc3339))
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			before, err := bootstrapActionTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state='uncertain',claim_owner='',
				claim_deadline_at='',recovery_count=recovery_count+1,alert_pending=1,last_error='claim expired after possible effect',updated_at=?
				WHERE id=? AND state='claimed' AND claim_epoch=?`, now.UTC().Format(rfc3339), id, before.ClaimEpoch); err != nil {
				return err
			}
			before.State, before.AlertPending, before.LastError = "uncertain", true, "claim expired after possible effect"
			if err := upsertBootstrapAttentionTx(ctx, tx, before, before.LastError, now); err != nil {
				return err
			}
			if err := appendBootstrapControlEventTx(ctx, tx, before.ProjectID, id, "bootstrap_action_claim_expired", "claimed", "uncertain", before.ActionEpoch, "{}", now); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

// RearmHeldBootstrapActions is the bounded capability-arrival seam. A runtime
// calls it only after the previously missing adapter/capability is positively
// advertised. It never re-arms uncertain delivery and never creates a new
// action identity.
func (s *Store) RearmHeldBootstrapActions(ctx context.Context, kind string, limit int, now time.Time) (int, error) {
	if limit <= 0 || limit > 100 {
		return 0, ErrBootstrapActionConflict
	}
	count := 0
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id FROM bootstrap_actions WHERE kind=? AND state='held'
			ORDER BY updated_at,id LIMIT ?`, kind, limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			before, err := bootstrapActionTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE bootstrap_actions SET state='pending',recovery_count=recovery_count+1,
				next_attempt_at=?,last_error='',alert_pending=0,updated_at=? WHERE id=? AND state='held'`,
				now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='capability_available',
				resolved_at=?,updated_at=? WHERE project_id=? AND dedup_key=? AND state<>'resolved'`,
				now.UTC().Format(rfc3339), now.UTC().Format(rfc3339), before.ProjectID, "bootstrap-action:"+id); err != nil {
				return err
			}
			if err := appendBootstrapControlEventTx(ctx, tx, before.ProjectID, id, "bootstrap_action_rearmed", "held", "pending", before.ActionEpoch, "{}", now); err != nil {
				return err
			}
			count++
		}
		return nil
	})
	return count, err
}

const bootstrapActionColumns = `id,bootstrap_id,project_id,kind,payload_json,payload_sha256,state,
	action_epoch,claim_owner,claim_epoch,claim_deadline_at,attempts,recovery_count,next_attempt_at,
	receipt_id,receipt_state,last_error,alert_pending,created_at,updated_at`

type bootstrapActionScanner interface{ Scan(...any) error }

func scanBootstrapAction(row bootstrapActionScanner) (BootstrapActionRecord, error) {
	var out BootstrapActionRecord
	var deadline, retry, created, updated string
	var alert int
	err := row.Scan(&out.ID, &out.BootstrapID, &out.ProjectID, &out.Kind, &out.PayloadJSON, &out.PayloadSHA256,
		&out.State, &out.ActionEpoch, &out.ClaimOwner, &out.ClaimEpoch, &deadline, &out.Attempts,
		&out.RecoveryCount, &retry, &out.ReceiptID, &out.ReceiptState, &out.LastError, &alert, &created, &updated)
	out.ClaimDeadlineAt, out.NextAttemptAt = parseOptionalTime(deadline), parseOptionalTime(retry)
	out.CreatedAt, out.UpdatedAt, out.AlertPending = parseOptionalTime(created), parseOptionalTime(updated), alert == 1
	return out, err
}

func bootstrapActionTx(ctx context.Context, tx *sql.Tx, id string) (BootstrapActionRecord, error) {
	return scanBootstrapAction(tx.QueryRowContext(ctx, `SELECT `+bootstrapActionColumns+` FROM bootstrap_actions WHERE id=?`, id))
}

func appendBootstrapControlEventTx(ctx context.Context, tx *sql.Tx, projectID, actionID, kind, from, to string, version int64, payload string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `INSERT INTO bootstrap_action_events
		(action_id,project_id,kind,from_state,to_state,action_epoch,payload_json,created_at)
		VALUES (?,?,?,?,?,?,?,?)`, actionID, projectID, kind, from, to, version, payload, stamp); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, projectID).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,?,?,?,?,'bootstrap',?,?,?)`, projectID, kind, from, to, version, actionID, payload, stamp)
	return err
}

func upsertBootstrapAttentionTx(ctx context.Context, tx *sql.Tx, action BootstrapActionRecord, detail string, now time.Time) error {
	// project_upsert is intentionally admitted before its projects row exists.
	// In that pre-project window alert_pending plus the append-only bootstrap
	// event is the durable visible signal; inserting project-scoped attention
	// would violate its FK and roll the hold itself back.
	var projectExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id=?`, action.ProjectID).Scan(&projectExists); err != nil {
		return err
	}
	if projectExists == 0 {
		return nil
	}
	dedup, stamp := "bootstrap-action:"+action.ID, now.UTC().Format(rfc3339)
	evidence, _ := json.Marshal(map[string]string{"action_id": action.ID, "kind": action.Kind, "state": action.State})
	res, err := tx.ExecContext(ctx, `UPDATE attention_items SET occurrences=occurrences+1,last_seen_at=?,
		evidence_json=?,detail=?,updated_at=? WHERE project_id=? AND dedup_key=? AND state<>'resolved'`,
		stamp, string(evidence), detail, stamp, action.ProjectID, dedup)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,priority,state,dedup_key,blocking,evidence_json,detail,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES (?,?, 'bootstrap_action_stalled',4,'open',?,1,?,?,?,?,?,?)`,
		"attention-"+action.ID, action.ProjectID, dedup, string(evidence), detail, stamp, stamp, stamp, stamp)
	if err != nil && isUniqueConstraintErr(err) {
		return nil
	}
	return err
}

func (a BootstrapActionRecord) String() string {
	return fmt.Sprintf("%s/%s/%s", a.ProjectID, a.Kind, a.State)
}
