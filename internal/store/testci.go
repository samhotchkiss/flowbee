package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
)

// F10: the pluggable ci_green@sha fact a Flowbee `test` job produces. The merge
// gate's ci_green@head is satisfied by EITHER reconcile-from-Actions
// (domain_b_facts.ci_green, written by reconcile-IN) OR a green row recorded here
// by a passing `test` job, bound to the same head sha. These store methods are the
// producer (RecordTestJobCI) and the reader (TestCIFacts) of that second source;
// the pure fold lives in job.CIGreenAtHead and is applied in ReviewResult.

// RecordTestJobCI records the result of a Flowbee `test` job against the BUILD job
// it tested. headSHA binds the fact (a green at a stale head can never satisfy a
// moved head — the I-5 supersession guard, mirroring the verdict's SHA binding).
// green=true is the ci_green@sha fact the gate honors; green=false records a red
// test run (the gate then has no green from this provenance). testJobID is the id
// of the `test` job that produced the fact (lineage/audit). Idempotent upsert per
// (build job, head_sha). It appends a ledger event onto the BUILD job for replay.
func (s *Store) RecordTestJobCI(ctx context.Context, buildJobID, headSHA, testJobID string, green bool, now time.Time) error {
	if headSHA == "" {
		return errors.New("RecordTestJobCI: empty head_sha (a ci_green fact must bind to a head)")
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		j, seq, err := loadJobTx(ctx, tx, buildJobID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO test_ci_facts (job_id, head_sha, green, provenance, test_job_id, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (job_id, head_sha) DO UPDATE SET
			    green = excluded.green, provenance = excluded.provenance,
			    test_job_id = excluded.test_job_id, updated_at = excluded.updated_at`,
			buildJobID, headSHA, b2i(green), string(job.CIProvFlowbeeTest), testJobID,
			now.Format(rfc3339)); err != nil {
			return err
		}
		nextSeq := seq + 1
		ev := ledger.Event{
			JobID: buildJobID, JobSeq: nextSeq, Kind: ledger.KindTestCIRecorded,
			FromState: j.State, ToState: j.State, LeaseEpoch: j.LeaseEpoch,
			Actor: "system", CreatedAt: now,
			Payload: ledger.Payload{HeadSHA: headSHA, CIGreen: green, TestJobID: testJobID},
		}
		if err := appendEvent(ctx, tx, ev); err != nil {
			return err
		}
		return setJobSeq(ctx, tx, buildJobID, nextSeq)
	})
}

// TestCIFacts returns the Flowbee `test`-job CI facts recorded for a build job (all
// heads). The gate folds these (via job.CIGreenAtHead) with the reconciled Actions
// fact so ci_green@head holds if EITHER producer is green at the reconciled head.
func (s *Store) TestCIFacts(ctx context.Context, buildJobID string) ([]job.CIFact, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT head_sha, green, provenance FROM test_ci_facts WHERE job_id = ?`, buildJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []job.CIFact
	for rows.Next() {
		var f job.CIFact
		var green int
		var prov string
		if err := rows.Scan(&f.HeadSHA, &green, &prov); err != nil {
			return nil, err
		}
		f.Green = green == 1
		f.Provenance = job.CIProvenance(prov)
		out = append(out, f)
	}
	return out, rows.Err()
}

// testCIFactsTx is TestCIFacts inside a transaction (the gate reads it while the
// review result is applied so the fold sees a consistent snapshot).
func testCIFactsTx(ctx context.Context, tx *sql.Tx, buildJobID string) ([]job.CIFact, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT head_sha, green, provenance FROM test_ci_facts WHERE job_id = ?`, buildJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []job.CIFact
	for rows.Next() {
		var f job.CIFact
		var green int
		var prov string
		if err := rows.Scan(&f.HeadSHA, &green, &prov); err != nil {
			return nil, err
		}
		f.Green = green == 1
		f.Provenance = job.CIProvenance(prov)
		out = append(out, f)
	}
	return out, rows.Err()
}
