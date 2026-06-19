package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/content"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

// jobPatchDiff reads a job's stored (untrusted) patch diff. Empty if none stored.
func (s *Store) jobPatchDiff(ctx context.Context, jobID string) (string, error) {
	var diff string
	err := s.DB.QueryRowContext(ctx, `SELECT patch_diff FROM jobs WHERE id = ?`, jobID).Scan(&diff)
	if err != nil {
		return "", err
	}
	return diff, nil
}

// JobPatchDiff is the exported reader for a job's stored build patch, so the lease
// path can ship it to a code_reviewer's agent (it judges the actual change).
func (s *Store) JobPatchDiff(ctx context.Context, jobID string) (string, error) {
	return s.jobPatchDiff(ctx, jobID)
}

// F8 — merge conflicts: blast-radius reservations, the resolve_conflict job, and
// integrated-head re-review (DESIGN §E). The store is the runtime side of three pure
// concerns: (1) fold the in-flight builds' declared write-sets into the scheduler's
// reservation set so two overlapping builds never co-dispatch; (2) on a base move,
// try a trivial auto-rebase and, on a real conflict, spawn a conflict_resolver job;
// (3) re-validate the verdict at the INTEGRATED head (not just CI) and re-arm a
// stacked descendant when its parent merges.

// ── (1) blast-radius reservations ───────────────────────────────────────────────

// declaredWriteSet folds a job's stored declared_blast_radius into a scheduler
// WriteSet. A `scope: wide` declaration is a wide reservation that single-flights the
// tree; a concrete path list reserves exactly those paths. PURE-ish (reads one
// column's bytes). NOTE: an EMPTY declaration here yields an empty (non-wide)
// WriteSet — callers decide whether absence means "no reservation" (the in-flight
// case: a build that declared nothing holds no reservation) or "conservatively wide"
// (the candidate case: an undeclared candidate cannot be proven disjoint).
func declaredWriteSet(declared string) scheduler.WriteSet {
	var br content.BlastRadius
	if declared != "" {
		_ = json.Unmarshal([]byte(declared), &br)
	}
	wide := strings.EqualFold(strings.TrimSpace(br.Scope), "wide")
	return scheduler.WriteSet{Paths: br.Paths, Wide: wide}
}

// hasDeclaration reports whether a job declared a meaningful blast-radius (concrete
// paths or an explicit wide scope). A build that declared NOTHING holds no
// reservation — most builds in normal operation declare nothing, and treating every
// undeclared in-flight build as a whole-tree single-flight would serialize the whole
// fleet. Reservations bite only when a write-set is actually declared.
func hasDeclaration(declared string) bool {
	if declared == "" {
		return false
	}
	var br content.BlastRadius
	if err := json.Unmarshal([]byte(declared), &br); err != nil {
		return false
	}
	return len(br.Paths) > 0 || strings.EqualFold(strings.TrimSpace(br.Scope), "wide")
}

// ActiveReservations returns the write-set reservation held by every IN-FLIGHT build
// (a job holding an active lease in a build/resolve state). The scheduler honors
// these so it never co-dispatches a candidate whose write-set overlaps one in flight
// (the §E "avoid first" rule). A build that declared a wide blast-radius single-
// flights the whole tree. Folded from the persisted declared_blast_radius.
func (s *Store) ActiveReservations(ctx context.Context) ([]scheduler.Reservation, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, declared_blast_radius, reservation_paths, reservation_wide
		  FROM jobs
		 WHERE state IN ('leased','building','code_review','review_pending',
		                 'resolving_conflict','mergeable','merging','merge_handoff')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Reservation
	for rows.Next() {
		var id, declared, resvPaths string
		var resvWide int
		if err := rows.Scan(&id, &declared, &resvPaths, &resvWide); err != nil {
			return nil, err
		}
		// A build that declared nothing (and recorded no reservation) holds NO
		// reservation: undeclared in-flight builds do not serialize the fleet.
		// Reservations bite only when a write-set was actually declared/recorded.
		if resvWide == 1 || resvPaths != "" {
			out = append(out, scheduler.Reservation{
				JobID:    id,
				WriteSet: scheduler.WriteSet{Paths: unmarshalStrings(resvPaths), Wide: resvWide == 1},
			})
			continue
		}
		if !hasDeclaration(declared) {
			continue
		}
		out = append(out, scheduler.Reservation{JobID: id, WriteSet: declaredWriteSet(declared)})
	}
	return out, rows.Err()
}

// ReadyCandidatesReserved is ReadyCandidates filtered by the active blast-radius
// reservations (F8 §E): a ready candidate whose declared write-set OVERLAPS an
// in-flight build's write-set is withheld (it would rebase onto that build and likely
// conflict). The atomic claim remains the correctness backstop; this is candidate
// selection only. With no in-flight builds it equals ReadyCandidates.
func (s *Store) ReadyCandidatesReserved(ctx context.Context) ([]scheduler.Candidate, error) {
	cands, err := s.ReadyCandidates(ctx)
	if err != nil {
		return nil, err
	}
	active, err := s.ActiveReservations(ctx)
	if err != nil {
		return nil, err
	}
	if len(active) == 0 {
		return cands, nil
	}
	// fold each ready candidate's declared write-set so the pure filter can compare.
	writeSets, err := s.readyWriteSets(ctx)
	if err != nil {
		return nil, err
	}
	return scheduler.ReservationFilter(cands, active, writeSets), nil
}

// readyWriteSets returns the declared write-set of every ready job (keyed by id).
func (s *Store) readyWriteSets(ctx context.Context) (map[string]scheduler.WriteSet, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, declared_blast_radius FROM jobs WHERE state='ready'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]scheduler.WriteSet{}
	for rows.Next() {
		var id, declared string
		if err := rows.Scan(&id, &declared); err != nil {
			return nil, err
		}
		out[id] = declaredWriteSet(declared)
	}
	return out, rows.Err()
}

// RecordReservation stamps a job's held write-set onto its row (called when a build
// is dispatched, so ActiveReservations reflects it even before the worker returns a
// diff). Folds from the job's declared blast-radius. Best-effort; never required for
// correctness (ActiveReservations also folds declared_blast_radius directly).
func (s *Store) RecordReservation(ctx context.Context, jobID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var declared string
		if err := tx.QueryRowContext(ctx,
			`SELECT declared_blast_radius FROM jobs WHERE id = ?`, jobID).Scan(&declared); err != nil {
			return err
		}
		ws := declaredWriteSet(declared)
		_, err := tx.ExecContext(ctx,
			`UPDATE jobs SET reservation_paths = ?, reservation_wide = ? WHERE id = ?`,
			marshalStrings(ws.Paths), b2i(ws.Wide), jobID)
		return err
	})
}

// ── (2) base-moved: trivial auto-rebase OR a resolve_conflict job ────────────────

// RebaseResult reports the outcome of RebaseOnto.
type RebaseResult struct {
	Clean          bool   // a trivial rebase succeeded (re-validated at NewSHA; no agent)
	ConflictJob    string // the spawned resolve_conflict job id (real conflict); "" if clean
	NewSHA         string // the rebased head on a clean rebase
	NewBaseSHA     string // the base it rebased onto
	ResolverNeeded bool   // a conflict_resolver lease is now armed (== ConflictJob != "")
}

// RebaseOnto handles a base move on a build whose PR's base advanced (F8 §E
// trivial/real split). It re-applies the build's stored patch onto newBaseSHA via the
// mirror: a CLEAN rebase advances the base + supersedes the SHA-bound verdict so
// review + CI re-arm at the new INTEGRATED head (no agent); a REAL conflict spawns a
// resolve_conflict job (a conflict_resolver lease) whose resolved diff is re-reviewed
// + re-CI'd. The mirror is optional in tests: when nil, the apply is decided from the
// caller-supplied Conflict flag (RebaseOntoParams), keeping the path testable without
// a git fixture. Returns the outcome.
type RebaseOntoParams struct {
	JobID      string
	NewBaseSHA string
	Now        time.Time
	// ForceConflict, when set, declares the rebase a REAL conflict WITHOUT a mirror
	// (tests + a caller that already proved the conflict). When a Mirror is provided
	// it is ignored — the real `git apply` decides.
	ForceConflict bool
}

// RebaseOnto applies the trivial/real conflict split. mirror may be nil (then the
// decision comes from p.ForceConflict). It is serialized in one transaction.
func (s *Store) RebaseOnto(ctx context.Context, mirror *gitops.Mirror, p RebaseOntoParams) (RebaseResult, error) {
	var res RebaseResult
	res.NewBaseSHA = p.NewBaseSHA

	// determine clean vs conflict OUTSIDE the tx (filesystem I/O), if a mirror is set.
	clean := !p.ForceConflict
	var newSHA string
	if mirror != nil {
		j, err := s.GetJob(ctx, p.JobID)
		if err != nil {
			return res, err
		}
		diff, err := s.jobPatchDiff(ctx, p.JobID)
		if err != nil {
			return res, err
		}
		if strings.TrimSpace(diff) == "" {
			// no stored patch to replay: treat as a clean fast-forward (nothing to conflict).
			clean = true
		} else {
			out, rerr := mirror.TryRebasePatch(p.JobID, j.LeaseEpoch, p.NewBaseSHA, diff,
				"flowbee auto-rebase onto "+shortSHA(p.NewBaseSHA))
			if rerr != nil {
				return res, fmt.Errorf("try rebase: %w", rerr)
			}
			clean = out.Clean
			newSHA = out.NewSHA
		}
	}

	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		nextSeq := seq + 1
		if clean {
			// TRIVIAL: advance the base, supersede the SHA-bound verdict, re-arm review +
			// CI at the new integrated head. No agent. This is exactly the I-5 supersede
			// edge, fired by an integrated-head move rather than a reconcile observation.
			ev := ledger.Event{
				JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindRebased,
				FromState: j.State, ToState: job.StateReviewPending, LeaseEpoch: j.LeaseEpoch + 1,
				Actor: "system", CreatedAt: p.Now,
				Payload: ledger.Payload{BaseSHA: p.NewBaseSHA},
			}
			if err := appendEvent(ctx, tx, ev); err != nil {
				return err
			}
			// route to review_pending (re-review + re-CI at the new head): the build
			// product stands but its verdict no longer binds, so it must be re-judged at
			// the integrated SHA. Bump the epoch (fence any worker), clear the verdict.
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'review_pending', role = 'eng_worker', stage = 'build',
				       required_capabilities = ?,
				       base_sha = ?, head_sha = ?,
				       verdict = NULL,
				       lease_epoch = lease_epoch + 1,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, lease_deadline = NULL,
				       updated_at = datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:code_reviewer"}), p.NewBaseSHA, newSHA, p.JobID); err != nil {
				return fmt.Errorf("apply clean rebase: %w", err)
			}
			// NOTE: do NOT write domain_b_facts here. rebaseStaleReviews force-pushes newSHA
			// to GitHub AFTER this tx commits, so recording it now would put the baseline
			// AHEAD of GitHub and a reconcile sweep in that window would read a (reverse)
			// move and supersede. The job's head_sha/base_sha above are the atomic record of
			// where Flowbee placed the branch; reconcile's `flowbeePlaced` guard reads THOSE
			// to recognise the rebase as Flowbee's own, not an external move (race-free).
			res.Clean = true
			res.NewSHA = newSHA
			return setJobSeq(ctx, tx, p.JobID, nextSeq)
		}

		// REAL conflict: route the build to resolving_conflict (a conflict_resolver
		// lease). The resolver rebases + resolves in a worktree and returns the resolved
		// diff, which is re-reviewed + re-CI'd like any build. Bump the epoch (fence any
		// running worker), record the conflicting base.
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindConflictDetected,
			FromState: j.State, ToState: job.StateResolvingConflict, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{BaseSHA: p.NewBaseSHA},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'resolving_conflict', role = 'conflict_resolver', stage = 'resolve_conflict',
			       required_capabilities = ?,
			       conflict_base_sha = ?, base_sha = ?, verdict = NULL,
			       lease_epoch = lease_epoch + 1,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL,
			       enqueued_at = ?, updated_at = datetime('now')
			 WHERE id = ?`,
			marshalStrings([]string{"role:conflict_resolver"}), p.NewBaseSHA, p.NewBaseSHA,
			p.Now.Format(rfc3339), p.JobID); err != nil {
			return fmt.Errorf("apply conflict detected: %w", err)
		}
		res.ConflictJob = p.JobID
		res.ResolverNeeded = true
		return setJobSeq(ctx, tx, p.JobID, nextSeq)
	})
	if err != nil {
		return RebaseResult{}, err
	}
	return res, nil
}

// RouteMergeConflict diverts a job whose MERGE failed with a conflict into the
// conflict_resolver path. A sibling can merge into the same area AFTER this change's
// verdict was minted (both were reviewed before either merged), so the rebase-before-
// review split never saw it and the merge itself hits GitHub's 405 "not mergeable".
// Without this the project-out sender retries that merge forever. Like the rebase split
// it emits KindConflictDetected and re-arms to resolving_conflict at newBaseSHA (current
// main), invalidating the now-stale verdict; the resolver re-applies the change on
// current main and the resolution is re-reviewed + re-merged. Idempotent: a job no
// longer in merging/mergeable (already merged or re-armed) is left untouched.
func (s *Store) RouteMergeConflict(ctx context.Context, jobID, newBaseSHA string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateMerging && j.State != job.StateMergeable {
			return nil
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindConflictDetected,
			FromState: j.State, ToState: job.StateResolvingConflict, LeaseEpoch: j.LeaseEpoch + 1,
			Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{BaseSHA: newBaseSHA},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'resolving_conflict', role = 'conflict_resolver', stage = 'resolve_conflict',
			       required_capabilities = ?,
			       conflict_base_sha = ?, base_sha = ?, verdict = NULL,
			       lease_epoch = lease_epoch + 1,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, lease_deadline = NULL,
			       enqueued_at = ?, updated_at = datetime('now')
			 WHERE id = ?`,
			marshalStrings([]string{"role:conflict_resolver"}), newBaseSHA, newBaseSHA,
			now.Format(rfc3339), jobID); err != nil {
			return fmt.Errorf("route merge conflict: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// RouteSelfMergeToHandoff downgrades an autonomous self-merge to the HUMAN merge gate
// (merge_handoff) when project-out's CP-authoritative re-check finds the ACTUAL branch
// diff (base..head on the mirror) hits the content denylist. The verdict-time gate ran
// over the worker's SELF-REPORTED patch; this is the defense-in-depth catch for a worker
// that under-reported what it changed — so a denylisted change can never autonomously
// merge to main on a worker's say-so. The job settles in merge_handoff for a human;
// `reason` records the hit (the denylist classes) for the operator queue.
func (s *Store) RouteSelfMergeToHandoff(ctx context.Context, jobID, reason string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateMerging && j.State != job.StateMergeable {
			return nil // already past the merge arm; nothing to downgrade
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindStateChanged,
			FromState: j.State, ToState: job.StateMergeHandoff, LeaseEpoch: j.LeaseEpoch,
			Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{RevokeReason: reason},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state = 'merge_handoff', updated_at = datetime('now') WHERE id = ?`,
			jobID); err != nil {
			return fmt.Errorf("route self-merge to handoff: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// StaleReviewBuilds returns the ids of review_pending build jobs whose base_sha is
// behind the current integration tip (mainTip) and that carry a stored patch to
// replay. These are the jobs to rebase-before-review (build-list: "rebase before the
// reviewer starts so it doesn't review something that won't merge"): the control
// plane replays each onto mainTip via RebaseOnto — a clean rebase re-arms review +
// CI at the integrated head; a real conflict diverts to a conflict_resolver BEFORE
// any review effort is spent. A job already on mainTip is skipped (no-op).
func (s *Store) StaleReviewBuilds(ctx context.Context, repoID, mainTip string) ([]string, error) {
	if strings.TrimSpace(mainTip) == "" {
		return nil, nil
	}
	// F9: scope to ONE repo so a job is only ever compared to (and rebased onto) its
	// OWN repo's integration tip — never another repo's. repoID "" matches all (the
	// legacy single-repo path).
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id FROM jobs
		 WHERE state = 'review_pending' AND kind = 'build'
		   AND (? = '' OR repo = ?)
		   AND base_sha != '' AND base_sha != ?
		   AND patch_diff IS NOT NULL AND patch_diff != ''
		 ORDER BY updated_at ASC`, repoID, repoID, mainTip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── (3) resolve_conflict result: re-review + re-CI ──────────────────────────────

// ResolveConflictParams is a conflict_resolver's returned resolution (the resolved
// diff). It is fenced like any worker result.
type ResolveConflictParams struct {
	JobID               string
	Epoch               int
	ResolvedDiff        string // the resolver's resolved patch (untrusted, re-gated)
	DeclaredBlastRadius string
	PushedRef           string
	// PushedSHA is the resolved commit the resolver force-pushed to the issue branch.
	// It MUST be recorded as the reconcile head baseline, or the next reconcile sweep
	// reads the resolver's (legitimate, Flowbee-authored) head advance as an unexpected
	// SHA move and SUPERSEDES the review back to build — which rebuilds, re-pushes, and
	// supersedes again: an organic-conflict resolve→supersede→rebuild loop (found live
	// by two issues editing the same file). Recording it makes prevHead == the resolved
	// head on the next sweep, so the resolution settles into review like any build.
	PushedSHA string
	Now       time.Time
}

// ResolveConflictResult accepts a conflict_resolver's resolution and routes the job
// back through build-review (resolving_conflict -> review_pending), re-arming the
// code_review gate + re-CI at the new head. The resolved diff is UNTRUSTED code —
// stored as the job's patch so the content gate + reviewer re-judge it. Fenced: a
// stale epoch -> ErrStaleEpoch. This realizes "resolution is just another job."
func (s *Store) ResolveConflictResult(ctx context.Context, p ResolveConflictParams) (ReviewResultResponse, error) {
	var resp ReviewResultResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		if p.Epoch != j.LeaseEpoch {
			return lease.ErrStaleEpoch
		}
		if j.State != job.StateResolvingConflict {
			return fmt.Errorf("resolve_conflict result on non-resolving job (state=%s)", j.State)
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindConflictResolved,
			FromState: j.State, ToState: job.StateReviewPending, LeaseEpoch: j.LeaseEpoch,
			Actor: j.BoundIdentity, CreatedAt: p.Now,
			Payload: ledger.Payload{BaseSHA: j.BaseSHA},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		// route to review_pending: the resolved diff is re-reviewed + re-CI'd. Store the
		// resolved patch + declared blast-radius (the content gate re-runs over it);
		// persist the BUILDER identity for anti-affinity; flip caps to code_reviewer.
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'review_pending', role = 'eng_worker', stage = 'build',
			       required_capabilities = ?,
			       builder_identity     = COALESCE(builder_identity, bound_identity),
			       builder_model_family = COALESCE(builder_model_family, bound_model_family),
			       head_ref = COALESCE(NULLIF(?, ''), head_ref),
			       head_sha = COALESCE(NULLIF(?, ''), head_sha),
			       patch_diff = ?, declared_blast_radius = ?,
			       verdict = NULL,
			       eng_worker_job = COALESCE(eng_worker_job, id),
			       lease_id = NULL, bound_identity = NULL,
			       bound_model_family = NULL, lease_hb_due = NULL,
			       updated_at = datetime('now')
			 WHERE id = ?`,
			marshalStrings([]string{"role:code_reviewer"}), p.PushedRef, p.PushedSHA,
			p.ResolvedDiff, p.DeclaredBlastRadius, p.JobID); err != nil {
			return fmt.Errorf("apply resolve_conflict result: %w", err)
		}
		// j.head_sha (set above to the resolved commit) + j.base_sha (already the rebased
		// base from RouteMergeConflict) record where Flowbee placed the branch. The
		// resolver force-pushed BEFORE submitting, so GitHub already has this head — and
		// reconcile's `flowbeePlaced` guard reads these to recognise the resolution as
		// Flowbee's own head/base advance, not an external move (uniform with the rebase
		// path, and race-free: the JOB row is the atomic record, not domain_b_facts).
		// close the resolver lease audit row.
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'completed'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			p.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close resolver lease: %w", err)
		}
		if err := setJobSeq(ctx, tx, p.JobID, nextSeq); err != nil {
			return err
		}
		resp = ReviewResultResponse{Accepted: true, JobState: string(job.StateReviewPending)}
		return nil
	})
	if err != nil {
		return ReviewResultResponse{}, err
	}
	return resp, nil
}

// ResolvingConflictCandidates returns every UNCLAIMED resolving_conflict job as a
// scheduler Candidate, for a conflict_resolver's long-poll loop to rank and claim.
func (s *Store) ResolvingConflictCandidates(ctx context.Context) ([]scheduler.Candidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, priority, enqueued_at, required_capabilities
		  FROM jobs WHERE state='resolving_conflict' AND lease_id IS NULL`)
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

// ClaimConflictParams describes a conflict_resolver claiming a resolving_conflict job.
type ClaimConflictParams struct {
	JobID       string
	LeaseID     string
	Identity    string
	ModelFamily string
	Attested    []string
	TTL         time.Duration
	Now         time.Time
}

// ClaimConflictJob runs the atomic claim for the resolve_conflict stage: it binds a
// conflict_resolver to an UNCLAIMED resolving_conflict job (state stays
// resolving_conflict, lease bound, epoch++). 0 rows -> ErrLostRace. The resolver
// then rebases + resolves in a worktree and returns the resolved diff via
// ResolveConflictResult. Anti-affinity: the resolver's identity/model_family must
// differ from the original builder's (a build's author should not resolve its own
// conflict under the same lens), mirroring the review claim's I-10 exclusion.
func (s *Store) ClaimConflictJob(ctx context.Context, p ClaimConflictParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	var result *lease.Lease
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var reqJSON, curState string
		var leaseID sql.NullString
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state, lease_id FROM jobs WHERE id = ?`, p.JobID).
			Scan(&reqJSON, &curState, &leaseID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("read conflict caps: %w", err)
		}
		if job.State(curState) != job.StateResolvingConflict || leaseID.Valid {
			return lease.ErrLostRace
		}
		if !job.CapabilitiesSatisfy(p.Attested, unmarshalStrings(reqJSON)) {
			return lease.ErrLostRace
		}
		// F6 per-model slot gate: a conflict_resolver is a real running agent — this closes
		// a resolver dispatched onto a box with zero free slots (the claim was ungated, and
		// resolving_conflict was missing from the slot-count clause, so the box was doubly
		// invisible to capacity). No-op without advertised slots.
		if err := modelSlotGateTx(ctx, tx, "", p.Identity, p.ModelFamily); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx, `
			UPDATE jobs
			   SET bound_identity     = ?,
			       bound_model_family = ?,
			       lease_epoch        = lease_epoch + 1,
			       lease_id           = ?,
			       lease_deadline     = ?,
			       lease_hb_due       = ?,
			       updated_at         = datetime('now')
			 WHERE id    = ?
			   AND state = 'resolving_conflict'
			   AND lease_id IS NULL
			   AND NOT EXISTS (
			        SELECT 1 FROM jobs sib
			         WHERE sib.id = COALESCE(jobs.eng_worker_job, jobs.id)
			           AND ( sib.builder_identity     = ?
			              OR sib.builder_model_family = ? ) )
			RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.LeaseID,
			deadline.Format(rfc3339), deadline.Format(rfc3339),
			p.JobID, p.Identity, p.ModelFamily)
		var newEpoch, prevSeq int
		if err := row.Scan(&newEpoch, &prevSeq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("atomic conflict claim: %w", err)
		}
		nextSeq := prevSeq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindLeaseClaimed,
			FromState: job.StateResolvingConflict, ToState: job.StateResolvingConflict,
			LeaseEpoch: newEpoch, Actor: p.Identity, CreatedAt: p.Now,
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
			return fmt.Errorf("insert conflict lease audit: %w", err)
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

// ── (4) stacked-PR auto-rebase + re-arm descendants ─────────────────────────────

// MarkStackedOn records that a job's branch is stacked atop a parent PR number (F8
// stacked-PR manager). When that parent merges, RearmStackedDescendants auto-rebases
// + re-arms this descendant. 0 clears the stack pointer.
func (s *Store) MarkStackedOn(ctx context.Context, jobID string, parentPR int) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE jobs SET stacked_on_pr = ?, updated_at = datetime('now') WHERE id = ?`,
		parentPR, jobID)
	return err
}

// RearmStackedDescendants is called when a parent PR merges (parentPR is its number,
// newBaseSHA the post-merge main SHA). Every job stacked on that PR is auto-rebased
// onto the new main and its SHA-bound verdict supersedes: the descendant re-arms
// review + CI at the new integrated head (clean) or becomes a resolve_conflict job
// (real conflict). Returns the descendant job ids that re-armed. Mirror is optional
// (tests pass nil + treat every descendant as a clean rebase). This is the §E "later"
// stacked-PR manager, made first-class.
func (s *Store) RearmStackedDescendants(ctx context.Context, mirror *gitops.Mirror, parentPR int, newBaseSHA string, now time.Time) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id FROM jobs WHERE stacked_on_pr = ? AND state NOT IN ('done','cancelled','superseded')`, parentPR)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var rearmed []string
	for _, id := range ids {
		// the descendant rebases onto the new main. A clean rebase re-arms review/CI at
		// the new head; a real conflict spawns a resolve_conflict job. We record a
		// stacked_rebased audit event first, then run the trivial/real split.
		if err := s.appendStackedRebased(ctx, id, parentPR, newBaseSHA, now); err != nil {
			return rearmed, err
		}
		if _, err := s.RebaseOnto(ctx, mirror, RebaseOntoParams{
			JobID: id, NewBaseSHA: newBaseSHA, Now: now,
		}); err != nil {
			return rearmed, err
		}
		// the parent is gone (merged): clear the stack pointer so a future merge of a
		// DIFFERENT PR with the same number can't re-trigger.
		if err := s.MarkStackedOn(ctx, id, 0); err != nil {
			return rearmed, err
		}
		rearmed = append(rearmed, id)
	}
	return rearmed, nil
}

// appendStackedRebased records the stacked_rebased audit event for a descendant.
func (s *Store) appendStackedRebased(ctx context.Context, jobID string, parentPR int, newBaseSHA string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindStackedRebased,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "system", CreatedAt: now,
			Payload: ledger.Payload{BaseSHA: newBaseSHA, PRNumber: parentPR},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
