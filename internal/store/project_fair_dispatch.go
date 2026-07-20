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

var errProjectAllocationHeld = errors.New("project allocation held")

// projectAllocationQueryer is implemented by both *sql.DB and *sql.Tx. Keeping
// the allocation fold behind one query is a correctness requirement: the fair
// snapshot, the final job-claim fence, and the v2 builder-launch fence must all
// count the same physical/service resources.
type projectAllocationQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// activeProjectResourcesSQL counts each physical v2 builder residency and each
// active job exactly once. A v2 builder has acquired compute once it has an
// exact seat and remains in an active epic state; a job occupies service when
// at least one unended lease exists. Resource-kind prefixes prevent an epic and
// a job which happen to share an id from collapsing into one allocation.
//
// SELECTing jobs through EXISTS also makes the count defensive against a corrupt
// duplicate active-lease row without silently granting extra project capacity.
const activeProjectResourcesSQL = `
	SELECT e.project_id,'epic:' || e.id AS resource_key
	  FROM epics e
	 WHERE e.seat_id<>'' AND e.state IN ` + epicActiveStatesSQL + `
	UNION
	SELECT j.project_id,'job:' || j.id AS resource_key
	  FROM jobs j
	 WHERE EXISTS (SELECT 1 FROM leases l WHERE l.job_id=j.id AND l.ended_at IS NULL)`

func loadProjectActiveAllocations(ctx context.Context, q projectAllocationQueryer) (map[string]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT project_id,COUNT(*) FROM (`+activeProjectResourcesSQL+`)
		GROUP BY project_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var projectID string
		var active int
		if err := rows.Scan(&projectID, &active); err != nil {
			return nil, err
		}
		out[projectID] = active
	}
	return out, rows.Err()
}

func projectActiveAllocation(ctx context.Context, q projectAllocationQueryer, projectID string) (int, error) {
	var active int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM (`+activeProjectResourcesSQL+`)
		WHERE project_id=?`, projectID).Scan(&active)
	return active, err
}

// LoadProjectFairSnapshot rebuilds the scheduling turn from durable state. A
// process restart therefore resumes with the same deficits and last-service
// clocks instead of giving a noisy project fresh credit.
func (s *Store) LoadProjectFairSnapshot(ctx context.Context, pool string) (ProjectFairSnapshot, error) {
	return loadProjectFairSnapshot(ctx, s.DB, pool)
}

func loadProjectFairSnapshot(ctx context.Context, q projectAllocationQueryer, pool string) (ProjectFairSnapshot, error) {
	out := ProjectFairSnapshot{Active: map[string]int{}, FairState: scheduler.FairState{
		DeficitByPool:    map[string]map[string]int64{pool: {}},
		LastServedByPool: map[string]map[string]time.Time{pool: {}},
	}}
	rows, err := q.QueryContext(ctx, `SELECT id,state,scheduler_weight,concurrency_cap FROM projects`)
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

	rows, err = q.QueryContext(ctx, `SELECT project_id,deficit,last_served_at FROM project_scheduler_state WHERE pool=?`, pool)
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

	// Read physical builder residency plus the lease ledger, never the occupancy
	// cache, for the correctness decision. This is the same fold repeated by the
	// final transactional claim/launch fence below.
	out.Active, err = loadProjectActiveAllocations(ctx, q)
	return out, err
}

// projectAllocationGateTx is the common final policy/cap fence for every
// project-owned compute/service acquisition. It must run in the transaction
// which binds the resource so two concurrent claims cannot consume the last
// project slot twice.
func projectAllocationGateTx(ctx context.Context, tx *sql.Tx, projectID, repoID string) error {
	var state string
	var cap int
	if err := tx.QueryRowContext(ctx, `SELECT state,concurrency_cap FROM projects WHERE id=?`, projectID).
		Scan(&state, &cap); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: project %q is missing", errProjectAllocationHeld, projectID)
		}
		return err
	}
	if state != "active" {
		return fmt.Errorf("%w: project %q is %s", errProjectAllocationHeld, projectID, state)
	}
	var breakerOpen int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM project_circuit_breakers
		WHERE project_id=? AND state<>'closed' AND (repo_id='' OR repo_id=?))`, projectID, repoID).
		Scan(&breakerOpen); err != nil {
		return err
	}
	if breakerOpen == 1 {
		return fmt.Errorf("%w: project/repository breaker is open", errProjectAllocationHeld)
	}
	if cap <= 0 {
		return nil
	}
	active, err := projectActiveAllocation(ctx, tx, projectID)
	if err != nil {
		return err
	}
	if active >= cap {
		return fmt.Errorf("%w: project %q has %d active resources at cap %d",
			errProjectAllocationHeld, projectID, active, cap)
	}
	return nil
}

// projectConcurrencyGateTx is the claim-time correctness backstop. Candidate
// selection is advisory; this serialized transaction prevents a paused project
// or concurrent final slot from being leased even under a stale HTTP snapshot.
func projectConcurrencyGateTx(ctx context.Context, tx *sql.Tx, jobID string) error {
	var projectID, repoID string
	if err := tx.QueryRowContext(ctx, `SELECT project_id,repo FROM jobs WHERE id=?`, jobID).
		Scan(&projectID, &repoID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return lease.ErrLostRace
		}
		return err
	}
	if err := projectAllocationGateTx(ctx, tx, projectID, repoID); err != nil {
		if errors.Is(err, errProjectAllocationHeld) {
			return lease.ErrLostRace
		}
		return err
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
	if err := commitProjectFairStateTx(ctx, tx, claim.Pool, claim.NextState, claim.Now); err != nil {
		return err
	}
	decisions, err := json.Marshal(claim.Decisions)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_scheduler_turns(lease_id,pool,project_id,job_id,forced_by_age,decisions_json,created_at) VALUES(?,?,?,?,?,?,?)`,
		leaseID, claim.Pool, claim.ProjectID, claim.JobID, boolInt(claim.ForcedByAge), string(decisions), claim.Now.UTC().Format(rfc3339))
	return err
}

func commitProjectFairStateTx(ctx context.Context, tx *sql.Tx, pool string,
	state scheduler.FairState, now time.Time) error {
	deficits := state.DeficitByPool[pool]
	served := state.LastServedByPool[pool]
	for id, deficit := range deficits {
		servedText := ""
		if !served[id].IsZero() {
			servedText = served[id].UTC().Format(rfc3339)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_scheduler_state(pool,project_id,deficit,last_served_at,state_version,updated_at)
			VALUES(?,?,?,?,1,?) ON CONFLICT(pool,project_id) DO UPDATE SET deficit=excluded.deficit,last_served_at=excluded.last_served_at,state_version=project_scheduler_state.state_version+1,updated_at=excluded.updated_at`,
			pool, id, deficit, servedText, now.UTC().Format(rfc3339)); err != nil {
			return err
		}
	}
	return nil
}

type projectFairEffect struct {
	Pool, ProjectID, ResourceKind, ResourceID string
	EffectKind, EffectID                      string
	EffectEpoch                               int64
	ForcedByAge                               bool
	NextState                                 scheduler.FairState
	Decisions                                 []scheduler.CandidateDecision
	Now                                       time.Time
}

func commitProjectFairEffectTx(ctx context.Context, tx *sql.Tx, effect projectFairEffect) error {
	if effect.Pool == "" || effect.ProjectID == "" || effect.ResourceKind == "" ||
		effect.ResourceID == "" || effect.EffectKind == "" || effect.EffectID == "" || effect.EffectEpoch < 0 {
		return errors.New("invalid project fair effect binding")
	}
	if err := commitProjectFairStateTx(ctx, tx, effect.Pool, effect.NextState, effect.Now); err != nil {
		return err
	}
	decisions, err := json.Marshal(effect.Decisions)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO project_scheduler_effects
		(pool,project_id,resource_kind,resource_id,effect_kind,effect_id,effect_epoch,
		 forced_by_age,decisions_json,created_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		effect.Pool, effect.ProjectID, effect.ResourceKind, effect.ResourceID,
		effect.EffectKind, effect.EffectID, effect.EffectEpoch, boolInt(effect.ForcedByAge),
		string(decisions), effect.Now.UTC().Format(rfc3339))
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
