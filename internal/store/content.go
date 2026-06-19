package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/samhotchkiss/flowbee/internal/content"
)

// contentResultTx computes the M9 content-integrity Result (§9.2, I-11) for a job
// from the stored patch + declared blast-radius. It is the runtime side of the
// gate: it gathers the untrusted bytes and runs the deterministic, non-LLM checks,
// returning a Result the PURE engine consumes (EngineState.Content). A job with no
// stored patch (e.g. an empty-diff build in a unit test) yields a clean Result over
// an empty diff — denylist-clear, blast-radius-consistent (nothing touched), and
// static-checks-green. The persisted content_result column caches the last run for
// audit.
//
// applies-clean@base is the one check that would need the git fixture; the build
// harness pushes to the epoch ref and the result already required a real push, so
// at this seam we treat the apply fact as UNKNOWN (not failed) unless a caller has
// recorded a definite negative — keeping the gate a pure function of stored inputs.
func (s *Store) contentResultTx(ctx context.Context, tx *sql.Tx, jobID string) (content.Result, error) {
	var diff, declared, repo string
	err := tx.QueryRowContext(ctx,
		`SELECT patch_diff, declared_blast_radius, COALESCE(repo,'') FROM jobs WHERE id = ?`, jobID).
		Scan(&diff, &declared, &repo)
	if err != nil {
		return content.Result{}, err
	}
	pol := s.ContentPolicy
	// a managed repo that is NOT the Flowbee control plane has the flowbee_source class
	// relaxed (its internal//cmd/ are its OWN code). Default (empty map) protects all.
	if s.AllowOwnSourceRepos[repo] {
		pol.AllowOwnSource = true
	}
	return computeContent(diff, declared, pol), nil
}

// computeContent runs the content-integrity gate over the stored diff + declared
// blast-radius under the operator-configured content Policy (F2). PURE. A blank
// declared blast-radius decodes to an empty BlastRadius (covers nothing) — so any
// touched path is undeclared (the safe default; a worker must declare what it
// touches). The zero Policy is exactly the shipped defaults.
func computeContent(diff, declared string, pol content.Policy) content.Result {
	var br content.BlastRadius
	if declared != "" {
		_ = json.Unmarshal([]byte(declared), &br)
	}
	return content.CheckWithPolicy(content.Patch{
		Diff:     diff,
		Declared: br,
	}, pol)
}

// persistContentResultTx caches the computed Result on the job row (audit + the
// §5.4 predicate read), best-effort within the caller's tx.
func persistContentResultTx(ctx context.Context, tx *sql.Tx, jobID string, r content.Result) error {
	blob, _ := json.Marshal(r)
	_, err := tx.ExecContext(ctx,
		`UPDATE jobs SET content_result = ? WHERE id = ?`, string(blob), jobID)
	return err
}
