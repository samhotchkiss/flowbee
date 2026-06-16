package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/engine"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// FactSource yields the reconciled Domain-B facts (§3.1.B) for a job. It is the
// AUTHORITY the I-9 code-review gate consumes — never the worker's claim. In M3 a
// DBFactSource serves rows the tests seed; in M6 the reconcile-IN sweep becomes
// the only writer behind the same interface. A job with no reconciled facts yet
// returns ok=false (the gate cannot mint an approval).
type FactSource interface {
	Facts(ctx context.Context, jobID string) (job.DomainBFacts, bool, error)
}

// DBFactSource reads reconciled facts from the domain_b_facts table.
type DBFactSource struct{ DB *sql.DB }

// Facts returns the reconciled Domain-B facts for a job.
func (f DBFactSource) Facts(ctx context.Context, jobID string) (job.DomainBFacts, bool, error) {
	var (
		facts                  job.DomainBFacts
		prExists, ciGreen, mrg int
	)
	err := f.DB.QueryRowContext(ctx, `
		SELECT pr_exists, pr_number, head_sha, base_sha, ci_green, merged
		  FROM domain_b_facts WHERE job_id = ?`, jobID).Scan(
		&prExists, &facts.PRNumber, &facts.HeadSHA, &facts.BaseSHA, &ciGreen, &mrg)
	if errors.Is(err, sql.ErrNoRows) {
		return job.DomainBFacts{}, false, nil
	}
	if err != nil {
		return job.DomainBFacts{}, false, err
	}
	facts.PRExists = prExists == 1
	facts.CIGreen = ciGreen == 1
	facts.Merged = mrg == 1
	return facts, true, nil
}

// UpsertDomainBFacts writes the reconciled Domain-B facts for a job (M3: test
// seam standing in for reconcile-IN). Only ever called by the reconcile path —
// never by a worker call.
func (s *Store) UpsertDomainBFacts(ctx context.Context, jobID string, f job.DomainBFacts) error {
	_, err := s.DB.ExecContext(ctx, `
		INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT (job_id) DO UPDATE SET
		    pr_exists = excluded.pr_exists, pr_number = excluded.pr_number,
		    head_sha = excluded.head_sha, base_sha = excluded.base_sha,
		    ci_green = excluded.ci_green, merged = excluded.merged,
		    updated_at = datetime('now')`,
		jobID, b2i(f.PRExists), f.PRNumber, f.HeadSHA, f.BaseSHA, b2i(f.CIGreen), b2i(f.Merged))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ClaimReviewParams describes a code_reviewer claiming a review_pending job (the
// gate stage). It mirrors ClaimReadyJob's fencing: review_pending -> code_review
// in one serialized UPDATE that bumps the epoch and binds the reviewer.
type ClaimReviewParams struct {
	JobID       string
	LeaseID     string
	Identity    string
	ModelFamily string
	Attested    []string
	TTL         time.Duration
	Now         time.Time
}

// ClaimReviewJob runs the atomic claim for the code_review gate stage:
// review_pending -> code_review, epoch++, reviewer bound. 0 rows -> ErrLostRace.
// The capability + anti-affinity checks belong to the reviewer role; M3 enforces
// only capability match (M4 wires the sibling-identity exclusion to real lineage).
func (s *Store) ClaimReviewJob(ctx context.Context, p ClaimReviewParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	var result *lease.Lease
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var reqJSON, curState string
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state FROM jobs WHERE id = ?`, p.JobID).Scan(&reqJSON, &curState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("read review caps: %w", err)
		}
		if job.State(curState) != job.StateReviewPending {
			return lease.ErrLostRace
		}
		if !job.CapabilitiesSatisfy(p.Attested, unmarshalStrings(reqJSON)) {
			return lease.ErrLostRace
		}

		// §6.3.1 atomic claim of the gate stage. The anti-affinity NOT EXISTS clause
		// excludes the eng_worker's identity AND model_family (I-10): a reviewer may
		// never judge its own build, nor may a same-model_family worker (uncorrelated
		// failure modes, §5.5). It reads the sibling's DURABLE builder_identity /
		// builder_model_family — the live bound_* columns were cleared when the build
		// result landed, so the builder identity is preserved in those columns (set
		// once at review_pending). The sibling pointer eng_worker_job points at this
		// same job (self-review case); the predicate generalizes to a distinct
		// build-job sibling once review is a separate job row. A claim that would
		// violate the term returns 0 rows (ErrLostRace) and the job stays
		// review_pending so its no_eligible_worker alarm can fire.
		row := tx.QueryRowContext(ctx, `
			UPDATE jobs
			   SET state              = 'code_review',
			       role               = 'code_reviewer',
			       stage              = 'review',
			       bound_identity     = ?,
			       bound_model_family = ?,
			       lease_epoch        = lease_epoch + 1,
			       lease_id           = ?,
			       lease_deadline     = ?,
			       lease_hb_due       = ?,
			       updated_at         = datetime('now')
			 WHERE id    = ?
			   AND state = 'review_pending'
			   AND NOT EXISTS (
			        SELECT 1 FROM jobs sib
			         WHERE sib.id = COALESCE(jobs.eng_worker_job, jobs.id)
			           AND ( sib.builder_identity     = ?
			              OR sib.builder_model_family = ? ) )
			RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.LeaseID,
			deadline.Format(rfc3339), deadline.Format(rfc3339),
			p.JobID, p.Identity, p.ModelFamily,
		)
		var newEpoch, prevSeq int
		if err := row.Scan(&newEpoch, &prevSeq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("atomic review claim: %w", err)
		}
		nextSeq := prevSeq + 1
		ev := ledger.Event{
			JobID:      p.JobID,
			JobSeq:     nextSeq,
			Kind:       ledger.KindReviewClaimed,
			FromState:  job.StateReviewPending,
			ToState:    job.StateCodeReview,
			LeaseEpoch: newEpoch,
			Actor:      p.Identity,
			CreatedAt:  p.Now,
			Payload: ledger.Payload{
				LeaseID: p.LeaseID, BoundIdentity: p.Identity, BoundModelFamily: p.ModelFamily,
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, p.JobID, nextSeq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO leases (lease_id, job_id, lease_epoch, identity, model_family, ttl_s, deadline)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.LeaseID, p.JobID, newEpoch, p.Identity, p.ModelFamily,
			int(p.TTL/time.Second), deadline.Format(rfc3339)); err != nil {
			return fmt.Errorf("insert review lease audit: %w", err)
		}
		result = &lease.Lease{
			LeaseID: p.LeaseID, JobID: p.JobID, Epoch: newEpoch,
			Identity: p.Identity, ModelFamily: p.ModelFamily, TTL: p.TTL,
			GrantedAt: p.Now, Deadline: deadline, HBDue: deadline, State: lease.StateActive,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ReviewResultParams is a fenced code-review result carrying the reviewer's verdict
// CLAIM (untrusted) + requested disposition + an idempotency key.
type ReviewResultParams struct {
	JobID          string
	Epoch          int
	Claim          job.VerdictValue
	Disposition    job.Disposition
	IdempotencyKey string
	Now            time.Time
}

// ReviewResultResponse is the gate's reply.
type ReviewResultResponse struct {
	Accepted bool   `json:"accepted"`
	JobState string `json:"job_state"`
	Verdict  string `json:"verdict"` // the minted value, or "" if none
	Minted   bool   `json:"minted"`  // true iff a tamper-evident verdict was minted (I-9)
}

// ReviewResult is the I-9 keystone: a fenced code-review result. The worker's
// claim is recorded as an UNTRUSTED verdict_claim event, then the engine runs the
// PURE gate over the RECONCILED facts (from the FactSource) — NEVER the claim — and
// either MINTS a SHA-bound tamper-evident verdict (approved) or bounces. Stale
// epoch -> ErrStaleEpoch (409); duplicate key -> cached response, no re-apply.
func (s *Store) ReviewResult(ctx context.Context, src FactSource, p job.Policy, in ReviewResultParams) (ReviewResultResponse, error) {
	// reconcile the facts OUTSIDE the tx (read-only); the gate decision is pure.
	facts, _, err := src.Facts(ctx, in.JobID)
	if err != nil {
		return ReviewResultResponse{}, fmt.Errorf("reconcile facts: %w", err)
	}

	var resp ReviewResultResponse
	err = s.tx(ctx, func(tx *sql.Tx) error {
		if in.IdempotencyKey != "" {
			var cached string
			e := tx.QueryRowContext(ctx,
				`SELECT response FROM result_idempotency WHERE job_id=? AND idempotency_key=?`,
				in.JobID, in.IdempotencyKey).Scan(&cached)
			if e == nil {
				return json.Unmarshal([]byte(cached), &resp)
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return fmt.Errorf("idempotency lookup: %w", e)
			}
		}

		j, seq, err := loadJobTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}

		// M9 (§9.2, I-11): run the deterministic content-integrity gate over the
		// stored (untrusted) patch + declared blast-radius and thread the Result into
		// the pure engine. A denylist hit / blast-radius mismatch / failed static
		// check forces handoff regardless of the reviewer's self_merge request.
		chk, err := contentResultTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}

		// M11 (§6.5.2, I-12): (job, epoch)-scoped CI gating. When epoch CI is in use
		// for this job, the live gate honors ONLY the LIVE build epoch's CI — a zombie
		// that pushed to a STALE epoch and turned its CI green wrote a row for the dead
		// epoch, so the live gate stays red. AND the reconciled CIGreen with the live
		// epoch's verdict; a non-promoted/stale epoch then can't satisfy the gate.
		inUse, liveGreen, err := epochGatedCITx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}
		if inUse {
			facts.CIGreen = facts.CIGreen && liveGreen
		}

		dec := engine.Decide(engine.EngineState{
			Job: j, Now: in.Now, Epoch: j.LeaseEpoch, GitHub: facts, Policy: p, Content: &chk,
		}, engine.ReviewClaim{Epoch: in.Epoch, Value: in.Claim, Disposition: in.Disposition})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}
		if err := persistContentResultTx(ctx, tx, in.JobID, chk); err != nil {
			return err
		}

		nextSeq := seq

		// 1) record the UNTRUSTED claim for audit (it advances no projection field).
		nextSeq++
		claimEv := ledger.Event{
			JobID: in.JobID, JobSeq: nextSeq, Kind: ledger.KindVerdictClaim,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: j.BoundIdentity, CreatedAt: in.Now,
			Payload: ledger.Payload{VerdictClaim: in.Claim, Disposition: in.Disposition},
		}
		if err := appendEvent(ctx, tx, claimEv); err != nil {
			return err
		}

		// 2) apply the gate decision (mint verdict / bounce / exhaust).
		final := j.State
		var minted *job.Verdict
		for _, t := range dec.Transitions {
			nextSeq++
			ev := ledger.Event{
				JobID: in.JobID, JobSeq: nextSeq, Kind: t.Kind,
				FromState: t.From, ToState: t.To, LeaseEpoch: j.LeaseEpoch,
				Actor: "system", CreatedAt: in.Now,
				Payload: ledger.Payload{BouncesDelta: t.BouncesDelta, Verdict: t.Verdict},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			final = t.To
			if t.Verdict != nil {
				minted = t.Verdict
			}
		}

		// 3) persist the projection mutation atomically with the events.
		var verdictJSON any
		if minted != nil {
			blob, _ := json.Marshal(minted)
			verdictJSON = string(blob)
		}
		bouncesDelta := 0
		for _, t := range dec.Transitions {
			bouncesDelta += t.BouncesDelta
		}
		headSHA := j.HeadSHA
		if minted != nil && minted.HeadSHA != "" {
			headSHA = minted.HeadSHA
		}
		if final == job.StateReady {
			// a bounce re-arms the build stage: re-leasable by an eng_worker against
			// the same base. Reset the role + capability requirement and the aging
			// clock; the gate lease is cleared.
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'ready', role = 'eng_worker', stage = 'build',
				       required_capabilities = ?, bounces = bounces + ?,
				       enqueued_at = ?,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:eng_worker"}), bouncesDelta,
				in.Now.Format(rfc3339), in.JobID); err != nil {
				return fmt.Errorf("apply bounce projection: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = ?, verdict = ?, head_sha = ?,
			       bounces = bounces + ?,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, updated_at = datetime('now')
			 WHERE id = ?`,
			string(final), verdictJSON, headSHA, bouncesDelta, in.JobID); err != nil {
			return fmt.Errorf("apply review projection: %w", err)
		}
		if err := setJobSeq(ctx, tx, in.JobID, nextSeq); err != nil {
			return err
		}
		// close the gate lease audit row.
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'completed'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			in.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close review lease: %w", err)
		}

		resp = ReviewResultResponse{
			Accepted: true, JobState: string(final),
			Minted: minted != nil,
		}
		if minted != nil {
			resp.Verdict = string(minted.Value)
		}
		if in.IdempotencyKey != "" {
			blob, _ := json.Marshal(resp)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO result_idempotency (job_id, idempotency_key, response) VALUES (?, ?, ?)`,
				in.JobID, in.IdempotencyKey, string(blob)); err != nil {
				return fmt.Errorf("store review idempotency: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return ReviewResultResponse{}, err
	}
	return resp, nil
}

// DispatchMergeParams advances a `mergeable` job onto its §5.4 branch-point arm.
type DispatchMergeParams struct {
	JobID string
	Now   time.Time
}

// DispatchMerge moves a `mergeable` job to merge_handoff (default) or merging
// (self_merge, only when policy enabled it AND the minted verdict carried it AND
// the §5.4 content/SHA predicate STILL holds). The engine decides the arm purely
// from the persisted verdict + reconciled facts + the content Result + policy.
// Returns the resulting state.
func (s *Store) DispatchMerge(ctx context.Context, src FactSource, p job.Policy, in DispatchMergeParams) (job.State, error) {
	// reconcile facts OUTSIDE the tx (read-only) for the §5.4 condition-5 SHA
	// re-binding; a nil/erroring source leaves facts zero (self_merge then denied,
	// the safe default — handoff).
	var facts job.DomainBFacts
	if src != nil {
		if f, _, ferr := src.Facts(ctx, in.JobID); ferr == nil {
			facts = f
		}
	}
	var final job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}
		chk, err := contentResultTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}
		dec := engine.Decide(engine.EngineState{
			Job: j, Now: in.Now, Epoch: j.LeaseEpoch, Policy: p, GitHub: facts, Content: &chk,
		}, engine.MergeDispatch{})
		if dec.Reject != nil {
			return fmt.Errorf("dispatch merge: %s", dec.Reject.Reason)
		}
		final = j.State
		nextSeq := seq
		for _, t := range dec.Transitions {
			nextSeq++
			ev := ledger.Event{
				JobID: in.JobID, JobSeq: nextSeq, Kind: t.Kind,
				FromState: t.From, ToState: t.To, LeaseEpoch: j.LeaseEpoch,
				Actor: "system", CreatedAt: in.Now,
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			final = t.To
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = ?, updated_at = datetime('now') WHERE id = ?`,
			string(final), in.JobID); err != nil {
			return fmt.Errorf("apply merge dispatch: %w", err)
		}
		return setJobSeq(ctx, tx, in.JobID, nextSeq)
	})
	if err != nil {
		return "", err
	}
	return final, nil
}
