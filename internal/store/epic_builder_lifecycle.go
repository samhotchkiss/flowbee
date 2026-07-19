package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type BuilderLifecycleIdentity struct {
	HostID, StoreID, TmuxServerInstanceID, LifecycleKey string
	TargetEpoch                                         int64
	SessionID, PaneInstanceID, AgentRunID               string
	Provider, ConversationID                            string
}

type BuilderLifecycleActionProjection struct {
	ActionID                                                         string
	Epoch                                                            int64
	ProjectID, EpicID, Kind, DedupKey                                string
	Payload, PayloadSHA256                                           string
	HeadSHA, BaseSHA                                                 string
	TargetHostID, TargetStoreID, TargetServerID, LifecycleKey        string
	TargetEpoch                                                      int64
	ProfileID, WorkspaceRootID, WorkspaceRelativePath                string
	LeaseID                                                          string
	LeaseEpoch                                                       int64
	RecipientSessionID, RecipientPaneInstanceID, RecipientAgentRunID string
}

type BuilderLifecycleReceiptProjection struct {
	ActionID, Operation, LifecycleKey, Status, AbsenceObservedAt string
	ActionEpoch, TargetEpoch                                     int64
	IdentityAfter                                                BuilderLifecycleIdentity
}

type BuilderRelaunchCapacityResult struct {
	Allowed bool
	Detail  string
}

// PrepareBuilderLaunch is the final fail-closed capacity recheck immediately
// before Driver mutation. Admission already committed the exact seat/action as
// one compute lease; this method binds the claimant epoch and refuses Ensure if
// the seat's current complete generation has since become stale or unroutable.
func (s *Store) PrepareBuilderLaunch(ctx context.Context, action BuilderLifecycleActionProjection,
	now time.Time, freshFor time.Duration) (BuilderRelaunchCapacityResult, error) {
	var out BuilderRelaunchCapacityResult
	if action.Kind != "builder_launch" || action.EpicID == "" || action.Epoch < 1 {
		return out, errors.New("builder launch capacity gate requires a claimed launch action")
	}
	if freshFor <= 0 {
		freshFor = 5 * time.Minute
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, seatID, deliveryState, holdKind, leaseActionID string
		var version int
		var leaseEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT e.project_id,e.seat_id,d.state,d.state_version,
			d.hold_kind,d.compute_lease_action_id,d.compute_lease_action_epoch
			FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id WHERE e.id=?`, action.EpicID).
			Scan(&projectID, &seatID, &deliveryState, &version, &holdKind,
				&leaseActionID, &leaseEpoch); err != nil {
			return err
		}
		if deliveryState != "admitted" || leaseActionID != action.ActionID {
			return fmt.Errorf("builder launch compute lease changed: %s/%s", deliveryState, leaseActionID)
		}
		if leaseEpoch > action.Epoch {
			return errors.New("builder launch claimant epoch is stale")
		}
		var currentHost, currentStore, currentServer string
		if err := tx.QueryRowContext(ctx, `SELECT i.host_id,i.store_id,t.tmux_server_instance_id
			FROM builder_driver_targets t JOIN driver_instances i ON i.instance_ref=t.instance_ref
			WHERE t.project_id=? AND t.seat_id=? AND t.enabled=1 AND i.state='live'`,
			projectID, seatID).Scan(&currentHost, &currentStore, &currentServer); err != nil {
			return fmt.Errorf("builder launch Driver target is no longer live: %w", err)
		}
		if currentHost != action.TargetHostID || currentStore != action.TargetStoreID ||
			currentServer != action.TargetServerID {
			return errors.New("builder launch Driver store/server incarnation changed after action commit")
		}
		decision, err := capacityRouteForSeatQueryExcludingEpic(ctx, tx, seatID,
			action.EpicID, now, freshFor)
		if err != nil {
			return err
		}
		if !decision.Routable {
			out.Detail = strings.Join(decision.Reasons, ",")
			var report BuilderLaunchReconcileResult
			return holdBuilderLaunchCapacityTx(ctx, tx, projectID, action.EpicID,
				out.Detail, version, now, &report)
		}
		stamp := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
			compute_lease_action_epoch=?,hold_kind='',hold_reason='',return_state='',
			last_error='',alert_pending=0,state_version=state_version+1,updated_at=?
			WHERE epic_id=? AND state='admitted' AND compute_lease_action_id=?
			AND compute_lease_action_epoch<=?`, action.Epoch, stamp, action.EpicID,
			action.ActionID, action.Epoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("builder launch lost claimant epoch fence")
		}
		if holdKind == "builder_capacity_unavailable" {
			if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
				resolution='capacity_reacquired',resolved_at=?,updated_at=? WHERE epic_id=?
				AND kind='capacity_pool_exhausted' AND state IN ('open','leased','delivering','awaiting_ack')`,
				stamp, stamp, action.EpicID); err != nil {
				return err
			}
		}
		out.Allowed = true
		return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID,
			"builder_launch_capacity_rechecked", "admitted", "admitted", version+1,
			"scheduler", `{"routable":true}`, now)
	})
	return out, err
}

// PrepareBuilderRelaunch atomically converts the parked epic row into the
// physical compute lease used by all existing occupancy/account counts. Under
// capacity-v2 the same transaction evaluates fresh seat+account truth first;
// no Driver Ensure is allowed while the durable hold is active.
func (s *Store) PrepareBuilderRelaunch(ctx context.Context, action BuilderLifecycleActionProjection,
	now time.Time, freshFor time.Duration) (BuilderRelaunchCapacityResult, error) {
	var out BuilderRelaunchCapacityResult
	if action.Kind != "builder_rework" || action.EpicID == "" {
		return out, errors.New("builder relaunch capacity gate requires a rework action")
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var projectID, epicState, seatID, deliveryState, affinity, leaseActionID string
		var version int
		var leaseActionEpoch int64
		if err := tx.QueryRowContext(ctx, `SELECT e.project_id,e.state,e.seat_id,d.state,
			d.builder_affinity_state,d.state_version,d.compute_lease_action_id,
			d.compute_lease_action_epoch FROM epics e JOIN epic_deliveries d
			ON d.epic_id=e.id WHERE e.id=?`, action.EpicID).Scan(&projectID, &epicState,
			&seatID, &deliveryState, &affinity, &version, &leaseActionID, &leaseActionEpoch); err != nil {
			return err
		}
		if epicState == "launching" && deliveryState == "changes_requested" && affinity == "relaunching" {
			if leaseActionID != action.ActionID || leaseActionEpoch != action.Epoch {
				return errors.New("builder relaunch compute lease belongs to a different action epoch")
			}
			out.Allowed = true
			return nil
		}
		if (epicState != "done" && epicState != "achieved") ||
			deliveryState != "changes_requested" || affinity != "relaunching" {
			return fmt.Errorf("builder relaunch capacity state is %s/%s/%s", epicState, deliveryState, affinity)
		}
		if s.EnableCapacityV2 {
			if seatID == "" {
				out.Detail = "builder seat binding missing"
				return markBuilderCapacityHoldTx(ctx, tx, projectID, action.EpicID, seatID,
					out.Detail, version, now)
			}
			decision, err := capacityRouteForSeatQuery(ctx, tx, seatID, now, freshFor)
			if err != nil {
				return err
			}
			if !decision.Routable {
				out.Detail = strings.Join(decision.Reasons, ",")
				return markBuilderCapacityHoldTx(ctx, tx, projectID, action.EpicID, seatID,
					out.Detail, version, now)
			}
		}
		stamp := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE epics SET state='launching',finished_at='',
			launched_at=?,updated_at=? WHERE id=? AND state IN ('done','achieved')`,
			stamp, stamp, action.EpicID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return errors.New("builder relaunch lost compute lease race")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET hold_kind='',hold_reason='',
			return_state='',last_error='',alert_pending=0,compute_lease_action_id=?,
			compute_lease_action_epoch=?,state_version=state_version+1,updated_at=? WHERE epic_id=?`,
			action.ActionID, action.Epoch, stamp, action.EpicID); err != nil {
			return err
		}
		dedup := builderCapacityDedup(action.EpicID, seatID)
		if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',
			resolution='capacity_reacquired',resolved_at=?,updated_at=? WHERE dedup_key=?
			AND state IN ('open','leased','delivering','awaiting_ack')`, stamp, stamp, dedup); err != nil {
			return err
		}
		payload, _ := json.Marshal(map[string]string{"seat_id": seatID, "action_id": action.ActionID})
		if err := appendEpicControlEventTx(ctx, tx, projectID, action.EpicID,
			"builder_capacity_acquired", deliveryState, deliveryState, version+1,
			"scheduler", string(payload), now); err != nil {
			return err
		}
		out.Allowed = true
		return nil
	})
	return out, err
}

func builderCapacityDedup(epicID, seatID string) string {
	h := sha256.Sum256([]byte(epicID + "\x00" + seatID + "\x00builder_relaunch"))
	return "builder_capacity_unavailable:" + epicID + ":" + hex.EncodeToString(h[:8])
}

func markBuilderCapacityHoldTx(ctx context.Context, tx *sql.Tx, projectID, epicID, seatID,
	detail string, version int, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET hold_kind='builder_capacity_unavailable',
		hold_reason=?,return_state='changes_requested',last_error=?,alert_pending=1,
		state_due_at=?,updated_at=? WHERE epic_id=? AND state='changes_requested'
		AND builder_affinity_state='relaunching' AND state_version=?`, detail, detail,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, epicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("builder capacity hold lost delivery race")
	}
	dedup := builderCapacityDedup(epicID, seatID)
	idHash := sha256.Sum256([]byte(dedup))
	attentionID := "builder-capacity-" + hex.EncodeToString(idHash[:12])
	evidence, _ := json.Marshal(map[string]string{"epic_id": epicID, "seat_id": seatID, "reason": detail})
	insertedResult, err := tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,kind,epic_id,repo,priority,state,dedup_key,blocking,leased_by,item_epoch,
		 lease_expires_at,awaiting_since,delivery_key,evidence_json,detail,resolution,verdict,
		 occurrences,first_seen_at,last_seen_at,resolved_at,created_at,updated_at)
		VALUES (?,'capacity_pool_exhausted',?,'',10,'open',?,1,'',0,'','','',?,?,'','',1,?,?,'',?,?)
		ON CONFLICT DO NOTHING`, attentionID, epicID, dedup, string(evidence), detail,
		stamp, stamp, stamp, stamp)
	if err != nil {
		return err
	}
	inserted, _ := insertedResult.RowsAffected()
	delta := 1
	if inserted == 1 {
		delta = 0
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET occurrences=occurrences+?,
		last_seen_at=?,detail=?,evidence_json=?,updated_at=? WHERE dedup_key=?
		AND state IN ('open','leased','delivering','awaiting_ack')`, delta, stamp, detail,
		string(evidence), stamp, dedup); err != nil {
		return err
	}
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID, "capacity_pool_exhausted",
		dedup, string(evidence), now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, epicID, "builder_capacity_held",
		"changes_requested", "changes_requested", version, "scheduler", string(evidence), now)
}

// ensureBuilderParkActionTx persists the exact Stop effect before a builder's
// legacy epics.state is allowed to become terminal. Until Driver proves positive
// absence the row remains physically active and continues consuming its seat.
func ensureBuilderParkActionTx(ctx context.Context, tx *sql.Tx, projectID, epicID, finalState string, now time.Time) error {
	binding, err := activeDriverSessionBindingTx(ctx, tx, projectID, BuilderDriverIdentity(epicID), DriverBuilderRole)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("park builder %s: %w", epicID, ErrDriverSessionBindingMissing)
		}
		return err
	}
	payloadBytes, _ := json.Marshal(map[string]string{
		"epic_id": epicID, "final_epic_state": finalState, "type": "builder_park",
	})
	dedup := fmt.Sprintf("%s:%s:builder_park:%s", projectID, epicID, binding.AgentRunID)
	return ensureBuilderLifecycleActionTx(ctx, tx, builderLifecycleAction{
		ProjectID: projectID, EpicID: epicID, Kind: "builder_park", Dedup: dedup,
		Payload: string(payloadBytes), Binding: binding, ExactTarget: true,
		TargetEpoch: binding.TargetEpoch, LeaseEpoch: binding.BindingEpoch,
		Now: now,
	})
}

// ensureBuilderReworkActionTx creates the one rejection effect against the
// parked affinity. EnsureSession receives target_epoch+1 and no old pane/run, so
// the stopped incarnation can never inherit authority.
func ensureBuilderReworkActionTx(ctx context.Context, tx *sql.Tx, projectID, epicID,
	dedup, payload, head, base string, now time.Time) error {
	var affinity string
	if err := tx.QueryRowContext(ctx, `SELECT builder_affinity_state FROM epic_deliveries
		WHERE epic_id=?`, epicID).Scan(&affinity); err != nil {
		return err
	}
	if affinity != "parked" {
		return fmt.Errorf("builder rework requires parked affinity, got %s", affinity)
	}
	binding, err := latestDriverSessionBindingTx(ctx, tx, projectID,
		BuilderDriverIdentity(epicID), DriverBuilderRole)
	if err != nil {
		return fmt.Errorf("relaunch builder %s: %w", epicID, err)
	}
	return ensureBuilderLifecycleActionTx(ctx, tx, builderLifecycleAction{
		ProjectID: projectID, EpicID: epicID, Kind: "builder_rework", Dedup: dedup,
		Payload: payload, HeadSHA: head, BaseSHA: base, Binding: binding, ExactTarget: false,
		TargetEpoch: binding.TargetEpoch + 1, LeaseEpoch: binding.BindingEpoch + 1,
		Now: now,
	})
}

// ensureBuilderConflictResolutionActionTx relaunches the same parked builder
// affinity under a fresh target epoch. The conflict resolver is never allowed to
// approve its own resolution; its only forward evidence is a new repository head.
func ensureBuilderConflictResolutionActionTx(ctx context.Context, tx *sql.Tx, projectID, epicID,
	dedup, payload, head, base string, now time.Time) error {
	var affinity string
	if err := tx.QueryRowContext(ctx, `SELECT builder_affinity_state FROM epic_deliveries
		WHERE epic_id=?`, epicID).Scan(&affinity); err != nil {
		return err
	}
	if affinity != "parked" {
		return fmt.Errorf("builder conflict resolution requires parked affinity, got %s", affinity)
	}
	binding, err := latestDriverSessionBindingTx(ctx, tx, projectID,
		BuilderDriverIdentity(epicID), DriverBuilderRole)
	if err != nil {
		return fmt.Errorf("relaunch conflict resolver %s: %w", epicID, err)
	}
	return ensureBuilderLifecycleActionTx(ctx, tx, builderLifecycleAction{
		ProjectID: projectID, EpicID: epicID, Kind: "conflict_resolution", Dedup: dedup,
		Payload: payload, HeadSHA: head, BaseSHA: base, Binding: binding, ExactTarget: false,
		TargetEpoch: binding.TargetEpoch + 1, LeaseEpoch: binding.BindingEpoch + 1,
		Now: now,
	})
}

type builderLifecycleAction struct {
	ProjectID, EpicID, Kind, Dedup, Payload string
	HeadSHA, BaseSHA                        string
	Binding                                 DriverSessionBinding
	ExactTarget                             bool
	TargetEpoch, LeaseEpoch                 int64
	Now                                     time.Time
}

func ensureBuilderLifecycleActionTx(ctx context.Context, tx *sql.Tx, p builderLifecycleAction) error {
	payloadHash := sha256.Sum256([]byte(p.Payload))
	idHash := sha256.Sum256([]byte(p.Dedup))
	actionID := p.Kind + "-" + hex.EncodeToString(idHash[:12])
	nowText := p.Now.UTC().Format(rfc3339)
	sessionID, paneID, runID := "", "", ""
	if p.ExactTarget {
		sessionID, paneID, runID = p.Binding.SessionID, p.Binding.PaneInstanceID, p.Binding.AgentRunID
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
		 recipient_session_id,recipient_pane_instance_id,recipient_agent_run_id,
		 head_sha,base_sha,next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,?,'pending',0,?,?,?,'driver_lifecycle','builder',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		actionID, p.ProjectID, p.EpicID, p.Kind, p.Dedup, p.Payload,
		"sha256:"+hex.EncodeToString(payloadHash[:]), p.Binding.HostID, p.Binding.StoreID,
		p.Binding.TmuxServerInstanceID, p.Binding.LifecycleKey, p.TargetEpoch, p.Binding.ProfileID,
		p.Binding.WorkspaceRootID, p.Binding.WorkspaceRelativePath,
		"builder-affinity:"+p.EpicID, p.LeaseEpoch, sessionID, paneID, runID, p.HeadSHA, p.BaseSHA,
		nowText, nowText, nowText)
	if err == nil {
		return nil
	}
	if !isUniqueConstraintErr(err) {
		return err
	}
	var kind, hash, host, storeID, server, lifecycle, recipientSession, recipientPane, recipientRun string
	var targetEpoch, leaseEpoch int64
	if qerr := tx.QueryRowContext(ctx, `SELECT kind,payload_sha256,target_host_id,target_store_id,
		target_server_id,lifecycle_key,target_epoch,lease_epoch,recipient_session_id,
		recipient_pane_instance_id,recipient_agent_run_id FROM epic_actions
		WHERE dedup_key=? AND state<>'cancelled_superseded'`, p.Dedup).Scan(&kind, &hash,
		&host, &storeID, &server, &lifecycle, &targetEpoch, &leaseEpoch,
		&recipientSession, &recipientPane, &recipientRun); qerr != nil {
		return err
	}
	if kind != p.Kind || hash != "sha256:"+hex.EncodeToString(payloadHash[:]) ||
		host != p.Binding.HostID || storeID != p.Binding.StoreID ||
		server != p.Binding.TmuxServerInstanceID || lifecycle != p.Binding.LifecycleKey ||
		targetEpoch != p.TargetEpoch || leaseEpoch != p.LeaseEpoch ||
		recipientSession != sessionID || recipientPane != paneID || recipientRun != runID {
		return fmt.Errorf("builder lifecycle action dedup collision for %s", p.Dedup)
	}
	return nil
}

func latestDriverSessionBindingTx(ctx context.Context, tx *sql.Tx, projectID, workerIdentity, role string) (DriverSessionBinding, error) {
	return activeDriverSessionBindingRow(tx.QueryRowContext(ctx, `SELECT
		binding_id,project_id,worker_identity,role,binding_epoch,host_id,store_id,
		tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		conversation_id,observed_at FROM driver_session_bindings
		WHERE project_id=? AND worker_identity=? AND role=?
		ORDER BY binding_epoch DESC LIMIT 1`, projectID, workerIdentity, role))
}

// ProjectLifecycleResult is called by Driver's lifecycle runtime before the
// action acknowledgement. It is replay-safe across a crash after this commit.
func (s *Store) ProjectBuilderLifecycleResult(ctx context.Context, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if receipt.ActionID != action.ActionID || receipt.ActionEpoch != action.Epoch ||
			receipt.LifecycleKey != action.LifecycleKey || receipt.TargetEpoch != action.TargetEpoch {
			return errors.New("Driver lifecycle receipt changed immutable action identity")
		}
		switch action.Kind {
		case "builder_park":
			return projectBuilderParkTx(ctx, tx, action, receipt, now)
		case "builder_launch":
			return projectBuilderLaunchTx(ctx, tx, action, receipt, now, s.HasDriverControlOrigin())
		case "builder_rework":
			return projectBuilderRelaunchTx(ctx, tx, action, receipt, now, s.HasDriverControlOrigin())
		case "conflict_resolution":
			return projectBuilderConflictRelaunchTx(ctx, tx, action, receipt, now, s.HasDriverControlOrigin())
		default:
			return fmt.Errorf("unsupported lifecycle action %s", action.Kind)
		}
	})
}

func projectBuilderLaunchTx(ctx context.Context, tx *sql.Tx, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time, controlOriginAvailable bool) error {
	if receipt.Operation != "ensure" || receipt.Status != "ensured" {
		return fmt.Errorf("builder launch not ensured: %s", receipt.Status)
	}
	id := receipt.IdentityAfter
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerInstanceID != action.TargetServerID || id.LifecycleKey != action.LifecycleKey ||
		id.TargetEpoch != action.TargetEpoch || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return errors.New("Driver builder launch returned a different incarnation fence")
	}
	var projectID, state, leaseActionID string
	var version int
	var leaseEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,
		compute_lease_action_id,compute_lease_action_epoch FROM epic_deliveries WHERE epic_id=?`,
		action.EpicID).Scan(&projectID, &state, &version, &leaseActionID, &leaseEpoch); err != nil {
		return err
	}
	if state != "admitted" || leaseActionID != action.ActionID || leaseEpoch != action.Epoch {
		return fmt.Errorf("builder launch projection lost compute lease: %s/%s/%d", state, leaseActionID, leaseEpoch)
	}
	// Exact replay after a crash between projection and lifecycle acknowledgement.
	if current, err := activeDriverSessionBindingTx(ctx, tx, projectID,
		BuilderDriverIdentity(action.EpicID), DriverBuilderRole); err == nil {
		if current.HostID != id.HostID || current.StoreID != id.StoreID ||
			current.SessionID != id.SessionID || current.PaneInstanceID != id.PaneInstanceID ||
			current.AgentRunID != id.AgentRunID || current.TargetEpoch != id.TargetEpoch {
			return errors.New("builder launch replay found a different active incarnation")
		}
		if !controlOriginAvailable {
			return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
		}
		return ensureBuilderLaunchContractTx(ctx, tx, action, current, now)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	binding := DriverSessionBinding{ProjectID: projectID,
		WorkerIdentity: BuilderDriverIdentity(action.EpicID), Role: DriverBuilderRole,
		BindingEpoch: 1, HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerInstanceID: id.TmuxServerInstanceID, LifecycleKey: id.LifecycleKey,
		TargetEpoch: id.TargetEpoch, ProfileID: action.ProfileID,
		WorkspaceRootID: action.WorkspaceRootID, WorkspaceRelativePath: action.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: id.Provider, ConversationID: id.ConversationID, ObservedAt: now}
	binding.BindingID = driverBindingID(binding, binding.BindingEpoch)
	_, err := tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
		(binding_id,project_id,worker_identity,role,binding_epoch,state,host_id,store_id,
		 tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		 workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		 conversation_id,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, binding.BindingID,
		projectID, binding.WorkerIdentity, binding.Role, binding.BindingEpoch, id.HostID,
		id.StoreID, id.TmuxServerInstanceID, id.LifecycleKey, id.TargetEpoch,
		action.ProfileID, action.WorkspaceRootID, action.WorkspaceRelativePath, id.SessionID,
		id.PaneInstanceID, id.AgentRunID, id.Provider, id.ConversationID, stamp, stamp, stamp)
	if err != nil {
		return err
	}
	if !controlOriginAvailable {
		return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
	}
	if err := ensureBuilderLaunchContractTx(ctx, tx, action, binding, now); err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"session_id": id.SessionID,
		"pane_instance_id": id.PaneInstanceID, "agent_run_id": id.AgentRunID})
	return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID,
		"builder_session_ensured", "admitted", "admitted", version,
		"driver", string(payload), now)
}

func ensureBuilderLaunchContractTx(ctx context.Context, tx *sql.Tx,
	parent BuilderLifecycleActionProjection, recipient DriverSessionBinding, now time.Time) error {
	var baselineSeq, uncertainty uint64
	if err := tx.QueryRowContext(ctx, `SELECT high_store_seq,uncertainty_epoch
		FROM driver_observation_cursors WHERE store_id=? AND active=1`, recipient.StoreID).
		Scan(&baselineSeq, &uncertainty); err != nil {
		return fmt.Errorf("builder launch evidence baseline: %w", err)
	}
	payload := "FLOWBEE EPIC CONTRACT\n" + parent.Payload +
		"\nAcknowledge this exact contract by beginning work. Report progress through Flowbee-owned stage evidence."
	hash := sha256.Sum256([]byte(payload))
	dedup := parent.DedupKey + ":contract:" + recipient.AgentRunID
	idHash := sha256.Sum256([]byte(dedup))
	actionID := "builder-launch-contract-" + hex.EncodeToString(idHash[:12])
	grantID := stableUUID("driver-builder-launch-grant/v1", dedup)
	stamp := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 evidence_baseline_store_seq,evidence_baseline_uncertainty_epoch,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
		 sender_principal_id,recipient_session_id,recipient_pane_instance_id,
		 recipient_agent_run_id,grant_id,grant_epoch,grant_expires_at,next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'builder_launch_contract','pending',0,?,?,?,?,?,'driver','builder',
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?)`, actionID, parent.ProjectID, parent.EpicID,
		dedup, payload, "sha256:"+hex.EncodeToString(hash[:]), baselineSeq, uncertainty,
		recipient.HostID, recipient.StoreID, recipient.TmuxServerInstanceID,
		recipient.LifecycleKey, recipient.TargetEpoch, recipient.ProfileID,
		recipient.WorkspaceRootID, recipient.WorkspaceRelativePath,
		"builder-compute:"+parent.EpicID, parent.Epoch, DriverControlIdentity,
		recipient.SessionID, recipient.PaneInstanceID, recipient.AgentRunID, grantID,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, stamp, stamp)
	if err == nil {
		return nil
	}
	if !isUniqueConstraintErr(err) {
		return err
	}
	var gotHash, gotSession, gotPane, gotRun, gotGrant string
	qerr := tx.QueryRowContext(ctx, `SELECT payload_sha256,recipient_session_id,
		recipient_pane_instance_id,recipient_agent_run_id,grant_id FROM epic_actions
		WHERE dedup_key=? AND state<>'cancelled_superseded'`, dedup).
		Scan(&gotHash, &gotSession, &gotPane, &gotRun, &gotGrant)
	if qerr != nil {
		return err
	}
	if gotHash != "sha256:"+hex.EncodeToString(hash[:]) || gotSession != recipient.SessionID ||
		gotPane != recipient.PaneInstanceID || gotRun != recipient.AgentRunID || gotGrant != grantID {
		return fmt.Errorf("builder launch contract dedup collision for %s", dedup)
	}
	return nil
}

func projectBuilderConflictRelaunchTx(ctx context.Context, tx *sql.Tx, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time, controlOriginAvailable bool) error {
	if receipt.Operation != "ensure" || receipt.Status != "ensured" {
		return fmt.Errorf("conflict resolver relaunch not ensured: %s", receipt.Status)
	}
	id := receipt.IdentityAfter
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerInstanceID != action.TargetServerID || id.LifecycleKey != action.LifecycleKey ||
		id.TargetEpoch != action.TargetEpoch || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return errors.New("Driver conflict relaunch returned a different incarnation fence")
	}
	var projectID, state, affinity string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,builder_affinity_state
		FROM epic_deliveries WHERE epic_id=?`, action.EpicID).Scan(&projectID, &state,
		&version, &affinity); err != nil {
		return err
	}
	if affinity == "active" && state == "conflict_resolution" {
		if !controlOriginAvailable {
			return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
		}
		return ensureBuilderReworkWakeTx(ctx, tx, action, id, now)
	}
	if affinity != "relaunching" || state != "conflict_resolution" {
		return fmt.Errorf("conflict resolver projection is %s/%s", state, affinity)
	}
	prior, err := latestDriverSessionBindingTx(ctx, tx, projectID,
		BuilderDriverIdentity(action.EpicID), DriverBuilderRole)
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
		superseded_at=?,updated_at=? WHERE project_id=? AND worker_identity=? AND role=? AND state='active'`,
		stamp, stamp, projectID, BuilderDriverIdentity(action.EpicID), DriverBuilderRole); err != nil {
		return err
	}
	newEpoch := prior.BindingEpoch + 1
	newBinding := DriverSessionBinding{ProjectID: projectID,
		WorkerIdentity: BuilderDriverIdentity(action.EpicID), Role: DriverBuilderRole,
		BindingEpoch: newEpoch, HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerInstanceID: id.TmuxServerInstanceID, LifecycleKey: id.LifecycleKey,
		TargetEpoch: id.TargetEpoch, ProfileID: prior.ProfileID,
		WorkspaceRootID: prior.WorkspaceRootID, WorkspaceRelativePath: prior.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: id.Provider, ConversationID: id.ConversationID, ObservedAt: now}
	newBinding.BindingID = driverBindingID(newBinding, newEpoch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
		(binding_id,project_id,worker_identity,role,binding_epoch,state,host_id,store_id,
		 tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		 workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		 conversation_id,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newBinding.BindingID,
		projectID, newBinding.WorkerIdentity, newBinding.Role, newEpoch, id.HostID, id.StoreID,
		id.TmuxServerInstanceID, id.LifecycleKey, id.TargetEpoch, prior.ProfileID,
		prior.WorkspaceRootID, prior.WorkspaceRelativePath, id.SessionID, id.PaneInstanceID,
		id.AgentRunID, id.Provider, id.ConversationID, stamp, stamp, stamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE epics SET state='running',finished_at='',
		launched_at=?,updated_at=? WHERE id=?`, stamp, stamp, action.EpicID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET builder_affinity_state='active',
		state_version=state_version+1,state_due_at=?,fact_progress_at=?,updated_at=?
		WHERE epic_id=? AND state_version=? AND state='conflict_resolution'
		AND builder_affinity_state='relaunching'`, now.Add(30*time.Minute).UTC().Format(rfc3339),
		stamp, stamp, action.EpicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("conflict resolver relaunch projection changed concurrently")
	}
	if !controlOriginAvailable {
		return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
	}
	if err := ensureBuilderReworkWakeTx(ctx, tx, action, id, now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "conflict_resolver_relaunched",
		state, state, version+1, "driver", `{"new_incarnation":true}`, now)
}

func projectBuilderParkTx(ctx context.Context, tx *sql.Tx, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	if receipt.Operation != "stop" || (receipt.Status != "stopped" && receipt.Status != "target_absent") ||
		receipt.AbsenceObservedAt == "" {
		return fmt.Errorf("builder park lacks positive absence: %s", receipt.Status)
	}
	var payload struct {
		FinalEpicState string `json:"final_epic_state"`
	}
	if err := json.Unmarshal([]byte(action.Payload), &payload); err != nil {
		return err
	}
	if payload.FinalEpicState != "done" && payload.FinalEpicState != "achieved" {
		return fmt.Errorf("invalid parked final epic state %q", payload.FinalEpicState)
	}
	var projectID, state, affinity string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,builder_affinity_state
		FROM epic_deliveries WHERE epic_id=?`, action.EpicID).Scan(&projectID, &state,
		&version, &affinity); err != nil {
		return err
	}
	if affinity == "parked" {
		return nil
	}
	if affinity != "active" {
		return fmt.Errorf("builder park affinity changed to %s", affinity)
	}
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `UPDATE epics SET state=?,finished_at=?,updated_at=? WHERE id=?`,
		payload.FinalEpicState, stamp, stamp, action.EpicID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET builder_affinity_state='parked',
		compute_lease_action_id='',compute_lease_action_epoch=0,
		state_version=state_version+1,fact_progress_at=?,updated_at=?
		WHERE epic_id=? AND state_version=? AND builder_affinity_state='active'`, stamp,
		stamp, action.EpicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("builder park projection changed concurrently")
	}
	bindingRes, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
		superseded_at=?,updated_at=? WHERE project_id=? AND worker_identity=?
		AND role=? AND state='active' AND session_id=? AND pane_instance_id=? AND agent_run_id=?`,
		stamp, stamp, projectID, BuilderDriverIdentity(action.EpicID), DriverBuilderRole,
		action.RecipientSessionID, action.RecipientPaneInstanceID, action.RecipientAgentRunID)
	if err != nil {
		return err
	}
	if n, _ := bindingRes.RowsAffected(); n != 1 {
		return errors.New("builder park exact session binding was superseded before Stop projection")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goal_sessions SET enabled=0,updated_at=?
		WHERE id=(SELECT tmux_name FROM epics WHERE id=?)`, stamp, action.EpicID); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "builder_parked",
		state, state, version+1, "driver", `{"remote_absence":true}`, now)
}

func projectBuilderRelaunchTx(ctx context.Context, tx *sql.Tx, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time, controlOriginAvailable bool) error {
	if receipt.Operation != "ensure" || receipt.Status != "ensured" {
		return fmt.Errorf("builder relaunch not ensured: %s", receipt.Status)
	}
	id := receipt.IdentityAfter
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerInstanceID != action.TargetServerID || id.LifecycleKey != action.LifecycleKey ||
		id.TargetEpoch != action.TargetEpoch || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return errors.New("Driver relaunch returned a different incarnation fence")
	}
	var projectID, state, affinity string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT project_id,state,state_version,builder_affinity_state
		FROM epic_deliveries WHERE epic_id=?`, action.EpicID).Scan(&projectID, &state,
		&version, &affinity); err != nil {
		return err
	}
	if affinity == "active" && state == "rebuild_in_flight" {
		if !controlOriginAvailable {
			return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
		}
		return ensureBuilderReworkWakeTx(ctx, tx, action, id, now)
	}
	if affinity != "relaunching" || state != "changes_requested" {
		return fmt.Errorf("builder relaunch projection is %s/%s", state, affinity)
	}
	prior, err := latestDriverSessionBindingTx(ctx, tx, projectID,
		BuilderDriverIdentity(action.EpicID), DriverBuilderRole)
	if err != nil {
		return err
	}
	if prior.HostID != action.TargetHostID || prior.StoreID != action.TargetStoreID ||
		prior.TmuxServerInstanceID != action.TargetServerID || prior.LifecycleKey != action.LifecycleKey ||
		prior.TargetEpoch+1 != action.TargetEpoch || prior.ProfileID != action.ProfileID ||
		prior.WorkspaceRootID != action.WorkspaceRootID ||
		prior.WorkspaceRelativePath != action.WorkspaceRelativePath ||
		prior.BindingEpoch+1 != action.LeaseEpoch {
		return errors.New("builder relaunch affinity changed after durable action commit")
	}
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
		superseded_at=?,updated_at=? WHERE project_id=? AND worker_identity=? AND role=? AND state='active'`,
		stamp, stamp, projectID, BuilderDriverIdentity(action.EpicID), DriverBuilderRole); err != nil {
		return err
	}
	newEpoch := prior.BindingEpoch + 1
	newBinding := DriverSessionBinding{ProjectID: projectID,
		WorkerIdentity: BuilderDriverIdentity(action.EpicID), Role: DriverBuilderRole,
		BindingEpoch: newEpoch, HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerInstanceID: id.TmuxServerInstanceID, LifecycleKey: id.LifecycleKey,
		TargetEpoch: id.TargetEpoch, ProfileID: prior.ProfileID,
		WorkspaceRootID: prior.WorkspaceRootID, WorkspaceRelativePath: prior.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: id.Provider, ConversationID: id.ConversationID, ObservedAt: now}
	newBinding.BindingID = driverBindingID(newBinding, newEpoch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
		(binding_id,project_id,worker_identity,role,binding_epoch,state,host_id,store_id,
		 tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		 workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		 conversation_id,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, newBinding.BindingID,
		projectID, newBinding.WorkerIdentity, newBinding.Role, newEpoch, id.HostID, id.StoreID,
		id.TmuxServerInstanceID, id.LifecycleKey, id.TargetEpoch, prior.ProfileID,
		prior.WorkspaceRootID, prior.WorkspaceRelativePath, id.SessionID, id.PaneInstanceID,
		id.AgentRunID, id.Provider, id.ConversationID, stamp, stamp, stamp); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE epics SET state='running',finished_at='',
		launched_at=?,updated_at=? WHERE id=?`, stamp, stamp, action.EpicID); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='rebuild_in_flight',
		builder_affinity_state='active',state_version=state_version+1,state_entered_at=?,
		state_due_at=?,fact_progress_at=?,updated_at=? WHERE epic_id=? AND state_version=?
		AND state='changes_requested' AND builder_affinity_state='relaunching'`, stamp,
		now.Add(30*time.Minute).UTC().Format(rfc3339), stamp, stamp, action.EpicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("builder relaunch projection changed concurrently")
	}
	if !controlOriginAvailable {
		return holdBuilderControlOriginTx(ctx, tx, projectID, action.EpicID, now)
	}
	if err := ensureBuilderReworkWakeTx(ctx, tx, action, id, now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, action.EpicID, "builder_relaunched",
		state, "rebuild_in_flight", version+1, "driver", `{"new_incarnation":true}`, now)
}

// holdBuilderControlOriginTx makes the post-lifecycle/pre-message seam visible.
// Driver may have successfully ensured the exact session, but that mechanical
// fact does not authorize Flowbee to impersonate a session sender.
func holdBuilderControlOriginTx(ctx context.Context, tx *sql.Tx, projectID, epicID string,
	now time.Time) error {
	var state, current string
	var version int
	if err := tx.QueryRowContext(ctx, `SELECT state,state_version,hold_kind
		FROM epic_deliveries WHERE epic_id=?`, epicID).Scan(&state, &version, &current); err != nil {
		return err
	}
	if current == "driver_control_origin_unavailable" {
		return nil
	}
	stamp := now.UTC().Format(rfc3339)
	detail := ErrDriverControlOriginUnavailable.Error()
	res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
		hold_kind='driver_control_origin_unavailable',hold_reason=?,return_state=state,
		last_error=?,alert_pending=1,state_version=state_version+1,state_due_at=?,updated_at=?
		WHERE epic_id=? AND state_version=?`, detail, detail,
		now.Add(24*time.Hour).UTC().Format(rfc3339), stamp, epicID, version)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("builder control-origin hold lost state fence")
	}
	dedup := "driver_control_origin_unavailable:" + epicID
	payload, _ := json.Marshal(map[string]string{"epic_id": epicID, "reason": detail})
	if err := ensureControlAlertTx(ctx, tx, projectID, epicID,
		"driver_control_origin_unavailable", dedup, string(payload), now); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, projectID, epicID,
		"driver_control_origin_held", state, state, version+1, "reconciler", string(payload), now)
}

func ensureBuilderReworkWakeTx(ctx context.Context, tx *sql.Tx, parent BuilderLifecycleActionProjection,
	recipient BuilderLifecycleIdentity, now time.Time) error {
	dedup := parent.DedupKey + ":wake:" + recipient.AgentRunID
	idHash := sha256.Sum256([]byte(dedup))
	actionID := "builder-rework-wake-" + hex.EncodeToString(idHash[:12])
	grantID := stableUUID("driver-builder-rework-grant/v1", dedup)
	stamp := now.UTC().Format(rfc3339)
	_, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
		 sender_principal_id,recipient_session_id,recipient_pane_instance_id,
		 recipient_agent_run_id,grant_id,grant_epoch,grant_expires_at,head_sha,base_sha,
		 next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'builder_rework_wake','pending',0,?,?,?,'driver','builder',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?,?,?)`,
		actionID, parent.ProjectID, parent.EpicID, dedup, parent.Payload, parent.PayloadSHA256,
		recipient.HostID, recipient.StoreID, recipient.TmuxServerInstanceID,
		recipient.LifecycleKey, recipient.TargetEpoch, parent.ProfileID,
		parent.WorkspaceRootID, parent.WorkspaceRelativePath, parent.LeaseID, parent.LeaseEpoch,
		DriverControlIdentity, recipient.SessionID, recipient.PaneInstanceID,
		recipient.AgentRunID, grantID, now.Add(10*time.Minute).UTC().Format(rfc3339),
		parent.HeadSHA, parent.BaseSHA, stamp, stamp, stamp)
	if err == nil {
		return nil
	}
	if !isUniqueConstraintErr(err) {
		return err
	}
	var kind, payloadHash, executor, targetHost, targetStore, targetServer, lifecycle string
	var targetEpoch, leaseEpoch int64
	var senderPrincipal, recipientSession, recipientPane, recipientRun, gotGrant string
	qerr := tx.QueryRowContext(ctx, `SELECT kind,payload_sha256,executor_kind,target_host_id,
		target_store_id,target_server_id,lifecycle_key,target_epoch,lease_epoch,
		sender_principal_id,recipient_session_id,recipient_pane_instance_id,
		recipient_agent_run_id,grant_id FROM epic_actions
		WHERE dedup_key=? AND state<>'cancelled_superseded'`, dedup).Scan(&kind, &payloadHash,
		&executor, &targetHost, &targetStore, &targetServer, &lifecycle, &targetEpoch,
		&leaseEpoch, &senderPrincipal, &recipientSession, &recipientPane,
		&recipientRun, &gotGrant)
	if qerr != nil {
		return err
	}
	if kind != "builder_rework_wake" || payloadHash != parent.PayloadSHA256 || executor != "driver" ||
		targetHost != recipient.HostID || targetStore != recipient.StoreID ||
		targetServer != recipient.TmuxServerInstanceID || lifecycle != recipient.LifecycleKey ||
		targetEpoch != recipient.TargetEpoch || leaseEpoch != parent.LeaseEpoch ||
		senderPrincipal != DriverControlIdentity ||
		recipientSession != recipient.SessionID || recipientPane != recipient.PaneInstanceID ||
		recipientRun != recipient.AgentRunID || gotGrant != grantID {
		return fmt.Errorf("builder rework wake dedup collision for %s", dedup)
	}
	return nil
}
