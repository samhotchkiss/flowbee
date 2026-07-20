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
)

// BuilderDriverTarget is the explicit inventory join between an authenticated
// capacity seat and the Driver instance/profile/workspace allowed to host it.
// The current store_id is intentionally resolved through instance_ref when an
// action is committed; a store reset therefore fences already-committed work.
type BuilderDriverTarget struct {
	ProjectID, SeatID, InstanceRef, TmuxServerDomainID, TmuxServerInstanceID string
	ProfileID, WorkspaceRootID, WorkspaceRelativeBase                        string
	Enabled                                                                  bool
}

func (s *Store) UpsertBuilderDriverTarget(ctx context.Context, in BuilderDriverTarget, now time.Time) error {
	if in.ProjectID == "" {
		in.ProjectID = "default"
	}
	for name, value := range map[string]string{
		"project_id": in.ProjectID, "seat_id": in.SeatID, "instance_ref": in.InstanceRef,
		"tmux_server_domain_id":   in.TmuxServerDomainID,
		"tmux_server_instance_id": in.TmuxServerInstanceID, "profile_id": in.ProfileID,
		"workspace_root_id": in.WorkspaceRootID, "workspace_relative_base": in.WorkspaceRelativeBase,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("builder Driver target requires %s", name)
		}
	}
	clean := path.Clean(in.WorkspaceRelativeBase)
	if clean != in.WorkspaceRelativeBase || clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return errors.New("builder Driver target workspace_relative_base must be a clean relative path")
	}
	var seatBox, expectedHost, driverHost, driverDomain, driverOwnership string
	if err := s.DB.QueryRowContext(ctx, `SELECT box,expected_host_id FROM seats WHERE id=?`, in.SeatID).
		Scan(&seatBox, &expectedHost); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSeatNotFound
		}
		return err
	}
	if err := s.DB.QueryRowContext(ctx, `SELECT host_id,tmux_server_domain_id,tmux_server_ownership
		FROM driver_instances WHERE instance_ref=?`, in.InstanceRef).Scan(&driverHost, &driverDomain, &driverOwnership); err != nil {
		return fmt.Errorf("builder Driver target instance: %w", err)
	}
	// v2 capacity enrollment carries the stable authenticated host_id. seat.box
	// is only an execution/SSH locator and is empty for a local seat; treating the
	// empty locator as a Driver identity made a local v2 seat impossible to bind.
	// Preserve the legacy fallback only for pre-v2 seats that have not enrolled a
	// capacity host yet.
	seatHost := expectedHost
	if seatHost == "" {
		seatHost = seatBox
	}
	if seatHost != driverHost {
		return fmt.Errorf("builder Driver target host %q does not match seat capacity host %q", driverHost, seatHost)
	}
	if driverDomain == "" || in.TmuxServerDomainID != driverDomain || driverOwnership != "managed_dedicated" {
		return errors.New("builder Driver target requires the exact managed tmux server domain")
	}
	enabled := 0
	if in.Enabled {
		enabled = 1
	}
	stamp := now.UTC().Format(rfc3339)
	_, err := s.DB.ExecContext(ctx, `INSERT INTO builder_driver_targets
		(project_id,seat_id,instance_ref,tmux_server_domain_id,tmux_server_instance_id,profile_id,
		 workspace_root_id,workspace_relative_base,enabled,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,seat_id) DO UPDATE SET
		 instance_ref=excluded.instance_ref,
		 tmux_server_domain_id=excluded.tmux_server_domain_id,
		 tmux_server_instance_id=excluded.tmux_server_instance_id,
		 profile_id=excluded.profile_id,workspace_root_id=excluded.workspace_root_id,
		 workspace_relative_base=excluded.workspace_relative_base,
		 enabled=excluded.enabled,updated_at=excluded.updated_at`, in.ProjectID, in.SeatID,
		in.InstanceRef, in.TmuxServerDomainID, in.TmuxServerInstanceID, in.ProfileID, in.WorkspaceRootID,
		in.WorkspaceRelativeBase, enabled, stamp, stamp)
	return err
}

type BuilderLaunchReconcileResult struct {
	Scanned, ActionsCreated, ReworksScheduled, Acknowledged, CapacityHeld, Stalled int
}

type builderLaunchCandidate struct {
	seatID, hostID, storeID, serverDomainID, serverID, profileID, workspaceRootID, workspaceBase string
	family, accountKey                                                                           string
	maxConcurrent                                                                                int
}

// ReconcileBuilderLaunches is the durable admitted -> building scheduler. It
// commits the compute lease and immutable lifecycle action in one transaction,
// and moves to building only after a separately routed launch contract has exact
// provider-message evidence. It never calls Driver or tmux itself.
func (s *Store) ReconcileBuilderLaunches(ctx context.Context, now time.Time,
	freshFor time.Duration, provider string, maximumTries int) (BuilderLaunchReconcileResult, error) {
	var out BuilderLaunchReconcileResult
	if freshFor <= 0 {
		freshFor = 5 * time.Minute
	}
	if provider == "" {
		provider = "codex"
	}
	if maximumTries < 1 {
		maximumTries = 5
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT d.epic_id FROM epic_deliveries d
		JOIN epics e ON e.id=d.epic_id WHERE d.state='admitted'
		AND d.hold_kind NOT IN ('paused','needs_human','driver_control_origin_unavailable')
		ORDER BY d.created_at,d.epic_id`)
	if err != nil {
		return out, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return out, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	for _, epicID := range ids {
		out.Scanned++
		result, err := s.reconcileOneBuilderLaunch(ctx, epicID, now, freshFor, provider, maximumTries)
		if err != nil {
			return out, err
		}
		out.ActionsCreated += result.ActionsCreated
		out.Acknowledged += result.Acknowledged
		out.CapacityHeld += result.CapacityHeld
		out.Stalled += result.Stalled
	}
	for {
		turn, err := s.scheduleOneFairBuilderCompute(ctx, now, freshFor, provider)
		if err != nil {
			return out, err
		}
		out.CapacityHeld += turn.CapacityHeld
		if !turn.Scheduled {
			break
		}
		if turn.Kind == "builder_rework" {
			out.ReworksScheduled++
		} else {
			out.ActionsCreated++
		}
	}
	return out, nil
}

func (s *Store) reconcileOneBuilderLaunch(ctx context.Context, epicID string, now time.Time,
	freshFor time.Duration, provider string, maximumTries int) (BuilderLaunchReconcileResult, error) {
	var out BuilderLaunchReconcileResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, repo, branch, slug, title, specPath, state, hold, leaseAction, stateDue string
		var version int
		if err := tx.QueryRowContext(ctx, `SELECT e.project_id,e.repo,e.branch,e.slug,e.title,
			e.file_path,d.state,d.state_version,d.hold_kind,d.compute_lease_action_id,d.state_due_at
			FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id WHERE e.id=?`, epicID).
			Scan(&projectID, &repo, &branch, &slug, &title, &specPath, &state, &version,
				&hold, &leaseAction, &stateDue); err != nil {
			return err
		}
		if state != "admitted" {
			return nil
		}

		// The independently evidenced terminal insertion is the only launch ack.
		var contractActionID string
		err := tx.QueryRowContext(ctx, `SELECT a.id FROM epic_actions a
			JOIN driver_action_evidence ev ON ev.action_id=a.id AND ev.action_epoch=a.action_epoch
			WHERE a.epic_id=? AND a.kind='builder_launch_contract' AND a.state='acknowledged'
			AND ev.state='confirmed' ORDER BY a.created_at DESC LIMIT 1`, epicID).Scan(&contractActionID)
		if err == nil {
			return acknowledgeBuilderLaunchTx(ctx, tx, projectID, epicID, contractActionID, version, now, &out)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		var actionID, actionState string
		var attempts int
		err = tx.QueryRowContext(ctx, `SELECT id,state,attempts FROM epic_actions
			WHERE epic_id=? AND kind IN ('builder_launch','builder_launch_contract')
			AND state<>'cancelled_superseded' ORDER BY created_at DESC LIMIT 1`, epicID).
			Scan(&actionID, &actionState, &attempts)
		if err == nil {
			due, _ := time.Parse(rfc3339, stateDue)
			if actionState == "dead_letter" || attempts >= maximumTries || !due.IsZero() && !now.Before(due) {
				return holdBuilderLaunchStalledTx(ctx, tx, projectID, epicID, actionID,
					attempts, version, now, &out)
			}
			return nil // one durable effect is already pending/in-flight/verifying/acked.
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if leaseAction != "" {
			return errors.New("builder launch compute lease has no live action")
		}
		return nil
	})
	return out, err
}

func builderLaunchCandidatesTx(ctx context.Context, tx *sql.Tx, projectID, provider string,
	capacityV2 bool) ([]builderLaunchCandidate, error) {
	// A v2 seat is addressed by the authenticated identity approved by
	// bind-capacity, not by the legacy execution locator. In particular, a local
	// seat intentionally has box='', while Driver still reports its stable
	// host_id. Using seats.box here made the documented local probe/bind path
	// impossible and copying seats.account_key into the epic could account the
	// lease against a different provider identity than the active generation.
	//
	// Keep the legacy query deliberately separate. This avoids making a partially
	// enrolled v2 seat look like a legacy seat and preserves the old box/account
	// semantics when capacity-v2 is explicitly disabled.
	identityHost := "s.box"
	accountKey := "s.account_key"
	identityEnrollment := ""
	if capacityV2 {
		identityHost = "s.expected_host_id"
		accountKey = "s.expected_account_key"
		identityEnrollment = ` AND s.expected_host_id<>'' AND s.expected_account_key<>''
			AND s.expected_credential_lineage<>''`
	}
	query := `SELECT s.id,i.host_id,i.store_id,t.tmux_server_domain_id,t.tmux_server_instance_id,
		t.profile_id,t.workspace_root_id,t.workspace_relative_base,s.agent_family,` + accountKey + `,
		s.max_concurrent FROM builder_driver_targets t JOIN seats s ON s.id=t.seat_id
		JOIN driver_instances i ON i.instance_ref=t.instance_ref
		JOIN driver_observation_cursors c ON c.instance_ref=i.instance_ref AND c.store_id=i.store_id AND c.active=1
		WHERE t.project_id=? AND t.enabled=1 AND s.enabled=1 AND s.agent_family=?
		AND i.state='live' AND i.host_id=` + identityHost + identityEnrollment + ` ORDER BY s.id`
	rows, err := tx.QueryContext(ctx, query, projectID, provider)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []builderLaunchCandidate
	for rows.Next() {
		var c builderLaunchCandidate
		if err := rows.Scan(&c.seatID, &c.hostID, &c.storeID, &c.serverDomainID, &c.serverID, &c.profileID,
			&c.workspaceRootID, &c.workspaceBase, &c.family, &c.accountKey, &c.maxConcurrent); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func holdBuilderLaunchCapacityTx(ctx context.Context, tx *sql.Tx, projectID, epicID, detail string,
	version int, now time.Time, out *BuilderLaunchReconcileResult) error {
	var current string
	_ = tx.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries WHERE epic_id=?`, epicID).Scan(&current)
	if current == "builder_capacity_unavailable" {
		return nil
	}
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET hold_kind='builder_capacity_unavailable',
		hold_reason=?,return_state='admitted',last_error=?,alert_pending=1,
		state_version=state_version+1,state_due_at=?,updated_at=? WHERE epic_id=?
		AND state='admitted' AND state_version=?`, detail, detail,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, epicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	payload, _ := json.Marshal(map[string]string{"epic_id": epicID, "reason": detail})
	dedup := "builder_capacity_unavailable:" + epicID
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "capacity_pool_exhausted", dedup, string(payload), now); err != nil {
		return err
	}
	if err := ensureBuilderLaunchAttentionTx(ctx, tx, epicID, "capacity_pool_exhausted", dedup, detail, now); err != nil {
		return err
	}
	out.CapacityHeld++
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "builder_launch_capacity_held",
		"admitted", "admitted", version+1, "scheduler", string(payload), now)
}

func holdBuilderLaunchStalledTx(ctx context.Context, tx *sql.Tx, projectID, epicID, actionID string,
	attempts, version int, now time.Time, out *BuilderLaunchReconcileResult) error {
	var current string
	_ = tx.QueryRowContext(ctx, `SELECT hold_kind FROM epic_deliveries WHERE epic_id=?`, epicID).Scan(&current)
	if current == "builder_launch_stalled" {
		return nil
	}
	stamp := now.UTC().Format(rfc3339)
	detail := fmt.Sprintf("builder launch action %s exhausted %d attempts", actionID, attempts)
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET hold_kind='builder_launch_stalled',
		hold_reason=?,return_state='admitted',last_error=?,alert_pending=1,
		state_version=state_version+1,state_due_at=?,updated_at=? WHERE epic_id=?
		AND state='admitted' AND state_version=?`, detail, detail,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, epicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil
	}
	payload, _ := json.Marshal(map[string]any{"epic_id": epicID, "action_id": actionID, "attempts": attempts})
	dedup := "builder_launch_stalled:" + epicID
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "builder_launch_stalled", dedup, string(payload), now); err != nil {
		return err
	}
	if err := ensureBuilderLaunchAttentionTx(ctx, tx, epicID, "builder_launch_stalled", dedup, detail, now); err != nil {
		return err
	}
	out.Stalled++
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "builder_launch_stalled",
		"admitted", "admitted", version+1, "scheduler", string(payload), now)
}

func ensureBuilderLaunchAttentionTx(ctx context.Context, tx *sql.Tx, epicID, kind, dedup, detail string, now time.Time) error {
	h := sha256.Sum256([]byte(dedup))
	id := kind + "-" + hex.EncodeToString(h[:12])
	stamp := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,verdict,
		 occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,?,?,'',10,'open',?,1,'',0,'','','','{}',?,'','',1,?,?,'',?,?)
		ON CONFLICT DO NOTHING`, id, kind, epicID, dedup, detail, stamp, stamp, stamp, stamp)
	return err
}

func acknowledgeBuilderLaunchTx(ctx context.Context, tx *sql.Tx, projectID, epicID, actionID string,
	version int, now time.Time, out *BuilderLaunchReconcileResult) error {
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epics SET state='running',launched_at=?,updated_at=?
		WHERE id=? AND state='launching'`, stamp, stamp, epicID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("builder launch acknowledgement lost legacy state fence")
	}
	res, err = tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='building',
		builder_affinity_state='active',state_version=state_version+1,state_entered_at=?,
		state_due_at=?,fact_progress_at=?,hold_kind='',hold_reason='',return_state='',
		last_error='',alert_pending=0,updated_at=? WHERE epic_id=? AND state='admitted'
		AND state_version=?`, stamp, now.Add(2*time.Hour).UTC().Format(rfc3339), stamp,
		stamp, epicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("builder launch acknowledgement lost delivery fence")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
		resolution='builder_launch_acknowledged',resolved_at=?,updated_at=? WHERE epic_id=?
		AND kind IN ('builder_launch_stalled','capacity_pool_exhausted')
		AND state IN ('open','leased','delivering','awaiting_ack')`, stamp, stamp, epicID); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"action_id": actionID})
	out.Acknowledged++
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "builder_launch_acknowledged",
		"admitted", "building", version+1, "driver", string(payload), now)
}
