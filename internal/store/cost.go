package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/engine"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// CostParams is a fenced cost report (§6.7, I-15): a worker's {tokens_in,
// tokens_out, $} delta reported on heartbeat/result. Dollars are MICRO-USD
// ($1.00 = 1_000_000) so the ceiling arithmetic is exact integer math.
type CostParams struct {
	JobID          string
	Epoch          int
	Now            time.Time
	TokensInDelta  int64
	TokensOutDelta int64
	MicroUSDDelta  int64
}

// CostResult reports what a RecordCost call did (for the API / tests).
type CostResult struct {
	Directive    engine.Directive // continue | cancel (over-budget -> cancel, I-15)
	Escalated    bool             // the ceiling was crossed -> needs_human
	CostMicroUSD int64            // the new accumulated per-job meter
	NewEpoch     int              // the fence after an escalation revoke
}

// RecordCost folds a worker-reported cost delta into the per-job meter and runs the
// PURE ceiling predicate (engine.CostMeter). Stale epoch -> lease.ErrStaleEpoch
// (409). Under the ceiling: accumulate + append a cost_metered event + return
// continue (the meter still rolls up per-flow, §12.6.5). At/over the ceiling: the
// escalation is a lease revocation — bump the epoch (fence the worker), route the
// job to needs_human, mark over_budget, enqueue the flowbee:over-budget label
// rendering (project-OUT), and return a `cancel` directive so the live worker
// learns its lease was pulled (I-15). All in one serialized transaction.
func (s *Store) RecordCost(ctx context.Context, p CostParams) (CostResult, error) {
	var res CostResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		// fold the delta into a COPY of the job so the engine sees the NEW meter (the
		// ceiling predicate compares the accumulated total, I-15). The persisted
		// projection is mutated below within the same tx.
		newIn := j.CostTokensIn + p.TokensInDelta
		newOut := j.CostTokensOut + p.TokensOutDelta
		newUSD := j.CostMicroUSD + p.MicroUSDDelta
		folded := j
		folded.CostTokensIn = newIn
		folded.CostTokensOut = newOut
		folded.CostMicroUSD = newUSD

		dec := engine.Decide(
			engine.EngineState{Job: folded, Now: p.Now, Epoch: j.LeaseEpoch},
			engine.CostMeter{Epoch: p.Epoch})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}
		if dec.Directive != nil {
			res.Directive = *dec.Directive
		}
		res.CostMicroUSD = newUSD
		res.NewEpoch = j.LeaseEpoch

		nextSeq := seq + 1
		if len(dec.Transitions) == 0 {
			// under the ceiling: record the metered report (audit + rollup replay) and
			// accumulate the meter. No state change.
			ev := ledger.Event{
				JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindCostMetered,
				FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
				Actor: j.BoundIdentity, CreatedAt: p.Now,
				Payload: ledger.Payload{
					CostTokensInDelta:  p.TokensInDelta,
					CostTokensOutDelta: p.TokensOutDelta,
					CostMicroUSDDelta:  p.MicroUSDDelta,
				},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs SET cost_tokens_in = ?, cost_tokens_out = ?, cost_micro_usd = ?,
				                updated_at = datetime('now')
				 WHERE id = ?`, newIn, newOut, newUSD, p.JobID); err != nil {
				return fmt.Errorf("accumulate cost: %w", err)
			}
			return setJobSeq(ctx, tx, p.JobID, nextSeq)
		}

		// over the ceiling: the cost_escalated transition revokes the lease (epoch++)
		// and routes to needs_human. It carries the final metered delta so the meter
		// reflects the report that tripped it.
		t := dec.Transitions[0]
		newEpoch := j.LeaseEpoch + 1 // BumpEpoch is always set on a cost escalation
		res.Escalated = true
		res.NewEpoch = newEpoch
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: t.Kind,
			FromState: t.From, ToState: t.To, LeaseEpoch: newEpoch,
			Actor: j.BoundIdentity, CreatedAt: p.Now,
			Payload: ledger.Payload{
				CostTokensInDelta:  p.TokensInDelta,
				CostTokensOutDelta: p.TokensOutDelta,
				CostMicroUSDDelta:  p.MicroUSDDelta,
				EscalationReason:   string(job.EscalationCost),
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET cost_tokens_in = ?, cost_tokens_out = ?, cost_micro_usd = ?,
			       state = ?, lease_epoch = ?, over_budget = 1,
			       escalation_reason = ?,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL, phase_deadline_at = NULL,
			       updated_at = datetime('now')
			 WHERE id = ?`,
			newIn, newOut, newUSD, string(t.To), newEpoch, string(job.EscalationCost),
			p.JobID); err != nil {
			return fmt.Errorf("apply cost escalation: %w", err)
		}
		if err := setJobSeq(ctx, tx, p.JobID, nextSeq); err != nil {
			return err
		}
		// close the lease audit row as revoked-for-cost.
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'over_budget'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			p.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close over-budget lease: %w", err)
		}
		// cancel any pending deadline timers for the OLD epoch (the epoch guard already
		// no-ops them; cancelling keeps the table tidy).
		if _, err := tx.ExecContext(ctx,
			`UPDATE timers SET fired = 1 WHERE job_id = ? AND expected_epoch = ? AND fired = 0`,
			p.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("cancel old-epoch timers: %w", err)
		}
		// project-OUT: stamp the flowbee:over-budget + flowbee:needs-human labels on
		// the PR (§12.6.5, §8.3 label table). Keyed by the job's head_sha so the
		// (job, action, head_sha) dedupe collapses re-escalations. Enqueued in the SAME
		// tx as the state change (the transactional-outbox guarantee, §8.2.2). A job
		// with no PR yet (no head_sha) still records the intent; the sender skips a
		// number-less label render until a PR exists.
		if j.PRNumber != 0 {
			labels := outboxPayload(map[string]any{
				"number": j.PRNumber,
				"labels": []string{"flowbee:over-budget", "flowbee:needs-human"},
			})
			if err := enqueueOutboxTx(ctx, tx, OutboxRow{
				JobID: p.JobID, Action: ActionSetLabels, HeadSHA: j.HeadSHA, Payload: labels,
			}); err != nil {
				return fmt.Errorf("enqueue over-budget label: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return CostResult{}, err
	}
	return res, nil
}

// FlowCostRow is the per-job line of a per-flow cost rollup (§12.6.5).
type FlowCostRow struct {
	JobID           string `json:"job_id"`
	Flow            string `json:"flow"`
	Stage           string `json:"stage"`
	Role            string `json:"role"`
	State           string `json:"state"`
	TokensIn        int64  `json:"tokens_in"`
	TokensOut       int64  `json:"tokens_out"`
	MicroUSD        int64  `json:"micro_usd"`
	CeilingMicroUSD *int64 `json:"ceiling_micro_usd,omitempty"`
	OverBudget      bool   `json:"over_budget"`
}

// FlowCostRollup is the answer to "what did this feature cost across spec+build+
// review?" (§12.6.5): every job sharing the flow_id, plus the summed totals.
type FlowCostRollup struct {
	FlowID         string        `json:"flow_id"`
	Jobs           []FlowCostRow `json:"jobs"`
	TotalTokensIn  int64         `json:"total_tokens_in"`
	TotalTokensOut int64         `json:"total_tokens_out"`
	TotalMicroUSD  int64         `json:"total_micro_usd"`
}

// FlowCostRollup returns the per-flow cost rollup for a flow_id (§12.6.5). It sums
// the meter across every job of the feature (spec_author + spec_reviewer +
// eng_worker + code_reviewer + merger), answering the end-to-end cost question.
func (s *Store) FlowCostRollup(ctx context.Context, flowID string) (FlowCostRollup, error) {
	rollup := FlowCostRollup{FlowID: flowID}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, flow, stage, role, state,
		       cost_tokens_in, cost_tokens_out, cost_micro_usd, cost_ceiling_micro_usd, over_budget
		  FROM jobs WHERE flow_id = ? ORDER BY id ASC`, flowID)
	if err != nil {
		return rollup, err
	}
	defer rows.Close()
	for rows.Next() {
		var r FlowCostRow
		var ceiling sql.NullInt64
		var over int
		if err := rows.Scan(&r.JobID, &r.Flow, &r.Stage, &r.Role, &r.State,
			&r.TokensIn, &r.TokensOut, &r.MicroUSD, &ceiling, &over); err != nil {
			return rollup, err
		}
		if ceiling.Valid {
			c := ceiling.Int64
			r.CeilingMicroUSD = &c
		}
		r.OverBudget = over != 0
		rollup.Jobs = append(rollup.Jobs, r)
		rollup.TotalTokensIn += r.TokensIn
		rollup.TotalTokensOut += r.TokensOut
		rollup.TotalMicroUSD += r.MicroUSD
	}
	return rollup, rows.Err()
}

// NeedsHumanRow is one job in the unified needs_human chokepoint (§12.6.1): the
// operator's primary attention queue, where all four escalation triggers deposit.
type NeedsHumanRow struct {
	JobID string `json:"job_id"`
	Flow  string `json:"flow"`
	Role  string `json:"role"`
	// Reason is the canonical trigger that escalated the job: attempts | bounces |
	// cost | stall (§6.7). Derived from the recorded escalation_reason, else from
	// the exhausted counter, so the lane shows WHY each job needs a human.
	Reason string `json:"reason"`
}

// NeedsHumanView returns every job in needs_human, each tagged with the trigger
// that escalated it (§12.6.1): the four independent conditions — max_attempts,
// max_bounces, cost ceiling (I-15), and the two-rung stall kill (I-13) — all
// funnel here. This is the single chokepoint THE ONE DECISION operates on.
func (s *Store) NeedsHumanView(ctx context.Context) ([]NeedsHumanRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, flow, role,
		       COALESCE(escalation_reason,''), over_budget,
		       attempts, max_attempts, bounces, max_bounces, stall_revocations
		  FROM jobs WHERE state = 'needs_human' ORDER BY updated_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NeedsHumanRow
	for rows.Next() {
		var r NeedsHumanRow
		var reason string
		var over, attempts, maxAttempts, bounces, maxBounces, stall int
		if err := rows.Scan(&r.JobID, &r.Flow, &r.Role, &reason, &over,
			&attempts, &maxAttempts, &bounces, &maxBounces, &stall); err != nil {
			return nil, err
		}
		r.Reason = classifyEscalation(reason, over, attempts, maxAttempts, bounces, maxBounces, stall)
		out = append(out, r)
	}
	return out, rows.Err()
}

// classifyEscalation maps a needs_human job to its escalation trigger (§6.7). The
// explicitly recorded reason wins (cost/stall escalations stamp it); otherwise the
// exhausted counter is inferred so EVERY needs_human job shows a trigger.
func classifyEscalation(reason string, over, attempts, maxAttempts, bounces, maxBounces, stall int) string {
	// the explicit cost stamp is unambiguous (I-15): a budget trip is always cost.
	if reason == string(job.EscalationCost) || over != 0 {
		return string(job.EscalationCost)
	}
	// a recorded stall/cap revoke (M8) routed here for one of two §6.7 reasons:
	// max_attempts exhausted (the attempts ceiling) OR the Rung-4 governor (a
	// genuine stall). The exhausted counter disambiguates — attempts wins when the
	// attempt budget is spent, else it is the stall governor.
	if reason == "two_rung_stall" || reason == "absolute_cap" {
		if maxAttempts > 0 && attempts >= maxAttempts {
			return string(job.EscalationAttempts)
		}
		return string(job.EscalationStall)
	}
	if reason != "" {
		return reason
	}
	// no explicit reason recorded (e.g. a gate bounce-exhaustion): infer from the
	// exhausted counter so EVERY needs_human job shows a trigger.
	if maxBounces > 0 && bounces >= maxBounces {
		return string(job.EscalationBounces)
	}
	if maxAttempts > 0 && attempts >= maxAttempts {
		return string(job.EscalationAttempts)
	}
	if stall > 0 {
		return string(job.EscalationStall)
	}
	return string(job.EscalationAttempts)
}
