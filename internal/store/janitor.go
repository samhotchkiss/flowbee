package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// mechanicalUnblockReasons are the needs_human escalation reasons the janitor is allowed
// to auto-requeue. A "mechanical" reason is one whose fix is a retry, not a decision:
//
//   - stall: the worker made real progress then went quiet (the two-rung ladder never
//     confirmed a kill, so it aged out to needs_human). A fresh lease usually finishes it.
//
// DELIBERATELY EXCLUDED — these are semantic dead-ends that a blind retry makes worse and
// must stay parked for a human (this list is the safety contract, not a TODO):
//
//	project_out          — a permanent GitHub 4xx (deleted branch/PR, 422/404); a retry
//	                       re-runs straight into the same error, burning a build+review.
//	pr_closed            — a human rejected the PR; requeuing fights that decision.
//	reviewer_rejections  — a genuine standoff with one reviewer; needs a human call.
//	cost                 — over the $ ceiling; a retry just spends more.
//	attempts / bounces   — out of build/review budget; the escalation IS the backstop.
//	design               — needs_design, a deliberate "the machine must not decide this".
var mechanicalUnblockReasons = map[string]bool{
	string(job.EscalationStall): true,
}

// JanitorConfig tunes the self-unblock watchdog. Zero values fall back to the defaults
// below (so a caller can pass JanitorConfig{} for stock behavior).
type JanitorConfig struct {
	// MaxUnblockAttempts caps how many times the janitor will auto-requeue ONE job before
	// it gives up and leaves it parked for a human. This is the per-job convergence
	// guarantee: a job that keeps re-stalling escalates for good after this many tries.
	MaxUnblockAttempts int
	// Cooldown is the minimum wall-clock gap between two auto-unblocks of the SAME job, so
	// a fast re-stall can't burn the whole attempt budget in one minute.
	Cooldown time.Duration
	// CorrelatedThreshold is the Rung-0 correlated-failure breaker: when at least this many
	// parked jobs share ONE mechanical reason, the janitor stands DOWN on that reason for
	// the pass (a shared root cause — a disk-full builder, a bad base, a broken dep — should
	// be fixed once, not fanned into N simultaneous requeues that all re-fail identically).
	CorrelatedThreshold int
	// MaxUnblocksPerPass bounds how many jobs the janitor moves in a single 60s tick, so
	// even absent a correlated signature it can never mass-requeue the backlog at once.
	MaxUnblocksPerPass int
}

func (c JanitorConfig) withDefaults() JanitorConfig {
	if c.MaxUnblockAttempts <= 0 {
		c.MaxUnblockAttempts = 2
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 10 * time.Minute
	}
	if c.CorrelatedThreshold <= 0 {
		c.CorrelatedThreshold = 5
	}
	if c.MaxUnblocksPerPass <= 0 {
		c.MaxUnblocksPerPass = 3
	}
	return c
}

// JanitorReport summarizes one self-unblock pass.
type JanitorReport struct {
	Unblocked int      // jobs auto-requeued out of needs_human this pass
	StoodDown []string // reasons skipped because the correlated-failure breaker tripped
}

// unblockCandidate is one parked-but-mechanically-recoverable job.
type unblockCandidate struct {
	id           string
	kind         job.Kind
	reason       string
	headSHA      string
	baseSHA      string
	unblockTries int
	lastProgress string
	nextAt       string
}

// JanitorUnblock is the automatic exit from the needs_human sink (0023). It is the
// forward-progress watchdog's sibling: where ReconcileStuck ESCALATES a wedged job TO
// needs_human, the janitor moves the MECHANICALLY-recoverable ones back OUT — bounded,
// cooled-down, breaker-gated, and signal-preserving — so a transient stall no longer needs
// an operator (or an always-watching model) to run `flowbee requeue` at 2am.
//
// The ladder, cheapest-and-safest first:
//
//	Rung 0 — correlated-failure breaker: if many parked jobs share one reason, a shared
//	         root cause is likelier than N independent transient stalls; stand down on that
//	         reason (surface it as a page instead of fanning N doomed requeues).
//	Rung C — per-job cap + cooldown + SHA-progress: a job that has already been auto-unblocked
//	         MaxUnblockAttempts times, or whose SHA has not moved since the last unblock (the
//	         retry accomplished nothing), stays parked. This is the anti-thrash backbone —
//	         every rung burns bounded budget, so a spinning job converges back to needs_human.
//	Rung B — the actual re-arm: append janitor_unblocked, routing the job to its entry stage
//	         (ready / spec_authoring) WITHOUT resetting attempts/bounces.
//
// A down fleet is left alone (nothing to unblock ONTO); ReconcileStuck / fleet-health own
// that. Only reasons in mechanicalUnblockReasons are ever touched.
func (s *Store) JanitorUnblock(ctx context.Context, now time.Time, staleHB time.Duration, cfg JanitorConfig) (JanitorReport, error) {
	cfg = cfg.withDefaults()
	var rep JanitorReport

	// liveness gate: unblocking to `ready` while no worker can claim it just relocates the
	// job. Require a live fleet, exactly like ReconcileStuck's escalation gate.
	live := 0
	if roster, err := s.Roster(ctx, now, staleHB); err == nil {
		for _, w := range roster {
			if !w.StaleHB {
				live++
			}
		}
	}
	if live == 0 {
		return rep, nil
	}

	cands, err := s.mechanicalUnblockCandidates(ctx)
	if err != nil {
		return rep, err
	}

	// Rung 0: correlated-failure breaker. Count parked jobs per mechanical reason; a reason
	// at/over the threshold is a probable shared root cause — stand down on it for this pass.
	perReason := map[string]int{}
	for _, c := range cands {
		perReason[c.reason]++
	}
	stoodDown := map[string]bool{}
	for reason, n := range perReason {
		if n >= cfg.CorrelatedThreshold {
			stoodDown[reason] = true
		}
	}
	if len(stoodDown) > 0 {
		reasons := make([]string, 0, len(stoodDown))
		for r := range stoodDown {
			reasons = append(reasons, r)
		}
		sort.Strings(reasons)
		rep.StoodDown = reasons
	}

	for _, c := range cands {
		if rep.Unblocked >= cfg.MaxUnblocksPerPass {
			break
		}
		if stoodDown[c.reason] {
			continue
		}
		// Rung C: per-job cap.
		if c.unblockTries >= cfg.MaxUnblockAttempts {
			continue
		}
		// Rung C: cooldown gate.
		if c.nextAt != "" {
			if t, perr := time.Parse(rfc3339, c.nextAt); perr == nil && now.Before(t) {
				continue
			}
		}
		snapshot := c.headSHA + "|" + c.baseSHA
		// Rung C: SHA-progress. If we have already auto-unblocked this job (tries>0) and its
		// head||base is IDENTICAL to the snapshot taken at that unblock, the retry moved
		// nothing — a churn plateau. Leave it parked rather than spend another attempt.
		if c.unblockTries > 0 && snapshot == c.lastProgress {
			continue
		}
		did, err := s.unblockOne(ctx, c.id, snapshot, "", now, cfg.Cooldown)
		if err != nil {
			return rep, err
		}
		if did {
			rep.Unblocked++
		}
	}
	return rep, nil
}

// advisableReasons are the needs_human reasons the Rung-E advisor may be consulted on.
// The goal is a system that drives EVERY issue to completion itself, so the advisor is the
// first responder for the "repeated failure" reasons — it reads the actual review findings /
// CI failures and re-arms with a concrete correction + a fresh budget, which is what makes a
// guided retry succeed where a blind one just re-fails:
//
//	stall               — worker went quiet; mechanical janitor tries first, advisor after.
//	bounces             — build bounced off review max times; advisor reads the findings.
//	attempts            — build failed its attempts; advisor reads the CI failures.
//	reviewer_rejections — one reviewer keeps rejecting; advisor judges + re-routes.
//
// Still EXCLUDED — the ones a retry genuinely can't fix without external action:
//
//	project_out — a permanent GitHub 4xx (deleted branch/PR); needs the GitHub state fixed.
//	pr_closed   — a human closed the PR; re-opening fights that.
//	cost        — over the $ ceiling; a retry just spends more (raise the ceiling instead).
//	design      — needs_design, a deliberate product decision (surfaced separately).
//
// project_out/pr_closed/cost are handled by their own reconcilers, not blind retries.
var advisableReasons = map[string]bool{
	string(job.EscalationStall):              true,
	string(job.EscalationBounces):            true,
	string(job.EscalationAttempts):           true,
	string(job.EscalationReviewerRejections): true,
}

// AdvisorCandidate is a stuck job the advisor may weigh in on, with its full re-entry
// context and the SHA-anchored trigger hash used for dedup.
type AdvisorCandidate struct {
	JobID           string
	Kind            string
	Reason          string
	HeadSHA         string
	BaseSHA         string
	TaskText        string
	SpecText        string
	Acceptance      string
	LastReviewNotes string
	LastCIFailures  string
	Attempts        int
	MaxAttempts     int
	Bounces         int
	MaxBounces      int
	UnblockAttempts int
	AdvisorAttempts int
	TriggerHash     string
}

// AdvisorCandidates returns the jobs eligible for a Rung-E advisor consult: parked in
// needs_human for an advisable reason, under the advisor's per-job cap, and NOT already
// consulted at this exact stuck signature (trigger_hash != advisor_last_hash — a SHA that
// moved is genuinely new work worth another look). For `stall` ONLY, the cheap mechanical
// janitor is given first crack: it is skipped until unblock_attempts >= minUnblock. The
// repeated-failure reasons (bounces/attempts/reviewer_rejections) have no mechanical stage —
// a blind requeue just re-fails — so the advisor is their FIRST responder. Deterministic id
// order.
func (s *Store) AdvisorCandidates(ctx context.Context, minUnblock, advisorCap int) ([]AdvisorCandidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, COALESCE(escalation_reason,''),
		       COALESCE(head_sha,''), COALESCE(base_sha,''),
		       COALESCE(task_text,''), COALESCE(spec_text,''), COALESCE(acceptance_criteria,''),
		       COALESCE(last_review_notes,''), COALESCE(last_ci_failures,''),
		       attempts, max_attempts, bounces, max_bounces,
		       COALESCE(unblock_attempts,0), COALESCE(advisor_attempts,0),
		       COALESCE(advisor_last_hash,'')
		  FROM jobs
		 WHERE state='needs_human'
		   AND COALESCE(advisor_attempts,0) < ?
		 ORDER BY id`, advisorCap)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdvisorCandidate
	for rows.Next() {
		var c AdvisorCandidate
		var lastHash string
		if err := rows.Scan(&c.JobID, &c.Kind, &c.Reason, &c.HeadSHA, &c.BaseSHA,
			&c.TaskText, &c.SpecText, &c.Acceptance, &c.LastReviewNotes, &c.LastCIFailures,
			&c.Attempts, &c.MaxAttempts, &c.Bounces, &c.MaxBounces,
			&c.UnblockAttempts, &c.AdvisorAttempts, &lastHash); err != nil {
			return nil, err
		}
		if !advisableReasons[c.Reason] {
			continue
		}
		// `stall` defers to the mechanical janitor until it has exhausted its cheap requeues.
		if mechanicalUnblockReasons[c.Reason] && c.UnblockAttempts < minUnblock {
			continue
		}
		c.TriggerHash = c.JobID + ":" + c.Reason + ":" + c.HeadSHA
		if c.TriggerHash == lastHash {
			continue // already consulted at this exact stuck signature — don't re-run the model
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ApplyAdvisorVerdict applies a Rung-E advisor decision to a parked job, event-sourced and
// fail-safe. A re-arm action (PLAN / CORRECTION / REPROMPT) re-arms the job ONCE with the
// advisor's note carried into the next lease context (StuckHint); STOP (or an unknown/empty
// action — the fail-safe) leaves the job parked for a human. Either way it records the
// advisor bookkeeping (advisor_attempts++, advisor_last_hash) so the model is never
// re-consulted at the same signature and converges to a permanent park. Returns whether it
// re-armed the job. No-ops (without recording) if the job is no longer an advisable park —
// a concurrent operator/reconcile won the race.
func (s *Store) ApplyAdvisorVerdict(ctx context.Context, jobID, action, note, hash string, now time.Time, cooldown time.Duration) (bool, error) {
	rearmed := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if cur.State != job.StateNeedsHuman || !advisableReasons[cur.EscalationReason] {
			return nil // race: no longer an advisable park
		}
		rearm := action == "PLAN" || action == "CORRECTION" || action == "REPROMPT"
		if rearm {
			snapshot := cur.HeadSHA + "|" + cur.BaseSHA
			if note == "" {
				note = "advisor: retry with fresh context" // never empty — the empty hint is the mechanical path's signal
			}
			// advisor-guided retry earns a fresh attempts+bounces budget (new guidance), bounded
			// by the per-job advisor cap.
			if err := rearmFromNeedsHumanTx(ctx, tx, cur, seq, snapshot, note, true, now, cooldown); err != nil {
				return err
			}
			rearmed = true
		}
		// record advisor bookkeeping regardless (projection-only; not folded — a DR rebuild
		// re-consulting once more is bounded + safe). On a re-arm this UPDATE lands on the
		// already-re-armed row; on STOP it is the only change (state stays needs_human).
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs SET advisor_attempts = advisor_attempts + 1, advisor_last_hash = ?,
			                updated_at = datetime('now')
			 WHERE id = ?`, hash, jobID); err != nil {
			return fmt.Errorf("advisor bookkeeping: %w", err)
		}
		return nil
	})
	return rearmed, err
}

// mechanicalUnblockCandidates loads the needs_human jobs whose escalation reason is
// mechanically recoverable, in deterministic id order.
func (s *Store) mechanicalUnblockCandidates(ctx context.Context) ([]unblockCandidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, kind, COALESCE(escalation_reason,''),
		       COALESCE(head_sha,''), COALESCE(base_sha,''),
		       COALESCE(unblock_attempts,0), COALESCE(last_progress_sha,''),
		       COALESCE(unblock_next_at,'')
		  FROM jobs
		 WHERE state='needs_human'
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []unblockCandidate
	for rows.Next() {
		var c unblockCandidate
		var kind string
		if err := rows.Scan(&c.id, &kind, &c.reason, &c.headSHA, &c.baseSHA,
			&c.unblockTries, &c.lastProgress, &c.nextAt); err != nil {
			return nil, err
		}
		if !mechanicalUnblockReasons[c.reason] {
			continue
		}
		c.kind = job.Kind(kind)
		out = append(out, c)
	}
	return out, rows.Err()
}

// unblockOne re-arms a single needs_human job, event-sourced. Re-reads the job inside the
// tx and no-ops if it is no longer parked (a concurrent operator requeue / reconcile won
// the race), so the janitor can never double-move a job. Returns whether it acted.
func (s *Store) unblockOne(ctx context.Context, jobID, snapshot, hint string, now time.Time, cooldown time.Duration) (bool, error) {
	acted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		cur, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if cur.State != job.StateNeedsHuman {
			return nil // race: someone else moved it — leave it be
		}
		// the mechanical path only touches mechanical reasons; the advisor path (hint != "")
		// has already gated eligibility, so it may re-arm any parked reason it was consulted on.
		if hint == "" && !mechanicalUnblockReasons[cur.EscalationReason] {
			return nil
		}
		// mechanical unblock preserves the budget (bounded, no new information).
		if err := rearmFromNeedsHumanTx(ctx, tx, cur, seq, snapshot, hint, false, now, cooldown); err != nil {
			return err
		}
		acted = true
		return nil
	})
	return acted, err
}

// rearmFromNeedsHumanTx is the shared re-arm both the mechanical janitor and the Rung-E
// advisor use to move a parked job back into its flow, event-sourced. It appends the
// janitor_unblocked event and mirrors the Fold (KindJanitorUnblocked) EXACTLY so
// projection == Fold(events). Factoring it out means the two callers can never drift from
// the fold (a divergence there silently corrupts a DR rebuild). hint is the advisor's note
// ("" for a plain mechanical unblock); it is carried on the event AND the projection.
func rearmFromNeedsHumanTx(ctx context.Context, tx *sql.Tx, cur job.Job, seq int, snapshot, hint string, resetBudget bool, now time.Time, cooldown time.Duration) error {
	// Re-arm to the job's OWN entry stage (spec restarts authoring; build restarts ready),
	// mirroring RequeueJob's routing. role/caps/escalation-clear are state-derived by Fold.
	target, role, stage, cap := job.StateReady, string(job.RoleEngWorker), "build", "role:eng_worker"
	if cur.Kind == job.KindSpec {
		target, role, stage, cap = job.StateSpecAuthoring, string(job.RoleSpecAuthor), "spec", "role:spec_author"
	}
	actor := "janitor"
	if hint != "" {
		actor = "advisor"
	}
	ev := ledger.Event{
		JobID: cur.ID, JobSeq: seq + 1, Kind: ledger.KindJanitorUnblocked,
		FromState: cur.State, ToState: target, LeaseEpoch: cur.LeaseEpoch + 1,
		Actor: actor, CreatedAt: now,
		// ResetCounters marks an advisor-guided retry: the advisor supplied NEW guidance
		// (a correction / plan), so the guided attempt earns a fresh attempts+bounces budget
		// rather than re-arming into an immediately-re-exhausted state. The whole loop stays
		// bounded by the per-job advisor cap, so this cannot thrash forever.
		Payload: ledger.Payload{UnblockSHA: snapshot, StuckHint: hint, ResetCounters: resetBudget},
	}
	if err := appendEvent(ctx, tx, ev); err != nil {
		return err
	}
	// Mirror the Fold (KindJanitorUnblocked) EXACTLY:
	//   - clear the prior attempt's artifacts (fresh candidate), like requeue
	//   - clear escalation_reason + over_budget (no longer a human-gate state)
	//   - bump the epoch (fence any lingering worker), clear the lease columns
	//   - bump unblock_attempts, stamp last_progress_sha + the cooldown gate + the hint
	//   - reset attempts/bounces/stall_revocations IFF resetBudget (advisor-guided retry);
	//     the plain mechanical unblock PRESERVES them (bounded, not a budget reset).
	enq := ""
	if target == job.StateReady {
		enq = now.Format(rfc3339)
	}
	reset := 0
	if resetBudget {
		reset = 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE jobs
		   SET state = ?, role = ?, stage = ?, required_capabilities = ?,
		       head_sha = '', verdict = NULL,
		       patch_diff = '', declared_blast_radius = '',
		       reservation_paths = '', reservation_wide = 0,
		       over_budget = 0, escalation_reason = '',
		       attempts = CASE WHEN ?=1 THEN 0 ELSE attempts END,
		       bounces = CASE WHEN ?=1 THEN 0 ELSE bounces END,
		       stall_revocations = CASE WHEN ?=1 THEN 0 ELSE stall_revocations END,
		       lease_epoch = lease_epoch + 1,
		       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
		       lease_hb_due = NULL, lease_deadline = NULL,
		       unblock_attempts = unblock_attempts + 1,
		       last_progress_sha = ?,
		       stuck_hint = ?,
		       unblock_next_at = ?,
		       enqueued_at = CASE WHEN ? <> '' THEN ? ELSE enqueued_at END,
		       updated_at = datetime('now')
		 WHERE id = ?`,
		string(target), role, stage, marshalStrings([]string{cap}),
		reset, reset, reset,
		snapshot, hint, now.Add(cooldown).Format(rfc3339),
		enq, enq, cur.ID); err != nil {
		return fmt.Errorf("janitor unblock: %w", err)
	}
	// close any stale lease audit row (defensive; needs_human holds no active lease).
	if _, err := tx.ExecContext(ctx, `
		UPDATE leases SET ended_at = datetime('now'), end_reason = 'janitor_unblock'
		 WHERE job_id = ? AND ended_at IS NULL`, cur.ID); err != nil {
		return err
	}
	return setJobSeq(ctx, tx, cur.ID, seq+1)
}
