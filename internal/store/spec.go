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
	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

// SeedSpecParams seeds a spec_author job (the first Flowbee job, §11.6). The chat
// is a lineage root, NOT a job: ChatRef anchors the root of chat->spec->issue->...
// The job starts in spec_authoring, ready to be leased by a spec_author.
type SeedSpecParams struct {
	ID         string
	ChatRef    string
	AuthorLens string
	Priority   int
	Repo       string // F9 repo-scope handle (a repos.id); reconcile-IN/project-OUT drain per repo
	// TaskText is the work item the spec_author must spec (shipped in its lease
	// context as $FLOWBEE_TASK). AcceptanceCriteria is the optional done-when.
	TaskText           string
	AcceptanceCriteria string
	Now                time.Time
}

// SeedSpecJob inserts a spec job in spec_authoring (§11.6). It is leasable by a
// spec_author worker; on the author's draft submission MaterializeSpec commits the
// bytes and opens the spec_review gate.
func (s *Store) SeedSpecJob(ctx context.Context, p SeedSpecParams) (job.Job, error) {
	err := s.tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO jobs (id, kind, flow, stage, state, role, chat_ref, author_lens, priority,
			                  blocked_by, required_capabilities, enqueued_at,
			                  lease_epoch, attempts, max_attempts, bounces, max_bounces, job_seq, repo,
			                  task_text, acceptance_criteria)
			VALUES (?, 'spec', 'spec', 'author', 'spec_authoring', 'spec_author', ?, ?, ?, '[]', ?, ?, 0, 0, 5, 0, 3, 1, ?, ?, ?)`,
			p.ID, p.ChatRef, p.AuthorLens, p.Priority,
			marshalStrings([]string{"role:spec_author"}), p.Now.Format(rfc3339), p.Repo,
			p.TaskText, p.AcceptanceCriteria)
		if err != nil {
			return fmt.Errorf("insert spec job: %w", err)
		}
		ev := ledger.Event{
			JobID: p.ID, JobSeq: 1, Kind: ledger.KindJobCreated,
			ToState: job.StateSpecAuthoring, Actor: "system", CreatedAt: p.Now,
			Payload: ledger.Payload{
				Kind: job.KindSpec, Flow: "spec", Stage: "author", Role: job.RoleSpecAuthor,
				Priority: p.Priority, RequiredCapabilities: []string{"role:spec_author"},
			},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, p.ID, 1)
	})
	if err != nil {
		return job.Job{}, err
	}
	return s.GetJob(ctx, p.ID)
}

// SpecAuthoringCandidates returns every spec_authoring job as a scheduler
// Candidate (for a spec_author's long-poll). ClaimSpecAuthor is the correctness
// backstop.
func (s *Store) SpecAuthoringCandidates(ctx context.Context) ([]scheduler.Candidate, error) {
	return s.candidatesInState(ctx, job.StateSpecAuthoring)
}

// SpecReviewCandidates returns every spec_review job as a scheduler Candidate (for
// a spec_reviewer's long-poll). ClaimSpecReview is the correctness backstop.
func (s *Store) SpecReviewCandidates(ctx context.Context) ([]scheduler.Candidate, error) {
	return s.candidatesInState(ctx, job.StateSpecReview)
}

// ClaimSpecAuthorParams describes a spec_author claiming a spec_authoring job.
type ClaimSpecAuthorParams struct {
	JobID       string
	LeaseID     string
	Identity    string
	ModelFamily string
	Attested    []string
	TTL         time.Duration
	Now         time.Time
}

// ClaimSpecAuthor runs the atomic claim for the spec_authoring stage. It binds the
// author, bumps the epoch (the fence MaterializeSpec checks), and records the
// lease audit row. Capability match on role:spec_author.
func (s *Store) ClaimSpecAuthor(ctx context.Context, p ClaimSpecAuthorParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	var result *lease.Lease
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var reqJSON, curState string
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state FROM jobs WHERE id = ?`, p.JobID).Scan(&reqJSON, &curState); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("read spec author caps: %w", err)
		}
		if job.State(curState) != job.StateSpecAuthoring {
			return lease.ErrLostRace
		}
		if !job.CapabilitiesSatisfy(p.Attested, unmarshalStrings(reqJSON)) {
			return lease.ErrLostRace
		}
		// Live-lease guard: unlike the build/review claims (which transition state on
		// claim, so a second claimer's WHERE no longer matches), the spec_author stage
		// STAYS spec_authoring while worked — so without this guard a SECOND spec_author
		// worker re-claims the in-flight job, bumps the epoch, and FENCES the first,
		// whose submit then 409s (the live multi-worker churn: 11 claims for one spec).
		// A job with an unexpired lease (heartbeats keep lease_deadline in the future) is
		// not claimable; an expired one (dead worker) is — the recovery path is preserved.
		row := tx.QueryRowContext(ctx, `
			UPDATE jobs
			   SET bound_identity = ?, bound_model_family = ?,
			       lease_epoch = lease_epoch + 1, lease_id = ?,
			       lease_deadline = ?, lease_hb_due = ?, updated_at = datetime('now')
			 WHERE id = ? AND state = 'spec_authoring'
			   AND (bound_identity IS NULL OR bound_identity = ''
			        OR lease_deadline IS NULL OR lease_deadline = '' OR lease_deadline < ?)
			RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.LeaseID,
			deadline.Format(rfc3339), deadline.Format(rfc3339), p.JobID, p.Now.Format(rfc3339))
		var newEpoch, prevSeq int
		if err := row.Scan(&newEpoch, &prevSeq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("atomic spec author claim: %w", err)
		}
		nextSeq := prevSeq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindLeaseClaimed,
			FromState: job.StateSpecAuthoring, ToState: job.StateSpecAuthoring,
			LeaseEpoch: newEpoch, Actor: p.Identity, CreatedAt: p.Now,
			Payload: ledger.Payload{LeaseID: p.LeaseID, BoundIdentity: p.Identity, BoundModelFamily: p.ModelFamily},
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
			return fmt.Errorf("insert spec author lease audit: %w", err)
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

// candidatesInState returns every job in the given state as a Candidate.
func (s *Store) candidatesInState(ctx context.Context, state job.State) ([]scheduler.Candidate, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, priority, enqueued_at, required_capabilities
		  FROM jobs WHERE state = ?`, string(state))
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

// MaterializeSpecParams records a committed spec revision (§11.1). Flowbee — never
// the worker — commits spec.md to the spec ref and computes ContentHash (the
// author may not self-address its own artifact, §11.1). The resolved hash is
// passed in; the store records it and opens the spec_review gate.
type MaterializeSpecParams struct {
	JobID       string
	ContentHash string // BLAKE3 of spec.md, computed by Flowbee (internal/spec)
	Version     int    // ordinal on the spec branch
	Markdown    string // the authored spec.md bytes, retained so project-OUT renders the issue body
	Epoch       int    // the author's lease epoch (fenced; stale -> 409)
	Now         time.Time
}

// MaterializeSpec commits the author's spec draft (spec_authoring -> spec_review,
// §11.6): it records the spec_content_hash + version on the job and opens the gate.
// Fenced by the author's lease epoch. This is where the spec gets its content
// address — the BLAKE3 hash the sign-off will bind to.
func (s *Store) MaterializeSpec(ctx context.Context, p MaterializeSpecParams) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, p.JobID)
		if err != nil {
			return err
		}
		if j.State != job.StateSpecAuthoring {
			return fmt.Errorf("materialize: job %s not in spec_authoring (%s)", p.JobID, j.State)
		}
		// fence: only the live author lease may submit the draft.
		if p.Epoch != j.LeaseEpoch {
			return lease.ErrStaleEpoch
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindSpecAuthored,
			FromState: job.StateSpecAuthoring, ToState: job.StateSpecReview,
			LeaseEpoch: j.LeaseEpoch, Actor: j.BoundIdentity, CreatedAt: p.Now,
			Payload: ledger.Payload{SpecContentHash: p.ContentHash, SpecVersion: p.Version},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		// spec_authoring -> spec_review: record the hash/version, switch the role
		// requirement to the spec_reviewer, release the author lease. The reviewer
		// claims this review_pending-equivalent gate stage.
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'spec_review', stage = 'review', role = 'spec_reviewer',
			       spec_content_hash = ?, spec_version = ?, spec_text = ?,
			       required_capabilities = ?,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, updated_at = datetime('now')
			 WHERE id = ?`,
			p.ContentHash, p.Version, p.Markdown, marshalStrings([]string{"role:spec_reviewer"}), p.JobID); err != nil {
			return fmt.Errorf("materialize spec projection: %w", err)
		}
		return setJobSeq(ctx, tx, p.JobID, nextSeq)
	})
}

// EditSpec records a NEW spec revision landing on an already-reviewed job (§11.5):
// the bytes changed, the content hash moves, any prior sign-off is SUPERSEDED, and
// the gate re-arms against the new bytes. This is the spec-flow analogue of the
// SHA-move supersession (I-5). Used when a human/author edits the spec.
func (s *Store) EditSpec(ctx context.Context, jobID, newHash string, newVersion int, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		// an edit only matters if the hash actually moved.
		if j.SpecContentHash == newHash {
			return nil
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindSpecSuperseded,
			FromState: j.State, ToState: job.StateSpecReview,
			LeaseEpoch: j.LeaseEpoch, Actor: "system", CreatedAt: now,
			Payload: ledger.Payload{SpecContentHash: newHash, SpecVersion: newVersion},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		// void the prior sign-off, advance the hash/version, re-arm the gate. Any
		// pending outbox renderings bound to the old hash are abandoned (§8.2.2):
		// the same edit that voids the sign-off voids its pending renderings.
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'spec_review', stage = 'review', role = 'spec_reviewer',
			       spec_content_hash = ?, spec_version = ?,
			       spec_signoff = NULL, spec_signoff_hash = NULL,
			       required_capabilities = ?,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, updated_at = datetime('now')
			 WHERE id = ?`,
			newHash, newVersion, marshalStrings([]string{"role:spec_reviewer"}), jobID); err != nil {
			return fmt.Errorf("edit spec projection: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// ResolveDesign records a human supplying the design decision for a job parked in
// needs_design (F4). It re-arms the spec_review gate (a fresh review judges the
// now-resolved spec), optionally advancing the spec to a new resolved hash/version
// if the human edited the spec to encode the decision. This is the resume edge of
// the design-fork escalation surfaced on /v1/needs-input. newHash may be "" to
// re-review the SAME bytes (the human's answer rode in the chat, not the spec).
func (s *Store) ResolveDesign(ctx context.Context, jobID, newHash string, newVersion int, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		if j.State != job.StateNeedsDesign {
			return fmt.Errorf("resolve design: job %s not in needs_design (%s)", jobID, j.State)
		}
		nextSeq := seq + 1
		hash := newHash
		version := newVersion
		if hash == "" {
			hash = j.SpecContentHash
			version = j.SpecVersion
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindDesignResolved,
			FromState: job.StateNeedsDesign, ToState: job.StateSpecReview,
			LeaseEpoch: j.LeaseEpoch, Actor: "human", CreatedAt: now,
			Payload: ledger.Payload{SpecContentHash: hash, SpecVersion: version},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE jobs
			   SET state = 'spec_review', stage = 'review', role = 'spec_reviewer',
			       spec_content_hash = ?, spec_version = ?, escalation_reason = '',
			       required_capabilities = ?,
			       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
			       lease_hb_due = NULL, updated_at = datetime('now')
			 WHERE id = ?`,
			hash, version, marshalStrings([]string{"role:spec_reviewer"}), jobID); err != nil {
			return fmt.Errorf("resolve design projection: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// NeedsInputItem is one entry on the /v1/needs-input surface (F4 / flow-pass §D):
// a job awaiting a human decision (a design fork). The user's board-check loop
// reads these, walks the human through, posts the answer, and Flowbee resumes.
type NeedsInputItem struct {
	JobID           string `json:"job_id"`
	State           string `json:"state"`
	Reason          string `json:"reason"`
	SpecContentHash string `json:"spec_content_hash,omitempty"`
	ChatRef         string `json:"chat_ref,omitempty"`
	EpicID          string `json:"epic_id,omitempty"`
}

// NeedsInput returns every job awaiting human input (needs_design), oldest first
// (F4). It is the read-model behind GET /v1/needs-input.
func (s *Store) NeedsInput(ctx context.Context) ([]NeedsInputItem, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT id, state, COALESCE(escalation_reason,''), COALESCE(spec_content_hash,''),
		       COALESCE(chat_ref,''), COALESCE(epic_id,'')
		  FROM jobs WHERE state = 'needs_design' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NeedsInputItem{}
	for rows.Next() {
		var it NeedsInputItem
		if err := rows.Scan(&it.JobID, &it.State, &it.Reason, &it.SpecContentHash, &it.ChatRef, &it.EpicID); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ClaimSpecReviewParams describes a spec_reviewer claiming the spec_review gate.
type ClaimSpecReviewParams struct {
	JobID       string
	LeaseID     string
	Identity    string
	ModelFamily string
	Lens        string
	Attested    []string
	TTL         time.Duration
	Now         time.Time
}

// ClaimSpecReview runs the atomic claim for the spec_review gate stage. The §5.5
// spec anti-affinity term (spec_author.lens != spec_reviewer.lens) is enforced
// here: a reviewer whose lens equals the author lens is excluded (0 rows ->
// ErrLostRace, the job stays armed for a distinct-lens reviewer). It binds the
// reviewer's lens for the gate's distinct-lens record.
func (s *Store) ClaimSpecReview(ctx context.Context, p ClaimSpecReviewParams) (*lease.Lease, error) {
	deadline := p.Now.Add(p.TTL)
	var result *lease.Lease
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var reqJSON, curState, authorLens string
		if err := tx.QueryRowContext(ctx,
			`SELECT required_capabilities, state, COALESCE(author_lens,'') FROM jobs WHERE id = ?`, p.JobID).
			Scan(&reqJSON, &curState, &authorLens); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("read spec review caps: %w", err)
		}
		if job.State(curState) != job.StateSpecReview {
			return lease.ErrLostRace
		}
		if !job.CapabilitiesSatisfy(p.Attested, unmarshalStrings(reqJSON)) {
			return lease.ErrLostRace
		}
		// §5.5 distinct-lens anti-affinity: a reviewer lens equal to the author lens
		// may NOT judge the spec (the independent-lens guarantee, I-10).
		if authorLens != "" && p.Lens == authorLens {
			return lease.ErrLostRace
		}
		// Live-lease guard (same as ClaimSpecAuthor): spec_review STAYS spec_review while
		// worked, so without this a second spec_reviewer steals the in-flight lease and
		// fences the first → submit 409 churn. Unexpired lease => not claimable.
		row := tx.QueryRowContext(ctx, `
			UPDATE jobs
			   SET role = 'spec_reviewer', stage = 'review',
			       bound_identity = ?, bound_model_family = ?, reviewer_lens = ?,
			       lease_epoch = lease_epoch + 1, lease_id = ?,
			       lease_deadline = ?, lease_hb_due = ?, updated_at = datetime('now')
			 WHERE id = ? AND state = 'spec_review'
			   AND (bound_identity IS NULL OR bound_identity = ''
			        OR lease_deadline IS NULL OR lease_deadline = '' OR lease_deadline < ?)
			RETURNING lease_epoch, job_seq`,
			p.Identity, p.ModelFamily, p.Lens, p.LeaseID,
			deadline.Format(rfc3339), deadline.Format(rfc3339), p.JobID, p.Now.Format(rfc3339))
		var newEpoch, prevSeq int
		if err := row.Scan(&newEpoch, &prevSeq); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return lease.ErrLostRace
			}
			return fmt.Errorf("atomic spec review claim: %w", err)
		}
		nextSeq := prevSeq + 1
		ev := ledger.Event{
			JobID: p.JobID, JobSeq: nextSeq, Kind: ledger.KindReviewClaimed,
			FromState: job.StateSpecReview, ToState: job.StateSpecReview,
			LeaseEpoch: newEpoch, Actor: p.Identity, CreatedAt: p.Now,
			Payload: ledger.Payload{LeaseID: p.LeaseID, BoundIdentity: p.Identity, BoundModelFamily: p.ModelFamily},
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
			return fmt.Errorf("insert spec review lease audit: %w", err)
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

// SpecReviewResultParams is a fenced spec-review result carrying the reviewer's
// CLAIM (untrusted) + the two sub-checks (§11.3) + the hash it judged.
type SpecReviewResultParams struct {
	JobID             string
	Epoch             int
	Claim             job.VerdictValue // signed_off | amended | needs_design | changes_requested
	BindsTo           string           // the spec_content_hash the worker judged
	MeetsStyle        bool
	MeetsRequirements bool
	// F4 amend-in-place: the reviewer's amended spec bytes. When the claim is
	// `amended`, FLOWBEE — never the worker — commits these bytes: the store computes
	// the BLAKE3 content hash (AmendedHash) and the new version, and the gate mints a
	// sign-off bound to the AMENDED hash. The author is never bounced.
	AmendedHash    string // resolved by the caller (api) via spec.ContentHash
	AmendedVersion int
	IdempotencyKey string
	Now            time.Time
}

// SpecReviewResultResponse is the spec gate's reply.
type SpecReviewResultResponse struct {
	Accepted    bool   `json:"accepted"`
	JobState    string `json:"job_state"`
	Minted      bool   `json:"minted"`       // a content-hash-bound sign-off was minted (I-9)
	Superseded  bool   `json:"superseded"`   // the claim bound to a stale hash (§11.5)
	Amended     bool   `json:"amended"`      // F4: the spec was amended in place + signed off (no author bounce)
	NeedsDesign bool   `json:"needs_design"` // F4: the spec needs human design input (design fork)
}

// SpecReviewResult is the I-9 spec keystone (§11.5): a fenced spec-review result.
// The worker's claim is recorded as an UNTRUSTED spec_claim event; the engine runs
// the PURE spec gate over the CURRENT content hash (the bytes, never the claim) and
// either MINTS a content-hash-bound sign-off (signed_off) or bounces/supersedes. On
// a sign-off, an issues.create outbox row is enqueued (materialize_issues, §11)
// transactionally with the mint. Stale lease epoch -> 409; duplicate key -> cached.
func (s *Store) SpecReviewResult(ctx context.Context, in SpecReviewResultParams) (SpecReviewResultResponse, error) {
	var resp SpecReviewResultResponse
	err := s.tx(ctx, func(tx *sql.Tx) error {
		if in.IdempotencyKey != "" {
			var cached string
			e := tx.QueryRowContext(ctx,
				`SELECT response FROM result_idempotency WHERE job_id=? AND idempotency_key=?`,
				in.JobID, in.IdempotencyKey).Scan(&cached)
			if e == nil {
				return json.Unmarshal([]byte(cached), &resp)
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return fmt.Errorf("spec idempotency lookup: %w", e)
			}
		}

		j, seq, err := loadJobTx(ctx, tx, in.JobID)
		if err != nil {
			return err
		}
		var authorLens, reviewerLens string
		var isEpic int
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(author_lens,''), COALESCE(reviewer_lens,''), COALESCE(is_epic,0) FROM jobs WHERE id = ?`, in.JobID).
			Scan(&authorLens, &reviewerLens, &isEpic)

		dec := engine.Decide(engine.EngineState{
			Job: j, Now: in.Now, Epoch: j.LeaseEpoch,
			Spec: engine.SpecState{
				CurrentHash: j.SpecContentHash, Version: j.SpecVersion,
				AuthorLens: authorLens, ReviewerLens: reviewerLens,
			},
		}, engine.SpecReviewClaim{
			Epoch: in.Epoch, Claim: in.Claim, ClaimBindsTo: in.BindsTo,
			MeetsStyle: in.MeetsStyle, MeetsRequirements: in.MeetsRequirements,
			AmendedHash: in.AmendedHash, AmendedVersion: in.AmendedVersion,
		})
		if dec.Reject != nil {
			return lease.ErrStaleEpoch
		}

		nextSeq := seq
		// 1) record the UNTRUSTED claim for audit.
		nextSeq++
		claimEv := ledger.Event{
			JobID: in.JobID, JobSeq: nextSeq, Kind: ledger.KindSpecClaim,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: j.BoundIdentity, CreatedAt: in.Now,
			Payload: ledger.Payload{VerdictClaim: in.Claim, SpecContentHash: in.BindsTo},
		}
		if err := appendEvent(ctx, tx, claimEv); err != nil {
			return err
		}

		// 2) apply the gate decision.
		final := j.State
		var minted *job.SpecSignoff
		amended := false
		needsDesign := false
		for _, t := range dec.Transitions {
			nextSeq++
			pay := ledger.Payload{BouncesDelta: t.BouncesDelta, SpecSignoff: t.SpecSignoff}
			// F4: an amend transition advances the spec to the AMENDED content address
			// in place; record it on the event so Fold re-binds the hash/version.
			if t.Kind == ledger.KindSpecAmended {
				pay.SpecContentHash = in.AmendedHash
				pay.SpecVersion = in.AmendedVersion
				amended = true
			}
			if t.Kind == ledger.KindSpecNeedsDesign {
				needsDesign = true
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
			if t.SpecSignoff != nil {
				minted = t.SpecSignoff
			}
		}

		bouncesDelta := 0
		for _, t := range dec.Transitions {
			bouncesDelta += t.BouncesDelta
		}

		switch {
		case minted != nil:
			// 3a) a sign-off (signed_off OR F4 amended-in-place): stamp it + bind the
			// hash, advance the content address on an amend, close the gate lease. The
			// job is `done`. On an amend the spec advanced IN PLACE to the AMENDED
			// hash/version — no author bounce — so the projection moves the content
			// address too.
			blob, _ := json.Marshal(minted)
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'done', spec_signoff = ?, spec_signoff_hash = ?,
				       spec_content_hash = ?, spec_version = ?,
				       bounces = bounces + ?,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				string(blob), minted.SpecHash, minted.SpecHash, minted.SpecVersion,
				bouncesDelta, in.JobID); err != nil {
				return fmt.Errorf("apply spec signoff projection: %w", err)
			}
			if isEpic == 1 {
				// F4 epic-level barrier: the ONE issue-review over the whole epic passed
				// (scope · coverage · dep-graph · standards). Mark the epic reviewed and
				// record the barrier event; the issues fan out via EpicFanOut. An epic
				// barrier materializes NO single issue (its children are the issues).
				nextSeq++
				erev := ledger.Event{
					JobID: in.JobID, JobSeq: nextSeq, Kind: ledger.KindEpicReviewed,
					FromState: job.StateSpecReview, ToState: job.StateDone,
					LeaseEpoch: j.LeaseEpoch, Actor: "system", CreatedAt: in.Now,
				}
				if err := appendEvent(ctx, tx, erev); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE jobs SET epic_reviewed = 1, updated_at = datetime('now') WHERE id = ?`, in.JobID); err != nil {
					return fmt.Errorf("mark epic reviewed: %w", err)
				}
				// the children fan out via the fan-out drain (a serve tick calling
				// FanOutReviewedEpics) — kept a SEPARATE step from the barrier so the
				// review and the release stay distinct (the §F4 barrier-before-fan-out
				// invariant). The bug this closes: that drain never existed, so an epic's
				// issues sat in backlog forever after review.
			} else {
				// materialize_issues: enqueue the issues.create rendering (§8.2.1). The
				// outbox key uses the content hash in place of a head_sha (spec is SHA-less).
				if err := enqueueOutboxTx(ctx, tx, OutboxRow{
					JobID: in.JobID, Action: ActionCreateIssue, HeadSHA: "",
					Payload: outboxPayload(map[string]any{"spec_hash": minted.SpecHash, "version": minted.SpecVersion}),
				}); err != nil {
					return fmt.Errorf("enqueue materialize_issues: %w", err)
				}
			}
			resp.Minted = true
			resp.Amended = amended
		case needsDesign:
			// 3a') F4 design fork: park the job in needs_design (surfaced on
			// /v1/needs-input), close the gate lease, record the escalation reason. No
			// sign-off, no author bounce. A human resolves it via ResolveDesign.
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'needs_design', escalation_reason = ?,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				string(job.EscalationDesign), in.JobID); err != nil {
				return fmt.Errorf("apply spec needs_design projection: %w", err)
			}
			resp.NeedsDesign = true
		case final == job.StateSpecAuthoring:
			// 3b) a bounce / supersession: re-arm the author stage.
			superseded := false
			for _, t := range dec.Transitions {
				if t.Kind == ledger.KindSpecSuperseded {
					superseded = true
				}
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = 'spec_authoring', stage = 'author', role = 'spec_author',
				       required_capabilities = ?, bounces = bounces + ?,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				marshalStrings([]string{"role:spec_author"}), bouncesDelta, in.JobID); err != nil {
				return fmt.Errorf("apply spec bounce projection: %w", err)
			}
			resp.Superseded = superseded
		default:
			// 3c) bounce exhausted -> needs_human.
			if _, err := tx.ExecContext(ctx, `
				UPDATE jobs
				   SET state = ?, bounces = bounces + ?,
				       lease_id = NULL, bound_identity = NULL, bound_model_family = NULL,
				       lease_hb_due = NULL, updated_at = datetime('now')
				 WHERE id = ?`,
				string(final), bouncesDelta, in.JobID); err != nil {
				return fmt.Errorf("apply spec exhaust projection: %w", err)
			}
		}

		if err := setJobSeq(ctx, tx, in.JobID, nextSeq); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE leases SET ended_at = datetime('now'), end_reason = 'completed'
			 WHERE job_id = ? AND lease_epoch = ? AND ended_at IS NULL`,
			in.JobID, j.LeaseEpoch); err != nil {
			return fmt.Errorf("close spec review lease: %w", err)
		}

		resp.Accepted = true
		resp.JobState = string(final)
		if in.IdempotencyKey != "" {
			blob, _ := json.Marshal(resp)
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO result_idempotency (job_id, idempotency_key, response) VALUES (?, ?, ?)`,
				in.JobID, in.IdempotencyKey, string(blob)); err != nil {
				return fmt.Errorf("store spec idempotency: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return SpecReviewResultResponse{}, err
	}
	return resp, nil
}

// StampIssueNumber records the GitHub issue number a materialize_issues drain
// returned (§11). issue_number is GitHub-owned; written only by the project-OUT
// materialize path (never by a worker). It also closes the lineage edge so a build
// job can descend from this spec.
func (s *Store) StampIssueNumber(ctx context.Context, jobID string, issueNumber int, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindIssueMaterialized,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{IssueNumber: issueNumber},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET issue_number = ?, updated_at = datetime('now') WHERE id = ?`,
			issueNumber, jobID); err != nil {
			return fmt.Errorf("stamp issue number: %w", err)
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}
