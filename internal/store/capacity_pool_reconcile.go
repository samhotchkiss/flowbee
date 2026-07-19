package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/attention"
)

// CapacityPoolRequirement is scheduling demand, not a capacity observation. The
// caller derives QueuedWork from its project-scoped build/review/operations queues;
// this reconciler independently checks the active v2 generation and makes a zero
// pool visible and push-alertable.
type CapacityPoolRequirement struct {
	ProjectID  string
	Pool       string
	Provider   string
	QueuedWork int
}

type CapacityPoolReconcileResult struct{ Checked, Pending, Alerted, Resolved int }

// CapacityPoolDemand derives queued work by project for the shipped role map.
// Capacity truth remains provider-neutral; the caller supplies the configured
// providers (Codex build, Grok review/operations in the default profile).
func (s *Store) CapacityPoolDemand(ctx context.Context, buildProvider, reviewProvider, operationsProvider string) ([]CapacityPoolRequirement, error) {
	for _, provider := range []string{buildProvider, reviewProvider, operationsProvider} {
		if provider != "codex" && provider != "grok" {
			return nil, fmt.Errorf("unsupported capacity pool provider %q", provider)
		}
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT p.id,
		(SELECT COUNT(*) FROM epic_deliveries d WHERE d.project_id=p.id
		 AND d.state IN ('admitted','building','rebuild_in_flight')),
		(SELECT COUNT(*) FROM epic_deliveries d WHERE d.project_id=p.id
		 AND d.state IN ('awaiting_review_dispatch','review_queued')),
		(SELECT COUNT(*) FROM control_alerts a WHERE a.project_id=p.id
		 AND a.state IN ('pending','delivering'))
		FROM projects p WHERE p.state='active' ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CapacityPoolRequirement
	for rows.Next() {
		var projectID string
		var build, review, operations int
		if err := rows.Scan(&projectID, &build, &review, &operations); err != nil {
			return nil, err
		}
		out = append(out,
			CapacityPoolRequirement{ProjectID: projectID, Pool: "build", Provider: buildProvider, QueuedWork: build},
			CapacityPoolRequirement{ProjectID: projectID, Pool: "review", Provider: reviewProvider, QueuedWork: review},
			CapacityPoolRequirement{ProjectID: projectID, Pool: "operations", Provider: operationsProvider, QueuedWork: operations},
		)
	}
	return out, rows.Err()
}

func (s *Store) ReconcileCapacityPools(ctx context.Context, requirements []CapacityPoolRequirement, now time.Time, threshold, freshFor time.Duration) (CapacityPoolReconcileResult, error) {
	if threshold <= 0 {
		threshold = 5 * time.Minute
	}
	if freshFor <= 0 {
		freshFor = 15 * time.Minute
	}
	var report CapacityPoolReconcileResult
	for _, requirement := range requirements {
		if requirement.ProjectID == "" || requirement.Pool == "" || requirement.Provider == "" || requirement.QueuedWork < 0 {
			return report, errors.New("capacity pool requirement is incomplete")
		}
		switch requirement.Pool {
		case "build", "review", "operations":
		default:
			return report, fmt.Errorf("unknown capacity pool %q", requirement.Pool)
		}
		switch requirement.Provider {
		case "codex", "grok":
		default:
			return report, fmt.Errorf("unknown capacity provider %q", requirement.Provider)
		}
		rows, err := s.DB.QueryContext(ctx, `SELECT id FROM seats WHERE agent_family=? AND enabled=1 ORDER BY id`, requirement.Provider)
		if err != nil {
			return report, err
		}
		var seatIDs []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return report, err
			}
			seatIDs = append(seatIDs, id)
		}
		if err := rows.Close(); err != nil {
			return report, err
		}
		routable := 0
		for _, seatID := range seatIDs {
			decision, err := s.CapacityRouteForSeat(ctx, seatID, now, freshFor)
			if err != nil {
				return report, err
			}
			if decision.Routable {
				routable++
			}
		}
		outcome, err := s.reconcileCapacityPool(ctx, requirement, routable, now, threshold)
		if err != nil {
			return report, err
		}
		report.Checked++
		report.Pending += outcome.Pending
		report.Alerted += outcome.Alerted
		report.Resolved += outcome.Resolved
	}
	return report, nil
}

func (s *Store) reconcileCapacityPool(ctx context.Context, req CapacityPoolRequirement, routable int, now time.Time, threshold time.Duration) (CapacityPoolReconcileResult, error) {
	var out CapacityPoolReconcileResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var priorState, firstZero string
		err := tx.QueryRowContext(ctx, `SELECT state,first_zero_at FROM capacity_pool_health WHERE project_id=? AND pool=? AND provider=?`, req.ProjectID, req.Pool, req.Provider).Scan(&priorState, &firstZero)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		nowText := now.UTC().Format(rfc3339)
		dedupBase := "capacity_pool_exhausted:" + req.ProjectID + ":" + req.Pool + ":" + req.Provider
		if req.QueuedWork == 0 || routable > 0 {
			_, err = tx.ExecContext(ctx, `INSERT INTO capacity_pool_health(project_id,pool,provider,queued_work,routable_seats,state,first_zero_at,last_checked_at,updated_at)
				VALUES (?,?,?,?,?,'healthy','',?,?) ON CONFLICT(project_id,pool,provider) DO UPDATE SET queued_work=excluded.queued_work,routable_seats=excluded.routable_seats,state='healthy',first_zero_at='',last_checked_at=excluded.last_checked_at,updated_at=excluded.updated_at`, req.ProjectID, req.Pool, req.Provider, req.QueuedWork, routable, nowText, nowText)
			if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='capacity_restored',resolved_at=?,updated_at=? WHERE dedup_key=? AND state IN ('open','leased','delivering','awaiting_ack')`, nowText, nowText, dedupBase)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				out.Resolved = int(n)
			}
			payload, _ := json.Marshal(map[string]any{"project_id": req.ProjectID, "pool": req.Pool, "provider": req.Provider, "state": "healthy", "queued_work": req.QueuedWork, "routable_seats": routable})
			return appendGlobalControlEventTx(ctx, tx, "capacity_pool_reconciled", string(payload), now)
		}
		if firstZero == "" {
			firstZero = nowText
		}
		first, _ := time.Parse(rfc3339, firstZero)
		state := "zero_pending"
		if !first.IsZero() && !now.Before(first.Add(threshold)) {
			state = "alerted"
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO capacity_pool_health(project_id,pool,provider,queued_work,routable_seats,state,first_zero_at,last_checked_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,pool,provider) DO UPDATE SET queued_work=excluded.queued_work,routable_seats=excluded.routable_seats,state=excluded.state,first_zero_at=excluded.first_zero_at,last_checked_at=excluded.last_checked_at,updated_at=excluded.updated_at`, req.ProjectID, req.Pool, req.Provider, req.QueuedWork, routable, state, firstZero, nowText, nowText)
		if err != nil {
			return err
		}
		evidence, _ := json.Marshal(map[string]any{"project_id": req.ProjectID, "pool": req.Pool, "provider": req.Provider, "queued_work": req.QueuedWork, "routable_seats": routable, "first_zero_at": firstZero})
		attentionID := "capacity-pool-" + stableID(dedupBase+"\x00"+firstZero)
		insert, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items(id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,detail,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
			VALUES (?,?, '', '',10,'open',?,1,?,?,1,?,?,?,?)`, attentionID, string(attention.KindCapacityPoolExhausted), dedupBase, string(evidence), fmt.Sprintf("%s %s pool has %d queued work and zero routable seats", req.Provider, req.Pool, req.QueuedWork), firstZero, nowText, nowText, nowText)
		if err != nil {
			return err
		}
		inserted, _ := insert.RowsAffected()
		delta := 1
		if inserted == 1 {
			delta = 0
		}
		_, err = tx.ExecContext(ctx, `UPDATE attention_items SET occurrences=occurrences+?,last_seen_at=?,evidence_json=?,updated_at=? WHERE dedup_key=? AND state IN ('open','leased','delivering','awaiting_ack')`, delta, nowText, string(evidence), nowText, dedupBase)
		if err != nil {
			return err
		}
		if state == "alerted" {
			alertDedup := dedupBase + ":" + firstZero
			if err := ensureControlAlertTx(ctx, tx, req.ProjectID, "", "capacity_pool_exhausted", alertDedup, string(evidence), now); err != nil {
				return err
			}
			out.Alerted = 1
		} else {
			out.Pending = 1
		}
		payload, _ := json.Marshal(map[string]any{"project_id": req.ProjectID, "pool": req.Pool, "provider": req.Provider, "state": state, "queued_work": req.QueuedWork, "routable_seats": routable})
		return appendGlobalControlEventTx(ctx, tx, "capacity_pool_reconciled", string(payload), now)
	})
	return out, err
}
