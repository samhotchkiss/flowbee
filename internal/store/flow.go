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
		SELECT pr_exists, pr_number, head_sha, base_sha, ci_green, merged, mergeable_state
		  FROM domain_b_facts WHERE job_id = ?`, jobID).Scan(
		&prExists, &facts.PRNumber, &facts.HeadSHA, &facts.BaseSHA, &ciGreen, &mrg,
		&facts.MergeableState)
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

func domainBFactsTx(ctx context.Context, tx *sql.Tx, jobID string) (job.DomainBFacts, bool, error) {
	var (
		facts                  job.DomainBFacts
		prExists, ciGreen, mrg int
	)
	err := tx.QueryRowContext(ctx, `
		SELECT pr_exists, pr_number, head_sha, base_sha, ci_green, merged, mergeable_state
		  FROM domain_b_facts WHERE job_id = ?`, jobID).Scan(
		&prExists, &facts.PRNumber, &facts.HeadSHA, &facts.BaseSHA, &ciGreen, &mrg,
		&facts.MergeableState)
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
		INSERT INTO domain_b_facts (job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged, mergeable_state, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT (job_id) DO UPDATE SET
		    pr_exists = excluded.pr_exists, pr_number = excluded.pr_number,
		    head_sha = excluded.head_sha, base_sha = excluded.base_sha,
		    ci_green = excluded.ci_green, merged = excluded.merged,
		    mergeable_state = excluded.mergeable_state,
		    updated_at = datetime('now')`,
		jobID, b2i(f.PRExists), f.PRNumber, f.HeadSHA, f.BaseSHA, b2i(f.CIGreen), b2i(f.Merged),
		f.MergeableState)
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
	SeatID      string // v2 fail-closed physical capacity seat; never inferred from account label.
	ModelFamily string
	Model       string // ACTUAL model/agent doing the work (e.g. "codex"); recorded on the bound event for the card. Distinct from the ModelFamily anti-affinity tag.
	// Lens is the resolved review lens (F5) fenced into the lease for this
	// reviewer (correctness|tests|security). It records WHICH lens of a
	// multi-reviewer fan-out this reviewer is acting under.
	Lens     string
	Attested []string
	TTL      time.Duration
	Now      time.Time
	Fair     *ProjectFairClaim
}

// ClaimReviewJob runs the atomic claim for the code_review gate stage:
// review_pending -> code_review, epoch++, reviewer bound. 0 rows -> ErrLostRace.
// The capability + anti-affinity checks belong to the reviewer role; M3 enforces
// only capability match (M4 wires the sibling-identity exclusion to real lineage).
func (s *Store) ClaimReviewJob(ctx context.Context, p ClaimReviewParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	var result *lease.Lease
	var bindingHold error
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if err := projectConcurrencyGateTx(ctx, tx, p.JobID); err != nil {
			return err
		}
		var reqJSON, curState, domain, epicID string
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state, COALESCE(workflow_domain,'legacy'),
			        COALESCE(epic_delivery_id,'') FROM jobs WHERE id = ?`, p.JobID).
			Scan(&reqJSON, &curState, &domain, &epicID); err != nil {
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
		// F6 per-model slot gate: a review spawns a real agent, so it counts against the
		// box's advertised budget exactly like a build. Without this a box at its claude
		// limit could still claim a review and overcommit. No-op when no slots advertised.
		if err := modelSlotGateTx(ctx, tx, "", p.Identity, p.ModelFamily); err != nil {
			return err
		}

		// Native epic reviews are routed only from durable, exact Driver
		// incarnations. The polling worker supplies its Flowbee identity, never raw
		// tmux/session coordinates. Missing or ambiguous route facts commit a visible
		// hold and do not claim the job or create a send action.
		var projectID, deliveryRepo, head, base string
		var prNumber, deliveryVersion int
		var recipientBinding DriverSessionBinding
		if domain == "epic_v2" && epicID != "" {
			if err := tx.QueryRowContext(ctx, `SELECT d.project_id,d.state_version,d.delivery_repo,
				COALESCE(a.pr_number,0),d.head_sha,d.base_sha
				FROM epic_deliveries d JOIN epic_artifacts a ON a.epic_id=d.epic_id
				WHERE d.epic_id=? AND d.state='review_queued' AND d.review_job_id=?
				  AND d.hold_reason=''`, epicID, p.JobID).
				Scan(&projectID, &deliveryVersion, &deliveryRepo, &prNumber, &head, &base); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return lease.ErrLostRace
				}
				return err
			}
			if !s.HasDriverControlOrigin() {
				detail := ErrDriverControlOriginUnavailable.Error()
				if err := markReviewClaimBindingHoldTx(ctx, tx, projectID, epicID, p.JobID,
					p.Identity, detail, p.Now); err != nil {
					return err
				}
				bindingHold = fmt.Errorf("%w: %s", ErrDriverSessionBindingMissing, detail)
				return nil
			}
			if s.EnableCapacityV2 {
				decision, routeErr := capacityRouteForSeatQuery(ctx, tx, p.SeatID, p.Now, 5*time.Minute)
				if routeErr != nil {
					return routeErr
				}
				if !decision.Routable {
					detail := "review capacity route held: " + strings.Join(decision.Reasons, ",")
					if err := markReviewCapacityHoldTx(ctx, tx, projectID, epicID, p.JobID, p.SeatID, detail, p.Now); err != nil {
						return err
					}
					bindingHold = fmt.Errorf("%w: %s", ErrNoCapacity, detail)
					return nil
				}
			}
			var routeErr error
			recipientBinding, routeErr = activeDriverSessionBindingTx(ctx, tx, projectID, p.Identity, DriverReviewerRole)
			if routeErr != nil {
				detail := fmt.Sprintf("reviewer %s cannot be routed through an exact active Driver binding: %v", p.Identity, routeErr)
				if err := markReviewClaimBindingHoldTx(ctx, tx, projectID, epicID, p.JobID, p.Identity, detail, p.Now); err != nil {
					return err
				}
				bindingHold = fmt.Errorf("%w: %s", ErrDriverSessionBindingMissing, detail)
				return nil
			}
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
			       bound_lens         = ?,
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
			   AND NOT EXISTS (
			        -- F5 panel anti-affinity: a reviewer may not be TWO of the N consensus
			        -- approvals. Exclude any identity that already approved in THIS round
			        -- (since the last head-establishing event). DISTINCT IDENTITY only, not
			        -- family — a codex panel runs every reviewer under one model, so requiring
			        -- distinct families would make N>1 unsatisfiable.
			        SELECT 1 FROM job_events ja
			         WHERE ja.job_id = jobs.id AND ja.kind = 'verdict_claim'
			           AND ja.actor = ?
			           AND json_extract(ja.payload, '$.VerdictClaim') = 'approved'
			           AND ja.job_seq > (SELECT COALESCE(MAX(job_seq),0) FROM job_events
			                              WHERE job_id = jobs.id
			                                AND kind IN ('result_accepted','rebased','conflict_resolved')) )
		RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.Lens, p.LeaseID,
			deadline.Format(rfc3339), deadline.Format(rfc3339),
			p.JobID, p.Identity, p.ModelFamily, p.Identity,
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
				LeaseID: p.LeaseID, BoundIdentity: p.Identity, BoundModelFamily: p.ModelFamily, BoundModel: p.Model,
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
		if err := commitProjectFairClaimTx(ctx, tx, p.LeaseID, p.Fair); err != nil {
			return fmt.Errorf("commit project fair turn: %w", err)
		}
		if domain == "epic_v2" && epicID != "" {
			if err := ensureReviewWakeActionTx(ctx, tx, reviewWakeActionParams{
				ProjectID: projectID, EpicID: epicID, JobID: p.JobID, Repo: deliveryRepo,
				PRNumber: prNumber, HeadSHA: head, BaseSHA: base, Lens: p.Lens,
				LeaseID: p.LeaseID, LeaseEpoch: newEpoch, Deadline: deadline,
				Recipient: recipientBinding, Now: p.Now,
			}); err != nil {
				return err
			}
			nowText := p.Now.UTC().Format(rfc3339)
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
				state='in_review', state_version=state_version+1, reviewer_identity=?,
				reviewer_model_family=?, review_started_at=?, last_reviewer_fact_at=?,
				state_entered_at=?, state_due_at=?, updated_at=?
				WHERE epic_id=? AND state_version=?`, p.Identity, p.ModelFamily, nowText,
				nowText, nowText, deadline.UTC().Format(rfc3339), nowText, epicID, deliveryVersion); err != nil {
				return err
			}
			var epicSeq int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(epic_seq),0)+1 FROM control_events WHERE epic_id=?`, epicID).Scan(&epicSeq); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
				(project_id,epic_id,kind,from_state,to_state,state_version,epic_seq,actor_kind,actor_id,payload_json,created_at)
				VALUES (?,?,'review_claimed','review_queued','in_review',?,?,'reviewer',?,'{}',?)`,
				projectID, epicID, deliveryVersion+1, epicSeq, p.Identity, nowText); err != nil {
				return err
			}
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
	if bindingHold != nil {
		return nil, bindingHold
	}
	return result, nil
}

type reviewWakeActionParams struct {
	ProjectID, EpicID, JobID, Repo string
	PRNumber                       int
	HeadSHA, BaseSHA, Lens         string
	LeaseID                        string
	LeaseEpoch                     int
	Deadline                       time.Time
	Recipient                      DriverSessionBinding
	Now                            time.Time
}

// ensureReviewWakeActionTx is called only after ClaimReviewJob's serialized
// review_pending→code_review UPDATE succeeds. Consequently review_queued means a
// durable obligation exists, while a Driver send action proves a real reviewer
// lease and exact target were atomically bound. Transport acknowledgement remains
// separate from review-stage completion.
func ensureReviewWakeActionTx(ctx context.Context, tx *sql.Tx, p reviewWakeActionParams) error {
	payloadBytes, err := json.Marshal(map[string]any{
		"assignment_version": 1,
		"base_sha":           p.BaseSHA,
		"epic_id":            p.EpicID,
		"head_sha":           p.HeadSHA,
		"job_id":             p.JobID,
		"lease_epoch":        p.LeaseEpoch,
		"lease_id":           p.LeaseID,
		"lens":               p.Lens,
		"pr_number":          p.PRNumber,
		"project_id":         p.ProjectID,
		"repo":               p.Repo,
		"type":               "review_assignment",
	})
	if err != nil {
		return err
	}
	dedup := fmt.Sprintf("%s:%s:review_wake:%s:%s:%d", p.ProjectID, p.EpicID,
		p.HeadSHA, p.BaseSHA, p.LeaseEpoch)
	idHash := sha256.Sum256([]byte(dedup))
	actionID := "review-wake-" + hex.EncodeToString(idHash[:12])
	grantID := stableUUID("driver-review-grant/v1", dedup)
	payloadHash := sha256.Sum256(payloadBytes)
	nowText := p.Now.UTC().Format(rfc3339)
	var evidenceBaselineStoreSeq, evidenceBaselineUncertaintyEpoch uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(high_store_seq),0),
		COALESCE(MAX(uncertainty_epoch),0) FROM driver_observation_cursors
		WHERE store_id=?`, p.Recipient.StoreID).Scan(&evidenceBaselineStoreSeq,
		&evidenceBaselineUncertaintyEpoch); err != nil {
		return fmt.Errorf("capture review wake evidence baseline: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
		 sender_principal_id,recipient_session_id,recipient_pane_instance_id,
		 recipient_agent_run_id,grant_id,grant_epoch,grant_expires_at,head_sha,base_sha,
		 next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'review_wake','pending',0,?,?,?,?,?,'driver','code_reviewer',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?)`,
		actionID, p.ProjectID, p.EpicID, dedup, string(payloadBytes),
		"sha256:"+hex.EncodeToString(payloadHash[:]), evidenceBaselineStoreSeq,
		evidenceBaselineUncertaintyEpoch, p.Recipient.HostID, p.Recipient.StoreID,
		p.Recipient.TmuxServerInstanceID, p.Recipient.LifecycleKey, p.Recipient.TargetEpoch,
		p.Recipient.ProfileID, p.Recipient.WorkspaceRootID, p.Recipient.WorkspaceRelativePath,
		p.LeaseID, p.LeaseEpoch, DriverControlIdentity,
		p.Recipient.SessionID, p.Recipient.PaneInstanceID, p.Recipient.AgentRunID, grantID,
		p.Deadline.UTC().Format(rfc3339), p.HeadSHA, p.BaseSHA, nowText, nowText, nowText)
	if err != nil {
		if !isUniqueConstraintErr(err) {
			return fmt.Errorf("materialize Driver review wake: %w", err)
		}
		var gotKind, gotHash, gotLeaseID, gotRecipientSession, gotRecipientPane, gotRecipientRun string
		var gotLeaseEpoch int
		if qerr := tx.QueryRowContext(ctx, `SELECT kind,payload_sha256,lease_id,lease_epoch,
			recipient_session_id,recipient_pane_instance_id,recipient_agent_run_id
			FROM epic_actions WHERE dedup_key=? AND state<>'cancelled_superseded'`, dedup).
			Scan(&gotKind, &gotHash, &gotLeaseID, &gotLeaseEpoch, &gotRecipientSession,
				&gotRecipientPane, &gotRecipientRun); qerr != nil {
			return fmt.Errorf("materialize Driver review wake collision: %w", qerr)
		}
		if gotKind != "review_wake" || gotHash != "sha256:"+hex.EncodeToString(payloadHash[:]) ||
			gotLeaseID != p.LeaseID || gotLeaseEpoch != p.LeaseEpoch ||
			gotRecipientSession != p.Recipient.SessionID || gotRecipientPane != p.Recipient.PaneInstanceID ||
			gotRecipientRun != p.Recipient.AgentRunID {
			return fmt.Errorf("Driver review wake dedup collision for %s", dedup)
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='driver_binding_claimed',
		resolved_at=?,updated_at=? WHERE epic_id=? AND kind='review_claim_stalled' AND state<>'resolved'`,
		nowText, nowText, p.EpicID)
	return err
}

func markReviewClaimBindingHoldTx(ctx context.Context, tx *sql.Tx, projectID, epicID, jobID, reviewer, detail string, now time.Time) error {
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT state_version FROM epic_deliveries
		WHERE epic_id=? AND state='review_queued' AND review_job_id=?`, epicID, jobID).Scan(&version); err != nil {
		return err
	}
	nowText := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
		state_version=state_version+1,hold_kind='review_session_unbound',hold_reason=?,
		return_state='review_queued',alert_pending=1,last_error=?,state_due_at=?,updated_at=?
		WHERE epic_id=? AND state='review_queued' AND review_job_id=? AND state_version=?`,
		detail, detail, now.Add(24*time.Hour).UTC().Format(rfc3339), nowText,
		epicID, jobID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return lease.ErrLostRace
	}
	dedupHash := sha256.Sum256([]byte(epicID + "\x00" + reviewer))
	dedup := "review_claim_stalled:" + epicID + ":" + hex.EncodeToString(dedupHash[:8])
	attentionID := "review-claim-stalled-" + hex.EncodeToString(dedupHash[:12])
	insertResult, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,verdict,
		 occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,'review_claim_stalled',?,'',10,'open',?,1,'',0,'','',?,
		 json_object('job_id',?,'reviewer_identity',?,'reason',?),?,'','',1,?,?,'',?,?)`, attentionID, epicID, dedup, jobID,
		jobID, reviewer, detail, detail, nowText, nowText, nowText, nowText)
	if err != nil {
		return err
	}
	inserted, _ := insertResult.RowsAffected()
	occurrenceDelta := 1
	if inserted == 1 {
		occurrenceDelta = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET
		occurrences=occurrences+?,last_seen_at=?,detail=?,
		evidence_json=json_object('job_id',?,'reviewer_identity',?,'reason',?),
		state=CASE WHEN state='resolved' THEN 'open' ELSE state END,resolved_at='',updated_at=?
		WHERE dedup_key=?`, occurrenceDelta, nowText, detail, jobID, reviewer, detail, nowText, dedup); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{
		"epic_id": epicID, "job_id": jobID, "reviewer_identity": reviewer, "reason": detail,
	})
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "review_claim_stalled", dedup, string(payload), now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "review_claim_held",
		"review_queued", "review_queued", version+1, "scheduler", string(payload), now)
}

// ReviewResultParams is a fenced code-review result carrying the reviewer's verdict
// CLAIM (untrusted) + requested disposition + an idempotency key.
type ReviewResultParams struct {
	JobID          string
	Epoch          int
	Claim          job.VerdictValue
	Disposition    job.Disposition
	IdempotencyKey string
	// Notes are the reviewer's findings (the "fix X, Y, Z"). On a changes-requested
	// bounce they are carried forward onto the job (LastReviewNotes) so the rebuild's
	// lease context surfaces them — the §F compounding-memory read side.
	Notes string
	// ReviewerHead is the issue-branch HEAD the reviewer advanced with its empty findings-
	// commit (empty when none). On a panel ACCUMULATE the job stays in review across rounds,
	// so the store records this as the job's head — otherwise the async reconcile reads the
	// reviewer's own commit as a SHA move and supersedes the round.
	ReviewerHead string
	Now          time.Time
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
		// A known job head is the patch the reviewer judged and must exactly match
		// reconciled facts. An empty head is not an assertion: fresh non-empty facts
		// establish it for legacy/control-plane result paths that cannot report a SHA.
		if (j.HeadSHA != "" && facts.HeadSHA != j.HeadSHA) ||
			(j.BaseSHA != "" && facts.BaseSHA != j.BaseSHA) {
			facts.CIGreen = false
		}

		// M9 (§9.2, I-11): run the deterministic content-integrity gate over the
		// stored (untrusted) patch + declared blast-radius and thread the Result into
		// the pure engine. A denylist hit / blast-radius mismatch / failed static
		// check forces handoff regardless of the reviewer's self_merge request.
		chk, err := s.contentResultTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}

		// GitHub-visible required CI is the non-bypassable authorization boundary for
		// PR review and merge. Flowbee `test` job facts remain audit data, but they
		// cannot substitute for required GitHub checks on the reconciled PR head: a
		// hidden Flowbee-only head, prior PR head, or base head must never mint a verdict
		// for the current GitHub-visible head.

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

		// per-review-node loop cap: count how many times THIS reviewer identity has
		// ALREADY requested changes on this job (from the ledger), so the gate can park
		// a task a single reviewer keeps rejecting — before the cruder total-bounce
		// backstop. Deterministic given the ledger, like the reconciled facts.
		var reviewerPrior int
		if j.BoundIdentity != "" {
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM job_events
				 WHERE job_id = ? AND kind = 'verdict_claim' AND actor = ?
				   AND json_extract(payload, '$.VerdictClaim') = 'changes_requested'`,
				in.JobID, j.BoundIdentity).Scan(&reviewerPrior); err != nil {
				return fmt.Errorf("count reviewer rejections: %w", err)
			}
		}

		// F5 multi-reviewer consensus: count the DISTINCT reviewers who have already approved
		// in the CURRENT round — i.e. since the head the panel is reviewing was last
		// (re)established. A round boundary is ANY event that puts a NEW reviewed head into
		// review_pending: a fresh build result (result_accepted), a clean auto-rebase onto a
		// moved base (rebased), or a conflict resolution (conflict_resolved). It is NOT
		// review_approved — that is the intra-round accumulate (the reviewer's own empty
		// findings commit), which must PRESERVE the count. Scoping only to result_accepted
		// would leak a prior-head approval into a post-rebase/post-resolve round, minting an
		// N-reviewer panel with fewer than N distinct reviewers of the actual merged code.
		// With RequiredReviewers=N, the gate mints only on the Nth distinct approval; below N
		// it accumulates (re-arms review_pending for the next reviewer). The panel anti-affinity
		// at claim time guarantees these approvers are distinct identities.
		var priorApprovals int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT actor) FROM job_events
			 WHERE job_id = ? AND kind = 'verdict_claim'
			   AND json_extract(payload, '$.VerdictClaim') = 'approved'
			   AND job_seq > (SELECT COALESCE(MAX(job_seq),0) FROM job_events
			                   WHERE job_id = ? AND kind IN ('result_accepted','rebased','conflict_resolved','adopt_rearmed'))`,
			in.JobID, in.JobID).Scan(&priorApprovals); err != nil {
			return fmt.Errorf("count round approvals: %w", err)
		}

		dec := engine.Decide(engine.EngineState{
			Job: j, Now: in.Now, Epoch: j.LeaseEpoch, GitHub: facts, Policy: p, Content: &chk,
		}, engine.ReviewClaim{Epoch: in.Epoch, Value: in.Claim, Disposition: in.Disposition, ReviewerPriorRejections: reviewerPrior, PriorApprovals: priorApprovals})
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
		// the per-reviewer rejection cap is what parks a review at needs_human while total
		// bounces is still under max — stamp that distinct reason on the entering event so
		// a re-fold preserves it (else the §12.6.1 view would mislabel the park). Computed
		// here, off the loop, because the condition doesn't depend on which transition fires.
		reviewerRejectionReason := ""
		if in.Claim == job.VerdictChangesRequested && reviewerPrior+1 >= job.MaxReviewerRejections {
			reviewerRejectionReason = string(job.EscalationReviewerRejections)
		}
		for _, t := range dec.Transitions {
			nextSeq++
			pay := ledger.Payload{BouncesDelta: t.BouncesDelta, Verdict: t.Verdict}
			if t.To == job.StateNeedsHuman {
				pay.EscalationReason = reviewerRejectionReason
			}
			// F5 panel accumulate: carry the head the reviewer just advanced so the fold tracks
			// it (the projection does too, below) — keeps the job's head current across rounds so
			// reconcile doesn't read the reviewer's own empty commit as a SHA move and supersede.
			//
			// TRUST ASSUMPTION (defense-in-depth gap, tracked): ReviewerHead is reported by the
			// worker and folded here WITHOUT server-side tree validation. It is safe because the
			// honest path can only ever advance the head by a SAME-TREE empty commit:
			// reviewerEmptyCommit (worker/review.go) builds the verdict commit in its OWN fresh
			// temp worktree the review agent never touches, then `git commit --allow-empty` with
			// nothing staged — structurally a no-content commit. A ROGUE worker that bypassed the
			// harness (pushed a different-tree commit to the issue branch AND reported its SHA
			// here) could launder that head into j.HeadSHA, defeating reconcile's flowbeePlaced
			// supersede guard and binding an N>=2 panel mint to a tree no reviewer judged. Closing
			// that fully needs reconcile to verify tree(head)==tree(parent) before treating a
			// head move as flowbeePlaced (a GitHub tree compare) — a threat-model call (defend
			// against a compromised authenticated fleet member?) deliberately left to the operator
			// rather than rushed into the live merge path. Not reachable via the honest fleet; the
			// panel is opt-in (RequiredReviewers default 1, where this head never persists across a
			// sweep with a banked approval waiting).
			if t.Kind == ledger.KindReviewApproved && in.ReviewerHead != "" {
				pay.HeadSHA = in.ReviewerHead
			}
			// carry the reviewer's findings forward on the bounce so the rebuild surfaces
			// them (§F read side); folded onto LastReviewNotes.
			if in.Claim == job.VerdictChangesRequested {
				pay.ReviewNotes = in.Notes
			}
			ev := ledger.Event{
				JobID: in.JobID, JobSeq: nextSeq, Kind: t.Kind,
				FromState: t.From, ToState: t.To, LeaseEpoch: j.LeaseEpoch,
				Actor: "system", CreatedAt: in.Now,
				Payload: pay,
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
			       patch_diff = CASE WHEN adopted = 1 THEN patch_diff ELSE '' END,
			       declared_blast_radius = CASE WHEN adopted = 1 THEN declared_blast_radius ELSE '' END,
				       reservation_paths = '', reservation_wide = 0,
				       enqueued_at = ?,
				       last_review_notes = CASE WHEN ? <> '' THEN ? ELSE last_review_notes END,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:eng_worker"}), bouncesDelta,
				in.Now.Format(rfc3339), in.Notes, in.Notes, in.JobID); err != nil {
				return fmt.Errorf("apply bounce projection: %w", err)
			}
		} else if final == job.StateReviewPending {
			// F5 panel sub-threshold approval: re-arm review_pending for the NEXT distinct
			// reviewer. Restore the review-pending baseline (role:eng_worker + the reviewer
			// capability) and release this reviewer's lease, mirroring the KindReviewApproved
			// fold exactly so projection == Fold(events).
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'review_pending', role = 'eng_worker',
				       required_capabilities = ?,
				       head_sha = CASE WHEN ? <> '' THEN ? ELSE head_sha END,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:code_reviewer"}), in.ReviewerHead, in.ReviewerHead, in.JobID); err != nil {
				return fmt.Errorf("apply panel-accumulate projection: %w", err)
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
		// stamp the explicit per-reviewer escalation reason: when this cap fires the
		// total bounces is still under max_bounces, so the inferred classifier would
		// mislabel the park. Make the trigger legible to the operator queue.
		if final == job.StateNeedsHuman && in.Claim == job.VerdictChangesRequested &&
			reviewerPrior+1 >= job.MaxReviewerRejections {
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET escalation_reason = ? WHERE id = ?`,
				string(job.EscalationReviewerRejections), in.JobID); err != nil {
				return fmt.Errorf("stamp reviewer-rejection escalation: %w", err)
			}
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
		if err := projectEpicReviewResultTx(ctx, tx, in.JobID, final, minted, in.Notes, in.Now); err != nil {
			return fmt.Errorf("project epic review result: %w", err)
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
	var final job.State
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}
		facts, _, err := domainBFactsTx(ctx, tx, in.JobID)
		if err != nil {
			return fmt.Errorf("read merge facts: %w", err)
		}
		chk, err := s.contentResultTx(ctx, tx, in.JobID)
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
