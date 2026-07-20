package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/scheduler"
)

type fairBuilderScheduleResult struct {
	Scheduled    bool
	Kind         string
	CapacityHeld int
}

type fairBuilderCandidate struct {
	Candidate scheduler.Candidate
	Kind      string
	ProjectID string
	EpicID    string
	Repo      string
	Branch    string
	Slug      string
	Title     string
	SpecPath  string
	Version   int
	ActionID  string
	SeatID    string
	Seat      builderLaunchCandidate
}

// scheduleOneFairBuilderCompute is the authoritative v2 builder dispatch turn.
// Eligibility, policy/breaker/cap checks, physical capacity acquisition,
// immutable action binding, fair-state debit, and the generic effect ledger all
// share one serialized SQLite transaction. A rollback therefore leaves neither
// a compute allocation nor a phantom service turn.
func (s *Store) scheduleOneFairBuilderCompute(ctx context.Context, now time.Time,
	freshFor time.Duration, provider string) (fairBuilderScheduleResult, error) {
	var out fairBuilderScheduleResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		snapshot, err := loadProjectFairSnapshot(ctx, tx, scheduler.PoolBuild)
		if err != nil {
			return err
		}
		items, held, err := s.fairBuilderCandidatesTx(ctx, tx, now, freshFor, provider)
		if err != nil {
			return err
		}
		out.CapacityHeld = held
		if len(items) == 0 {
			return nil
		}
		candidates := make([]scheduler.Candidate, 0, len(items))
		byID := make(map[string]fairBuilderCandidate, len(items))
		for _, item := range items {
			candidates = append(candidates, item.Candidate)
			byID[item.EpicID] = item
		}
		turn := scheduler.PickProjectFair(candidates, snapshot.Policies, snapshot.Active,
			snapshot.FairState, scheduler.FairConfig{Pool: scheduler.PoolBuild, Now: now,
				StarvationBound: ProjectStarvationBound})
		if !turn.OK {
			return nil
		}
		selected, ok := byID[turn.Selected.JobID]
		if !ok || selected.ProjectID != turn.WinningProject {
			return errors.New("fair builder turn selected an unknown resource")
		}
		var effectID string
		switch selected.Kind {
		case "builder_launch":
			effectID, err = commitFairBuilderLaunchTx(ctx, s, tx, selected, now)
		case "builder_rework":
			effectID, err = commitFairBuilderReworkTx(ctx, tx, selected, now)
		default:
			err = fmt.Errorf("unsupported fair builder resource %s", selected.Kind)
		}
		if err != nil {
			return err
		}
		if err := commitProjectFairEffectTx(ctx, tx, projectFairEffect{
			Pool: scheduler.PoolBuild, ProjectID: selected.ProjectID,
			ResourceKind: "epic_builder", ResourceID: selected.EpicID,
			EffectKind: selected.Kind, EffectID: effectID, EffectEpoch: 0,
			ForcedByAge: turn.ForcedByAge, NextState: turn.NextState,
			Decisions: turn.Decisions, Now: now,
		}); err != nil {
			return err
		}
		out.Scheduled, out.Kind = true, selected.Kind
		return nil
	})
	return out, err
}

func (s *Store) fairBuilderCandidatesTx(ctx context.Context, tx *sql.Tx, now time.Time,
	freshFor time.Duration, provider string) ([]fairBuilderCandidate, int, error) {
	if freshFor <= 0 {
		freshFor = 5 * time.Minute
	}
	if provider == "" {
		provider = "codex"
	}
	var out []fairBuilderCandidate
	held := 0

	rows, err := tx.QueryContext(ctx, `SELECT e.project_id,e.id,e.repo,e.branch,e.slug,e.title,e.file_path,
		d.state_version,d.created_at,d.hold_kind
		FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id
		WHERE d.state='admitted' AND d.compute_lease_action_id='' AND e.seat_id=''
		  AND (d.hold_kind='' OR d.hold_kind='builder_capacity_unavailable')
		  AND NOT EXISTS (SELECT 1 FROM epic_actions a WHERE a.epic_id=e.id
		    AND a.kind IN ('builder_launch','builder_launch_contract')
		    AND a.state<>'cancelled_superseded')
		ORDER BY d.created_at,e.id`)
	if err != nil {
		return nil, 0, err
	}
	type freshRow struct {
		projectID, epicID, repo, branch, slug, title, specPath, created, hold string
		version                                                               int
	}
	var fresh []freshRow
	for rows.Next() {
		var item freshRow
		if err := rows.Scan(&item.projectID, &item.epicID, &item.repo, &item.branch,
			&item.slug, &item.title, &item.specPath, &item.version, &item.created,
			&item.hold); err != nil {
			rows.Close()
			return nil, 0, err
		}
		fresh = append(fresh, item)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	for _, item := range fresh {
		if err := projectAllocationGateTx(ctx, tx, item.projectID, item.repo); err != nil {
			if errors.Is(err, errProjectAllocationHeld) {
				continue
			}
			return nil, held, err
		}
		seats, err := builderLaunchCandidatesTx(ctx, tx, item.projectID, provider, s.EnableCapacityV2)
		if err != nil {
			return nil, held, err
		}
		var chosen *builderLaunchCandidate
		var reasons []string
		for i := range seats {
			decision, err := capacityRouteForSeatQuery(ctx, tx, seats[i].seatID, now, freshFor)
			if err != nil {
				return nil, held, err
			}
			if decision.Routable {
				chosen = &seats[i]
				break
			}
			reasons = append(reasons, seats[i].seatID+":"+strings.Join(decision.Reasons, ","))
		}
		if chosen == nil {
			if item.hold != "builder_capacity_unavailable" {
				detail := "no exact routable " + provider + " builder seat"
				if len(reasons) > 0 {
					detail += ": " + strings.Join(reasons, ";")
				}
				var report BuilderLaunchReconcileResult
				if err := holdBuilderLaunchCapacityTx(ctx, tx, item.projectID, item.epicID,
					detail, item.version, now, &report); err != nil {
					return nil, held, err
				}
				held += report.CapacityHeld
			}
			continue
		}
		enqueued, _ := time.Parse(rfc3339, item.created)
		out = append(out, fairBuilderCandidate{Candidate: scheduler.Candidate{
			ProjectID: item.projectID, JobID: item.epicID, Pool: scheduler.PoolBuild,
			Priority: 5, EnqueuedAt: enqueued,
		}, Kind: "builder_launch", ProjectID: item.projectID, EpicID: item.epicID,
			Repo: item.repo, Branch: item.branch, Slug: item.slug, Title: item.title,
			SpecPath: item.specPath, Version: item.version, Seat: *chosen})
	}

	rows, err = tx.QueryContext(ctx, `SELECT e.project_id,e.id,e.repo,e.seat_id,d.state_version,
		d.state_entered_at,d.hold_kind,a.id
		FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id
		JOIN epic_actions a ON a.epic_id=e.id AND a.kind='builder_rework'
		WHERE d.state='changes_requested' AND d.builder_affinity_state='relaunching'
		  AND d.compute_lease_action_id='' AND e.state IN ('done','achieved')
		  AND a.state='pending' AND a.action_epoch=0
		ORDER BY d.state_entered_at,e.id`)
	if err != nil {
		return nil, held, err
	}
	type reworkRow struct {
		projectID, epicID, repo, seatID, entered, hold, actionID string
		version                                                  int
	}
	var reworks []reworkRow
	for rows.Next() {
		var item reworkRow
		if err := rows.Scan(&item.projectID, &item.epicID, &item.repo, &item.seatID,
			&item.version, &item.entered, &item.hold, &item.actionID); err != nil {
			rows.Close()
			return nil, held, err
		}
		reworks = append(reworks, item)
	}
	if err := rows.Close(); err != nil {
		return nil, held, err
	}
	for _, item := range reworks {
		if err := projectAllocationGateTx(ctx, tx, item.projectID, item.repo); err != nil {
			if errors.Is(err, errProjectAllocationHeld) {
				continue
			}
			return nil, held, err
		}
		if item.seatID == "" {
			if item.hold != "builder_capacity_unavailable" {
				if err := markBuilderCapacityHoldTx(ctx, tx, item.projectID, item.epicID,
					item.seatID, "builder seat binding missing", item.version, now); err != nil {
					return nil, held, err
				}
				held++
			}
			continue
		}
		decision, err := capacityRouteForSeatQuery(ctx, tx, item.seatID, now, freshFor)
		if err != nil {
			return nil, held, err
		}
		if !decision.Routable {
			if item.hold != "builder_capacity_unavailable" {
				if err := markBuilderCapacityHoldTx(ctx, tx, item.projectID, item.epicID,
					item.seatID, strings.Join(decision.Reasons, ","), item.version, now); err != nil {
					return nil, held, err
				}
				held++
			}
			continue
		}
		enqueued, _ := time.Parse(rfc3339, item.entered)
		out = append(out, fairBuilderCandidate{Candidate: scheduler.Candidate{
			ProjectID: item.projectID, JobID: item.epicID, Pool: scheduler.PoolBuild,
			Priority: 1, EnqueuedAt: enqueued, ReleasesCapacity: true,
		}, Kind: "builder_rework", ProjectID: item.projectID, EpicID: item.epicID,
			Repo: item.repo, Version: item.version, ActionID: item.actionID, SeatID: item.seatID})
	}
	return out, held, nil
}

func commitFairBuilderLaunchTx(ctx context.Context, s *Store, tx *sql.Tx, item fairBuilderCandidate,
	now time.Time) (string, error) {
	dedicatedWorkers, err := dedicatedEpicWorkersEnabledTx(ctx, s, tx)
	if err != nil {
		return "", err
	}
	if dedicatedWorkers {
		if err := validateExactlyTwoEpicWorkerPlansTx(ctx, tx, item.ProjectID, item.EpicID,
			item.Seat.family); err != nil {
			return "", fmt.Errorf("builder launch dedicated-worker invariant: %w", err)
		}
		if item.Seat.profileID != epicWorkerProfileID(item.Seat.family, "builder") {
			return "", fmt.Errorf("builder lifecycle profile %q does not match admitted family %q",
				item.Seat.profileID, item.Seat.family)
		}
	}
	payload, _ := json.Marshal(map[string]any{
		"type": "builder_launch", "project_id": item.ProjectID, "epic_id": item.EpicID,
		"repo": item.Repo, "branch": item.Branch, "slug": item.Slug, "title": item.Title,
		"spec_path": item.SpecPath,
	})
	if dedicatedWorkers {
		if err := validateEpicWorkerLifecycleBootstrapSizeTx(ctx, tx, item.EpicID, "builder", string(payload)); err != nil {
			return "", err
		}
	}
	dedup := "builder_launch:" + item.ProjectID + ":" + item.EpicID + ":" + item.Seat.storeID
	idHash := sha256.Sum256([]byte(dedup))
	actionID := "builder-launch-" + hex.EncodeToString(idHash[:12])
	payloadHash := sha256.Sum256(payload)
	workspacePath := path.Join(item.Seat.workspaceBase, item.ProjectID, item.EpicID, "builder")
	baseSHA := ""
	if dedicatedWorkers {
		if err := tx.QueryRowContext(ctx, `SELECT json_extract(bootstrap_payload,'$.source_commit_sha')
			FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role='builder'`,
			item.EpicID, item.ProjectID).Scan(&baseSHA); err != nil {
			return "", fmt.Errorf("builder immutable source commit: %w", err)
		}
		if !validGitObjectID(baseSHA) {
			return "", errors.New("builder immutable source commit is absent or invalid")
		}
	}
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epics SET host=?,seat_id=?,account_key=?,
		builder_model_family=?,agent=?,tmux_name=?,updated_at=? WHERE id=? AND seat_id=''
		AND state='launching'`, item.Seat.hostID, item.Seat.seatID, item.Seat.accountKey,
		item.Seat.family, item.Seat.family, EpicWorkerDisplayName(item.ProjectID, item.Seat.family, item.Slug),
		stamp, item.EpicID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", errors.New("fair builder launch lost atomic compute lease race")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_domain_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
		 base_sha,next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'builder_launch','pending',0,?,?,?,'driver_lifecycle','builder',?,?,?,?,?,1,?,?,?,?,1,?,?,?,?)`,
		actionID, item.ProjectID, item.EpicID, dedup, string(payload),
		"sha256:"+hex.EncodeToString(payloadHash[:]), item.Seat.hostID, item.Seat.storeID,
		item.Seat.serverDomainID, item.Seat.serverID,
		EpicWorkerLifecycleKey(item.EpicID, "builder"), item.Seat.profileID,
		item.Seat.workspaceRootID, workspacePath, "builder-compute:"+item.EpicID, baseSHA,
		stamp, stamp, stamp); err != nil {
		return "", err
	}
	if dedicatedWorkers {
		// Once this boundary is authoritative, absence of the credential row is
		// corruption, never evidence that this is a legacy launch. Validation above
		// proves it exists; preserve any later sql.ErrNoRows/concurrent delete as a
		// hard transaction failure.
		if _, err := s.issueEpicWorkerCredentialTx(ctx, tx, item.EpicID, "builder", actionID, now); err != nil {
			return "", err
		}
	}
	res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET builder_model_family=?,
		builder_affinity_state='pending',compute_lease_action_id=?,compute_lease_action_epoch=0,
		hold_kind='',hold_reason='',return_state='',last_error='',alert_pending=0,
		state_version=state_version+1,state_due_at=?,updated_at=?
		WHERE epic_id=? AND state='admitted' AND state_version=?`, item.Seat.family, actionID,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, item.EpicID, item.Version)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", errors.New("fair builder launch delivery changed during compute lease")
	}
	res, err = tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='ensure_pending',
		model_family=model_family,seat_id=?,ensure_action_id=?,target_epoch=1,updated_at=?
		WHERE epic_id=? AND worker_role='builder' AND model_family=? AND state='planned'`,
		item.Seat.seatID, actionID, stamp, item.EpicID, item.Seat.family)
	if err != nil {
		return "", err
	}
	if dedicatedWorkers {
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return "", err
			}
			return "", fmt.Errorf("builder launch lost exact dedicated worker plan fence: updated %d rows", n)
		}
	}
	if err := appendEpicControlEventTx(ctx, tx, item.ProjectID, item.EpicID,
		"builder_launch_committed", "admitted", "admitted", item.Version+1,
		"scheduler", string(payload), now); err != nil {
		return "", err
	}
	return actionID, nil
}

func commitFairBuilderReworkTx(ctx context.Context, tx *sql.Tx, item fairBuilderCandidate,
	now time.Time) (string, error) {
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epics SET state='launching',finished_at='',
		launched_at=?,updated_at=? WHERE id=? AND seat_id=? AND state IN ('done','achieved')
		AND EXISTS (SELECT 1 FROM epic_actions a WHERE a.id=? AND a.epic_id=epics.id
		  AND a.kind='builder_rework' AND a.state='pending' AND a.action_epoch=0)`,
		stamp, stamp, item.EpicID, item.SeatID, item.ActionID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", errors.New("fair builder rework lost atomic compute lease race")
	}
	res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET hold_kind='',hold_reason='',
		return_state='',last_error='',alert_pending=0,compute_lease_action_id=?,
		compute_lease_action_epoch=0,state_version=state_version+1,updated_at=?
		WHERE epic_id=? AND state='changes_requested' AND builder_affinity_state='relaunching'
		AND state_version=?`, item.ActionID, stamp, item.EpicID, item.Version)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return "", errors.New("fair builder rework delivery changed during compute lease")
	}
	dedup := builderCapacityDedup(item.EpicID, item.SeatID)
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
		resolution='capacity_reacquired',resolved_at=?,updated_at=? WHERE dedup_key=?
		AND project_id=? AND state IN ('open','leased','delivering','awaiting_ack')`,
		stamp, stamp, dedup, item.ProjectID); err != nil {
		return "", err
	}
	payload, _ := json.Marshal(map[string]string{"seat_id": item.SeatID, "action_id": item.ActionID})
	if err := appendEpicControlEventTx(ctx, tx, item.ProjectID, item.EpicID,
		"builder_capacity_acquired", "changes_requested", "changes_requested", item.Version+1,
		"scheduler", string(payload), now); err != nil {
		return "", err
	}
	return item.ActionID, nil
}
