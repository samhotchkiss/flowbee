package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const epicWorkerAutomaticRecoveryLimit = 3

// EpicWorkerLivenessTarget is an exact, currently-authoritative Driver
// incarnation. It intentionally contains no tmux name, cwd, PID, or wall-clock
// correlation fields.
type EpicWorkerLivenessTarget struct {
	ProjectID, EpicID, WorkerRole, WorkerIdentity, FlowbeeIdentity, BindingID string
	SeatID, HostID, StoreID, ServerDomainID, ServerID                         string
	LifecycleKey, ProfileID, WorkspaceRootID, WorkspaceRelativePath           string
	SessionID, PaneInstanceID, AgentRunID                                     string
	TargetEpoch, BindingEpoch                                                 int64
	Terminal                                                                  bool
}

type EpicWorkerAbsenceResult struct {
	Stopped, ReplacementCreated, Held bool
	ActionID                          string
}

func (s *Store) ActiveEpicWorkerLivenessTargets(ctx context.Context) ([]EpicWorkerLivenessTarget, error) {
	enabled := s.EnableEpicDedicatedWorkersV2
	if !enabled {
		var err error
		enabled, err = s.DurableEpicDedicatedWorkersV2(ctx)
		if err != nil {
			return nil, err
		}
	}
	if !enabled {
		return nil, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT w.project_id,w.epic_id,w.worker_role,w.worker_identity,
		w.flowbee_identity,w.seat_id,b.binding_id,b.binding_epoch,b.host_id,b.store_id,
		b.tmux_server_domain_id,b.tmux_server_instance_id,b.lifecycle_key,b.target_epoch,b.profile_id,
		b.workspace_root_id,b.workspace_relative_path,b.session_id,b.pane_instance_id,b.agent_run_id,
		CASE WHEN d.state IN ('merged','cleanup_pending','complete','abandoned') OR COALESCE(a.merged,0)=1
		THEN 1 ELSE 0 END
		FROM epic_worker_sessions w
		JOIN driver_session_bindings b ON b.binding_id=w.binding_id AND b.state='active'
		JOIN epic_deliveries d ON d.epic_id=w.epic_id
		LEFT JOIN epic_artifacts a ON a.epic_id=w.epic_id
		WHERE w.state='active' ORDER BY w.project_id,w.epic_id,w.worker_role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EpicWorkerLivenessTarget
	for rows.Next() {
		var item EpicWorkerLivenessTarget
		var terminal int
		if err := rows.Scan(&item.ProjectID, &item.EpicID, &item.WorkerRole, &item.WorkerIdentity,
			&item.FlowbeeIdentity, &item.SeatID, &item.BindingID, &item.BindingEpoch,
			&item.HostID, &item.StoreID, &item.ServerDomainID, &item.ServerID,
			&item.LifecycleKey, &item.TargetEpoch, &item.ProfileID, &item.WorkspaceRootID,
			&item.WorkspaceRelativePath, &item.SessionID, &item.PaneInstanceID,
			&item.AgentRunID, &terminal); err != nil {
			return nil, err
		}
		item.Terminal = terminal == 1
		out = append(out, item)
	}
	return out, rows.Err()
}

// RecordEpicWorkerPresenceUncertain keeps the exact binding authoritative but
// raises a durable visible hold. Only positive exact absence may fence it.
func (s *Store) RecordEpicWorkerPresenceUncertain(ctx context.Context, target EpicWorkerLivenessTarget,
	detail string, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions w
			JOIN driver_session_bindings b ON b.binding_id=w.binding_id
			WHERE w.epic_id=? AND w.worker_role=? AND w.state='active' AND b.binding_id=?
			AND b.state='active' AND b.target_epoch=? AND b.agent_run_id=?`, target.EpicID,
			target.WorkerRole, target.BindingID, target.TargetEpoch, target.AgentRunID).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return nil
		}
		return holdEpicWorkerLivenessTx(ctx, tx, target, "epic_worker_presence_uncertain", detail, now)
	})
}

func holdEpicWorkerLivenessTx(ctx context.Context, tx *sql.Tx, target EpicWorkerLivenessTarget,
	kind, detail string, now time.Time) error {
	stamp := now.UTC().Format(rfc3339)
	dedup := kind + ":" + target.EpicID + ":" + target.WorkerRole
	payload, _ := json.Marshal(map[string]any{"epic_id": target.EpicID, "worker_role": target.WorkerRole,
		"binding_id": target.BindingID, "target_epoch": target.TargetEpoch,
		"agent_run_id": target.AgentRunID, "detail": detail})
	attentionID := kind + "-" + target.EpicID + "-" + target.WorkerRole
	if _, err := tx.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,
		 detail,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES (?,?,?,?,'',10,'open',?,1,?,?,1,?,?,?,?)
		ON CONFLICT DO UPDATE SET occurrences=attention_items.occurrences+1,
		last_seen_at=excluded.last_seen_at,detail=excluded.detail,evidence_json=excluded.evidence_json,
		updated_at=excluded.updated_at WHERE attention_items.state<>'resolved'`, attentionID,
		target.ProjectID, kind, target.EpicID, dedup, string(payload), detail,
		stamp, stamp, stamp, stamp); err != nil {
		return err
	}
	return ensureControlAlertTx(ctx, tx, target.ProjectID, target.EpicID, kind, dedup, string(payload), now)
}

// RecoverEpicWorkerExactAbsence fences the old incarnation under a full CAS.
// Terminal work is closed locally. Non-terminal work receives one immutable,
// higher-epoch lifecycle action and credential generation in the same tx.
func (s *Store) RecoverEpicWorkerExactAbsence(ctx context.Context, target EpicWorkerLivenessTarget,
	now time.Time) (EpicWorkerAbsenceResult, error) {
	var out EpicWorkerAbsenceResult
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var state string
		var currentTarget int64
		if err := tx.QueryRowContext(ctx, `SELECT state,target_epoch FROM epic_worker_sessions
			WHERE epic_id=? AND project_id=? AND worker_role=? AND binding_id=?`, target.EpicID,
			target.ProjectID, target.WorkerRole, target.BindingID).Scan(&state, &currentTarget); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if state != "active" || currentTarget != target.TargetEpoch {
			return nil
		}
		stamp := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
			superseded_at=?,updated_at=? WHERE binding_id=? AND state='active' AND project_id=?
			AND target_epoch=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?`, stamp, stamp,
			target.BindingID, target.ProjectID, target.TargetEpoch, target.SessionID,
			target.PaneInstanceID, target.AgentRunID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		if target.Terminal {
			if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stopped',binding_id=NULL,
				state_due_at='',stopped_at=?,updated_at=? WHERE epic_id=? AND worker_role=? AND state='active'`,
				stamp, stamp, target.EpicID, target.WorkerRole); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='revoked',
				revoked_at=?,updated_at=? WHERE epic_id=? AND worker_role=? AND state<>'revoked'`,
				stamp, stamp, target.EpicID, target.WorkerRole); err != nil {
				return err
			}
			out.Stopped = true
			return nil
		}
		if target.TargetEpoch >= epicWorkerAutomaticRecoveryLimit+1 {
			if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='held',binding_id=NULL,
				state_due_at=?,updated_at=? WHERE epic_id=? AND worker_role=? AND state='active'`,
				now.Add(5*time.Minute).UTC().Format(rfc3339), stamp, target.EpicID, target.WorkerRole); err != nil {
				return err
			}
			out.Held = true
			return holdEpicWorkerLivenessTx(ctx, tx, target, "epic_worker_recovery_exhausted",
				"exact Driver incarnation repeatedly died; automatic replacement budget exhausted", now)
		}
		newTargetEpoch := target.TargetEpoch + 1
		dedup := fmt.Sprintf("epic_worker_recover:%s:%s:%d", target.EpicID, target.WorkerRole, newTargetEpoch)
		h := sha256.Sum256([]byte(dedup))
		actionID := "worker-recover-" + hex.EncodeToString(h[:12])
		payload, _ := json.Marshal(map[string]any{"type": "worker_recover", "project_id": target.ProjectID,
			"epic_id": target.EpicID, "worker_role": target.WorkerRole, "seat_id": target.SeatID,
			"prior_binding_id": target.BindingID, "prior_agent_run_id": target.AgentRunID,
			"target_epoch": newTargetEpoch})
		payloadHash := sha256.Sum256(payload)
		role := DriverBuilderRole
		headSHA, baseSHA := "", ""
		if target.WorkerRole == "reviewer" {
			role = DriverReviewerRole
			if err := tx.QueryRowContext(ctx, `SELECT head_sha,base_sha
				FROM epic_deliveries WHERE epic_id=? AND project_id=?`, target.EpicID,
				target.ProjectID).Scan(&headSHA, &baseSHA); err != nil {
				return err
			}
			if !validGitObjectID(headSHA) {
				return holdEpicWorkerLivenessTx(ctx, tx, target, "epic_worker_workspace_source_missing",
					"reviewer recovery has no immutable artifact head SHA", now)
			}
		} else {
			if err := tx.QueryRowContext(ctx, `SELECT json_extract(bootstrap_payload,'$.source_commit_sha')
				FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role='builder'`,
				target.EpicID, target.ProjectID).Scan(&baseSHA); err != nil {
				return err
			}
			if !validGitObjectID(baseSHA) {
				return holdEpicWorkerLivenessTx(ctx, tx, target, "epic_worker_workspace_source_missing",
					"builder recovery has no immutable source commit SHA", now)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO epic_actions
			(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
			 executor_kind,target_role,target_host_id,target_store_id,target_server_domain_id,target_server_id,
			 lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
			 head_sha,base_sha,next_attempt_at,created_at,updated_at)
			VALUES (?,?,?,'worker_recover','pending',0,?,?,?,'driver_lifecycle',?,?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?)`,
			actionID, target.ProjectID, target.EpicID, dedup, string(payload),
			"sha256:"+hex.EncodeToString(payloadHash[:]), role, target.HostID, target.StoreID,
			target.ServerDomainID, target.ServerID, target.LifecycleKey, newTargetEpoch,
			target.ProfileID, target.WorkspaceRootID, target.WorkspaceRelativePath,
			"worker-recovery:"+target.EpicID+":"+target.WorkerRole, headSHA, baseSHA,
			stamp, stamp, stamp); err != nil {
			return err
		}
		if _, err := s.issueEpicWorkerCredentialGenerationTx(ctx, tx, target.EpicID,
			target.WorkerRole, actionID, true, now); err != nil {
			return err
		}
		if target.WorkerRole == "reviewer" {
			if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='review_pending',lease_id=NULL,
				lease_deadline=NULL,lease_hb_due=NULL,bound_identity=NULL,bound_model_family=NULL,bound_lens=NULL,
				updated_at=? WHERE id=(SELECT review_job_id FROM epic_deliveries WHERE epic_id=?)
				AND state='code_review' AND bound_identity=?`, stamp, target.EpicID,
				target.FlowbeeIdentity); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE leases SET ended_at=?,end_reason='session_lost'
				WHERE job_id=(SELECT review_job_id FROM epic_deliveries WHERE epic_id=?)
				AND ended_at IS NULL AND identity=?`, stamp, target.EpicID, target.FlowbeeIdentity); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET state='review_queued',
				state_version=state_version+1,reviewer_identity='',reviewer_model_family='',review_started_at='',
				last_reviewer_fact_at='',state_entered_at=?,state_due_at=?,updated_at=?
				WHERE epic_id=? AND state='in_review'`, stamp, now.Add(10*time.Minute).UTC().Format(rfc3339),
				stamp, target.EpicID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='ensure_pending',
			target_epoch=?,ensure_action_id=?,binding_id=NULL,state_due_at=?,updated_at=?
			WHERE epic_id=? AND worker_role=? AND state='active'`, newTargetEpoch, actionID,
			now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, target.EpicID, target.WorkerRole); err != nil {
			return err
		}
		out.ReplacementCreated, out.ActionID = true, actionID
		return appendEpicControlEventTx(ctx, tx, target.ProjectID, target.EpicID,
			"epic_worker_incarnation_lost", "", "", 0, "reconciler", string(payload), now)
	})
	return out, err
}

func (s *Store) projectEpicWorkerRecoveryTx(ctx context.Context, tx *sql.Tx,
	action BuilderLifecycleActionProjection, receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	if receipt.Operation != "ensure" || receipt.Status != "ensured" {
		return fmt.Errorf("worker recovery not ensured: %s", receipt.Status)
	}
	id := receipt.IdentityAfter
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerDomainID != action.TargetServerDomainID || id.TmuxServerInstanceID != action.TargetServerID ||
		id.LifecycleOwnership != "driver_managed" || id.LifecycleKey != action.LifecycleKey ||
		id.TargetEpoch != action.TargetEpoch || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return errors.New("Driver worker recovery returned a different incarnation fence")
	}
	var payload struct {
		WorkerRole, SeatID, PriorBindingID, PriorAgentRunID string
		TargetEpoch                                         int64
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(action.Payload), &raw); err != nil {
		return err
	}
	_ = json.Unmarshal(raw["worker_role"], &payload.WorkerRole)
	_ = json.Unmarshal(raw["seat_id"], &payload.SeatID)
	_ = json.Unmarshal(raw["prior_binding_id"], &payload.PriorBindingID)
	_ = json.Unmarshal(raw["prior_agent_run_id"], &payload.PriorAgentRunID)
	_ = json.Unmarshal(raw["target_epoch"], &payload.TargetEpoch)
	if (payload.WorkerRole != "builder" && payload.WorkerRole != "reviewer") ||
		payload.PriorBindingID == "" || payload.PriorAgentRunID == "" || payload.TargetEpoch != action.TargetEpoch {
		return errors.New("worker recovery payload is incomplete")
	}
	var workerIdentity, family, state, ensureActionID string
	var targetEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT worker_identity,model_family,state,ensure_action_id,target_epoch
		FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role=?`, action.EpicID,
		action.ProjectID, payload.WorkerRole).Scan(&workerIdentity, &family, &state,
		&ensureActionID, &targetEpoch); err != nil {
		return err
	}
	if state != "ensure_pending" || ensureActionID != action.ActionID || targetEpoch != action.TargetEpoch {
		return errors.New("worker recovery lost immutable replacement fence")
	}
	if id.Provider != "" && id.Provider != family {
		return errors.New("worker recovery provider changed admitted family")
	}
	role := DriverBuilderRole
	seatID := ""
	if payload.WorkerRole == "reviewer" {
		role, seatID = DriverReviewerRole, payload.SeatID
	}
	prior, err := latestDriverSessionBindingTx(ctx, tx, action.ProjectID, workerIdentity, role)
	if err != nil {
		return err
	}
	if prior.BindingID != payload.PriorBindingID || prior.AgentRunID != payload.PriorAgentRunID ||
		prior.TargetEpoch >= action.TargetEpoch {
		return errors.New("worker recovery prior incarnation fence changed")
	}
	stamp := now.UTC().Format(rfc3339)
	binding := DriverSessionBinding{ProjectID: action.ProjectID, WorkerIdentity: workerIdentity,
		Role: role, SeatID: seatID, BindingEpoch: prior.BindingEpoch + 1,
		HostID: id.HostID, StoreID: id.StoreID, TmuxServerDomainID: id.TmuxServerDomainID,
		TmuxServerInstanceID: id.TmuxServerInstanceID, LifecycleOwnership: id.LifecycleOwnership,
		LifecycleKey: id.LifecycleKey, TargetEpoch: id.TargetEpoch, ProfileID: action.ProfileID,
		WorkspaceRootID: action.WorkspaceRootID, WorkspaceRelativePath: action.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: family, ConversationID: id.ConversationID, ObservedAt: now}
	binding.BindingID = driverBindingID(binding, binding.BindingEpoch)
	if _, err := tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
		(binding_id,project_id,worker_identity,role,seat_id,binding_epoch,state,host_id,store_id,
		 tmux_server_domain_id,tmux_server_instance_id,lifecycle_ownership,lifecycle_key,target_epoch,
		 profile_id,workspace_root_id,workspace_relative_path,session_id,pane_instance_id,agent_run_id,
		 provider,conversation_id,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, binding.BindingID,
		binding.ProjectID, binding.WorkerIdentity, binding.Role, binding.SeatID, binding.BindingEpoch,
		binding.HostID, binding.StoreID, binding.TmuxServerDomainID, binding.TmuxServerInstanceID,
		binding.LifecycleOwnership, binding.LifecycleKey, binding.TargetEpoch, binding.ProfileID,
		binding.WorkspaceRootID, binding.WorkspaceRelativePath, binding.SessionID, binding.PaneInstanceID,
		binding.AgentRunID, binding.Provider, binding.ConversationID, stamp, stamp, stamp); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='active',binding_id=?,
		bootstrap_state='route_pending',state_due_at='',updated_at=? WHERE epic_id=? AND worker_role=?
		AND state='ensure_pending' AND ensure_action_id=? AND target_epoch=?`, binding.BindingID, stamp,
		action.EpicID, payload.WorkerRole, action.ActionID, action.TargetEpoch)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("worker recovery replacement was superseded")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE attention_items SET state='resolved',resolution='replacement_ensured',
		resolved_at=?,updated_at=? WHERE project_id=? AND epic_id=? AND kind IN
		('epic_worker_presence_uncertain','epic_worker_recovery_exhausted')
		AND state IN ('open','leased','delivering','awaiting_ack')`, stamp, stamp,
		action.ProjectID, action.EpicID); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, action.ProjectID, action.EpicID,
		"epic_worker_recovered", "", "", 0, "driver", action.Payload, now)
}
