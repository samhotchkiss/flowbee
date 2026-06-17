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
	"github.com/samhotchkiss/flowbee/internal/liveness"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

// rfc3339 is the canonical timestamp format stored in TEXT columns that carry a
// precise instant (lease deadline / hb_due). datetime('now') columns use SQLite's
// default 'YYYY-MM-DD HH:MM:SS'; instants we compare against a clock use RFC3339.
const rfc3339 = time.RFC3339Nano

// SeedParams describes a hand-seeded job (the M1/M2 manual seed). If BlockedBy is
// non-empty and any predecessor is not yet `done`, the job is seeded `blocked`;
// otherwise it is seeded `ready`. RequiredCapabilities are the tags a worker must
// attest to win the lease (§6.6).
type SeedParams struct {
	ID                   string
	Kind                 job.Kind
	Flow                 string
	Stage                string
	Role                 job.Role
	BaseSHA              string
	Priority             int
	BlockedBy            []string
	RequiredCapabilities []string
	Now                  time.Time
	// M10 cost metering (§6.7, I-15). CostCeilingMicroUSD is the enforced per-job $
	// ceiling in micro-USD ($1.00 = 1_000_000); nil/0 = no ceiling. FlowID groups
	// the spec+build+review jobs of one feature for the per-flow rollup (§12.6.5);
	// empty falls back to the job's own id.
	CostCeilingMicroUSD *int64
	FlowID              string
	// F1 task/context: the human intent folded onto the job, shipped (resolved) in
	// the lease grant's context block. Settable via `flowbee seed` or a GitHub
	// issue body. Empty is fine (a bare M1 seed carries no task).
	TaskText           string
	SpecText           string
	AcceptanceCriteria string
	// Repo is the F9 repo-scope handle (a repos.id). Empty is the legacy single-repo
	// default. A seeded job is bound to exactly one repo so reconcile-IN/project-OUT
	// can scope it; the scheduler still ranks across all repos.
	Repo string
}

// SeedJob inserts a job and its job_created event in one transaction (append +
// projection are atomic). The job starts `blocked` if it has any not-yet-`done`
// predecessor, else `ready`. Returns the seeded job.
func (s *Store) SeedJob(ctx context.Context, p SeedParams) (job.Job, error) {
	var j job.Job
	err := s.tx(ctx, func(tx *sql.Tx) error {
		blocked, err := hasUnsatisfiedDeps(ctx, tx, p.BlockedBy)
		if err != nil {
			return err
		}
		state := job.StateReady
		if blocked {
			state = job.StateBlocked
		}
		blockedJSON := marshalStrings(p.BlockedBy)
		reqJSON := marshalStrings(p.RequiredCapabilities)
		flowID := p.FlowID
		if flowID == "" {
			flowID = p.ID // a standalone job is a flow of one (§12.6.5)
		}
		var ceiling any
		if p.CostCeilingMicroUSD != nil {
			ceiling = *p.CostCeilingMicroUSD
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, base_sha, priority,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq,
			                  cost_ceiling_micro_usd, flow_id,
			                  task_text, spec_text, acceptance_criteria, repo)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, 5, 0, 3, 0, ?, ?, ?, ?, ?, ?)`,
			p.ID, string(p.Kind), p.Flow, p.Stage, string(state), string(p.Role), p.BaseSHA, p.Priority,
			blockedJSON, reqJSON, p.Now.Format(rfc3339), ceiling, flowID,
			p.TaskText, p.SpecText, p.AcceptanceCriteria, p.Repo)
		if err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
		ev := ledger.Event{
			JobID:     p.ID,
			JobSeq:    1,
			Kind:      ledger.KindJobCreated,
			ToState:   state,
			Actor:     "system",
			CreatedAt: p.Now,
			Payload: ledger.Payload{
				Kind: p.Kind, Flow: p.Flow, Stage: p.Stage, Role: p.Role,
				BaseSHA: p.BaseSHA, Priority: p.Priority,
				BlockedBy: p.BlockedBy, RequiredCapabilities: p.RequiredCapabilities,
				TaskText: p.TaskText, SpecText: p.SpecText, AcceptanceCriteria: p.AcceptanceCriteria,
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if err := setJobSeq(ctx, tx, p.ID, 1); err != nil {
			return err
		}
		if state == job.StateReady {
			if err := s.armNoEligibleTimerTx(ctx, tx, p.ID, 0, p.Now); err != nil {
				return fmt.Errorf("arm alarm: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return j, err
	}
	return s.GetJob(ctx, p.ID)
}

// hasUnsatisfiedDeps reports whether any predecessor id is not in state `done`.
func hasUnsatisfiedDeps(ctx context.Context, tx *sql.Tx, deps []string) (bool, error) {
	for _, dep := range deps {
		var state string
		err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id = ?`, dep).Scan(&state)
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil // an unknown predecessor blocks (defensive)
		}
		if err != nil {
			return false, fmt.Errorf("check dep %s: %w", dep, err)
		}
		if job.State(state) != job.StateDone {
			return true, nil
		}
	}
	return false, nil
}

// ClaimParams describes one atomic-claim attempt against a specific job.
type ClaimParams struct {
	JobID       string
	LeaseID     string
	Identity    string
	ModelFamily string
	// WorkerID is the box claiming (F6): used to gate the claim on the box's
	// advertised PER-MODEL slot budget (don't start a 4th claude job on a box that
	// advertised claude:3). Empty skips the slot gate (legacy single-slot worker).
	WorkerID string
	// Lens is the resolved review lens (F5) the worker is fenced to apply for this
	// lease (correctness|tests|security|""). It is part of the resolved identity
	// the flow layer fences into the lease so a multi-reviewer fan-out's reviewers
	// each carry their own lens. Empty for non-review stages.
	Lens string
	Role        job.Role
	// Attested is the worker's attested capability set; the claim only succeeds
	// if it satisfies the job's required_capabilities (§6.6). A worker lacking a
	// required capability gets ErrLostRace (the job stays `ready`).
	Attested []string
	TTL      time.Duration
	Now      time.Time
}

// ClaimReadyJob runs the §6.3.1 atomic claim against the named job: a single
// UPDATE ... WHERE state='ready' that bumps the epoch and binds the worker.
// 0 rows affected -> ErrLostRace (another worker won, or the job left `ready`).
// On success it appends the lease_claimed event + inserts the leases audit row,
// all in one serialized transaction.
func (s *Store) ClaimReadyJob(ctx context.Context, p ClaimParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	hbDue := p.Now.Add(p.TTL) // M1: heartbeat-due == deadline window; tightened later
	var result *lease.Lease

	err := s.tx(ctx, func(tx *sql.Tx) error {
		// §6.6 capability match: a worker lacking a required attested capability
		// must NOT win the row. Read the candidate's required set and check it
		// against the attested set; on mismatch return ErrLostRace (the job stays
		// `ready`, so the no_eligible_worker alarm can eventually fire, I-6). The
		// store serializes writes (MaxOpenConns=1), so this read+UPDATE is atomic.
		var reqJSON, curState string
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state FROM jobs WHERE id = ?`, p.JobID).Scan(&reqJSON, &curState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("read required caps: %w", err)
		}
		if job.State(curState) != job.StateReady {
			return lease.ErrLostRace
		}
		if !job.CapabilitiesSatisfy(p.Attested, unmarshalStrings(reqJSON)) {
			return lease.ErrLostRace
		}

		// F6 per-model SLOT gate: the box may not exceed its advertised concurrency
		// for this model (claude:3 means at most 3 concurrent claude leases on the
		// box). A full slot budget returns ErrNoCapacity -> the lease loop treats it
		// like a lost race (the job stays leasable; another box can take it).
		if err := modelSlotGateTx(ctx, tx, p.WorkerID, p.Identity, p.ModelFamily); err != nil {
			if errors.Is(err, ErrNoCapacity) {
				return lease.ErrLostRace
			}
			return err
		}
		// F6 ceiling-gated account selection (ROLLOVER): pick the lowest-rank account
		// for the model that is BELOW its ceiling. If every account is at/over ceiling
		// the box waits (ErrNoCapacity -> lost race -> the job stays `ready` and the
		// no_eligible_worker alarm can fire). The chosen account is bound on the job so
		// per-account usage attribution and slot accounting are exact.
		acct, aerr := selectAccountTx(ctx, tx, p.ModelFamily)
		if aerr != nil {
			if errors.Is(aerr, ErrNoCapacity) {
				return lease.ErrLostRace
			}
			return aerr
		}

		// §6.3.1 atomic claim. The anti-affinity NOT EXISTS clauses are inert in
		// M1 (null sibling pointers) but wired for M4.
		row := tx.QueryRowContext(ctx, `
			UPDATE jobs
			   SET state              = 'leased',
			       bound_identity     = ?,
			       bound_model_family = ?,
			       bound_lens         = ?,
			       bound_account      = ?,
			       lease_epoch        = lease_epoch + 1,
			       lease_id           = ?,
			       lease_deadline     = ?,
			       lease_hb_due       = ?,
			       updated_at         = datetime('now')
			 WHERE id    = ?
			   AND state = 'ready'
			   AND NOT EXISTS (
			        SELECT 1 FROM jobs sib
			         WHERE sib.id = jobs.eng_worker_job
			           AND ? = 'code_reviewer'
			           AND ( sib.bound_identity = ? OR sib.bound_model_family = ? ) )
			   AND NOT EXISTS (
			        SELECT 1 FROM jobs sib
			         WHERE sib.id = jobs.code_reviewer_job
			           AND ? = 'merger'
			           AND sib.bound_identity = ? )
			RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.Lens, acct.AccountID, p.LeaseID,
			deadline.Format(rfc3339), hbDue.Format(rfc3339),
			p.JobID,
			string(p.Role), p.Identity, p.ModelFamily,
			string(p.Role), p.Identity,
		)
		var newEpoch, prevSeq int
		if err := row.Scan(&newEpoch, &prevSeq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("atomic claim: %w", err)
		}

		nextSeq := prevSeq + 1
		ev := ledger.Event{
			JobID:      p.JobID,
			JobSeq:     nextSeq,
			Kind:       ledger.KindLeaseClaimed,
			FromState:  job.StateReady,
			ToState:    job.StateLeased,
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
		_, err := tx.ExecContext(ctx, `
			INSERT INTO leases (lease_id, job_id, lease_epoch, identity, model_family, ttl_s, deadline)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.LeaseID, p.JobID, newEpoch, p.Identity, p.ModelFamily,
			int(p.TTL/time.Second), deadline.Format(rfc3339))
		if err != nil {
			return fmt.Errorf("insert lease audit: %w", err)
		}

		result = &lease.Lease{
			LeaseID:     p.LeaseID,
			JobID:       p.JobID,
			Epoch:       newEpoch,
			Identity:    p.Identity,
			ModelFamily: p.ModelFamily,
			TTL:         p.TTL,
			GrantedAt:   p.Now,
			Deadline:    deadline,
			HBDue:       hbDue,
			State:       lease.StateActive,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// HeartbeatParams is a fenced heartbeat. The liveness observations (§10.2) feed the
// lower, gameable rungs (HINTS) and the two free fast-paths (§10.6): a Rung-0
// agent_exited_zombie -> failed; awaiting_input -> cancel + clean re-dispatch.
type HeartbeatParams struct {
	JobID string
	Epoch int
	Now   time.Time
	// Rung-0/1 hints (worker-reported; never kill alone, I-13). Recorded so a later
	// Rung-2/Rung-3 corroboration can satisfy the two-rung rule.
	Health        liveness.AgentHealth
	Rung1         liveness.Rung1Class
	AwaitingInput bool // §10.6 fast-path: directive cancel
	AgentExited   bool // §10.6 fast-path: zombie -> failed
}

// Heartbeat applies engine.Decide for a fenced heartbeat. Stale epoch ->
// lease.ErrStaleEpoch (409). On continue it extends lease_hb_due, records the
// Rung-0/1 hints, and appends a heartbeat event. The two free fast-paths (§10.6)
// short-circuit to a cancel directive + the revoke transition (agent_exited ->
// failed; awaiting_input -> ready), applied transactionally with the epoch bump.
func (s *Store) Heartbeat(ctx context.Context, p HeartbeatParams) (engine.Directive, error) {
	var dir engine.Directive
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		dec := engine.Decide(
			engine.EngineState{Job: j, Now: p.Now, Epoch: j.LeaseEpoch},
			engine.Heartbeat{
				Epoch: p.Epoch, Health: p.Health,
				AwaitingInput: p.AwaitingInput, AgentExited: p.AgentExited,
			})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}
		if dec.Directive != nil {
			dir = *dec.Directive
		}
		// §10.6 fast-path: the heartbeat carried a conclusive signal. Apply the revoke
		// transition (epoch++ + compensation) transactionally and return the cancel.
		if len(dec.Transitions) > 0 {
			t := dec.Transitions[0]
			newEpoch := j.LeaseEpoch
			if t.BumpEpoch {
				newEpoch = j.LeaseEpoch + 1
			}
			return applyRevokeTx(ctx, tx, &j, seq, t, newEpoch, p.Now)
		}
		// record liveness: bump last-seen + the Rung-0/1 hints (the absolute lease
		// deadline is unchanged — only the un-gameable Rung-3 clock can move it).
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET lease_hb_due = ?, last_heartbeat_at = ?,
			                agent_health = ?, rung1_class = ?, updated_at = datetime('now')
			 WHERE id = ?`,
			p.Now.Format(rfc3339), p.Now.Format(rfc3339),
			string(p.Health), string(p.Rung1), p.JobID); err != nil {
			return fmt.Errorf("extend heartbeat: %w", err)
		}
		// bump the bound worker's last_seen so the roster's stale-hb badge clears on
		// a live heartbeat (§12.6.2). Keyed by the job's bound identity.
		if j.BoundIdentity != "" {
			if _, err := tx.ExecContext(ctx,
				`UPDATE workers SET last_seen_at = ? WHERE identity = ?`,
				p.Now.Format(rfc3339), j.BoundIdentity); err != nil {
				return fmt.Errorf("touch worker last_seen: %w", err)
			}
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindHeartbeat,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: j.BoundIdentity, CreatedAt: p.Now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, p.JobID, nextSeq)
	})
	if err != nil {
		return "", err
	}
	return dir, nil
}

// ResultParams is a fenced, idempotent work-product result.
type ResultParams struct {
	JobID          string
	Epoch          int
	IdempotencyKey string
	Now            time.Time
	// PushedRef is the epoch-namespaced ref the eng_worker pushed its build to
	// (§7.3). Flowbee records it on the job so it can validate+promote it (the full
	// PR-open trigger is M7); an empty value (e.g. a review result) is ignored.
	PushedRef string
	// PatchDiff is the eng_worker's returned diff (§7.3) — UNTRUSTED DATA the M9
	// content-integrity gate (§9.2, I-11) judges. Stored verbatim at build-result
	// time; the code_review gate later runs the deterministic checks over it.
	PatchDiff string
	// DeclaredBlastRadius is the worker's DECLARED scope (paths + scope), a
	// commitment Flowbee verifies against the actual diff (§9.2b). JSON-encoded.
	DeclaredBlastRadius string
}

// ResultResponse is the cached/applied response for a result POST.
type ResultResponse struct {
	Accepted bool   `json:"accepted"`
	JobState string `json:"job_state"`
}

// Result applies engine.Decide for a fenced result. Idempotency: a duplicate key
// returns the cached response with NO re-apply / no re-emit. Stale epoch ->
// lease.ErrStaleEpoch (409).
func (s *Store) Result(ctx context.Context, p ResultParams) (ResultResponse, error) {
	var resp ResultResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		// idempotency: return cached response with no side effects.
		if p.IdempotencyKey != "" {
			var cached string
			err := tx.QueryRowContext(ctx,
				`SELECT response FROM result_idempotency WHERE job_id=? AND idempotency_key=?`,
				p.JobID, p.IdempotencyKey).Scan(&cached)
			if err == nil {
				return json.Unmarshal([]byte(cached), &resp)
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("idempotency lookup: %w", err)
			}
		}

		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		dec := engine.Decide(engine.EngineState{Job: j, Now: p.Now, Epoch: j.LeaseEpoch}, engine.WorkResult{Epoch: p.Epoch})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}
		from := j.State
		nextSeq := seq
		for _, t := range dec.Transitions {
			nextSeq++
			ev := ledger.Event{
				JobID: p.JobID, JobSeq: nextSeq, Kind: t.Kind,
				FromState: t.From, ToState: t.To, LeaseEpoch: j.LeaseEpoch,
				Actor: j.BoundIdentity, CreatedAt: p.Now,
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
		}
		final := from
		if n := len(dec.Transitions); n > 0 {
			final = dec.Transitions[n-1].To
		}
		// apply the projection: clear the live lease, advance state. When a build
		// job lands review_pending, the NEXT stage is the code_review gate — so the
		// job's required capabilities flip to the reviewer role's (§5.2). This is
		// what makes the gate leasable by a code_reviewer and NOT by an eng_worker,
		// and lets M4's anti-affinity exclusion bite on a distinct reviewer.
		if final == job.StateReviewPending {
			// I-10 anti-affinity input: persist the BUILDER's identity + model_family
			// DURABLY before clearing the live bound_* columns. The review claim
			// (§6.3.1) reads these from the sibling (eng_worker_job = this job) to
			// exclude the builder's identity AND model_family from the code_reviewer
			// lease — a reviewer may never judge its own (or a same-model) build.
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = ?, role = 'eng_worker',
				       required_capabilities = ?,
				       builder_identity     = COALESCE(builder_identity, bound_identity),
				       builder_model_family = COALESCE(builder_model_family, bound_model_family),
				       head_ref = COALESCE(NULLIF(?, ''), head_ref),
				       patch_diff = ?, declared_blast_radius = ?,
				       lease_id = NULL, bound_identity = NULL,
				       bound_model_family = NULL, lease_hb_due = NULL,
				       eng_worker_job = COALESCE(eng_worker_job, id),
				       updated_at = datetime('now')
				 WHERE id = ?`,
				string(final), marshalStrings([]string{"role:code_reviewer"}), p.PushedRef,
				p.PatchDiff, p.DeclaredBlastRadius, p.JobID); err != nil {
				return fmt.Errorf("apply result projection: %w", err)
			}
			// arm the review-stage no_eligible_worker alarm (I-6): if no compliant,
			// independent code_reviewer claims this review_pending job before the
			// window, the alarm fires. This is the surface where a single-provider
			// fleet reveals itself — the model_family anti-affinity term is
			// unsatisfiable, so the review stage alarms (§6.6, §5.6). The guard epoch
			// is the job's current (post-build) lease_epoch; a later review claim
			// bumps it, making the timer a no-op.
			if err := s.armNoEligibleTimerTx(ctx, tx, p.JobID, j.LeaseEpoch, p.Now); err != nil {
				return fmt.Errorf("arm review alarm: %w", err)
			}
		} else if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = ?, lease_id = NULL, bound_identity = NULL,
			       bound_model_family = NULL, lease_hb_due = NULL,
			       updated_at = datetime('now')
			 WHERE id = ?`, string(final), p.JobID); err != nil {
			return fmt.Errorf("apply result projection: %w", err)
		}
		if err := setJobSeq(ctx, tx, p.JobID, nextSeq); err != nil {
			return err
		}
		// close the lease audit row.
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'completed'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			p.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close lease: %w", err)
		}

		resp = ResultResponse{Accepted: true, JobState: string(final)}
		if p.IdempotencyKey != "" {
			blob, _ := json.Marshal(resp)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO result_idempotency (job_id, idempotency_key, response) VALUES (?, ?, ?)`,
				p.JobID, p.IdempotencyKey, string(blob)); err != nil {
				return fmt.Errorf("store idempotency: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return ResultResponse{}, err
	}
	return resp, nil
}

// ReleaseParams is a fenced voluntary release back to `ready`.
type ReleaseParams struct {
	JobID string
	Epoch int
	Now   time.Time
	// NoPenalty re-arms WITHOUT burning an attempt: for an abandon that is not a build
	// failure — e.g. the worker built successfully but lost a fast-forward race when
	// the issue branch moved under it (a rebase-before-review). Counting re-validation
	// churn as failures can exhaust max_attempts and escalate a GOOD change to
	// needs_human; this keeps the attempt budget for genuine build failures.
	NoPenalty bool
}

// Release applies engine.Decide for a fenced release: state -> ready, attempts++,
// lease audit closed. Stale epoch -> lease.ErrStaleEpoch (409).
func (s *Store) Release(ctx context.Context, p ReleaseParams) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		dec := engine.Decide(engine.EngineState{Job: j, Now: p.Now, Epoch: j.LeaseEpoch}, engine.Release{Epoch: p.Epoch})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}
		// re-arm to the state the transition table says a release lands in: a build
		// lease (leased/building) -> ready (a re-buildable abandon), a GATE lease
		// (code_review / spec_review / spec_authoring) -> back to its gate state. A
		// release that hard-coded 'ready' would re-arm the BUILD when a reviewer
		// merely let go — rebuilding good work. Only a build abandon burns an attempt.
		toState, terr := job.Next(j.State, job.TriggerReleased)
		if terr != nil {
			toState = job.StateReady
		}
		attemptsDelta := 0
		// A build abandon (-> ready) OR a failed conflict resolution (-> resolving_conflict)
		// burns an attempt, so a worker that keeps failing escalates after max_attempts
		// instead of churning the claim forever. Gate releases that merely let go of good
		// work (code_review -> review_pending, spec_review -> spec_authoring) do not.
		if !p.NoPenalty && (toState == job.StateReady || toState == job.StateResolvingConflict) {
			attemptsDelta = 1
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindLeaseReleased,
			FromState: j.State, ToState: toState, LeaseEpoch: j.LeaseEpoch,
			Actor: j.BoundIdentity, CreatedAt: p.Now,
			Payload: ledger.Payload{AttemptsDelta: attemptsDelta},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = ?, lease_id = NULL, bound_identity = NULL,
			       bound_model_family = NULL, lease_hb_due = NULL,
			       attempts = attempts + ?, updated_at = datetime('now')
			 WHERE id = ?`, string(toState), attemptsDelta, p.JobID); err != nil {
			return fmt.Errorf("apply release projection: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'released'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			p.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close lease: %w", err)
		}
		// A penalty release that exhausts the attempts budget escalates to needs_human
		// rather than re-arming a `ready` build that would just be re-claimed and fail
		// the same way. Without this, an agent that consistently produces no output (or
		// fails fast) churns FOREVER: the claim path has no attempts guard,
		// EvaluateLiveness only sees active leases (it misses the sub-second lease
		// windows of a fast no-output cycle), and the claim→release churn keeps
		// updated_at fresh so the forward-progress watchdog never reads the stall.
		// Escalate with a distinct KindStateChanged (the watchdog's pattern), keeping the
		// lease_released event's "abandoned, attempt burned" meaning clean. max_attempts
		// == 0 means "no cap" — such a job stays re-armable by design.
		if attemptsDelta == 1 && j.MaxAttempts > 0 && j.Attempts+1 >= j.MaxAttempts {
			escSeq := nextSeq + 1
			esc := ledger.Event{
				JobID: p.JobID, JobSeq: escSeq, Kind: ledger.KindStateChanged,
				FromState: toState, ToState: job.StateNeedsHuman, LeaseEpoch: j.LeaseEpoch,
				Actor: "attempts-exhausted", CreatedAt: p.Now,
			}
			if err := appendEvent(ctx, tx, esc); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET state = 'needs_human', escalation_reason = ?, updated_at = datetime('now') WHERE id = ?`,
				string(job.EscalationAttempts), p.JobID); err != nil {
				return fmt.Errorf("escalate exhausted release: %w", err)
			}
			return setJobSeq(ctx, tx, p.JobID, escSeq)
		}
		return setJobSeq(ctx, tx, p.JobID, nextSeq)
	})
}

// ReadyCandidates returns every `ready` job as a scheduler.Candidate, for the
// long-poll loop to rank (aging + priority) and filter (capability match). The
// atomic claim's WHERE state='ready' remains the correctness guarantee; this is
// candidate selection only.
func (s *Store) ReadyCandidates(ctx context.Context) ([]scheduler.Candidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, priority, enqueued_at, required_capabilities
		  FROM jobs WHERE state='ready'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Candidate
	for rows.Next() {
		var c scheduler.Candidate
		var enqueued, reqJSON string
		if err := rows.Scan(&c.JobID, &c.Priority, &enqueued, &reqJSON); err != nil {
			return nil, err
		}
		if ts, perr := time.Parse(rfc3339, enqueued); perr == nil {
			c.EnqueuedAt = ts
		}
		c.RequiredCapabilities = unmarshalStrings(reqJSON)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ReviewPendingCandidates returns every `review_pending` job as a scheduler
// Candidate, for a code_reviewer's long-poll loop to rank and claim. The atomic
// review claim's WHERE state='review_pending' remains the correctness guarantee.
func (s *Store) ReviewPendingCandidates(ctx context.Context) ([]scheduler.Candidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT j.id, j.priority, j.enqueued_at, j.required_capabilities,
		       COALESCE(f.pr_exists,0), COALESCE(f.ci_green,0), COALESCE(f.merged,0)
		  FROM jobs j
		  LEFT JOIN domain_b_facts f ON f.job_id = j.id
		 WHERE j.state='review_pending'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Candidate
	for rows.Next() {
		var c scheduler.Candidate
		var enqueued, reqJSON string
		var prExists, ciGreen, merged int
		if err := rows.Scan(&c.JobID, &c.Priority, &enqueued, &reqJSON, &prExists, &ciGreen, &merged); err != nil {
			return nil, err
		}
		if ts, perr := time.Parse(rfc3339, enqueued); perr == nil {
			c.EnqueuedAt = ts
		}
		c.RequiredCapabilities = unmarshalStrings(reqJSON)
		// CIReady mirrors the grant's ci_ready (server): a review is offerable only when
		// its reconciled CI is green. A not-ready review is NOT a candidate at all — it
		// waits quietly in review_pending until CI reconciles green (then it appears
		// here) or red (reconcile bounces it). Offering it would make every reviewer
		// poll claim-then-instantly-release it for the whole CI window — a fleet-wide
		// busy-wait that inflated lease_epoch and bloated the ledger by ~200 events per
		// CI run (observed: 98 churn cycles over a 40-min CI wait). The forward-progress
		// watchdog treats a PR-open/CI-pending review as healthy (it no longer relies on
		// that churn to keep updated_at fresh — see ReconcileStuck).
		c.CIReady = prExists == 1 && ciGreen == 1 && merged == 0
		if !c.CIReady {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CompleteParams marks a job done (M2: review_pending -> done, hand-driven). The
// completion clears any dependents whose blocked_by is now fully satisfied
// (blocked -> ready), all in one serialized transaction. Returns the ids of the
// dependents that became `ready` (so the runtime can publish/poke them).
type CompleteParams struct {
	JobID string
	Now   time.Time
}

// CompleteJob transitions a `review_pending` job to `done` and clears dependents.
// It appends the job_completed event, then for every job whose blocked_by lists
// this job AND whose every predecessor is now `done`, transitions blocked->ready
// and appends a deps_cleared event. Returns the unblocked dependent ids.
func (s *Store) CompleteJob(ctx context.Context, p CompleteParams) ([]string, error) {
	var unblocked []string
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		to, err := job.Next(j.State, job.TriggerCompleted)
		if err != nil {
			return fmt.Errorf("complete %s: %w", p.JobID, err)
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindJobCompleted,
			FromState: j.State, ToState: to, Actor: "system", CreatedAt: p.Now,
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state=?, updated_at=datetime('now') WHERE id=?`,
			string(to), p.JobID); err != nil {
			return fmt.Errorf("apply completion: %w", err)
		}
		if err := setJobSeq(ctx, tx, p.JobID, nextSeq); err != nil {
			return err
		}

		// clear dependents now fully unblocked.
		rows, err := tx.QueryContext(ctx,
			`SELECT id, blocked_by, job_seq FROM jobs WHERE state='blocked'`)
		if err != nil {
			return fmt.Errorf("scan dependents: %w", err)
		}
		type dep struct {
			id      string
			blocked []string
			seq     int
		}
		var deps []dep
		for rows.Next() {
			var d dep
			var bj string
			if err := rows.Scan(&d.id, &bj, &d.seq); err != nil {
				rows.Close()
				return err
			}
			d.blocked = unmarshalStrings(bj)
			deps = append(deps, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		for _, d := range deps {
			dependsOnThis := false
			for _, b := range d.blocked {
				if b == p.JobID {
					dependsOnThis = true
					break
				}
			}
			if !dependsOnThis {
				continue
			}
			stillBlocked, err := hasUnsatisfiedDeps(ctx, tx, d.blocked)
			if err != nil {
				return err
			}
			if stillBlocked {
				continue
			}
			dseq := d.seq + 1
			dev := ledger.Event{
				JobID: d.id, JobSeq: dseq, Kind: ledger.KindDepsCleared,
				FromState: job.StateBlocked, ToState: job.StateReady,
				Actor: "system", CreatedAt: p.Now,
			}
			if err := appendEvent(ctx, tx, dev); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE jobs SET state='ready', enqueued_at=?, updated_at=datetime('now') WHERE id=?`,
				p.Now.Format(rfc3339), d.id); err != nil {
				return fmt.Errorf("unblock %s: %w", d.id, err)
			}
			if err := setJobSeq(ctx, tx, d.id, dseq); err != nil {
				return err
			}
			// the dependent is now `ready` at epoch 0: arm its alarm.
			if err := s.armNoEligibleTimerTx(ctx, tx, d.id, 0, p.Now); err != nil {
				return fmt.Errorf("arm alarm for %s: %w", d.id, err)
			}
			unblocked = append(unblocked, d.id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return unblocked, nil
}

// GetJob reads the jobs projection for id.
func (s *Store) GetJob(ctx context.Context, id string) (job.Job, error) {
	return scanJob(s.DB.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, id))
}

// JobHeadRef returns the epoch-namespaced ref the eng_worker pushed its build to,
// recorded on the build result (§7.3). Empty if none recorded yet.
func (s *Store) JobHeadRef(ctx context.Context, id string) (string, error) {
	var ref sql.NullString
	err := s.DB.QueryRowContext(ctx, `SELECT head_ref FROM jobs WHERE id = ?`, id).Scan(&ref)
	if err != nil {
		return "", err
	}
	return ref.String, nil
}

// LoadEvents reads the full ordered event stream for a job (for replay tests).
func (s *Store) LoadEvents(ctx context.Context, jobID string) ([]ledger.Event, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT job_id, job_seq, kind, from_state, to_state, lease_epoch, actor, created_at, payload
		  FROM job_events WHERE job_id = ? ORDER BY job_seq ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ledger.Event
	for rows.Next() {
		var (
			e           ledger.Event
			from, to    sql.NullString
			actor, blob string
			created     string
			epoch       sql.NullInt64
		)
		if err := rows.Scan(&e.JobID, &e.JobSeq, &e.Kind, &from, &to, &epoch, &actor, &created, &blob); err != nil {
			return nil, err
		}
		e.FromState = job.State(from.String)
		e.ToState = job.State(to.String)
		e.Actor = actor
		if epoch.Valid {
			e.LeaseEpoch = int(epoch.Int64)
		}
		if ts, err := time.Parse(rfc3339, created); err == nil {
			e.CreatedAt = ts
		}
		if err := json.Unmarshal([]byte(blob), &e.Payload); err != nil {
			return nil, fmt.Errorf("decode payload: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── internal helpers ──

func (s *Store) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func appendEvent(ctx context.Context, tx *sql.Tx, e ledger.Event) error {
	blob, err := json.Marshal(e.Payload)
	if err != nil {
		return fmt.Errorf("encode payload: %w", err)
	}
	from := sql.NullString{String: string(e.FromState), Valid: e.FromState != ""}
	to := sql.NullString{String: string(e.ToState), Valid: e.ToState != ""}
	epoch := sql.NullInt64{Int64: int64(e.LeaseEpoch), Valid: e.Kind != ledger.KindJobCreated}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO job_events (job_id, job_seq, kind, from_state, to_state, lease_epoch, actor, payload, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.JobID, e.JobSeq, string(e.Kind), from, to, epoch, e.Actor, string(blob), e.CreatedAt.Format(rfc3339))
	if err != nil {
		return fmt.Errorf("append event %s: %w", e.Kind, err)
	}
	return nil
}

func setJobSeq(ctx context.Context, tx *sql.Tx, jobID string, seq int) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET job_seq = ? WHERE id = ?`, seq, jobID)
	return err
}

// loadJobTx reads a job row inside a tx, returning the job and its current
// job_seq cursor.
func loadJobTx(ctx context.Context, tx *sql.Tx, id string) (job.Job, int, error) {
	j, err := scanJob(tx.QueryRowContext(ctx, jobSelect+` WHERE id = ?`, id))
	if err != nil {
		return job.Job{}, 0, err
	}
	return j, j.JobSeq, nil
}

const jobSelect = `
	SELECT id, kind, flow, stage, state, role,
	       COALESCE(base_sha,''), COALESCE(head_sha,''),
	       priority, blocked_by, required_capabilities, enqueued_at,
	       COALESCE(lease_id,''), lease_epoch,
	       COALESCE(bound_identity,''), COALESCE(bound_model_family,''), COALESCE(bound_lens,''),
	       attempts, max_attempts, bounces, max_bounces, stall_revocations,
	       COALESCE(verdict,''), job_seq,
	       COALESCE(spec_content_hash,''), COALESCE(spec_version,0), COALESCE(spec_signoff,''),
	       COALESCE(issue_number,0), COALESCE(pr_number,0),
	       cost_tokens_in, cost_tokens_out, cost_micro_usd, cost_ceiling_micro_usd,
	       over_budget, COALESCE(flow_id,''), COALESCE(escalation_reason,''),
	       build_epoch, COALESCE(merge_provenance,''),
	       COALESCE(task_text,''), COALESCE(spec_text,''), COALESCE(acceptance_criteria,''),
	       COALESCE(epic_id,''), COALESCE(is_epic,0), COALESCE(epic_reviewed,0),
	       COALESCE(repo,'')
	  FROM jobs`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (job.Job, error) {
	var j job.Job
	var kind, role, blockedJSON, reqJSON, enqueued, verdictJSON, specSignoffJSON string
	var overBudget, isEpic, epicReviewed int
	var ceiling sql.NullInt64
	err := row.Scan(&j.ID, &kind, &j.Flow, &j.Stage, (*string)(&j.State), &role,
		&j.BaseSHA, &j.HeadSHA, &j.Priority, &blockedJSON, &reqJSON, &enqueued,
		&j.LeaseID, &j.LeaseEpoch,
		&j.BoundIdentity, &j.BoundModelFamily, &j.BoundLens,
		&j.Attempts, &j.MaxAttempts, &j.Bounces, &j.MaxBounces, &j.StallRevocations,
		&verdictJSON, &j.JobSeq,
		&j.SpecContentHash, &j.SpecVersion, &specSignoffJSON,
		&j.IssueNum, &j.PRNumber,
		&j.CostTokensIn, &j.CostTokensOut, &j.CostMicroUSD, &ceiling,
		&overBudget, &j.FlowID, &j.EscalationReason,
		&j.BuildEpoch, &j.MergeProvenance,
		&j.TaskText, &j.SpecText, &j.AcceptanceCriteria,
		&j.EpicID, &isEpic, &epicReviewed,
		&j.Repo)
	if err != nil {
		return j, err
	}
	j.IsEpic = isEpic != 0
	j.EpicReviewed = epicReviewed != 0
	if ceiling.Valid {
		c := ceiling.Int64
		j.CostCeilingMicroUSD = &c
	}
	j.OverBudget = overBudget != 0
	j.Kind = job.Kind(kind)
	j.Role = job.Role(role)
	j.BlockedBy = unmarshalStrings(blockedJSON)
	j.RequiredCapabilities = unmarshalStrings(reqJSON)
	if ts, perr := time.Parse(rfc3339, enqueued); perr == nil {
		j.EnqueuedAt = ts
	}
	if verdictJSON != "" && verdictJSON != "null" {
		var v job.Verdict
		if json.Unmarshal([]byte(verdictJSON), &v) == nil && v.Value != "" {
			j.Verdict = &v
		}
	}
	if specSignoffJSON != "" && specSignoffJSON != "null" {
		var s job.SpecSignoff
		if json.Unmarshal([]byte(specSignoffJSON), &s) == nil && s.Value != "" {
			j.SpecSignoff = &s
		}
	}
	return j, nil
}

func marshalStrings(ss []string) string {
	if len(ss) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ss)
	return string(b)
}

func unmarshalStrings(s string) []string {
	if s == "" || s == "[]" {
		return nil
	}
	var out []string
	_ = json.Unmarshal([]byte(s), &out)
	return out
}
