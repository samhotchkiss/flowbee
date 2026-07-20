package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ProjectActorLivenessTarget is the exact post-ack managed actor incarnation.
// Managed actor replacement remains held until the same v3 bootstrap and
// credential law used for workers is available; Flowbee must not silently fall
// back to an under-specified v2 Ensure.
type ProjectActorLivenessTarget struct {
	ProjectID, Role, ActorID, BindingID, InstanceRef                    string
	HostID, StoreID, ServerDomainID, ServerID, LifecycleKey, ProfileID  string
	WorkspaceRootID, WorkspaceRelativePath                              string
	SessionID, PaneInstanceID, AgentRunID                               string
	TargetEpoch, BindingEpoch, LifecycleStateVersion, RouteStateVersion int64
	LifecycleOwnership                                                  string
	ManagedRecoveryProfileID                                            string
	ManagedRecoveryWorkspaceRootID                                      string
	ManagedRecoveryWorkspaceRelativePath                                string
}

func (s *Store) ActiveManagedProjectActorLivenessTargets(ctx context.Context) ([]ProjectActorLivenessTarget, error) {
	targets, err := s.ActiveProjectActorLivenessTargets(ctx)
	if err != nil {
		return nil, err
	}
	out := targets[:0]
	for _, target := range targets {
		if target.LifecycleOwnership == "driver_managed" {
			out = append(out, target)
		}
	}
	return out, nil
}

// ActiveProjectActorLivenessTargets includes adopted external Interactors so a
// dead human-facing session is recovered through the same Driver-owned v3
// lifecycle boundary. The LEFT JOIN is intentional: a legacy adopted row with
// no durable recovery policy remains visible to the runtime as an error and is
// never silently promoted from guessed defaults.
func (s *Store) ActiveProjectActorLivenessTargets(ctx context.Context) ([]ProjectActorLivenessTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT l.project_id,l.role,l.actor_id,l.active_binding_id,
		l.instance_ref,b.host_id,b.store_id,b.tmux_server_domain_id,b.tmux_server_instance_id,
		b.lifecycle_key,b.target_epoch,b.profile_id,b.workspace_root_id,b.workspace_relative_path,
		b.session_id,b.pane_instance_id,b.agent_run_id,b.binding_epoch,l.state_version,l.route_state_version,
		b.lifecycle_ownership,COALESCE(p.profile_id,''),COALESCE(p.workspace_root_id,''),
		COALESCE(p.workspace_relative_path,'')
		FROM project_actor_lifecycles l JOIN driver_session_bindings b
		ON b.binding_id=l.active_binding_id AND b.state='active'
		LEFT JOIN project_actor_managed_recovery_policies p
		ON p.project_id=l.project_id AND p.role=l.role AND p.actor_id=l.actor_id
		WHERE l.state='active' AND l.desired_state='active'
		AND (l.lifecycle_ownership='driver_managed' OR
		     (l.lifecycle_ownership='external_observed' AND l.role='interactor'))
		ORDER BY l.project_id,l.role,l.actor_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProjectActorLivenessTarget
	for rows.Next() {
		var item ProjectActorLivenessTarget
		if err := rows.Scan(&item.ProjectID, &item.Role, &item.ActorID, &item.BindingID,
			&item.InstanceRef, &item.HostID, &item.StoreID, &item.ServerDomainID, &item.ServerID,
			&item.LifecycleKey, &item.TargetEpoch, &item.ProfileID, &item.WorkspaceRootID,
			&item.WorkspaceRelativePath, &item.SessionID, &item.PaneInstanceID, &item.AgentRunID,
			&item.BindingEpoch, &item.LifecycleStateVersion, &item.RouteStateVersion,
			&item.LifecycleOwnership, &item.ManagedRecoveryProfileID,
			&item.ManagedRecoveryWorkspaceRootID,
			&item.ManagedRecoveryWorkspaceRelativePath); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (target ProjectActorLivenessTarget) ValidateManagedRecovery() error {
	if target.Role != DriverInteractorRole || target.LifecycleOwnership != "external_observed" ||
		target.ServerDomainID != "default" || target.ManagedRecoveryProfileID != "claude_interactor_managed" ||
		target.ManagedRecoveryWorkspaceRootID == "" || target.ManagedRecoveryWorkspaceRelativePath == "" {
		return errors.New("adopted Interactor has no exact managed v3 recovery policy")
	}
	return nil
}

// PromoteAdoptedInteractorExactAbsence is the crash boundary for external to
// managed promotion. The stale binding fence, higher-epoch desired state, Q3
// launch material, and immutable Ensure action commit in one transaction.
func (s *Store) PromoteAdoptedInteractorExactAbsence(ctx context.Context,
	target ProjectActorLivenessTarget, now time.Time) (ProjectActorLifecycleAction, bool, error) {
	if err := target.ValidateManagedRecovery(); err != nil {
		return ProjectActorLifecycleAction{}, false, err
	}
	var action ProjectActorLifecycleAction
	promoted := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		lifecycle, err := projectActorLifecycleTx(ctx, tx, target.ProjectID, target.Role, target.ActorID)
		if err != nil {
			return err
		}
		if lifecycle.State != "active" || lifecycle.DesiredState != "active" ||
			lifecycle.LifecycleOwnership != "external_observed" || lifecycle.StateVersion != target.LifecycleStateVersion ||
			lifecycle.ActiveBindingID != target.BindingID || lifecycle.TargetEpoch != target.TargetEpoch {
			return nil // a newer fact/action already fenced this observation
		}
		var profile, rootID, relativePath string
		if err := tx.QueryRowContext(ctx, `SELECT profile_id,workspace_root_id,workspace_relative_path
			FROM project_actor_managed_recovery_policies WHERE project_id=? AND role=? AND actor_id=?`,
			target.ProjectID, target.Role, target.ActorID).Scan(&profile, &rootID, &relativePath); err != nil {
			return err
		}
		if profile != target.ManagedRecoveryProfileID || rootID != target.ManagedRecoveryWorkspaceRootID ||
			relativePath != target.ManagedRecoveryWorkspaceRelativePath {
			return ErrProjectActorLifecycleConflict
		}
		command := ProjectActorLifecycleCommand{ProjectID: target.ProjectID, Role: target.Role,
			ActorID: target.ActorID, ExpectedRouteStateVersion: target.RouteStateVersion,
			ExpectedLifecycleStateVersion: target.LifecycleStateVersion, Operation: "ensure",
			IdempotencyKey: fmt.Sprintf("adopted-interactor-recover:%s:%s:%d", target.ProjectID,
				target.ActorID, target.TargetEpoch+1), InstanceRef: target.InstanceRef,
			TargetHostID: target.HostID, TargetStoreID: target.StoreID,
			TargetServerDomainID: target.ServerDomainID, TargetServerID: target.ServerID,
			LifecycleOwnership: "driver_managed", LifecycleKey: target.LifecycleKey,
			TargetEpoch: target.TargetEpoch + 1, ProfileID: profile,
			WorkspaceRootID: rootID, WorkspaceRelativePath: relativePath}
		if err := validateActorCommandShape(command); err != nil {
			return err
		}
		payload, payloadSHA, err := actorCommandPayload(command)
		if err != nil {
			return err
		}
		stamp := formatActorTime(now)
		res, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
			superseded_at=?,updated_at=? WHERE binding_id=? AND project_id=? AND worker_identity=?
			AND role=? AND binding_epoch=? AND state='active' AND host_id=? AND store_id=?
			AND tmux_server_domain_id=? AND tmux_server_instance_id=? AND lifecycle_ownership='external_observed'
			AND lifecycle_key=? AND target_epoch=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?`,
			stamp, stamp, target.BindingID, target.ProjectID, target.ActorID, target.Role,
			target.BindingEpoch, target.HostID, target.StoreID, target.ServerDomainID, target.ServerID,
			target.LifecycleKey, target.TargetEpoch, target.SessionID, target.PaneInstanceID, target.AgentRunID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil
		}
		next := lifecycle
		next.RouteStateVersion = target.RouteStateVersion
		next.StateVersion++
		next.ActionGeneration++
		next.DesiredOperation = "ensure"
		next.LifecycleOwnership = "driver_managed"
		next.State = actorAwaitingState("ensure")
		next.IntentIdempotencyKey, next.IntentPayload, next.IntentPayloadSHA = command.IdempotencyKey, payload, payloadSHA
		next.InstanceRef, next.TargetHostID, next.TargetStoreID = target.InstanceRef, target.HostID, target.StoreID
		next.TargetServerDomainID, next.TargetServerID = target.ServerDomainID, target.ServerID
		next.LifecycleKey, next.TargetEpoch, next.ProfileID = target.LifecycleKey, target.TargetEpoch+1, profile
		next.WorkspaceRootID, next.WorkspaceRelativePath, next.ExternalWatchID = rootID, relativePath, ""
		next.BootstrapFormat, next.BootstrapPayload, next.BootstrapSHA256 = "", "", ""
		next.CredentialInstallRef, next.CredentialGeneration, next.CredentialEnvelopeRef = "", 0, ""
		next.CredentialPayloadSHA256, next.CredentialExpiresAt = "", ""
		next.CredentialEnvelopeDeletedAt, next.CredentialRevokedAt, next.PresentationName = "", "", ""
		next.ExpectedBindingID, next.ExpectedBindingEpoch = "", 0
		next.ExpectedSessionID, next.ExpectedPaneInstanceID, next.ExpectedAgentRunID = "", "", ""
		next.ActiveBindingID, next.CurrentActionID = "", ""
		next.StateEnteredAt, next.StateDueAt, next.FactProgressAt = now,
			now.Add(projectActorLifecycleActionTimeout), now
		next.ReturnState, next.HoldKind, next.HoldReason, next.LastError = "", "", "", ""
		next.AlertPending, next.UpdatedAt = false, now
		if err := s.prepareProjectActorQ3Tx(ctx, tx, command, lifecycle, nil, &next, now); err != nil {
			return err
		}
		if err := upsertProjectActorLifecycleTx(ctx, tx, next); err != nil {
			return err
		}
		action, err = insertProjectActorActionTx(ctx, tx, next, command.IdempotencyKey, payload, payloadSHA, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id=?,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND state_version=? AND current_action_id=''`,
			action.ID, stamp, next.ProjectID, next.Role, next.ActorID, next.StateVersion); err != nil {
			return err
		}
		next.CurrentActionID = action.ID
		if err := appendProjectActorControlEventTx(ctx, tx, next,
			"adopted_interactor_promoted_after_exact_absence", map[string]any{
				"prior_binding_id": target.BindingID, "prior_agent_run_id": target.AgentRunID,
				"action_id": action.ID, "target_epoch": action.TargetEpoch}, now); err != nil {
			return err
		}
		alertPayload, err := json.Marshal(map[string]any{
			"project_id": target.ProjectID, "role": target.Role, "actor_id": target.ActorID,
			"prior_binding_id": target.BindingID, "prior_agent_run_id": target.AgentRunID,
			"recovery_action_id": action.ID, "recovery_target_epoch": action.TargetEpoch,
		})
		if err != nil {
			return err
		}
		if err := ensureControlAlertTx(ctx, tx, target.ProjectID, "",
			"project_actor_incarnation_recovered",
			fmt.Sprintf("project_actor_incarnation_recovered:%s:%s:%d", target.ProjectID,
				target.Role, action.TargetEpoch), string(alertPayload), now); err != nil {
			return err
		}
		promoted = true
		return nil
	})
	return action, promoted, err
}

// HoldManagedProjectActorExactAbsence fences the dead incarnation and exposes
// the replacement gap. A future v3 actor-material transition can resume this
// hold with target_epoch+1; until then no legacy Ensure may recreate it.
func (s *Store) HoldManagedProjectActorExactAbsence(ctx context.Context,
	target ProjectActorLivenessTarget, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		stamp := formatActorTime(now)
		res, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
			superseded_at=?,updated_at=? WHERE binding_id=? AND project_id=? AND worker_identity=?
			AND role=? AND binding_epoch=? AND state='active' AND host_id=? AND store_id=?
			AND tmux_server_domain_id=? AND tmux_server_instance_id=? AND lifecycle_key=?
			AND target_epoch=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?`, stamp, stamp,
			target.BindingID, target.ProjectID, target.ActorID, target.Role, target.BindingEpoch,
			target.HostID, target.StoreID, target.ServerDomainID, target.ServerID, target.LifecycleKey,
			target.TargetEpoch, target.SessionID, target.PaneInstanceID, target.AgentRunID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil // a newer binding already fenced this observation
		}
		reason := "exact Driver actor incarnation is absent; v3 actor bootstrap/credential replacement is required"
		res, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state='held',
			state_version=state_version+1,active_binding_id='',state_entered_at=?,state_due_at=?,
			fact_progress_at=?,return_state='active',hold_kind='actor_incarnation_lost',
			hold_reason=?,last_error=?,alert_pending=1,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND state='active' AND state_version=?
			AND active_binding_id=?`, stamp, formatActorTime(now.Add(5*time.Minute)), stamp,
			reason, reason, stamp, target.ProjectID, target.Role, target.ActorID,
			target.LifecycleStateVersion, target.BindingID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorLifecycleConflict
		}
		dedup := "project_actor_incarnation_lost:" + target.ProjectID + ":" + target.Role
		payload, _ := json.Marshal(map[string]any{"project_id": target.ProjectID, "role": target.Role,
			"actor_id": target.ActorID, "binding_id": target.BindingID,
			"target_epoch": target.TargetEpoch, "agent_run_id": target.AgentRunID,
			"required_next_action": "v3_actor_replacement_with_higher_target_and_credential_epochs"})
		attentionID := "actor-incarnation-lost-" + target.ProjectID + "-" + target.Role
		if _, err := tx.ExecContext(ctx, `INSERT INTO attention_items
			(id,project_id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,
			 detail,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
			VALUES (?,?,'project_actor_incarnation_lost','','',10,'open',?,1,?,?,1,?,?,?,?)
			ON CONFLICT DO NOTHING`, attentionID, target.ProjectID, dedup, string(payload), reason,
			stamp, stamp, stamp, stamp); err != nil {
			return err
		}
		if err := ensureControlAlertTx(ctx, tx, target.ProjectID, "",
			"project_actor_incarnation_lost", dedup, string(payload), now); err != nil {
			return err
		}
		lifecycle, err := projectActorLifecycleTx(ctx, tx, target.ProjectID, target.Role, target.ActorID)
		if err != nil {
			return err
		}
		return appendProjectActorControlEventTx(ctx, tx, lifecycle,
			"project_actor_incarnation_lost", map[string]any{"binding_id": target.BindingID}, now)
	})
}
