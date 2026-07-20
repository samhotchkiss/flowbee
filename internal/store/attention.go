package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// AttentionItem is one row of the attention_items table (0027, epic-lane Phase 5).
// The queue is the master's DURABLE memory (plan §1): every actionable supervision
// condition is a typed, deduped, epoch-fenced row a fresh (post-`/clear`) master
// rediscovers idempotently. The pure decision core (which items to lease, is a fenced
// call live, when to escalate) lives in internal/attention; this file is the I/O seam
// that wires those decisions into serialized store transactions and ledgers each move.
type AttentionItem struct {
	ID             string
	ProjectID      string
	Kind           string
	EpicID         string
	Repo           string
	Priority       int // lower = more urgent (0021 convention)
	State          string
	DedupKey       string
	Blocking       bool
	LeasedBy       string // supervisors.id holding the lease
	ItemEpoch      int    // the item's own monotonic fence (bumped on every lease)
	LeaseExpiresAt string
	AwaitingSince  string // when the row entered awaiting_ack (the send-and-ack clock, §12.3)
	DeliveryKey    string
	Evidence       map[string]string
	Detail         string
	Resolution     string
	Verdict        string
	Occurrences    int
	FirstSeenAt    string
	LastSeenAt     string
	ResolvedAt     string
	CreatedAt      string
	UpdatedAt      string
}

var (
	// ErrAttentionNotFound is returned when an item id does not exist.
	ErrAttentionNotFound = errors.New("attention item not found")
	// ErrAttentionState is returned when an item is not in the state a transition
	// requires (e.g. AckAttention on an item that is not awaiting_ack).
	ErrAttentionState = errors.New("attention item not in the expected state")
	// ErrAttentionProject is returned when a caller tries to address an item through
	// a project other than the immutable owner derived from its epic or producer.
	ErrAttentionProject = errors.New("attention item project mismatch")
)

// A fenced deliver/resolve call from a superseded incarnation is rejected by wrapping
// lease.ErrStaleEpoch — identical fencing semantics to the jobs engine, so the API
// layer's existing 409-fenced mapping (errors.Is(err, lease.ErrStaleEpoch)) applies
// unchanged.

const attentionSelect = `
	SELECT id, project_id, kind, epic_id, repo, priority, state, dedup_key, blocking, leased_by,
	       item_epoch, lease_expires_at, awaiting_since, delivery_key, evidence_json, detail,
	       resolution, verdict, occurrences, first_seen_at, last_seen_at, resolved_at,
	       created_at, updated_at
	  FROM attention_items`

func scanAttentionItem(row rowScanner) (AttentionItem, error) {
	var a AttentionItem
	var blocking int
	var evidenceJSON string
	err := row.Scan(&a.ID, &a.ProjectID, &a.Kind, &a.EpicID, &a.Repo, &a.Priority, &a.State, &a.DedupKey,
		&blocking, &a.LeasedBy, &a.ItemEpoch, &a.LeaseExpiresAt, &a.AwaitingSince, &a.DeliveryKey,
		&evidenceJSON, &a.Detail, &a.Resolution, &a.Verdict, &a.Occurrences, &a.FirstSeenAt,
		&a.LastSeenAt, &a.ResolvedAt, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return AttentionItem{}, ErrAttentionNotFound
	}
	if err != nil {
		return AttentionItem{}, err
	}
	a.Blocking = blocking != 0
	a.Evidence = unmarshalEvidence(evidenceJSON)
	return a, nil
}

// GetAttentionItem returns one item by id (for tests + the read-only view).
func (s *Store) GetAttentionItem(ctx context.Context, id string) (AttentionItem, error) {
	return s.GetAttentionItemForProject(ctx, "default", id)
}

// GetAttentionItemForProject returns an item only through its exact project
// namespace. A cross-project id is indistinguishable from a missing id.
func (s *Store) GetAttentionItemForProject(ctx context.Context, projectID, id string) (AttentionItem, error) {
	if projectID == "" {
		return AttentionItem{}, ErrAttentionProject
	}
	return scanAttentionItem(s.DB.QueryRowContext(ctx, attentionSelect+` WHERE project_id = ? AND id = ?`, projectID, id))
}

// UpsertAttentionItem is the ONE entry point every producer calls (plan §1.3 "Dedup
// discipline"). If an ACTIVE item already exists for the dedup_key it bumps
// occurrences + last_seen_at and refreshes evidence/detail/priority (created=false) —
// NEVER a second row. Otherwise it inserts a fresh open item (created=true) and
// ledgers attention_opened. The open set is thus a pure function of current reality,
// not of how many ticks fired — the property that makes a fresh master idempotent. The
// SELECT-then-INSERT is safe under the store's MaxOpenConns=1 serialization; the
// partial UNIQUE index (state IN active-set) is the structural backstop.
func (s *Store) UpsertAttentionItem(ctx context.Context, item AttentionItem, now time.Time) (created bool, id string, err error) {
	if item.ID == "" {
		return false, "", errors.New("attention item id is required")
	}
	if item.Kind == "" {
		return false, "", errors.New("attention kind is required")
	}
	if item.DedupKey == "" {
		return false, "", errors.New("attention dedup_key is required")
	}
	ts := now.Format(rfc3339)
	evidenceJSON := marshalEvidence(item.Evidence)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		projectID, e := attentionOwnerProjectTx(ctx, tx, item)
		if e != nil {
			return e
		}
		item.ProjectID = projectID
		existingID, found, e := activeItemIDByDedupTx(ctx, tx, projectID, item.DedupKey)
		if e != nil {
			return e
		}
		if found {
			// refresh the live item — a re-seen condition, never a duplicate row.
			if err := refreshActiveAttentionTx(ctx, tx, existingID, item, evidenceJSON, ts); err != nil {
				return err
			}
			created = false
			id = existingID
			return nil
		}
		// NOTE (awaiting_since): deliberately omitted from the INSERT — a fresh row is
		// 'open', so it defaults to '' and is set ONLY on entry to awaiting_ack (below).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO attention_items
			    (id, project_id, kind, epic_id, repo, priority, state, dedup_key, blocking, leased_by,
			     item_epoch, lease_expires_at, delivery_key, evidence_json, detail, resolution,
			     verdict, occurrences, first_seen_at, last_seen_at, resolved_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, '', 0, '', '', ?, ?, '', '', 1, ?, ?, '', ?, ?)`,
			item.ID, projectID, item.Kind, item.EpicID, item.Repo, item.Priority, item.DedupKey, b2i(item.Blocking),
			evidenceJSON, item.Detail, ts, ts, ts, ts); err != nil {
			if isUniqueConstraintErr(err) {
				// The partial UNIQUE index fired: an active row for this dedup_key already
				// exists (unreachable under MaxOpenConns=1, but honor the comment). Fall back
				// to a refresh so a producer NEVER surfaces a raw constraint error.
				existingID, found, e2 := activeItemIDByDedupTx(ctx, tx, projectID, item.DedupKey)
				if e2 != nil {
					return e2
				}
				if found {
					if err := refreshActiveAttentionTx(ctx, tx, existingID, item, evidenceJSON, ts); err != nil {
						return err
					}
					created = false
					id = existingID
					return nil
				}
				return fmt.Errorf("upsert attention %q: %w", item.ID, err)
			}
			return fmt.Errorf("insert attention %q: %w", item.ID, err)
		}
		created = true
		id = item.ID
		return appendEpicLedger(ctx, tx, ledgerKeyFor(item.EpicID, item.ID),
			ledger.KindAttentionOpened, "system", 0, item.ID, item.Kind, now)
	})
	return created, id, err
}

// attentionOwnerProjectTx derives ownership from the immutable epic whenever an
// epic is present. ProjectID supplied by a caller is an assertion, never authority.
// Empty-project, missing-epic rows retain the legacy default-project behavior used
// by operational attention created before Phase 2; new non-default producers must
// name an existing project explicitly.
func attentionOwnerProjectTx(ctx context.Context, tx *sql.Tx, item AttentionItem) (string, error) {
	asserted := strings.TrimSpace(item.ProjectID)
	if item.EpicID != "" {
		var owner string
		err := tx.QueryRowContext(ctx, `SELECT project_id FROM epics WHERE id=?`, item.EpicID).Scan(&owner)
		if err == nil {
			if asserted != "" && asserted != owner {
				return "", fmt.Errorf("%w: project %q does not own epic %q (owner %q)",
					ErrAttentionProject, asserted, item.EpicID, owner)
			}
			return owner, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if asserted != "" && asserted != "default" {
			return "", fmt.Errorf("%w: epic %q has no durable owner", ErrAttentionProject, item.EpicID)
		}
	}
	if asserted == "" {
		return "default", nil
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, asserted).Scan(&exists); err != nil {
		return "", err
	}
	if exists != 1 {
		return "", fmt.Errorf("%w: project %q does not exist", ErrAttentionProject, asserted)
	}
	return asserted, nil
}

// activeItemIDByDedupTx returns the id of the (at most one) ACTIVE item for a dedup_key.
func activeItemIDByDedupTx(ctx context.Context, tx *sql.Tx, projectID, dedupKey string) (string, bool, error) {
	var id string
	e := tx.QueryRowContext(ctx,
		`SELECT id FROM attention_items WHERE project_id = ? AND dedup_key = ? AND state IN `+attentionActiveStatesSQL+` LIMIT 1`,
		projectID, dedupKey).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return "", false, nil
	}
	if e != nil {
		return "", false, e
	}
	return id, true, nil
}

// refreshActiveAttentionTx bumps occurrences/last_seen_at and refreshes the mutable
// producer fields on a re-seen active item. It deliberately does NOT touch awaiting_since
// (the send-and-ack clock) or the lease/state columns — a refresh of a leased/delivering/
// awaiting_ack row must not disturb the master's in-flight work.
func refreshActiveAttentionTx(ctx context.Context, tx *sql.Tx, id string, item AttentionItem, evidenceJSON, ts string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE attention_items
		   SET occurrences = occurrences + 1, last_seen_at = ?, evidence_json = ?,
		       detail = ?, priority = ?, blocking = ?, repo = ?, epic_id = ?, updated_at = ?
		 WHERE id = ? AND project_id = ?`,
		ts, evidenceJSON, item.Detail, item.Priority, b2i(item.Blocking), item.Repo, item.EpicID, ts, id, item.ProjectID)
	return err
}

// AutoResolveCleared resolves a NOT-IN-FLIGHT item for a dedup_key with
// resolution='cleared' — the producer's call when the underlying condition clears (pane
// left AWAITING_INPUT, CI green, account dropped below critical). A no-op (resolved=false)
// if nothing matches. This is the other half of dedup discipline: the open set tracks
// current reality in BOTH directions.
//
// Scoped to state IN ('open','awaiting_ack') by design: a 'leased'/'delivering' row is the
// MASTER's to finish — yanking it out from under an in-flight steer (or overwriting a
// just-recorded verdict with 'cleared') would corrupt the exactly-once loop. If the
// condition genuinely cleared while the master held the lease, its own resolve closes the
// item, and the NEXT producer tick's re-upsert simply no-ops. For 'open' (never leased) and
// 'awaiting_ack' (delivered, awaiting confirmation) clear-wins is correct: reality moved on.
func (s *Store) AutoResolveCleared(ctx context.Context, dedupKey string, now time.Time) (resolved bool, err error) {
	return s.AutoResolveClearedForProject(ctx, "default", dedupKey, now)
}

func (s *Store) AutoResolveClearedForProject(ctx context.Context, projectID, dedupKey string, now time.Time) (resolved bool, err error) {
	if projectID == "" {
		return false, ErrAttentionProject
	}
	ts := now.Format(rfc3339)
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var id, epicID string
		var itemEpoch int
		e := tx.QueryRowContext(ctx,
			`SELECT id, epic_id, item_epoch FROM attention_items WHERE project_id = ? AND dedup_key = ? AND state IN ('open','awaiting_ack') LIMIT 1`,
			projectID, dedupKey).Scan(&id, &epicID, &itemEpoch)
		if errors.Is(e, sql.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'resolved', resolution = 'cleared', leased_by = '', delivery_key = '',
			       resolved_at = ?, updated_at = ?
			 WHERE id = ? AND project_id = ?`, ts, ts, id, projectID); err != nil {
			return err
		}
		resolved = true
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id),
			ledger.KindAttentionResolved, "system", itemEpoch, id, "cleared", now)
	})
	return resolved, err
}

// LeaseAttention atomically leases up to max open items to a master (plan §1.4). It
// FENCES the supervisor first (must exist, be active, and present the live epoch — a
// stale master is a superseded incarnation, rejected with lease.ErrStaleEpoch), then
// asks the pure core (attention.GrantLease) which eligible open items to grant given
// max/kinds and the ONE-in-flight-item-per-epic rule (never two masters, and never one
// master twice, driving one pane). Each granted row flips open->leased, bumps
// item_epoch (its fence), sets lease_expires_at=now+ttl, and ledgers attention_leased.
// Returns the leased rows (with the bumped item_epoch), most-urgent first.
func (s *Store) LeaseAttention(ctx context.Context, masterID string, epoch, max int, kinds []string, ttl time.Duration, now time.Time) ([]AttentionItem, error) {
	return s.LeaseAttentionForProject(ctx, "default", masterID, epoch, max, kinds, ttl, now)
}

func (s *Store) LeaseAttentionForProject(ctx context.Context, projectID, masterID string, epoch, max int, kinds []string, ttl time.Duration, now time.Time) ([]AttentionItem, error) {
	if projectID == "" {
		return nil, ErrAttentionProject
	}
	var out []AttentionItem
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var liveEpoch int
		var state string
		e := tx.QueryRowContext(ctx, `SELECT epoch, state FROM supervisors WHERE id = ?`, masterID).
			Scan(&liveEpoch, &state)
		if errors.Is(e, sql.ErrNoRows) {
			return ErrSupervisorNotFound
		}
		if e != nil {
			return e
		}
		if state != "active" || liveEpoch != epoch {
			return fmt.Errorf("lease attention as %q (epoch %d, live %d, state %s): %w",
				masterID, epoch, liveEpoch, state, lease.ErrStaleEpoch)
		}
		open, inflight, err := gatherAttentionForLease(ctx, tx, projectID)
		if err != nil {
			return err
		}
		granted := attention.GrantLease(open, inflight, max, kinds)
		ts := now.Format(rfc3339)
		exp := now.Add(ttl).Format(rfc3339)
		for _, g := range granted {
			newItemEpoch := g.ItemEpoch + 1
			res, err := tx.ExecContext(ctx, `
				UPDATE attention_items
				   SET state = 'leased', leased_by = ?, item_epoch = ?, lease_expires_at = ?, updated_at = ?
				 WHERE id = ? AND project_id = ? AND state = 'open'`,
				masterID, newItemEpoch, exp, ts, g.ID, projectID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				// the row moved out of 'open' since gather (impossible under serialization,
				// but never fabricate a lease we did not win).
				continue
			}
			if err := appendEpicLedger(ctx, tx, ledgerKeyFor(g.EpicID, g.ID),
				ledger.KindAttentionLeased, masterID, newItemEpoch, g.ID, string(g.Kind), now); err != nil {
				return err
			}
			it, err := scanAttentionItem(tx.QueryRowContext(ctx, attentionSelect+` WHERE id = ? AND project_id = ?`, g.ID, projectID))
			if err != nil {
				return err
			}
			out = append(out, it)
		}
		return nil
	})
	return out, err
}

// BeginDelivery is the FENCED transition marking an item state=delivering with its
// idempotency key, taken just before a verified send into the epic pane (plan §1.5
// step 3). It rejects unless the item is leased-by-caller with a matching item_epoch
// AND the caller's supervisor epoch matches the live one (attention.FenceOK) — else
// lease.ErrStaleEpoch (409 fenced). delivery_key is the crash-window recovery anchor:
// the stranded-delivery reaper re-captures the pane and matches against it rather than
// blindly re-sending.
func (s *Store) BeginDelivery(ctx context.Context, id, masterID string, epoch, itemEpoch int, deliveryKey string, now time.Time) error {
	return s.BeginDeliveryForProject(ctx, "default", id, masterID, epoch, itemEpoch, deliveryKey, now)
}

func (s *Store) BeginDeliveryForProject(ctx context.Context, projectID, id, masterID string, epoch, itemEpoch int, deliveryKey string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	if deliveryKey == "" {
		return errors.New("delivery_key is required")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		f, err := loadFenceForProjectTx(ctx, tx, projectID, id, masterID, epoch, itemEpoch, attention.StateLeased)
		if err != nil {
			return err
		}
		if !attention.FenceOK(f) {
			return fmt.Errorf("begin delivery %q: %w", id, lease.ErrStaleEpoch)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE attention_items SET state = 'delivering', delivery_key = ?, updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = 'leased'`,
			deliveryKey, now.Format(rfc3339), id, projectID)
		return err
	})
}

// RecordDeliveryVerdict records the outcome of a verified send (plan §1.5 step 3),
// fenced identically to BeginDelivery (the item must be delivering-by-caller):
//   - strong | weak -> state=awaiting_ack (NOT resolved: the send-and-ack loop, §12.3,
//     re-checks that the steer was PROCESSED before resolving);
//   - failed        -> back to state=open with detail=delivery_failed (the caller does a
//     fast master retry, plan §15.4; persistent failure escalates as blocked_non_resumable).
//
// Every verdict ledgers epic_intervention.
func (s *Store) RecordDeliveryVerdict(ctx context.Context, id, masterID string, epoch, itemEpoch int, verdict string, now time.Time) error {
	return s.RecordDeliveryVerdictForProject(ctx, "default", id, masterID, epoch, itemEpoch, verdict, now)
}

func (s *Store) RecordDeliveryVerdictForProject(ctx context.Context, projectID, id, masterID string, epoch, itemEpoch int, verdict string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	switch verdict {
	case "strong", "weak", "failed":
	default:
		return fmt.Errorf("invalid delivery verdict %q (want strong|weak|failed)", verdict)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		f, err := loadFenceForProjectTx(ctx, tx, projectID, id, masterID, epoch, itemEpoch, attention.StateDelivering)
		if err != nil {
			return err
		}
		if !attention.FenceOK(f) {
			return fmt.Errorf("record delivery verdict %q: %w", id, lease.ErrStaleEpoch)
		}
		var epicID string
		if e := tx.QueryRowContext(ctx, `SELECT epic_id FROM attention_items WHERE id = ? AND project_id = ?`, id, projectID).Scan(&epicID); e != nil {
			return e
		}
		ts := now.Format(rfc3339)
		if verdict == "failed" {
			_, err = tx.ExecContext(ctx, `
				UPDATE attention_items
				   SET state = 'open', detail = 'delivery_failed', verdict = 'failed',
				       leased_by = '', delivery_key = '', lease_expires_at = '', updated_at = ?
				 WHERE id = ? AND project_id = ? AND state = 'delivering'`, ts, id, projectID)
		} else {
			// entry into awaiting_ack: stamp the send-and-ack clock (awaiting_since), the
			// ONLY place it is written alongside RecoverStrandedAwaitingAck.
			_, err = tx.ExecContext(ctx, `
				UPDATE attention_items SET state = 'awaiting_ack', verdict = ?, awaiting_since = ?, updated_at = ?
				 WHERE id = ? AND project_id = ? AND state = 'delivering'`, verdict, ts, ts, id, projectID)
		}
		if err != nil {
			return err
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id),
			ledger.KindEpicIntervention, masterID, itemEpoch, id, verdict, now)
	})
}

// AckAttention closes an awaiting_ack item as resolution='acked' (the send-and-ack loop
// confirmed the steer was PROCESSED, plan §12.3). ErrAttentionNotFound if the id is
// unknown; ErrAttentionState if it is not awaiting_ack.
func (s *Store) AckAttention(ctx context.Context, id string, now time.Time) error {
	return s.AckAttentionForProject(ctx, "default", id, now)
}

func (s *Store) AckAttentionForProject(ctx context.Context, projectID, id string, now time.Time) error {
	return s.resolveFromStateTx(ctx, projectID, id, "acked", attention.StateAwaitingAck,
		ledger.KindAttentionResolved, now)
}

// ReopenUnacked reopens an awaiting_ack item as detail='steer_not_processed' (the ack
// loop saw no behavior change within T_ack, plan §12.3 — a politely-stalling agent that
// absorbed a nudge and kept drifting must not look handled). The item returns to open,
// its lease cleared, item_epoch preserved (a late fenced call from the old lease still
// fails). Ledgers attention_opened (the reopen). ErrAttentionState if not awaiting_ack.
func (s *Store) ReopenUnacked(ctx context.Context, id string, now time.Time) error {
	return s.ReopenUnackedForProject(ctx, "default", id, now)
}

func (s *Store) ReopenUnackedForProject(ctx context.Context, projectID, id string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		epicID, itemEpoch, _, err := loadItemMetaForProjectTx(ctx, tx, projectID, id)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'open', detail = 'steer_not_processed', leased_by = '',
			       delivery_key = '', lease_expires_at = '', awaiting_since = '', updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = 'awaiting_ack'`, ts, id, projectID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAttentionState
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id),
			ledger.KindAttentionOpened, "system", itemEpoch, id, "steer_not_processed", now)
	})
}

// ResolveAttention resolves an active item with the given disposition — the dismiss and
// escalate paths (plan §1.5 "Other actions"). resolution is the disposition string
// (e.g. "dismissed", "escalated"); an "escalated" disposition ledgers attention_escalated
// (routed to the operator), any other ledgers attention_resolved. ErrAttentionState if
// the item is already resolved.
func (s *Store) ResolveAttention(ctx context.Context, id, resolution string, now time.Time) error {
	return s.ResolveAttentionForProject(ctx, "default", id, resolution, now)
}

func (s *Store) ResolveAttentionForProject(ctx context.Context, projectID, id, resolution string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	if resolution == "" {
		return errors.New("resolution is required")
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		epicID, itemEpoch, state, err := loadItemMetaForProjectTx(ctx, tx, projectID, id)
		if err != nil {
			return err
		}
		if state == attention.StateResolved {
			return ErrAttentionState
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'resolved', resolution = ?, leased_by = '', delivery_key = '',
			       resolved_at = ?, updated_at = ?
			 WHERE id = ? AND project_id = ?`, resolution, ts, ts, id, projectID); err != nil {
			return err
		}
		kind := ledger.KindAttentionResolved
		if resolution == "escalated" {
			kind = ledger.KindAttentionEscalated
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id), kind, "system", itemEpoch, id, resolution, now)
	})
}

// ResolveAttentionFenced is the FENCED no-send resolve (dismiss/ack/escalate — plan §1.5
// "Other actions") the master API's resolve path uses. It closes the TOCTOU a Go-side
// read-then-resolve leaves open (m1): the fence check AND the write happen in ONE serialized
// tx, rejecting unless the item is leased-by-caller with a matching item_epoch AND the
// caller's supervisor epoch matches the live one (attention.FenceOK) — else
// lease.ErrStaleEpoch (409 fenced), identical to BeginDelivery. So a stale incarnation can
// never dismiss/escalate an item another master re-leased across an epoch boundary between
// the read and the write. An "escalated" resolution ledgers attention_escalated (routed to
// the operator sink in Phase 7); anything else ledgers attention_resolved.
func (s *Store) ResolveAttentionFenced(ctx context.Context, id, masterID string, epoch, itemEpoch int, resolution string, now time.Time) error {
	return s.ResolveAttentionFencedForProject(ctx, "default", id, masterID, epoch, itemEpoch, resolution, now)
}

func (s *Store) ResolveAttentionFencedForProject(ctx context.Context, projectID, id, masterID string, epoch, itemEpoch int, resolution string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	if resolution == "" {
		return errors.New("resolution is required")
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		f, err := loadFenceForProjectTx(ctx, tx, projectID, id, masterID, epoch, itemEpoch, attention.StateLeased)
		if err != nil {
			return err
		}
		if !attention.FenceOK(f) {
			return fmt.Errorf("resolve attention %q: %w", id, lease.ErrStaleEpoch)
		}
		var epicID string
		if e := tx.QueryRowContext(ctx, `SELECT epic_id FROM attention_items WHERE id = ? AND project_id = ?`, id, projectID).Scan(&epicID); e != nil {
			return e
		}
		// belt-and-suspenders: the WHERE re-asserts the fence atomically with the write.
		res, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'resolved', resolution = ?, leased_by = '', delivery_key = '',
			       resolved_at = ?, updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = 'leased' AND leased_by = ? AND item_epoch = ?`,
			resolution, ts, ts, id, projectID, masterID, itemEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("resolve attention %q: %w", id, lease.ErrStaleEpoch)
		}
		kind := ledger.KindAttentionResolved
		if resolution == "escalated" {
			kind = ledger.KindAttentionEscalated
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id), kind, masterID, itemEpoch, id, resolution, now)
	})
}

// ReapExpiredLeases returns every LEASED item whose lease has expired back to open
// (plan §1.4/§1.6) — the master died or stalled and the lease TTL passed. Returns the
// reaped rows. Deliberately does NOT touch state=delivering rows: a crash mid-send is
// handled by ListStrandedDeliveries (a pane re-check, never a blind reopen). Timestamps
// are parsed in Go rather than compared as SQL strings (RFC3339Nano does not sort
// lexically across the fractional-second boundary).
func (s *Store) ReapExpiredLeases(ctx context.Context, now time.Time) ([]AttentionItem, error) {
	var reaped []AttentionItem
	ts := now.Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		expired, err := expiredIDsTx(ctx, tx, "leased", now)
		if err != nil {
			return err
		}
		for _, ex := range expired {
			res, err := tx.ExecContext(ctx, `
				UPDATE attention_items
				   SET state = 'open', leased_by = '', delivery_key = '', lease_expires_at = '', updated_at = ?
				 WHERE id = ? AND project_id = ? AND state = 'leased'`, ts, ex.id, ex.projectID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue
			}
			if err := appendEpicLedger(ctx, tx, ledgerKeyFor(ex.epicID, ex.id),
				ledger.KindAttentionOpened, "system", ex.itemEpoch, ex.id, "lease_expired", now); err != nil {
				return err
			}
			it, err := scanAttentionItem(tx.QueryRowContext(ctx, attentionSelect+` WHERE id = ? AND project_id = ?`, ex.id, ex.projectID))
			if err != nil {
				return err
			}
			reaped = append(reaped, it)
		}
		return nil
	})
	return reaped, err
}

// ListStrandedDeliveries returns every DELIVERING item whose lease TTL has passed — a
// master that crashed between a verified send and recording the verdict (plan §1.5
// "Crash-window handling"). It does NOT modify state: the caller re-captures the target
// pane and asks tmuxio "does the last submitted line already match this payload?" —
// yes -> mark awaiting_ack (idempotent recovery, no second send); moved on -> reopen for
// a fresh decision. The one thing we NEVER do is blindly re-send, so this is a pure read.
func (s *Store) ListStrandedDeliveries(ctx context.Context, now time.Time) ([]AttentionItem, error) {
	expired, err := expiredIDsTxDB(ctx, s.DB, "delivering", now)
	if err != nil {
		return nil, err
	}
	var out []AttentionItem
	for _, ex := range expired {
		it, err := s.GetAttentionItemForProject(ctx, ex.projectID, ex.id)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// RecoverStrandedAwaitingAck is the crash-window recovery WRITE for the case where the
// ticker's pane re-check found the payload ALREADY submitted (plan §1.5 "Crash-window
// handling"): the verified send landed but the crashed master never recorded a verdict.
// This is a SYSTEM-actor, UNFENCED (the leaseholder is dead — there is no live epoch to
// present), STATE-GUARDED move delivering->awaiting_ack. It is guarded by delivery_key so
// a relaunched/moved-on pane whose key differs is never silently acked (that case takes
// ReopenStranded instead). Idempotent: a second call (already awaiting_ack) no-ops. Stamps
// awaiting_since (the ack loop's clock) so a recovered delivery still ack-expires normally.
// ErrAttentionState if the row is not delivering-with-this-key (and not already recovered).
func (s *Store) RecoverStrandedAwaitingAck(ctx context.Context, id, deliveryKey string, now time.Time) error {
	return s.RecoverStrandedAwaitingAckForProject(ctx, "default", id, deliveryKey, now)
}

func (s *Store) RecoverStrandedAwaitingAckForProject(ctx context.Context, projectID, id, deliveryKey string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	if deliveryKey == "" {
		return errors.New("delivery_key is required")
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		epicID, itemEpoch, state, err := loadItemMetaForProjectTx(ctx, tx, projectID, id)
		if err != nil {
			return err
		}
		if state == attention.StateAwaitingAck {
			return nil // idempotent: an earlier recovery (or verdict) already advanced it
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'awaiting_ack', detail = 'recovered_stranded', awaiting_since = ?, updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = 'delivering' AND delivery_key = ?`, ts, ts, id, projectID, deliveryKey)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAttentionState
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id),
			ledger.KindEpicIntervention, "system", itemEpoch, id, "recovered", now)
	})
}

// ReopenStranded is the crash-window recovery WRITE for the case where the ticker's pane
// re-check found the payload NOT submitted / the pane moved on (plan §1.5): a fresh
// decision is needed. A SYSTEM-actor, UNFENCED, STATE-GUARDED move delivering->open that
// clears leased_by/delivery_key/lease_expires_at (freeing the dedup_key's active slot AND
// the epic's one-in-flight slot for a re-lease) while PRESERVING item_epoch — so a late
// fenced RecordDeliveryVerdict from the dead master still rejects (its state/leaseholder
// no longer match, and a subsequent re-lease bumps item_epoch out from under it). Ledgers
// attention_opened. ErrAttentionState if the row is not delivering.
func (s *Store) ReopenStranded(ctx context.Context, id string, now time.Time) error {
	return s.ReopenStrandedForProject(ctx, "default", id, now)
}

func (s *Store) ReopenStrandedForProject(ctx context.Context, projectID, id string, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		epicID, itemEpoch, _, err := loadItemMetaForProjectTx(ctx, tx, projectID, id)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'open', detail = 'delivery_stranded', leased_by = '', delivery_key = '',
			       lease_expires_at = '', updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = 'delivering'`, ts, id, projectID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAttentionState
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id),
			ledger.KindAttentionOpened, "system", itemEpoch, id, "delivery_stranded", now)
	})
}

// ListOpenAttention is the read-only queue view (plan §1.4 "GET .../attention"). state
// filters to a single state ("" = all active states); kinds (if non-empty) and repo (if
// non-empty) narrow further. Most-urgent first (priority asc, then oldest first_seen_at).
func (s *Store) ListOpenAttention(ctx context.Context, state string, kinds []string, repo string) ([]AttentionItem, error) {
	return s.ListOpenAttentionForProject(ctx, "default", state, kinds, repo)
}

// ListAllOpenAttention is reserved for internal portfolio reconcilers that must
// intentionally enumerate projects and then use project-scoped transition APIs.
func (s *Store) ListAllOpenAttention(ctx context.Context, state string, kinds []string, repo string) ([]AttentionItem, error) {
	return s.listOpenAttention(ctx, "", state, kinds, repo)
}

func (s *Store) ListOpenAttentionForProject(ctx context.Context, projectID, state string, kinds []string, repo string) ([]AttentionItem, error) {
	if projectID == "" {
		return nil, ErrAttentionProject
	}
	return s.listOpenAttention(ctx, projectID, state, kinds, repo)
}

func (s *Store) listOpenAttention(ctx context.Context, projectID, state string, kinds []string, repo string) ([]AttentionItem, error) {
	var where []string
	var args []any
	if projectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, projectID)
	}
	if state == "" {
		where = append(where, `state IN `+attentionActiveStatesSQL)
	} else {
		where = append(where, `state = ?`)
		args = append(args, state)
	}
	if len(kinds) > 0 {
		ph := make([]string, len(kinds))
		for i, k := range kinds {
			ph[i] = "?"
			args = append(args, k)
		}
		where = append(where, `kind IN (`+strings.Join(ph, ",")+`)`)
	}
	if repo != "" {
		where = append(where, `repo = ?`)
		args = append(args, repo)
	}
	q := attentionSelect + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY priority ASC, first_seen_at ASC, id ASC`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AttentionItem
	for rows.Next() {
		a, err := scanAttentionItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ── internal helpers ──

// attentionActiveStatesSQL is the in-flight state set: the exact predicate the partial
// UNIQUE index uses. A single constant so the dedup SELECT, the read view, and the index
// can never drift on what "active" means.
const attentionActiveStatesSQL = `('open','leased','delivering','awaiting_ack')`

// resolveFromStateTx resolves an item that must currently be in `fromState`, stamping
// resolution + resolved_at and ledgering `kind`. ErrAttentionState if the state guard
// fails (the row exists but is not in fromState).
func (s *Store) resolveFromStateTx(ctx context.Context, projectID, id, resolution, fromState string, kind ledger.EventKind, now time.Time) error {
	if projectID == "" {
		return ErrAttentionProject
	}
	ts := now.Format(rfc3339)
	return s.tx(ctx, func(tx *sql.Tx) error {
		epicID, itemEpoch, _, err := loadItemMetaForProjectTx(ctx, tx, projectID, id)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE attention_items
			   SET state = 'resolved', resolution = ?, leased_by = '', delivery_key = '',
			       resolved_at = ?, updated_at = ?
			 WHERE id = ? AND project_id = ? AND state = ?`, resolution, ts, ts, id, projectID, fromState)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrAttentionState
		}
		return appendEpicLedger(ctx, tx, ledgerKeyFor(epicID, id), kind, "system", itemEpoch, id, resolution, now)
	})
}

// loadItemMetaTx reads the epic id, item_epoch, and state for an item, or
// ErrAttentionNotFound. Used by the transition helpers for the ledger key + state guard.
func loadItemMetaTx(ctx context.Context, tx *sql.Tx, id string) (epicID string, itemEpoch int, state string, err error) {
	e := tx.QueryRowContext(ctx, `SELECT epic_id, item_epoch, state FROM attention_items WHERE id = ?`, id).
		Scan(&epicID, &itemEpoch, &state)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, "", ErrAttentionNotFound
	}
	return epicID, itemEpoch, state, e
}

func loadItemMetaForProjectTx(ctx context.Context, tx *sql.Tx, projectID, id string) (epicID string, itemEpoch int, state string, err error) {
	e := tx.QueryRowContext(ctx, `SELECT epic_id, item_epoch, state FROM attention_items WHERE project_id = ? AND id = ?`, projectID, id).
		Scan(&epicID, &itemEpoch, &state)
	if errors.Is(e, sql.ErrNoRows) {
		return "", 0, "", ErrAttentionNotFound
	}
	return epicID, itemEpoch, state, e
}

// loadFenceTx builds the attention.Fence for a fenced call: the item's live state,
// leased_by, and item_epoch, plus the live supervisor epoch of whoever holds the lease.
// A missing item is ErrAttentionNotFound; a missing/empty leaseholder yields a live
// supervisor epoch of -1 (which no claim can match), so the fence fails closed.
func loadFenceTx(ctx context.Context, tx *sql.Tx, id, masterID string, epoch, itemEpoch int, expect string) (attention.Fence, error) {
	return loadFenceForProjectTx(ctx, tx, "", id, masterID, epoch, itemEpoch, expect)
}

func loadFenceForProjectTx(ctx context.Context, tx *sql.Tx, projectID, id, masterID string, epoch, itemEpoch int, expect string) (attention.Fence, error) {
	var state, leasedBy string
	var liveItemEpoch int
	var e error
	if projectID == "" {
		e = tx.QueryRowContext(ctx, `SELECT state, leased_by, item_epoch FROM attention_items WHERE id = ?`, id).
			Scan(&state, &leasedBy, &liveItemEpoch)
	} else {
		e = tx.QueryRowContext(ctx, `SELECT state, leased_by, item_epoch FROM attention_items WHERE project_id = ? AND id = ?`, projectID, id).
			Scan(&state, &leasedBy, &liveItemEpoch)
	}
	if errors.Is(e, sql.ErrNoRows) {
		return attention.Fence{}, ErrAttentionNotFound
	}
	if e != nil {
		return attention.Fence{}, e
	}
	liveSupEpoch := -1
	if leasedBy != "" {
		se := tx.QueryRowContext(ctx, `SELECT epoch FROM supervisors WHERE id = ?`, leasedBy).Scan(&liveSupEpoch)
		if se != nil && !errors.Is(se, sql.ErrNoRows) {
			return attention.Fence{}, se
		}
	}
	return attention.Fence{
		State: state, ExpectState: expect,
		LeasedBy: leasedBy, Caller: masterID,
		ClaimItemEpoch: itemEpoch, LiveItemEpoch: liveItemEpoch,
		ClaimSupervisorEpoch: epoch, LiveSupervisorEpoch: liveSupEpoch,
	}, nil
}

// gatherAttentionForLease reads the current active set for a lease decision: every
// state=open item folded into an attention.Item, plus the set of epic ids that already
// hold an in-flight (leased/delivering/awaiting_ack) item (the one-in-flight-per-epic
// backstop the pure core enforces).
func gatherAttentionForLease(ctx context.Context, tx *sql.Tx, projectID string) ([]attention.Item, map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, kind, epic_id, priority, state, blocking, first_seen_at, item_epoch, leased_by
		  FROM attention_items WHERE project_id = ? AND state IN `+attentionActiveStatesSQL, projectID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var open []attention.Item
	inflight := map[string]bool{}
	for rows.Next() {
		var id, kind, epicID, state, firstSeen, leasedBy string
		var prio, itemEpoch, blocking int
		if err := rows.Scan(&id, &kind, &epicID, &prio, &state, &blocking, &firstSeen, &itemEpoch, &leasedBy); err != nil {
			return nil, nil, err
		}
		if state == attention.StateOpen {
			open = append(open, attention.Item{
				ID: id, Kind: attention.Kind(kind), EpicID: epicID, Priority: prio,
				State: attention.StateOpen, Blocking: blocking != 0,
				FirstSeenAt: parseStoreTime(firstSeen), ItemEpoch: itemEpoch, LeasedBy: leasedBy,
			})
		} else if epicID != "" {
			inflight[epicID] = true
		}
	}
	return open, inflight, rows.Err()
}

type expiredRef struct {
	id        string
	projectID string
	epicID    string
	itemEpoch int
}

// expiredIDsTx returns the ids of items in `state` whose lease_expires_at has passed at
// `now`. Parsed in Go (not an SQL string compare) — see ReapExpiredLeases.
func expiredIDsTx(ctx context.Context, tx *sql.Tx, state string, now time.Time) ([]expiredRef, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, project_id, epic_id, item_epoch, lease_expires_at FROM attention_items WHERE state = ? AND lease_expires_at <> ''`,
		state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExpired(rows, now)
}

func expiredIDsTxDB(ctx context.Context, db *sql.DB, state string, now time.Time) ([]expiredRef, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, project_id, epic_id, item_epoch, lease_expires_at FROM attention_items WHERE state = ? AND lease_expires_at <> ''`,
		state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectExpired(rows, now)
}

func collectExpired(rows *sql.Rows, now time.Time) ([]expiredRef, error) {
	var out []expiredRef
	for rows.Next() {
		var id, projectID, epicID, exp string
		var itemEpoch int
		if err := rows.Scan(&id, &projectID, &epicID, &itemEpoch, &exp); err != nil {
			return nil, err
		}
		t, perr := time.Parse(rfc3339, exp)
		if perr != nil {
			continue // unparseable expiry: leave it (degrade to inert, never a false reap)
		}
		if !now.Before(t) { // now >= expiry
			out = append(out, expiredRef{id: id, projectID: projectID, epicID: epicID, itemEpoch: itemEpoch})
		}
	}
	return out, rows.Err()
}

// ledgerKeyFor is the job_events stream key for an attention event: the EPIC id for an
// epic-scoped item (so the operator drawer reads the epic's intervention timeline via
// LoadEvents("att:"+epic) — plan §1.5), or the item id when there is no epic (e.g.
// master_absent). The "att:" prefix makes the namespace disjointness (see appendEpicLedger)
// STRUCTURAL rather than merely conventional.
func ledgerKeyFor(epicID, itemID string) string {
	if epicID != "" {
		return attnLedgerPrefix + epicID
	}
	return attnLedgerPrefix + itemID
}

// Ledger stream-key prefixes (m5). job_events.job_id is a shared namespace: a real job
// keys on its ULID; attention/supervisor audit rows key on these prefixed streams. The
// prefixes guarantee the epic-lane keys can NEVER collide with a jobs.id ULID (or with
// each other), keeping UNIQUE(job_id, job_seq) well-defined per stream.
const (
	attnLedgerPrefix = "att:"
	supLedgerPrefix  = "sup:"
)

// appendEpicLedger appends one epic-lane audit event to job_events (the ledger spine).
// `key` is the event stream (an "att:"/"sup:"-prefixed key — see ledgerKeyFor /
// supLedgerPrefix); the per-key job_seq is computed within the tx (safe under
// MaxOpenConns=1). `epoch` carries the fence in force (item_epoch or supervisor epoch).
// itemID/reason ride the Payload's existing LeaseID/RevokeReason fields — a DOCUMENTED
// reuse: Fold has no case for these kinds (it ignores them), so no ledger.Payload schema
// change is needed and the shared ledger.go diff stays additive (constants only), per the
// parallel-builder contract.
//
// NAMESPACE INVARIANT (m5): job_events.job_id is a shared TEXT space with UNIQUE(job_id,
// job_seq). Real jobs key on jobs.id ULIDs; epic-lane streams key on the "att:"/"sup:"
// prefixes above. Those spaces are disjoint BY CONSTRUCTION (a ULID never starts "att:"/
// "sup:"), so an epic slug or supervisor label can never masquerade as a jobs.id and
// diverge the per-stream job_seq sequence. Keep every epic-lane append prefixed.
func appendEpicLedger(ctx context.Context, tx *sql.Tx, key string, kind ledger.EventKind, actor string, epoch int, itemID, reason string, now time.Time) error {
	if actor == "" {
		actor = "system"
	}
	var seq int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(job_seq),0) FROM job_events WHERE job_id = ?`, key).Scan(&seq); err != nil {
		return err
	}
	return appendEvent(ctx, tx, ledger.Event{
		JobID: key, JobSeq: seq + 1, Kind: kind, LeaseEpoch: epoch,
		Actor: actor, CreatedAt: now,
		Payload: ledger.Payload{LeaseID: itemID, RevokeReason: reason},
	})
}

func marshalEvidence(m map[string]string) string {
	if len(m) == 0 {
		return "{}"
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func unmarshalEvidence(s string) map[string]string {
	if s == "" || s == "{}" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

// parseStoreTime parses an RFC3339Nano store timestamp, returning the zero time on any
// error (an unparseable/empty timestamp reads as "unknown age", never a panic).
func parseStoreTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(rfc3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
