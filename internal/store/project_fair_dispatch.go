package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

// ProjectFairSnapshot is the complete durable input for one capability-pool
// scheduling turn. Occupancy is global across pools because a project's cap is
// a control-plane concurrency limit, not a per-role allowance.
type ProjectFairSnapshot struct {
	Policies  []scheduler.ProjectPolicy
	Active    map[string]int
	FairState scheduler.FairState
}

// ProjectFairClaim carries the pure scheduler result into the atomic lease
// claim. It is committed only if that exact job wins the claim fence.
type ProjectFairClaim struct {
	Pool        string
	ProjectID   string
	JobID       string
	ForcedByAge bool
	NextState   scheduler.FairState
	Decisions   []scheduler.CandidateDecision
	Now         time.Time
}

// LoadProjectFairSnapshot rebuilds the scheduling turn from durable state. A
// process restart therefore resumes with the same deficits and last-service
// clocks instead of giving a noisy project fresh credit.
func (s *Store) LoadProjectFairSnapshot(ctx context.Context, pool string) (ProjectFairSnapshot, error) {
	out := ProjectFairSnapshot{Active: map[string]int{}, FairState: scheduler.FairState{
		DeficitByPool:    map[string]map[string]int64{pool: {}},
		LastServedByPool: map[string]map[string]time.Time{pool: {}},
	}}
	rows, err := s.DB.QueryContext(ctx, `SELECT id,state,scheduler_weight,concurrency_cap FROM projects`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var p scheduler.ProjectPolicy
		if err := rows.Scan(&p.ProjectID, &p.State, &p.Weight, &p.ConcurrencyCap); err != nil {
			rows.Close()
			return out, err
		}
		out.Policies = append(out.Policies, p)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}

	rows, err = s.DB.QueryContext(ctx, `SELECT project_id,deficit,last_served_at FROM project_scheduler_state WHERE pool=?`, pool)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var projectID, served string
		var deficit int64
		if err := rows.Scan(&projectID, &deficit, &served); err != nil {
			rows.Close()
			return out, err
		}
		out.FairState.DeficitByPool[pool][projectID] = deficit
		if served != "" {
			if ts, parseErr := time.Parse(rfc3339, served); parseErr == nil {
				out.FairState.LastServedByPool[pool][projectID] = ts
			}
		}
	}
	if err := rows.Close(); err != nil {
		return out, err
	}

	// Read the lease ledger, not the cache, for the correctness decision. The
	// occupancy table is an operator projection and is rebuilt by triggers.
	rows, err = s.DB.QueryContext(ctx, `SELECT j.project_id,COUNT(*) FROM leases l JOIN jobs j ON j.id=l.job_id WHERE l.ended_at IS NULL GROUP BY j.project_id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var projectID string
		var active int
		if err := rows.Scan(&projectID, &active); err != nil {
			return out, err
		}
		out.Active[projectID] = active
	}
	return out, rows.Err()
}

// projectConcurrencyGateTx is the claim-time correctness backstop. Candidate
// selection is advisory; this serialized transaction prevents a paused project
// or concurrent final slot from being leased even under a stale HTTP snapshot.
func projectConcurrencyGateTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	var projectID, repoID, state string
	var cap int
	if err := tx.QueryRowContext(ctx, `SELECT j.project_id,j.repo,p.state,p.concurrency_cap FROM jobs j JOIN projects p ON p.id=j.project_id WHERE j.id=?`, jobID).Scan(&projectID, &repoID, &state, &cap); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lease.ErrLostRace
		}
		return err
	}
	if state != "active" {
		return lease.ErrLostRace
	}
	var breakerOpen int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_circuit_breakers
		WHERE project_id=? AND state<>'closed' AND (repo_id='' OR repo_id=?))`, projectID, repoID).Scan(&breakerOpen); err != nil {
		return err
	}
	if breakerOpen == 1 {
		return lease.ErrLostRace
	}
	if cap <= 0 {
		return nil
	}
	var active int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM leases l JOIN jobs j ON j.id=l.job_id WHERE l.ended_at IS NULL AND j.project_id=?`, projectID).Scan(&active); err != nil {
		return err
	}
	if active >= cap {
		return lease.ErrLostRace
	}
	return nil
}

// commitProjectFairClaimTx makes scheduler accounting part of the lease grant's
// transaction. A failed/lost claim advances no credit; a committed lease can
// never exist without its service turn, including across process crashes.
func commitProjectFairClaimTx(ctx context.Context, tx *sql.Tx, leaseID string, claim *ProjectFairClaim) error {
	if claim == nil {
		return nil
	}
	var projectID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id FROM jobs WHERE id=?`, claim.JobID).Scan(&projectID); err != nil {
		return err
	}
	if claim.Pool == "" || claim.JobID == "" || claim.ProjectID != projectID {
		return fmt.Errorf("invalid project fair claim binding")
	}
	deficits := claim.NextState.DeficitByPool[claim.Pool]
	served := claim.NextState.LastServedByPool[claim.Pool]
	for id, deficit := range deficits {
		servedText := ""
		if !served[id].IsZero() {
			servedText = served[id].UTC().Format(rfc3339)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_scheduler_state(pool,project_id,deficit,last_served_at,state_version,updated_at)
			VALUES(?,?,?,?,1,?) ON CONFLICT(pool,project_id) DO UPDATE SET deficit=excluded.deficit,last_served_at=excluded.last_served_at,state_version=project_scheduler_state.state_version+1,updated_at=excluded.updated_at`,
			claim.Pool, id, deficit, servedText, claim.Now.UTC().Format(rfc3339)); err != nil {
			return err
		}
	}
	decisions, err := json.Marshal(claim.Decisions)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_scheduler_turns(lease_id,pool,project_id,job_id,forced_by_age,decisions_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		leaseID, claim.Pool, claim.ProjectID, claim.JobID, boolInt(claim.ForcedByAge), string(decisions), claim.Now.UTC().Format(rfc3339))
	return err
}

// LastProjectSchedulerTurn exposes the durable why/why-not shadow used by API
// diagnostics and tests.
func (s *Store) LastProjectSchedulerTurn(ctx context.Context, pool string) (ProjectFairClaim, error) {
	var out ProjectFairClaim
	var forced int
	var decisions, created string
	err := s.DB.QueryRowContext(ctx, `SELECT pool,project_id,job_id,forced_by_age,decisions_json,created_at FROM project_scheduler_turns WHERE pool=? ORDER BY seq DESC LIMIT 1`, pool).
		Scan(&out.Pool, &out.ProjectID, &out.JobID, &forced, &decisions, &created)
	if err != nil {
		return out, err
	}
	out.ForcedByAge = forced == 1
	if err := json.Unmarshal([]byte(decisions), &out.Decisions); err != nil {
		return out, err
	}
	out.Now, _ = time.Parse(rfc3339, created)
	return out, nil
}
