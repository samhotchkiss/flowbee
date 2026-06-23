package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// OutboxRow is one desired project-OUT side-effect (§8.2). It is enqueued
// transactionally with the Domain-A state change that motivated it and drained by
// a single serialized sender. The (JobID, Action, HeadSHA) triple is the
// idempotency key: enqueuing the same triple twice is a no-op (ON CONFLICT).
type OutboxRow struct {
	ID       int64
	JobID    string
	Action   string
	HeadSHA  string
	Payload  string // action-specific JSON args
	Status   string
	Attempts int // failed-send count so far (the dead-letter backstop reads this)
}

// outbox action constants (§8.2.1).
const (
	ActionOpenPR       = "pulls.create"
	ActionCreateIssue  = "issues.create"
	ActionSetLabels    = "labels.set"
	ActionCreateCheck  = "checks.create"
	ActionEnqueueMerge = "mergeQueue.enqueue"
	ActionComment      = "pulls.comment"
	// M11 compensation (§6.5.4, I-12): draft-back a PR opened for a now-dead epoch's
	// attempt — never leave a revoked zombie's PR ready-for-review.
	ActionDraftPR = "pulls.draft"
	// ActionDeleteBranch deletes a merged job's flowbee/issue-N branch (post-merge
	// cleanup) so the repo doesn't accumulate stale flowbee/issue-* branches forever.
	ActionDeleteBranch = "git.deleteBranch"
)

// EnqueueOutbox writes one outbox row in the caller's transaction (the
// transactional-enqueue guarantee, §8.2.2: the row is written in the SAME tx as
// the state change, so there is no window where Flowbee believes it rendered
// something it never enqueued). A duplicate (job, action, head_sha) is ignored —
// the dedupe key collapses re-enqueues to one effect.
func enqueueOutboxTx(ctx context.Context, tx *sql.Tx, row OutboxRow) error {
	payload := row.Payload
	if payload == "" {
		payload = "{}"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO outbox (job_id, action, head_sha, payload, status)
		VALUES (?, ?, ?, ?, 'pending')
		ON CONFLICT (job_id, action, head_sha) DO NOTHING`,
		row.JobID, row.Action, row.HeadSHA, payload)
	return err
}

// EnqueueOutbox writes one outbox row in its own transaction (the standalone
// enqueue path; the spec/PR-open paths enqueue within their own state-change tx).
func (s *Store) EnqueueOutbox(ctx context.Context, row OutboxRow) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		return enqueueOutboxTx(ctx, tx, row)
	})
}

// NextPendingOutbox claims the oldest pending outbox row for sending. It returns
// ok=false when the queue is empty. Because the store serializes writes
// (MaxOpenConns=1) and there is exactly ONE sender (§8.2.4), no locking dance is
// needed — the read-then-send-then-mark sequence cannot interleave with another
// sender.
func (s *Store) NextPendingOutbox(ctx context.Context) (OutboxRow, bool, error) {
	var row OutboxRow
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, job_id, action, head_sha, payload, status, attempts
		  FROM outbox WHERE status = 'pending' ORDER BY id ASC LIMIT 1`).
		Scan(&row.ID, &row.JobID, &row.Action, &row.HeadSHA, &row.Payload, &row.Status, &row.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, err
	}
	return row, true, nil
}

// NextPendingOutboxForRepo claims the oldest pending outbox row whose job belongs
// to the given F9 repo scope (build-list F9). One control plane runs one project-OUT
// Sender per repo — each over the repo's own github.Writer — so the drains must be
// repo-scoped: a sender only renders side-effects for its own repo's jobs, never
// another repo's. An empty repo scopes to legacy single-repo (repo=”) jobs, so the
// pre-F9 NextPendingOutbox is the degenerate single-repo case of this one.
func (s *Store) NextPendingOutboxForRepo(ctx context.Context, repo string) (OutboxRow, bool, error) {
	var row OutboxRow
	err := s.DB.QueryRowContext(ctx, `
		SELECT o.id, o.job_id, o.action, o.head_sha, o.payload, o.status, o.attempts
		  FROM outbox o JOIN jobs j ON j.id = o.job_id
		 WHERE o.status = 'pending' AND j.repo = ?
		 ORDER BY o.id ASC LIMIT 1`, repo).
		Scan(&row.ID, &row.JobID, &row.Action, &row.HeadSHA, &row.Payload, &row.Status, &row.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return OutboxRow{}, false, nil
	}
	if err != nil {
		return OutboxRow{}, false, err
	}
	return row, true, nil
}

// MarkOutboxSent flips an outbox row to 'sent' and writes the audit-log row in
// the SAME transaction (§3.3): every GitHub action appears ONCE in the audit log,
// keyed identically to the outbox (job_id, action, head_sha). The audit UNIQUE
// key makes a re-drain idempotent at the audit layer too — so a re-sent row never
// produces a second audit entry. detail carries the returned PR/issue number.
func (s *Store) MarkOutboxSent(ctx context.Context, id int64, detail string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var row OutboxRow
		if err := tx.QueryRowContext(ctx,
			`SELECT job_id, action, head_sha FROM outbox WHERE id = ?`, id).
			Scan(&row.JobID, &row.Action, &row.HeadSHA); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox SET status='sent', sent_at=datetime('now'), attempts=attempts+1 WHERE id = ?`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO audit_log (job_id, action, head_sha, detail)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (job_id, action, head_sha) DO NOTHING`,
			row.JobID, row.Action, row.HeadSHA, detail); err != nil {
			return err
		}
		return nil
	})
}

// MarkOutboxSuppressed abandons an outbox row WITHOUT writing an audit entry (the
// §8.2.3 / I-16 ADOPT exception): a suppressed action on a quiescent job is not a
// GitHub action — it never happened — so it must not appear in the audit log.
func (s *Store) MarkOutboxSuppressed(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE outbox SET status='abandoned', sent_at=datetime('now') WHERE id = ?`, id)
	return err
}

// BumpOutboxAttempts increments the attempts counter on a row that failed to send
// (kept pending for retry). Used for transient send errors / Retry-After parks.
func (s *Store) BumpOutboxAttempts(ctx context.Context, id int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE outbox SET attempts=attempts+1 WHERE id = ?`, id)
	return err
}

// RetryAbandonedOutbox re-arms a job's dead-lettered (abandoned) outbox actions back to
// `pending` (attempts reset) so the project-OUT drain re-attempts them — the operator recovery
// after the underlying cause is fixed (a transient race like the §F archive, or a re-POSTed spec
// after a malformed one's issue-create failed). Every drained action is idempotent (an abandoned
// action took no effect, so a re-attempt can't double-apply), so this is safe to call. Returns
// the number of actions re-armed.
func (s *Store) RetryAbandonedOutbox(ctx context.Context, jobID string) (int, error) {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE outbox SET status='pending', attempts=0 WHERE job_id=? AND status='abandoned'`, jobID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RetryAbandonedOutboxForRepo re-arms every abandoned outbox action whose owning job
// belongs to repo. This is the bulk recovery path after fixing a repo-scoped GitHub
// problem (expired token, branch protection drift, deleted refs) without looping over
// individual job ids.
func (s *Store) RetryAbandonedOutboxForRepo(ctx context.Context, repo string) (int, error) {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE outbox
		   SET status='pending', attempts=0
		 WHERE status='abandoned'
		   AND job_id IN (SELECT id FROM jobs WHERE repo = ?)`, repo)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// RetryAllAbandonedOutbox re-arms every abandoned outbox action across every repo.
// Use only after the global cause is fixed; project-OUT actions are idempotent, so
// retrying an abandoned action does not double-apply a GitHub write.
func (s *Store) RetryAllAbandonedOutbox(ctx context.Context) (int, error) {
	res, err := s.DB.ExecContext(ctx,
		`UPDATE outbox SET status='pending', attempts=0 WHERE status='abandoned'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// OutboxAbandonedByAction counts ACTIONABLE abandoned (dead-lettered) outbox actions per
// action type — GitHub writes that never took effect AND whose owning job is still live (or
// parked at needs_human). Critical abandons (create-issue/merge) escalate the owning job to
// needs_human, but cosmetic ones (comments, the §F archive) are otherwise silent, so this is
// the one alertable signal for "work was dropped". A growing count is the page (e.g. an
// expired token, a persistent 4xx).
//
// It EXCLUDES abandons whose owning job has since reached `done` or `cancelled`: those are
// benign by construction — a stale-SHA void (AbandonOutboxForJobSHA), a superseded merge
// attempt for a PR that merged anyway, or a side-effect on a cancelled job. Counting them
// produced a permanent false "work dropped" warning that never drained (it shouldn't —
// the job already completed), drowning the real signal (russ #215).
func (s *Store) OutboxAbandonedByAction(ctx context.Context) (map[string]int, error) {
	// LEFT JOIN so an orphaned abandon (job row gone) still surfaces — only a job that
	// DEFINITIVELY reached done/cancelled is excluded as benign.
	rows, err := s.DB.QueryContext(ctx, `
		SELECT o.action, COUNT(*)
		  FROM outbox o LEFT JOIN jobs j ON j.id = o.job_id
		 WHERE o.status='abandoned' AND COALESCE(j.state,'') NOT IN ('done','cancelled')
		 GROUP BY o.action`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var action string
		var n int
		if err := rows.Scan(&action, &n); err != nil {
			return nil, err
		}
		out[action] = n
	}
	return out, rows.Err()
}

// AbandonedOutboxRow is one dead-lettered outbox action with the context an operator needs to
// triage it: which job + repo produced it, that job's CURRENT state, and whether the abandon
// is Actionable (the job is still live / parked at needs_human and genuinely missing this
// side-effect) or benign (the job reached done/cancelled anyway).
type AbandonedOutboxRow struct {
	ID         int64
	JobID      string
	Repo       string
	Action     string
	JobState   string
	Attempts   int
	AgeHours   int
	Actionable bool
}

// AbandonedOutbox lists every abandoned outbox row with its owning job's state, newest first,
// so `flowbee outbox` can show WHY each write was dropped and whether it still needs attention.
func (s *Store) AbandonedOutbox(ctx context.Context) ([]AbandonedOutboxRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT o.id, o.job_id, COALESCE(j.repo,''), o.action, COALESCE(j.state,'(no job)'), o.attempts,
		       CAST((julianday('now')-julianday(o.enqueued_at))*24 AS INT)
		  FROM outbox o LEFT JOIN jobs j ON j.id = o.job_id
		 WHERE o.status='abandoned'
		 ORDER BY o.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AbandonedOutboxRow
	for rows.Next() {
		var r AbandonedOutboxRow
		if err := rows.Scan(&r.ID, &r.JobID, &r.Repo, &r.Action, &r.JobState, &r.Attempts, &r.AgeHours); err != nil {
			return nil, err
		}
		r.Actionable = r.JobState != string(job.StateDone) && r.JobState != string(job.StateCancelled)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeadLetterOutbox abandons a poison outbox row — a PERMANENT GitHub failure (a 4xx:
// deleted branch/PR, 422, 404) or one that exhausted its retry budget — so the rest of
// the repo's GitHub writes keep flowing instead of wedging behind it (the outbox is
// serialized oldest-first; a stuck head row blocks everything). When `escalate` (a
// CRITICAL action: open-PR / merge / create-issue), the owning job is surfaced to
// needs_human with `reason` so a human fixes the GitHub state and requeues; a cosmetic
// action (comment / label / check) is simply dropped. The row is abandoned WITHOUT an
// audit entry — the action never took effect on GitHub.
func (s *Store) DeadLetterOutbox(ctx context.Context, rowID int64, jobID, reason, detail string, escalate bool, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`UPDATE outbox SET status='abandoned', sent_at=datetime('now'), attempts=attempts+1 WHERE id = ?`, rowID); err != nil {
			return err
		}
		if !escalate {
			return nil
		}
		j, seq, err := loadJobTx(ctx, tx, jobID)
		if err != nil {
			return err
		}
		// already terminal / human-owned: drop the row, don't rewind state.
		if j.State == job.StateDone || j.State == job.StateNeedsHuman || j.State == job.StateCancelled {
			return nil
		}
		nextSeq := seq + 1
		// record the failing GitHub error on the escalation event so a needs_human job is
		// SELF-EXPLAINING (the operator sees why the write was abandoned, instead of having
		// to reproduce it). RevokeReason is the existing "why forced off the lease" field.
		if len(detail) > 500 {
			detail = detail[:500]
		}
		ev := ledger.Event{
			JobID: jobID, JobSeq: nextSeq, Kind: ledger.KindStateChanged,
			FromState: j.State, ToState: job.StateNeedsHuman, LeaseEpoch: j.LeaseEpoch,
			Actor: "project-out", CreatedAt: now,
			Payload: ledger.Payload{RevokeReason: detail, EscalationReason: reason},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE jobs SET state='needs_human', escalation_reason=?, lease_id=NULL,
			     bound_identity=NULL, lease_hb_due=NULL, updated_at=datetime('now') WHERE id=?`,
			reason, jobID); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, jobID, nextSeq)
	})
}

// AbandonOutboxForJobSHA voids every pending outbox row for a job whose head_sha
// is NOT the given current SHA (§8.2.2: when the SHA moves, stale outbox rows are
// abandoned and fresh ones enqueued for the new SHA — the same mechanism that
// voids the sign-off voids its pending renderings). Rows with an empty head_sha
// (SHA-less spec actions) are left untouched.
func (s *Store) AbandonOutboxForJobSHA(ctx context.Context, jobID, currentSHA string) error {
	_, err := s.DB.ExecContext(ctx, `
		UPDATE outbox SET status='abandoned'
		 WHERE job_id = ? AND status='pending' AND head_sha <> '' AND head_sha <> ?`,
		jobID, currentSHA)
	return err
}

// AuditRow is one recorded GitHub action (§3.3), keyed (job_id, action, head_sha).
type AuditRow struct {
	JobID   string    `json:"job_id"`
	Action  string    `json:"action"`
	HeadSHA string    `json:"head_sha"`
	Detail  string    `json:"detail"`
	ActedAt time.Time `json:"acted_at"`
}

// AuditLog returns the audit-log rows for a job, in order (for the §3.3
// once-per-key assertion + the audit UI).
func (s *Store) AuditLog(ctx context.Context, jobID string) ([]AuditRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT job_id, action, head_sha, detail, acted_at
		  FROM audit_log WHERE job_id = ? ORDER BY id ASC`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		var acted string
		if err := rows.Scan(&a.JobID, &a.Action, &a.HeadSHA, &a.Detail, &acted); err != nil {
			return nil, err
		}
		if ts, perr := time.Parse(sqliteTS, acted); perr == nil {
			a.ActedAt = ts
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllAudit returns every audit row (for the audit board + whole-run assertions).
func (s *Store) AllAudit(ctx context.Context) ([]AuditRow, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT job_id, action, head_sha, detail, acted_at FROM audit_log ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		var acted string
		if err := rows.Scan(&a.JobID, &a.Action, &a.HeadSHA, &a.Detail, &acted); err != nil {
			return nil, err
		}
		if ts, perr := time.Parse(sqliteTS, acted); perr == nil {
			a.ActedAt = ts
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// outboxPayload encodes action args to JSON for an outbox row.
func outboxPayload(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// sqliteTS is SQLite's default datetime('now') format (UTC, no zone).
const sqliteTS = "2006-01-02 15:04:05"
