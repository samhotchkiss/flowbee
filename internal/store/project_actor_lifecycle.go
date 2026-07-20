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

var (
	ErrProjectActorLifecycleConflict = errors.New("project actor lifecycle conflict")
	ErrProjectActorActionStale       = errors.New("project actor lifecycle action is stale")
)

const projectActorLifecycleActionTimeout = 10 * time.Minute

// ProjectActorLifecycle is Flowbee's durable desired and projected state for
// one project-scoped actor. ExpectedPaneInstanceID is Driver's stable UUID; the
// schema intentionally has no raw tmux %N selector or tmux name.
type ProjectActorLifecycle struct {
	ProjectID, Role, ActorID                              string
	RouteStateVersion, StateVersion, ActionGeneration     int64
	DesiredState, DesiredOperation, LifecycleOwnership    string
	State                                                 string
	IntentIdempotencyKey, IntentPayload, IntentPayloadSHA string
	InstanceRef, TargetHostID, TargetStoreID              string
	TargetServerDomainID, TargetServerID                  string
	LifecycleKey                                          string
	TargetEpoch                                           int64
	ProfileID, WorkspaceRootID, WorkspaceRelativePath     string
	ExternalWatchID                                       string
	ExpectedBindingID                                     string
	ExpectedBindingEpoch                                  int64
	ExpectedSessionID, ExpectedPaneInstanceID             string
	ExpectedAgentRunID                                    string
	ActiveBindingID, CurrentActionID                      string
	RecoveryCount                                         int
	StateEnteredAt, StateDueAt, FactProgressAt            time.Time
	ReturnState, HoldKind, HoldReason, LastError          string
	AlertPending                                          bool
	CreatedAt, UpdatedAt                                  time.Time
}

type ProjectActorLifecycleAction struct {
	ID, ProjectID, Role, ActorID                  string
	RouteStateVersion, IntentStateVersion         int64
	ActionGeneration                              int64
	Operation, State                              string
	ActionEpoch                                   int64
	IdempotencyKey, DedupKey, Payload, PayloadSHA string
	InstanceRef, TargetHostID, TargetStoreID      string
	TargetServerDomainID, TargetServerID          string
	LifecycleOwnership, LifecycleKey              string
	TargetEpoch                                   int64
	ProfileID, WorkspaceRootID                    string
	WorkspaceRelativePath, ExternalWatchID        string
	ExpectedBindingID                             string
	ExpectedBindingEpoch                          int64
	ExpectedSessionID, ExpectedPaneInstanceID     string
	ExpectedAgentRunID, LeaseID                   string
	LeaseEpoch                                    int64
	Attempts, RecoveryCount                       int
	NextAttemptAt, ClaimDeadlineAt                time.Time
	ClaimOwner, LastError                         string
	CreatedAt, UpdatedAt                          time.Time
}

// ProjectActorLifecycleCommand is accepted only by the commit-before-effect
// transaction. Adopt carries a Driver watch UUID plus exact stable identities;
// Ensure carries only a managed profile/workspace target. Stop, Release, and
// Reattach derive their exact target from the active binding in the transaction.
type ProjectActorLifecycleCommand struct {
	ProjectID, Role, ActorID                  string
	ExpectedRouteStateVersion                 int64
	ExpectedLifecycleStateVersion             int64
	Operation, IdempotencyKey                 string
	InstanceRef                               string
	TargetHostID, TargetStoreID               string
	TargetServerDomainID, TargetServerID      string
	LifecycleOwnership, LifecycleKey          string
	TargetEpoch                               int64
	ProfileID, WorkspaceRootID                string
	WorkspaceRelativePath, ExternalWatchID    string
	ExpectedSessionID, ExpectedPaneInstanceID string
	ExpectedAgentRunID                        string
}

type ProjectActorLifecycleIdentity struct {
	HostID, StoreID, TmuxServerDomainID, TmuxServerInstanceID string
	LifecycleOwnership, LifecycleKey                          string
	TargetEpoch                                               int64
	SessionID, PaneInstanceID, AgentRunID                     string
	Provider, ConversationID                                  string
}

type ProjectActorLifecycleReceiptProjection struct {
	ActionID, Operation, LifecycleKey, Status string
	ActionEpoch, TargetEpoch                  int64
	LeaseID                                   string
	LeaseEpoch                                int64
	TmuxServerDomainID                        string
	ExternalWatchID, AbsenceObservedAt        string
	IdentityBefore, IdentityAfter             ProjectActorLifecycleIdentity
}

// ProjectActorLifecycleReceipt is the durable copy of Driver lifecycle
// evidence. It is committed before projection so a restart can resume the fold
// without asking Driver to repeat a possibly mutating operation.
type ProjectActorLifecycleReceipt struct {
	ID, ActionID, Operation, LifecycleKey, Status string
	ActionEpoch, TargetEpoch                      int64
	LeaseID                                       string
	LeaseEpoch                                    int64
	TmuxServerDomainID, ExternalWatchID           string
	IdentityBefore, IdentityAfter                 ProjectActorLifecycleIdentity
	AbsenceObservedAt, DiagnosticCode             string
	CreatedAt, UpdatedAt                          time.Time
}

func actorAwaitingState(operation string) string { return "awaiting_" + operation }

func actorTerminalState(operation string) string {
	switch operation {
	case "ensure", "adopt", "reattach":
		return "active"
	case "stop":
		return "stopped"
	case "release":
		return "released"
	default:
		return ""
	}
}

func actorExecutingState(operation string) string {
	switch operation {
	case "ensure":
		return "ensuring"
	case "adopt":
		return "adopting"
	case "reattach":
		return "reattaching"
	case "stop":
		return "stopping"
	case "release":
		return "releasing"
	default:
		return ""
	}
}

func actorVerifyingState(operation string) string { return "verifying_" + operation }

func normalizeProjectActorLifecycleCommand(command ProjectActorLifecycleCommand) (ProjectActorLifecycleCommand, error) {
	command.ProjectID = strings.TrimSpace(command.ProjectID)
	command.Role = strings.TrimSpace(command.Role)
	command.ActorID = strings.TrimSpace(command.ActorID)
	command.Operation = strings.TrimSpace(command.Operation)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	if command.ProjectID == "" || command.ActorID == "" || command.IdempotencyKey == "" ||
		(command.Role != DriverInteractorRole && command.Role != DriverOrchestratorRole) ||
		command.ExpectedRouteStateVersion < 1 || len(command.IdempotencyKey) > 200 {
		return command, ErrProjectActorLifecycleConflict
	}
	switch command.Operation {
	case "ensure", "stop", "reattach", "adopt", "release":
	default:
		return command, ErrProjectActorLifecycleConflict
	}
	return command, nil
}

// CommitProjectActorLifecycleIntent atomically commits desired state and its
// immutable Driver action. It performs no Driver call. A lost CLI/API response
// is replayable under the same payload-bound idempotency key.
func (s *Store) CommitProjectActorLifecycleIntent(ctx context.Context, command ProjectActorLifecycleCommand,
	now time.Time) (ProjectActorLifecycle, ProjectActorLifecycleAction, error) {
	command, err := normalizeProjectActorLifecycleCommand(command)
	if err != nil {
		return ProjectActorLifecycle{}, ProjectActorLifecycleAction{}, err
	}
	var lifecycle ProjectActorLifecycle
	var action ProjectActorLifecycleAction
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var routeActor, routeState string
		var routeVersion int64
		if err := tx.QueryRowContext(ctx, `SELECT actor_id,state,state_version FROM project_actor_routes
			WHERE project_id=? AND role=?`, command.ProjectID, command.Role).
			Scan(&routeActor, &routeState, &routeVersion); err != nil {
			return ErrProjectActorLifecycleConflict
		}
		if routeState != "active" || routeVersion != command.ExpectedRouteStateVersion {
			return ErrProjectActorLifecycleConflict
		}

		existing, existingErr := projectActorLifecycleTx(ctx, tx, command.ProjectID, command.Role, command.ActorID)
		if existingErr != nil && !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		if routeActor != command.ActorID {
			// Route replacement activates the successor before its lifecycle can
			// run. The predecessor may still be retired under the successor route
			// version, but it may never be reactivated or reattached.
			if (command.Operation != "stop" && command.Operation != "release") || existingErr != nil ||
				existing.RouteStateVersion >= routeVersion || existing.State != "active" {
				return ErrProjectActorLifecycleConflict
			}
		}

		if command.Operation == "stop" || command.Operation == "release" || command.Operation == "reattach" {
			binding, err := activeDriverSessionBindingTx(ctx, tx, command.ProjectID, command.ActorID, command.Role)
			if err != nil {
				return ErrProjectActorLifecycleConflict
			}
			if existingErr != nil || existing.ActiveBindingID != binding.BindingID {
				return ErrProjectActorLifecycleConflict
			}
			command.TargetHostID, command.TargetStoreID = binding.HostID, binding.StoreID
			command.TargetServerDomainID, command.TargetServerID = binding.TmuxServerDomainID, binding.TmuxServerInstanceID
			command.LifecycleOwnership, command.LifecycleKey = binding.LifecycleOwnership, binding.LifecycleKey
			command.TargetEpoch, command.ProfileID = binding.TargetEpoch, binding.ProfileID
			command.WorkspaceRootID, command.WorkspaceRelativePath = binding.WorkspaceRootID, binding.WorkspaceRelativePath
			command.ExternalWatchID = binding.ExternalWatchID
			command.ExpectedSessionID, command.ExpectedPaneInstanceID = binding.SessionID, binding.PaneInstanceID
			command.ExpectedAgentRunID = binding.AgentRunID
			if command.InstanceRef == "" {
				if err := tx.QueryRowContext(ctx, `SELECT instance_ref FROM driver_instances
					WHERE host_id=? AND store_id=? AND tmux_server_domain_id=? AND state='live'`,
					binding.HostID, binding.StoreID, binding.TmuxServerDomainID).Scan(&command.InstanceRef); err != nil {
					return ErrProjectActorLifecycleConflict
				}
			}
		}
		if err := validateActorCommandShape(command); err != nil {
			return err
		}

		payload, payloadSHA, err := actorCommandPayload(command)
		if err != nil {
			return err
		}
		if existingErr == nil && existing.IntentIdempotencyKey == command.IdempotencyKey {
			if existing.IntentPayloadSHA != payloadSHA {
				return ErrProjectActorLifecycleConflict
			}
			lifecycle = existing
			if existing.CurrentActionID == "" {
				return nil
			}
			action, err = projectActorActionByIDTx(ctx, tx, existing.CurrentActionID)
			return err
		}
		if replay, replayErr := actorActionByIdempotencyTx(ctx, tx, command.ProjectID, command.IdempotencyKey); replayErr == nil {
			if replay.PayloadSHA != payloadSHA {
				return ErrProjectActorLifecycleConflict
			}
			action = replay
			lifecycle, err = projectActorLifecycleTx(ctx, tx, command.ProjectID, command.Role, command.ActorID)
			return err
		} else if !errors.Is(replayErr, sql.ErrNoRows) {
			return replayErr
		}
		if existingErr == nil {
			if command.ExpectedLifecycleStateVersion < 1 || existing.StateVersion != command.ExpectedLifecycleStateVersion {
				return ErrProjectActorLifecycleConflict
			}
		} else if command.ExpectedLifecycleStateVersion != 0 {
			return ErrProjectActorLifecycleConflict
		}

		if existingErr == nil && existing.CurrentActionID != "" {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM project_actor_lifecycle_actions WHERE id=?`,
				existing.CurrentActionID).Scan(&state); err != nil || state == "pending" || state == "delivering" || state == "verifying" {
				return ErrProjectActorLifecycleConflict
			}
		}
		generation, stateVersion, created := int64(1), int64(1), now
		if existingErr == nil {
			if command.TargetEpoch < existing.TargetEpoch {
				return ErrProjectActorLifecycleConflict
			}
			generation, stateVersion, created = existing.ActionGeneration+1, existing.StateVersion+1, existing.CreatedAt
		}
		desired := "active"
		if command.Operation == "stop" || command.Operation == "release" {
			desired = "retired"
		}
		lifecycle = ProjectActorLifecycle{
			ProjectID: command.ProjectID, Role: command.Role, ActorID: command.ActorID,
			RouteStateVersion: routeVersion, StateVersion: stateVersion, ActionGeneration: generation,
			DesiredState: desired, DesiredOperation: command.Operation, LifecycleOwnership: command.LifecycleOwnership,
			State: actorAwaitingState(command.Operation), InstanceRef: command.InstanceRef,
			IntentIdempotencyKey: command.IdempotencyKey, IntentPayload: payload, IntentPayloadSHA: payloadSHA,
			TargetHostID: command.TargetHostID, TargetStoreID: command.TargetStoreID,
			TargetServerDomainID: command.TargetServerDomainID, TargetServerID: command.TargetServerID,
			LifecycleKey: command.LifecycleKey, TargetEpoch: command.TargetEpoch, ProfileID: command.ProfileID,
			WorkspaceRootID: command.WorkspaceRootID, WorkspaceRelativePath: command.WorkspaceRelativePath,
			ExternalWatchID: command.ExternalWatchID, ExpectedSessionID: command.ExpectedSessionID,
			ExpectedPaneInstanceID: command.ExpectedPaneInstanceID, ExpectedAgentRunID: command.ExpectedAgentRunID,
			StateEnteredAt: now, StateDueAt: now.Add(projectActorLifecycleActionTimeout), FactProgressAt: now,
			CreatedAt: created, UpdatedAt: now,
		}
		if existingErr == nil {
			lifecycle.ExpectedBindingID, lifecycle.ExpectedBindingEpoch = existing.ActiveBindingID, existing.ExpectedBindingEpoch
			lifecycle.ActiveBindingID = existing.ActiveBindingID
			if command.Operation == "stop" || command.Operation == "release" || command.Operation == "reattach" {
				binding, _ := activeDriverSessionBindingTx(ctx, tx, command.ProjectID, command.ActorID, command.Role)
				lifecycle.ExpectedBindingID, lifecycle.ExpectedBindingEpoch = binding.BindingID, binding.BindingEpoch
			}
		}
		var priorActor string
		priorErr := sql.ErrNoRows
		if desired == "active" {
			priorErr = tx.QueryRowContext(ctx, `SELECT actor_id FROM project_actor_lifecycles
				WHERE project_id=? AND role=? AND actor_id<>? AND state NOT IN ('stopped','released')
				ORDER BY updated_at DESC LIMIT 1`, command.ProjectID, command.Role, command.ActorID).Scan(&priorActor)
		}
		if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
			return priorErr
		}
		if priorErr == nil {
			lifecycle.State = "held"
			lifecycle.ReturnState = actorAwaitingState(command.Operation)
			lifecycle.HoldKind = "prior_actor_retirement"
			lifecycle.HoldReason = "prior actor " + priorActor + " must be stopped or released"
			lifecycle.StateDueAt = now.Add(time.Minute)
			if err := upsertProjectActorLifecycleTx(ctx, tx, lifecycle); err != nil {
				return err
			}
			return appendProjectActorControlEventTx(ctx, tx, lifecycle, "project_actor_lifecycle_held",
				map[string]any{"hold_kind": lifecycle.HoldKind, "prior_actor_id": priorActor}, now)
		}
		if err := upsertProjectActorLifecycleTx(ctx, tx, lifecycle); err != nil {
			return err
		}
		action, err = insertProjectActorActionTx(ctx, tx, lifecycle, command.IdempotencyKey, payload, payloadSHA, now)
		if err != nil {
			return err
		}
		lifecycle.CurrentActionID = action.ID
		if _, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id=?,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND state_version=?`, action.ID, formatActorTime(now),
			lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID, lifecycle.StateVersion); err != nil {
			return err
		}
		return appendProjectActorControlEventTx(ctx, tx, lifecycle, "project_actor_lifecycle_intent_committed",
			map[string]any{"action_id": action.ID, "operation": action.Operation}, now)
	})
	return lifecycle, action, err
}

func validateActorCommandShape(c ProjectActorLifecycleCommand) error {
	completeTarget := c.InstanceRef != "" && c.TargetHostID != "" && c.TargetStoreID != "" &&
		c.TargetServerDomainID != "" && c.TargetServerID != "" && c.LifecycleKey != "" &&
		c.TargetEpoch > 0 && c.ProfileID != ""
	exact := c.ExpectedSessionID != "" && c.ExpectedPaneInstanceID != "" && c.ExpectedAgentRunID != ""
	if !completeTarget {
		return ErrProjectActorLifecycleConflict
	}
	switch c.Operation {
	case "ensure":
		if c.LifecycleOwnership != "driver_managed" || c.ExternalWatchID != "" || exact ||
			c.WorkspaceRootID == "" || c.WorkspaceRelativePath == "" {
			return ErrProjectActorLifecycleConflict
		}
	case "adopt":
		if c.LifecycleOwnership != "external_observed" || c.ExternalWatchID == "" || !exact ||
			c.WorkspaceRootID != "" || c.WorkspaceRelativePath != "" {
			return ErrProjectActorLifecycleConflict
		}
	case "stop":
		if c.LifecycleOwnership != "driver_managed" || c.ExternalWatchID != "" || !exact ||
			c.WorkspaceRootID == "" || c.WorkspaceRelativePath == "" {
			return ErrProjectActorLifecycleConflict
		}
	case "release":
		if c.LifecycleOwnership != "external_observed" || c.ExternalWatchID == "" || !exact ||
			c.WorkspaceRootID != "" || c.WorkspaceRelativePath != "" {
			return ErrProjectActorLifecycleConflict
		}
	case "reattach":
		if !exact || (c.LifecycleOwnership == "external_observed" && (c.ExternalWatchID == "" || c.WorkspaceRootID != "" || c.WorkspaceRelativePath != "")) ||
			(c.LifecycleOwnership == "driver_managed" && (c.ExternalWatchID != "" || c.WorkspaceRootID == "" || c.WorkspaceRelativePath == "")) {
			return ErrProjectActorLifecycleConflict
		}
	}
	return nil
}

func actorCommandPayload(c ProjectActorLifecycleCommand) (string, string, error) {
	value := struct {
		ProjectID, Role, ActorID, Operation, InstanceRef                                    string
		TargetHostID, TargetStoreID, TargetServerDomainID, TargetServerID                   string
		LifecycleOwnership, LifecycleKey, ProfileID, WorkspaceRootID, WorkspaceRelativePath string
		ExternalWatchID, ExpectedSessionID, ExpectedPaneInstanceID, ExpectedAgentRunID      string
		RouteStateVersion, TargetEpoch                                                      int64
	}{c.ProjectID, c.Role, c.ActorID, c.Operation, c.InstanceRef, c.TargetHostID, c.TargetStoreID,
		c.TargetServerDomainID, c.TargetServerID, c.LifecycleOwnership, c.LifecycleKey,
		c.ProfileID, c.WorkspaceRootID, c.WorkspaceRelativePath, c.ExternalWatchID,
		c.ExpectedSessionID, c.ExpectedPaneInstanceID, c.ExpectedAgentRunID,
		c.ExpectedRouteStateVersion, c.TargetEpoch}
	payload, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(payload)
	return string(payload), "sha256:" + hex.EncodeToString(sum[:]), nil
}

func formatActorTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(rfc3339)
}

type projectActorScanner interface{ Scan(...any) error }

const projectActorLifecycleColumns = `project_id,role,actor_id,route_state_version,
	desired_state,desired_operation,lifecycle_ownership,state,state_version,action_generation,
	intent_idempotency_key,intent_payload_json,intent_payload_sha256,instance_ref,target_host_id,target_store_id,
	target_server_domain_id,target_server_id,lifecycle_key,target_epoch,profile_id,
	workspace_root_id,workspace_relative_path,external_watch_id,expected_binding_id,
	expected_binding_epoch,expected_session_id,expected_pane_instance_id,expected_agent_run_id,
	active_binding_id,current_action_id,recovery_count,state_entered_at,state_due_at,
	fact_progress_at,return_state,hold_kind,hold_reason,last_error,alert_pending,created_at,updated_at`

func scanProjectActorLifecycle(row projectActorScanner) (ProjectActorLifecycle, error) {
	var out ProjectActorLifecycle
	var entered, due, progress, created, updated string
	var alert int
	err := row.Scan(&out.ProjectID, &out.Role, &out.ActorID, &out.RouteStateVersion,
		&out.DesiredState, &out.DesiredOperation, &out.LifecycleOwnership, &out.State,
		&out.StateVersion, &out.ActionGeneration, &out.IntentIdempotencyKey, &out.IntentPayload, &out.IntentPayloadSHA,
		&out.InstanceRef, &out.TargetHostID, &out.TargetStoreID, &out.TargetServerDomainID,
		&out.TargetServerID, &out.LifecycleKey, &out.TargetEpoch, &out.ProfileID,
		&out.WorkspaceRootID, &out.WorkspaceRelativePath, &out.ExternalWatchID,
		&out.ExpectedBindingID, &out.ExpectedBindingEpoch, &out.ExpectedSessionID,
		&out.ExpectedPaneInstanceID, &out.ExpectedAgentRunID, &out.ActiveBindingID,
		&out.CurrentActionID, &out.RecoveryCount, &entered, &due, &progress,
		&out.ReturnState, &out.HoldKind, &out.HoldReason, &out.LastError, &alert, &created, &updated)
	if err != nil {
		return out, err
	}
	out.StateEnteredAt, out.StateDueAt = parseOptionalTime(entered), parseOptionalTime(due)
	out.FactProgressAt, out.CreatedAt, out.UpdatedAt = parseOptionalTime(progress), parseOptionalTime(created), parseOptionalTime(updated)
	out.AlertPending = alert == 1
	return out, nil
}

func projectActorLifecycleTx(ctx context.Context, tx *sql.Tx, projectID, role, actorID string) (ProjectActorLifecycle, error) {
	return scanProjectActorLifecycle(tx.QueryRowContext(ctx, `SELECT `+projectActorLifecycleColumns+`
		FROM project_actor_lifecycles WHERE project_id=? AND role=? AND actor_id=?`, projectID, role, actorID))
}

func (s *Store) GetProjectActorLifecycle(ctx context.Context, projectID, role, actorID string) (ProjectActorLifecycle, error) {
	return scanProjectActorLifecycle(s.DB.QueryRowContext(ctx, `SELECT `+projectActorLifecycleColumns+`
		FROM project_actor_lifecycles WHERE project_id=? AND role=? AND actor_id=?`, projectID, role, actorID))
}

func (s *Store) CurrentProjectActorLifecycle(ctx context.Context, projectID, role string) (ProjectActorLifecycle, error) {
	return scanProjectActorLifecycle(s.DB.QueryRowContext(ctx, `SELECT `+projectActorLifecycleColumns+`
		FROM project_actor_lifecycles
		WHERE project_id=? AND role=? AND actor_id=(
			SELECT actor_id FROM project_actor_routes
			WHERE project_id=? AND role=? AND state='active'
		)`, projectID, role, projectID, role))
}

func upsertProjectActorLifecycleTx(ctx context.Context, tx *sql.Tx, l ProjectActorLifecycle) error {
	stamp, entered, due, progress := formatActorTime(l.UpdatedAt), formatActorTime(l.StateEnteredAt),
		formatActorTime(l.StateDueAt), formatActorTime(l.FactProgressAt)
	created := formatActorTime(l.CreatedAt)
	_, err := tx.ExecContext(ctx, `INSERT INTO project_actor_lifecycles
		(project_id,role,actor_id,route_state_version,desired_state,desired_operation,lifecycle_ownership,
		 state,state_version,action_generation,intent_idempotency_key,intent_payload_json,intent_payload_sha256,
		 instance_ref,target_host_id,target_store_id,target_server_domain_id,target_server_id,lifecycle_key,
		 target_epoch,profile_id,workspace_root_id,workspace_relative_path,external_watch_id,
		 expected_binding_id,expected_binding_epoch,expected_session_id,expected_pane_instance_id,
		 expected_agent_run_id,active_binding_id,current_action_id,recovery_count,state_entered_at,
		 state_due_at,fact_progress_at,return_state,hold_kind,hold_reason,last_error,alert_pending,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(project_id,role,actor_id) DO UPDATE SET
		 route_state_version=excluded.route_state_version,desired_state=excluded.desired_state,
		 desired_operation=excluded.desired_operation,lifecycle_ownership=excluded.lifecycle_ownership,
		 state=excluded.state,state_version=excluded.state_version,action_generation=excluded.action_generation,
		 intent_idempotency_key=excluded.intent_idempotency_key,intent_payload_json=excluded.intent_payload_json,
		 intent_payload_sha256=excluded.intent_payload_sha256,
		 instance_ref=excluded.instance_ref,target_host_id=excluded.target_host_id,
		 target_store_id=excluded.target_store_id,target_server_domain_id=excluded.target_server_domain_id,
		 target_server_id=excluded.target_server_id,lifecycle_key=excluded.lifecycle_key,
		 target_epoch=excluded.target_epoch,profile_id=excluded.profile_id,
		 workspace_root_id=excluded.workspace_root_id,workspace_relative_path=excluded.workspace_relative_path,
		 external_watch_id=excluded.external_watch_id,expected_binding_id=excluded.expected_binding_id,
		 expected_binding_epoch=excluded.expected_binding_epoch,expected_session_id=excluded.expected_session_id,
		 expected_pane_instance_id=excluded.expected_pane_instance_id,expected_agent_run_id=excluded.expected_agent_run_id,
		 active_binding_id=excluded.active_binding_id,current_action_id=excluded.current_action_id,
		 recovery_count=excluded.recovery_count,state_entered_at=excluded.state_entered_at,
		 state_due_at=excluded.state_due_at,fact_progress_at=excluded.fact_progress_at,
		 return_state=excluded.return_state,hold_kind=excluded.hold_kind,hold_reason=excluded.hold_reason,
		 last_error=excluded.last_error,alert_pending=excluded.alert_pending,updated_at=excluded.updated_at`,
		l.ProjectID, l.Role, l.ActorID, l.RouteStateVersion, l.DesiredState, l.DesiredOperation,
		l.LifecycleOwnership, l.State, l.StateVersion, l.ActionGeneration, l.IntentIdempotencyKey,
		l.IntentPayload, l.IntentPayloadSHA, l.InstanceRef, l.TargetHostID, l.TargetStoreID, l.TargetServerDomainID,
		l.TargetServerID, l.LifecycleKey, l.TargetEpoch, l.ProfileID, l.WorkspaceRootID,
		l.WorkspaceRelativePath, l.ExternalWatchID, l.ExpectedBindingID, l.ExpectedBindingEpoch,
		l.ExpectedSessionID, l.ExpectedPaneInstanceID, l.ExpectedAgentRunID, l.ActiveBindingID,
		l.CurrentActionID, l.RecoveryCount, entered, due, progress, l.ReturnState, l.HoldKind,
		l.HoldReason, l.LastError, b2i(l.AlertPending), created, stamp)
	return err
}

const projectActorActionColumns = `id,project_id,role,actor_id,route_state_version,
	intent_state_version,action_generation,operation,state,action_epoch,idempotency_key,dedup_key,
	payload_json,payload_sha256,instance_ref,target_host_id,target_store_id,target_server_domain_id,
	target_server_id,lifecycle_ownership,lifecycle_key,target_epoch,profile_id,workspace_root_id,
	workspace_relative_path,external_watch_id,expected_binding_id,expected_binding_epoch,
	expected_session_id,expected_pane_instance_id,expected_agent_run_id,lease_id,lease_epoch,
	attempts,recovery_count,next_attempt_at,claim_owner,claim_deadline_at,last_error,created_at,updated_at`

func scanProjectActorAction(row projectActorScanner) (ProjectActorLifecycleAction, error) {
	var out ProjectActorLifecycleAction
	var next, deadline, created, updated string
	err := row.Scan(&out.ID, &out.ProjectID, &out.Role, &out.ActorID, &out.RouteStateVersion,
		&out.IntentStateVersion, &out.ActionGeneration, &out.Operation, &out.State, &out.ActionEpoch,
		&out.IdempotencyKey, &out.DedupKey, &out.Payload, &out.PayloadSHA, &out.InstanceRef,
		&out.TargetHostID, &out.TargetStoreID, &out.TargetServerDomainID, &out.TargetServerID,
		&out.LifecycleOwnership, &out.LifecycleKey, &out.TargetEpoch, &out.ProfileID,
		&out.WorkspaceRootID, &out.WorkspaceRelativePath, &out.ExternalWatchID,
		&out.ExpectedBindingID, &out.ExpectedBindingEpoch, &out.ExpectedSessionID,
		&out.ExpectedPaneInstanceID, &out.ExpectedAgentRunID, &out.LeaseID, &out.LeaseEpoch,
		&out.Attempts, &out.RecoveryCount, &next, &out.ClaimOwner, &deadline, &out.LastError,
		&created, &updated)
	if err != nil {
		return out, err
	}
	out.NextAttemptAt, out.ClaimDeadlineAt = parseOptionalTime(next), parseOptionalTime(deadline)
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, nil
}

func projectActorActionByIDTx(ctx context.Context, tx *sql.Tx, id string) (ProjectActorLifecycleAction, error) {
	return scanProjectActorAction(tx.QueryRowContext(ctx, `SELECT `+projectActorActionColumns+`
		FROM project_actor_lifecycle_actions WHERE id=?`, id))
}

func actorActionByIdempotencyTx(ctx context.Context, tx *sql.Tx, projectID, key string) (ProjectActorLifecycleAction, error) {
	return scanProjectActorAction(tx.QueryRowContext(ctx, `SELECT `+projectActorActionColumns+`
		FROM project_actor_lifecycle_actions WHERE project_id=? AND idempotency_key=?`, projectID, key))
}

func (s *Store) GetProjectActorLifecycleAction(ctx context.Context, id string) (ProjectActorLifecycleAction, error) {
	return scanProjectActorAction(s.DB.QueryRowContext(ctx, `SELECT `+projectActorActionColumns+`
		FROM project_actor_lifecycle_actions WHERE id=?`, id))
}

func insertProjectActorActionTx(ctx context.Context, tx *sql.Tx, l ProjectActorLifecycle,
	idempotencyKey, payload, payloadSHA string, now time.Time) (ProjectActorLifecycleAction, error) {
	dedup := fmt.Sprintf("project_actor:%s:%s:%s:%s:%s:%d:%d", l.ProjectID, l.Role, l.ActorID,
		l.DesiredOperation, l.LifecycleKey, l.TargetEpoch, l.ActionGeneration)
	action := ProjectActorLifecycleAction{
		ID: stableUUID("project-actor-lifecycle-action/v1", dedup), ProjectID: l.ProjectID,
		Role: l.Role, ActorID: l.ActorID, RouteStateVersion: l.RouteStateVersion,
		IntentStateVersion: l.StateVersion, ActionGeneration: l.ActionGeneration,
		Operation: l.DesiredOperation, State: "pending", IdempotencyKey: idempotencyKey,
		DedupKey: dedup, Payload: payload, PayloadSHA: payloadSHA, InstanceRef: l.InstanceRef,
		TargetHostID: l.TargetHostID, TargetStoreID: l.TargetStoreID,
		TargetServerDomainID: l.TargetServerDomainID, TargetServerID: l.TargetServerID,
		LifecycleOwnership: l.LifecycleOwnership, LifecycleKey: l.LifecycleKey,
		TargetEpoch: l.TargetEpoch, ProfileID: l.ProfileID, WorkspaceRootID: l.WorkspaceRootID,
		WorkspaceRelativePath: l.WorkspaceRelativePath, ExternalWatchID: l.ExternalWatchID,
		ExpectedBindingID: l.ExpectedBindingID, ExpectedBindingEpoch: l.ExpectedBindingEpoch,
		ExpectedSessionID: l.ExpectedSessionID, ExpectedPaneInstanceID: l.ExpectedPaneInstanceID,
		ExpectedAgentRunID: l.ExpectedAgentRunID,
		LeaseID:            "actor-lease-" + stableUUID("project-actor-lifecycle-lease/v1", l.ProjectID+"\x00"+l.Role+"\x00"+l.ActorID),
		LeaseEpoch:         l.ActionGeneration, NextAttemptAt: now, CreatedAt: now, UpdatedAt: now,
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO project_actor_lifecycle_actions
		(id,project_id,role,actor_id,route_state_version,intent_state_version,action_generation,
		 operation,state,action_epoch,idempotency_key,dedup_key,payload_json,payload_sha256,
		 instance_ref,target_host_id,target_store_id,target_server_domain_id,target_server_id,
		 lifecycle_ownership,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		 workspace_relative_path,external_watch_id,expected_binding_id,expected_binding_epoch,
		 expected_session_id,expected_pane_instance_id,expected_agent_run_id,lease_id,lease_epoch,
		 next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,'pending',0,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		action.ID, action.ProjectID, action.Role, action.ActorID, action.RouteStateVersion,
		action.IntentStateVersion, action.ActionGeneration, action.Operation, action.IdempotencyKey,
		action.DedupKey, action.Payload, action.PayloadSHA, action.InstanceRef, action.TargetHostID,
		action.TargetStoreID, action.TargetServerDomainID, action.TargetServerID,
		action.LifecycleOwnership, action.LifecycleKey, action.TargetEpoch, action.ProfileID,
		action.WorkspaceRootID, action.WorkspaceRelativePath, action.ExternalWatchID,
		action.ExpectedBindingID, action.ExpectedBindingEpoch, action.ExpectedSessionID,
		action.ExpectedPaneInstanceID, action.ExpectedAgentRunID, action.LeaseID, action.LeaseEpoch,
		formatActorTime(now), formatActorTime(now), formatActorTime(now))
	return action, err
}

func appendProjectActorControlEventTx(ctx context.Context, tx *sql.Tx, l ProjectActorLifecycle,
	kind string, payload any, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,epic_id,kind,state_version,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,'',?,?,'flowbee',?,?,?)`, l.ProjectID, kind, l.StateVersion, l.ActorID,
		string(encoded), formatActorTime(now))
	return err
}

type ProjectActorLifecycleReconcileReport struct {
	Materialized int
	Held         int
	Resumed      int
}

// ReconcileProjectActorLifecycleActions repairs a missing action only from the
// durable desired intent. It never observes or calls Driver. A replacement actor
// remains visibly held until every prior actor for that role is stopped/released.
func (s *Store) ReconcileProjectActorLifecycleActions(ctx context.Context, now time.Time,
	limit int) (ProjectActorLifecycleReconcileReport, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT project_id,role,actor_id FROM project_actor_lifecycles
		WHERE (state LIKE 'awaiting_%' AND current_action_id='')
		   OR (state='held' AND hold_kind='prior_actor_retirement'
		       AND state_due_at<>'' AND julianday(state_due_at)<=julianday(?))
		ORDER BY state_due_at,updated_at LIMIT ?`, formatActorTime(now), limit)
	if err != nil {
		return ProjectActorLifecycleReconcileReport{}, err
	}
	type key struct{ project, role, actor string }
	var keys []key
	for rows.Next() {
		var item key
		if err := rows.Scan(&item.project, &item.role, &item.actor); err != nil {
			rows.Close()
			return ProjectActorLifecycleReconcileReport{}, err
		}
		keys = append(keys, item)
	}
	if err := rows.Close(); err != nil {
		return ProjectActorLifecycleReconcileReport{}, err
	}
	var report ProjectActorLifecycleReconcileReport
	for _, item := range keys {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			lifecycle, err := projectActorLifecycleTx(ctx, tx, item.project, item.role, item.actor)
			if err != nil {
				return err
			}
			if lifecycle.CurrentActionID != "" {
				return nil
			}
			if lifecycle.State == "held" {
				var prior int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_actor_lifecycles
					WHERE project_id=? AND role=? AND actor_id<>? AND state NOT IN ('stopped','released')`,
					lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID).Scan(&prior); err != nil {
					return err
				}
				if prior > 0 {
					_, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state_due_at=?,updated_at=?
						WHERE project_id=? AND role=? AND actor_id=? AND state='held'`,
						formatActorTime(now.Add(time.Minute)), formatActorTime(now), lifecycle.ProjectID,
						lifecycle.Role, lifecycle.ActorID)
					report.Held++
					return err
				}
				lifecycle.State, lifecycle.ReturnState = lifecycle.ReturnState, ""
				lifecycle.HoldKind, lifecycle.HoldReason = "", ""
				lifecycle.StateVersion++
				lifecycle.StateEnteredAt, lifecycle.StateDueAt, lifecycle.FactProgressAt = now,
					now.Add(projectActorLifecycleActionTimeout), now
				lifecycle.UpdatedAt = now
				report.Resumed++
			}
			if !strings.HasPrefix(lifecycle.State, "awaiting_") {
				return nil
			}
			var existingGeneration int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_actor_lifecycle_actions
				WHERE project_id=? AND role=? AND actor_id=? AND action_generation=?`, lifecycle.ProjectID,
				lifecycle.Role, lifecycle.ActorID, lifecycle.ActionGeneration).Scan(&existingGeneration); err != nil {
				return err
			}
			idempotency := lifecycle.IntentIdempotencyKey
			if existingGeneration > 0 {
				lifecycle.ActionGeneration++
				idempotency = fmt.Sprintf("reconcile:%s:%s:%s:%d", lifecycle.ProjectID,
					lifecycle.Role, lifecycle.ActorID, lifecycle.ActionGeneration)
			}
			if err := upsertProjectActorLifecycleTx(ctx, tx, lifecycle); err != nil {
				return err
			}
			action, err := insertProjectActorActionTx(ctx, tx, lifecycle, idempotency,
				lifecycle.IntentPayload, lifecycle.IntentPayloadSHA, now)
			if err != nil {
				return err
			}
			lifecycle.CurrentActionID = action.ID
			if _, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id=?,updated_at=?
				WHERE project_id=? AND role=? AND actor_id=? AND current_action_id=''`, action.ID,
				formatActorTime(now), lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID); err != nil {
				return err
			}
			report.Materialized++
			return appendProjectActorControlEventTx(ctx, tx, lifecycle,
				"project_actor_lifecycle_action_materialized", map[string]any{"action_id": action.ID}, now)
		})
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// HoldProjectActorLifecycle is a CAS transition for pre-effect failures. It may
// not hide a live/uncertain action; those remain visible in the action outbox.
func (s *Store) HoldProjectActorLifecycle(ctx context.Context, projectID, role, actorID string,
	expectedVersion int64, kind, reason string, dueAt, now time.Time) (ProjectActorLifecycle, error) {
	if expectedVersion < 1 || strings.TrimSpace(kind) == "" || strings.TrimSpace(reason) == "" || dueAt.IsZero() {
		return ProjectActorLifecycle{}, ErrProjectActorLifecycleConflict
	}
	var out ProjectActorLifecycle
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := projectActorLifecycleTx(ctx, tx, projectID, role, actorID)
		if err != nil {
			return err
		}
		if current.StateVersion != expectedVersion || current.CurrentActionID != "" ||
			current.State == "active" || current.State == "stopped" || current.State == "released" || current.State == "failed" {
			return ErrProjectActorLifecycleConflict
		}
		returnState := current.State
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state='held',
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,
			return_state=?,hold_kind=?,hold_reason=?,last_error=?,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND state_version=? AND current_action_id=''`,
			formatActorTime(now), formatActorTime(dueAt), formatActorTime(now), returnState,
			kind, reason, reason, formatActorTime(now), projectID, role, actorID, expectedVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorLifecycleConflict
		}
		out, err = projectActorLifecycleTx(ctx, tx, projectID, role, actorID)
		if err != nil {
			return err
		}
		return appendProjectActorControlEventTx(ctx, tx, out, "project_actor_lifecycle_held",
			map[string]any{"hold_kind": kind, "reason": reason}, now)
	})
	return out, err
}

// ClaimNextProjectActorLifecycleAction leases one immutable action for an
// executor. The action epoch is advanced in the same transaction as the
// lifecycle's visible executing state, fencing receipts from prior claims.
func (s *Store) ClaimNextProjectActorLifecycleAction(ctx context.Context, owner string, now, deadline time.Time) (ProjectActorLifecycleAction, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || deadline.IsZero() || !deadline.After(now) {
		return ProjectActorLifecycleAction{}, ErrProjectActorActionStale
	}
	var out ProjectActorLifecycleAction
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM project_actor_lifecycle_actions
			WHERE state='pending' AND (next_attempt_at='' OR julianday(next_attempt_at)<=julianday(?))
			ORDER BY next_attempt_at,created_at,id LIMIT 1`, formatActorTime(now)).Scan(&id); err != nil {
			return err
		}
		action, err := projectActorActionByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		nextEpoch := action.ActionEpoch + 1
		stamp := formatActorTime(now)
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
			SET state='delivering',action_epoch=?,attempts=attempts+1,claim_owner=?,
			claim_deadline_at=?,delivery_started_at=?,updated_at=?
			WHERE id=? AND state='pending' AND action_epoch=?`, nextEpoch, owner,
			formatActorTime(deadline), stamp, stamp, id, action.ActionEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		res, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state=?,
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND current_action_id=?
			AND state=?`, actorExecutingState(action.Operation), stamp, formatActorTime(deadline), stamp, stamp,
			action.ProjectID, action.Role, action.ActorID, action.ID, actorAwaitingState(action.Operation))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		out, err = projectActorActionByIDTx(ctx, tx, id)
		return err
	})
	return out, err
}

// MarkProjectActorLifecycleActionVerifying records that transport may already
// have mutated Driver state. From this point recovery must observe exact Driver
// facts; it must not blindly resend the action.
func (s *Store) MarkProjectActorLifecycleActionVerifying(ctx context.Context, id, owner string, epoch int64,
	now, deadline time.Time, diagnostic string) error {
	if id == "" || owner == "" || epoch < 1 || deadline.IsZero() || !deadline.After(now) {
		return ErrProjectActorActionStale
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		action, err := projectActorActionByIDTx(ctx, tx, id)
		if err != nil || action.ActionEpoch != epoch || action.ClaimOwner != owner || action.State != "delivering" {
			return ErrProjectActorActionStale
		}
		stamp := formatActorTime(now)
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions
			SET state='verifying',claim_deadline_at=?,last_error=?,updated_at=?
			WHERE id=? AND state='delivering' AND action_epoch=? AND claim_owner=?`,
			formatActorTime(deadline), diagnostic, stamp, id, epoch, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		res, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state=?,
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,
			last_error=?,updated_at=? WHERE project_id=? AND role=? AND actor_id=?
			AND current_action_id=? AND state=?`, actorVerifyingState(action.Operation), stamp,
			formatActorTime(deadline), stamp, diagnostic, stamp, action.ProjectID, action.Role,
			action.ActorID, action.ID, actorExecutingState(action.Operation))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		return nil
	})
}

// RecordProjectActorLifecyclePreEffectFailure retries only a failure known to
// have happened before Driver accepted/mutated the target. Callers must use the
// verifying path for submitted, delivering, uncertain, or otherwise ambiguous
// outcomes.
func (s *Store) RecordProjectActorLifecyclePreEffectFailure(ctx context.Context, id, owner string, epoch int64,
	reason string, now, retryAt time.Time, maxRecovery int) error {
	if id == "" || owner == "" || epoch < 1 || strings.TrimSpace(reason) == "" ||
		retryAt.IsZero() || !retryAt.After(now) || maxRecovery < 1 {
		return ErrProjectActorActionStale
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		action, err := projectActorActionByIDTx(ctx, tx, id)
		if err != nil || action.ActionEpoch != epoch || action.ClaimOwner != owner || action.State != "delivering" {
			return ErrProjectActorActionStale
		}
		stamp := formatActorTime(now)
		nextRecovery := action.RecoveryCount + 1
		if nextRecovery >= maxRecovery {
			res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET state='dead_letter',
				recovery_count=?,claim_owner='',claim_deadline_at='',dead_lettered_at=?,last_error=?,updated_at=?
				WHERE id=? AND state='delivering' AND action_epoch=? AND claim_owner=?`, nextRecovery,
				stamp, reason, stamp, id, epoch, owner)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrProjectActorActionStale
			}
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET state='pending',
			recovery_count=?,next_attempt_at=?,claim_owner='',claim_deadline_at='',last_error=?,updated_at=?
			WHERE id=? AND state='delivering' AND action_epoch=? AND claim_owner=?`, nextRecovery,
			formatActorTime(retryAt), reason, stamp, id, epoch, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		res, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state=?,
			state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,last_error=?,updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND current_action_id=? AND state=?`,
			actorAwaitingState(action.Operation), stamp, formatActorTime(retryAt), stamp, reason, stamp,
			action.ProjectID, action.Role, action.ActorID, action.ID, actorExecutingState(action.Operation))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		return nil
	})
}

type ProjectActorLifecycleRecoveryReport struct {
	DeliveryUncertain int
	VerificationReady int
	DeadLettered      int
}

// ReconcileExpiredProjectActorLifecycleClaims is the restart/backstop loop.
// An expired delivery is uncertain and moves only to verification. Repeated
// verification expiry consumes a bounded recovery budget and eventually pages
// through the migration's dead-letter trigger; it is never made resendable.
func (s *Store) ReconcileExpiredProjectActorLifecycleClaims(ctx context.Context, now time.Time,
	maxRecovery, limit int) (ProjectActorLifecycleRecoveryReport, error) {
	if maxRecovery < 1 {
		maxRecovery = 3
	}
	if limit < 1 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT id FROM project_actor_lifecycle_actions
		WHERE state IN ('delivering','verifying') AND claim_deadline_at<>''
		  AND julianday(claim_deadline_at)<=julianday(?) ORDER BY claim_deadline_at,id LIMIT ?`,
		formatActorTime(now), limit)
	if err != nil {
		return ProjectActorLifecycleRecoveryReport{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ProjectActorLifecycleRecoveryReport{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return ProjectActorLifecycleRecoveryReport{}, err
	}
	var report ProjectActorLifecycleRecoveryReport
	for _, id := range ids {
		err := s.tx(ctx, func(tx *sql.Tx) error {
			action, err := projectActorActionByIDTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if (action.State != "delivering" && action.State != "verifying") ||
				action.ClaimDeadlineAt.IsZero() || action.ClaimDeadlineAt.After(now) {
				return nil
			}
			stamp := formatActorTime(now)
			if action.State == "delivering" {
				res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET state='verifying',
					claim_owner='',claim_deadline_at=?,last_error='delivery claim expired; verify exact Driver facts',
					updated_at=? WHERE id=? AND state='delivering' AND action_epoch=?`,
					formatActorTime(now.Add(projectActorLifecycleActionTimeout)), stamp, id, action.ActionEpoch)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return nil
				}
				_, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state=?,
					state_version=state_version+1,state_entered_at=?,state_due_at=?,fact_progress_at=?,
					last_error='delivery claim expired; verify exact Driver facts',updated_at=?
					WHERE project_id=? AND role=? AND actor_id=? AND current_action_id=?`,
					actorVerifyingState(action.Operation), stamp,
					formatActorTime(now.Add(projectActorLifecycleActionTimeout)), stamp, stamp,
					action.ProjectID, action.Role, action.ActorID, action.ID)
				report.DeliveryUncertain++
				return err
			}
			nextRecovery := action.RecoveryCount + 1
			if nextRecovery >= maxRecovery {
				_, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET state='dead_letter',
					recovery_count=?,claim_owner='',claim_deadline_at='',dead_lettered_at=?,
					last_error='lifecycle verification overdue',updated_at=?
					WHERE id=? AND state='verifying' AND action_epoch=?`, nextRecovery, stamp, stamp,
					id, action.ActionEpoch)
				if err == nil {
					report.DeadLettered++
				}
				return err
			}
			res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET recovery_count=?,
				claim_owner='',claim_deadline_at=?,last_error='lifecycle verification overdue',updated_at=?
				WHERE id=? AND state='verifying' AND action_epoch=?`, nextRecovery,
				formatActorTime(now.Add(projectActorLifecycleActionTimeout)), stamp, id, action.ActionEpoch)
			if err == nil {
				if n, _ := res.RowsAffected(); n == 1 {
					report.VerificationReady++
				}
			}
			return err
		})
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// ClaimNextProjectActorLifecycleVerification leases observation-only recovery.
// It deliberately preserves action_epoch: the verifier is checking the effect
// of that exact delivery epoch, not authorizing a new mutation.
func (s *Store) ClaimNextProjectActorLifecycleVerification(ctx context.Context, owner string,
	now, deadline time.Time) (ProjectActorLifecycleAction, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" || deadline.IsZero() || !deadline.After(now) {
		return ProjectActorLifecycleAction{}, ErrProjectActorActionStale
	}
	var out ProjectActorLifecycleAction
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var id string
		if err := tx.QueryRowContext(ctx, `SELECT id FROM project_actor_lifecycle_actions
			WHERE state='verifying' AND claim_owner='' AND claim_deadline_at<>''
			ORDER BY claim_deadline_at,updated_at,id LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		action, err := projectActorActionByIDTx(ctx, tx, id)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET claim_owner=?,
			claim_deadline_at=?,updated_at=? WHERE id=? AND state='verifying' AND claim_owner=''
			AND action_epoch=?`, owner, formatActorTime(deadline), formatActorTime(now), id, action.ActionEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		out, err = projectActorActionByIDTx(ctx, tx, id)
		return err
	})
	return out, err
}

// AdvanceProjectActorLifecycleVerificationEpoch authorizes one explicit,
// observation-only VerifyLifecycleEffect call. It does not make the lifecycle
// mutation resendable; the action remains in verifying throughout.
func (s *Store) AdvanceProjectActorLifecycleVerificationEpoch(ctx context.Context, id, owner string,
	expectedEpoch int64, now, deadline time.Time) (ProjectActorLifecycleAction, error) {
	if id == "" || owner == "" || expectedEpoch < 1 || deadline.IsZero() || !deadline.After(now) {
		return ProjectActorLifecycleAction{}, ErrProjectActorActionStale
	}
	var out ProjectActorLifecycleAction
	err := s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET action_epoch=action_epoch+1,
			claim_deadline_at=?,updated_at=? WHERE id=? AND state='verifying' AND claim_owner=?
			AND action_epoch=?`, formatActorTime(deadline), formatActorTime(now), id, owner, expectedEpoch)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		out, err = projectActorActionByIDTx(ctx, tx, id)
		return err
	})
	return out, err
}

func actorIdentityMatchesAction(id ProjectActorLifecycleIdentity, action ProjectActorLifecycleAction) bool {
	return id.HostID == action.TargetHostID && id.StoreID == action.TargetStoreID &&
		id.TmuxServerDomainID == action.TargetServerDomainID && id.TmuxServerInstanceID == action.TargetServerID &&
		id.LifecycleKey == action.LifecycleKey && id.TargetEpoch == action.TargetEpoch &&
		id.SessionID == action.ExpectedSessionID && id.PaneInstanceID == action.ExpectedPaneInstanceID &&
		id.AgentRunID == action.ExpectedAgentRunID
}

func actorIdentityAfterValid(id ProjectActorLifecycleIdentity, action ProjectActorLifecycleAction) bool {
	return id.HostID == action.TargetHostID && id.StoreID == action.TargetStoreID &&
		id.TmuxServerDomainID == action.TargetServerDomainID && id.TmuxServerInstanceID == action.TargetServerID &&
		id.LifecycleOwnership == action.LifecycleOwnership && id.LifecycleKey == action.LifecycleKey &&
		id.TargetEpoch == action.TargetEpoch && id.SessionID != "" && id.PaneInstanceID != "" && id.AgentRunID != ""
}

func projectActorReceiptProjection(receipt ProjectActorLifecycleReceipt) ProjectActorLifecycleReceiptProjection {
	return ProjectActorLifecycleReceiptProjection{
		ActionID: receipt.ActionID, Operation: receipt.Operation, LifecycleKey: receipt.LifecycleKey,
		Status: receipt.Status, ActionEpoch: receipt.ActionEpoch, TargetEpoch: receipt.TargetEpoch,
		LeaseID: receipt.LeaseID, LeaseEpoch: receipt.LeaseEpoch,
		TmuxServerDomainID: receipt.TmuxServerDomainID, ExternalWatchID: receipt.ExternalWatchID,
		AbsenceObservedAt: receipt.AbsenceObservedAt, IdentityBefore: receipt.IdentityBefore,
		IdentityAfter: receipt.IdentityAfter,
	}
}

func scanProjectActorReceipt(row projectActorScanner) (ProjectActorLifecycleReceipt, error) {
	var out ProjectActorLifecycleReceipt
	var beforeJSON, afterJSON, created, updated string
	err := row.Scan(&out.ID, &out.ActionID, &out.ActionEpoch, &out.Operation, &out.LifecycleKey,
		&out.TargetEpoch, &out.LeaseID, &out.LeaseEpoch, &out.TmuxServerDomainID,
		&out.ExternalWatchID, &out.Status, &beforeJSON, &afterJSON, &out.AbsenceObservedAt,
		&out.DiagnosticCode, &created, &updated)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(beforeJSON), &out.IdentityBefore); err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(afterJSON), &out.IdentityAfter); err != nil {
		return out, err
	}
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, nil
}

const projectActorReceiptColumns = `lifecycle_receipt_id,action_id,action_epoch,operation,
	lifecycle_key,target_epoch,lease_id,lease_epoch,tmux_server_domain_id,external_watch_id,
	status,identity_before_json,identity_after_json,absence_observed_at,diagnostic_code,created_at,updated_at`

func projectActorReceiptByActionTx(ctx context.Context, tx *sql.Tx, actionID string) (ProjectActorLifecycleReceipt, error) {
	return scanProjectActorReceipt(tx.QueryRowContext(ctx, `SELECT `+projectActorReceiptColumns+`
		FROM project_actor_lifecycle_receipts WHERE action_id=?`, actionID))
}

func (s *Store) GetProjectActorLifecycleReceiptByAction(ctx context.Context, actionID string) (ProjectActorLifecycleReceipt, error) {
	return scanProjectActorReceipt(s.DB.QueryRowContext(ctx, `SELECT `+projectActorReceiptColumns+`
		FROM project_actor_lifecycle_receipts WHERE action_id=?`, actionID))
}

// PersistProjectActorLifecycleReceipt stores Driver evidence before applying it
// to Flowbee projections. Exact duplicate delivery is a no-op; any changed body
// for the same action is rejected.
func (s *Store) PersistProjectActorLifecycleReceipt(ctx context.Context, receipt ProjectActorLifecycleReceipt,
	now time.Time) (ProjectActorLifecycleReceipt, error) {
	if receipt.ActionID == "" || receipt.ActionEpoch < 1 || receipt.LeaseID == "" || receipt.LeaseEpoch < 1 ||
		receipt.Operation == "" || receipt.LifecycleKey == "" || receipt.TargetEpoch < 1 ||
		receipt.TmuxServerDomainID == "" || receipt.Status == "" {
		return ProjectActorLifecycleReceipt{}, ErrProjectActorActionStale
	}
	receipt.ID = "actor-receipt-" + stableUUID("project-actor-lifecycle-receipt/v1", receipt.ActionID)
	beforeJSON, err := json.Marshal(receipt.IdentityBefore)
	if err != nil {
		return ProjectActorLifecycleReceipt{}, err
	}
	afterJSON, err := json.Marshal(receipt.IdentityAfter)
	if err != nil {
		return ProjectActorLifecycleReceipt{}, err
	}
	err = s.tx(ctx, func(tx *sql.Tx) error {
		existing, existingErr := projectActorReceiptByActionTx(ctx, tx, receipt.ActionID)
		if existingErr == nil {
			if existing.ActionEpoch != receipt.ActionEpoch || existing.Operation != receipt.Operation ||
				existing.LifecycleKey != receipt.LifecycleKey || existing.TargetEpoch != receipt.TargetEpoch ||
				existing.LeaseID != receipt.LeaseID || existing.LeaseEpoch != receipt.LeaseEpoch ||
				existing.TmuxServerDomainID != receipt.TmuxServerDomainID ||
				existing.ExternalWatchID != receipt.ExternalWatchID || existing.Status != receipt.Status ||
				existing.IdentityBefore != receipt.IdentityBefore || existing.IdentityAfter != receipt.IdentityAfter ||
				existing.AbsenceObservedAt != receipt.AbsenceObservedAt || existing.DiagnosticCode != receipt.DiagnosticCode {
				return ErrProjectActorActionStale
			}
			receipt = existing
			return nil
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		action, err := projectActorActionByIDTx(ctx, tx, receipt.ActionID)
		if err != nil || action.ActionEpoch != receipt.ActionEpoch || action.Operation != receipt.Operation ||
			action.LifecycleKey != receipt.LifecycleKey || action.TargetEpoch != receipt.TargetEpoch ||
			action.LeaseID != receipt.LeaseID || action.LeaseEpoch != receipt.LeaseEpoch ||
			action.TargetServerDomainID != receipt.TmuxServerDomainID ||
			(action.State != "delivering" && action.State != "verifying") {
			return ErrProjectActorActionStale
		}
		stamp := formatActorTime(now)
		_, err = tx.ExecContext(ctx, `INSERT INTO project_actor_lifecycle_receipts
			(lifecycle_receipt_id,action_id,action_epoch,operation,lifecycle_key,target_epoch,
			 lease_id,lease_epoch,tmux_server_domain_id,external_watch_id,status,
			 identity_before_json,identity_after_json,absence_observed_at,diagnostic_code,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, receipt.ID, receipt.ActionID, receipt.ActionEpoch,
			receipt.Operation, receipt.LifecycleKey, receipt.TargetEpoch, receipt.LeaseID, receipt.LeaseEpoch,
			receipt.TmuxServerDomainID, receipt.ExternalWatchID, receipt.Status, string(beforeJSON),
			string(afterJSON), receipt.AbsenceObservedAt, receipt.DiagnosticCode, stamp, stamp)
		if err == nil {
			receipt.CreatedAt, receipt.UpdatedAt = now, now
		}
		return err
	})
	return receipt, err
}

// ProjectPersistedProjectActorLifecycleReceipt folds only already-durable
// evidence. Runtime adapters should call Persist first, then this method.
func (s *Store) ProjectPersistedProjectActorLifecycleReceipt(ctx context.Context, actionID string, now time.Time) error {
	receipt, err := s.GetProjectActorLifecycleReceiptByAction(ctx, actionID)
	if err != nil {
		return err
	}
	return s.ProjectProjectActorLifecycleResult(ctx, projectActorReceiptProjection(receipt), now)
}

// ProjectProjectActorLifecycleResult folds one already-durable Driver terminal
// receipt into the actor lifecycle and exact session-binding registry in one
// transaction. It is replay-safe if the process dies before action ack.
func (s *Store) ProjectProjectActorLifecycleResult(ctx context.Context, receipt ProjectActorLifecycleReceiptProjection,
	now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		action, err := projectActorActionByIDTx(ctx, tx, receipt.ActionID)
		if err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(action.Payload))
		if action.PayloadSHA != "sha256:"+hex.EncodeToString(sum[:]) || action.ActionEpoch != receipt.ActionEpoch ||
			action.Operation != receipt.Operation || action.LifecycleKey != receipt.LifecycleKey ||
			action.TargetEpoch != receipt.TargetEpoch || action.LeaseID != receipt.LeaseID ||
			action.LeaseEpoch != receipt.LeaseEpoch || action.TargetServerDomainID != receipt.TmuxServerDomainID ||
			(action.State != "delivering" && action.State != "verifying" && action.State != "acknowledged") {
			return ErrProjectActorActionStale
		}
		lifecycle, err := projectActorLifecycleTx(ctx, tx, action.ProjectID, action.Role, action.ActorID)
		if err != nil {
			return err
		}
		terminal := actorTerminalState(action.Operation)
		if lifecycle.CurrentActionID != action.ID {
			if lifecycle.State == terminal {
				return nil
			}
			return ErrProjectActorActionStale
		}

		var bindingID string
		switch action.Operation {
		case "ensure", "adopt", "reattach":
			wantStatus := map[string]string{"ensure": "ensured", "adopt": "adopted", "reattach": "reattached"}[action.Operation]
			if receipt.Status != wantStatus || !actorIdentityAfterValid(receipt.IdentityAfter, action) {
				return ErrProjectActorActionStale
			}
			if action.Operation == "adopt" && (receipt.ExternalWatchID != action.ExternalWatchID || receipt.IdentityAfter.LifecycleOwnership != "external_observed") {
				return ErrProjectActorActionStale
			}
			if action.Operation == "ensure" && (receipt.ExternalWatchID != "" || receipt.IdentityAfter.LifecycleOwnership != "driver_managed") {
				return ErrProjectActorActionStale
			}
			if action.Operation == "reattach" && (!actorIdentityMatchesAction(receipt.IdentityBefore, action) ||
				receipt.ExternalWatchID != action.ExternalWatchID) {
				return ErrProjectActorActionStale
			}
			if lifecycle.State == terminal && lifecycle.ActiveBindingID != "" {
				var state, hostID, storeID, domainID, serverID, ownership, lifecycleKey string
				var targetEpoch int64
				var sessionID, paneInstanceID, agentRunID string
				err := tx.QueryRowContext(ctx, `SELECT state,host_id,store_id,tmux_server_domain_id,
					tmux_server_instance_id,lifecycle_ownership,lifecycle_key,target_epoch,
					session_id,pane_instance_id,agent_run_id FROM driver_session_bindings
					WHERE binding_id=?`, lifecycle.ActiveBindingID).Scan(&state, &hostID, &storeID,
					&domainID, &serverID, &ownership, &lifecycleKey, &targetEpoch, &sessionID,
					&paneInstanceID, &agentRunID)
				if err != nil {
					return err
				}
				if state == "active" && hostID == receipt.IdentityAfter.HostID &&
					storeID == receipt.IdentityAfter.StoreID && domainID == receipt.IdentityAfter.TmuxServerDomainID &&
					serverID == receipt.IdentityAfter.TmuxServerInstanceID && ownership == receipt.IdentityAfter.LifecycleOwnership &&
					lifecycleKey == receipt.IdentityAfter.LifecycleKey && targetEpoch == receipt.IdentityAfter.TargetEpoch &&
					sessionID == receipt.IdentityAfter.SessionID && paneInstanceID == receipt.IdentityAfter.PaneInstanceID &&
					agentRunID == receipt.IdentityAfter.AgentRunID {
					return nil
				}
				return ErrProjectActorActionStale
			}
			bindingID, err = projectActorBindingFromReceiptTx(ctx, tx, lifecycle, action, receipt.IdentityAfter, now)
			if err != nil {
				return err
			}
		case "stop":
			if (receipt.Status != "stopped" && receipt.Status != "target_absent") || receipt.AbsenceObservedAt == "" ||
				action.LifecycleOwnership != "driver_managed" {
				return ErrProjectActorActionStale
			}
			if receipt.IdentityBefore != (ProjectActorLifecycleIdentity{}) && !actorIdentityMatchesAction(receipt.IdentityBefore, action) {
				return ErrProjectActorActionStale
			}
			if lifecycle.State == terminal {
				var state string
				if err := tx.QueryRowContext(ctx, `SELECT state FROM driver_session_bindings WHERE binding_id=?`,
					action.ExpectedBindingID).Scan(&state); err != nil || state != "superseded" {
					return ErrProjectActorActionStale
				}
				return nil
			}
			if err := supersedeProjectActorBindingTx(ctx, tx, action, now); err != nil {
				return err
			}
		case "release":
			if receipt.Status != "released" || receipt.AbsenceObservedAt != "" ||
				receipt.ExternalWatchID != action.ExternalWatchID || action.LifecycleOwnership != "external_observed" ||
				!actorIdentityMatchesAction(receipt.IdentityBefore, action) {
				return ErrProjectActorActionStale
			}
			if lifecycle.State == terminal {
				var state string
				if err := tx.QueryRowContext(ctx, `SELECT state FROM driver_session_bindings WHERE binding_id=?`,
					action.ExpectedBindingID).Scan(&state); err != nil || state != "superseded" {
					return ErrProjectActorActionStale
				}
				return nil
			}
			if err := supersedeProjectActorBindingTx(ctx, tx, action, now); err != nil {
				return err
			}
		default:
			return ErrProjectActorActionStale
		}

		stamp := formatActorTime(now)
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET state=?,
			state_version=state_version+1,active_binding_id=?,state_entered_at=?,state_due_at='',
			fact_progress_at=?,return_state='',hold_kind='',hold_reason='',last_error='',
			alert_pending=0,updated_at=? WHERE project_id=? AND role=? AND actor_id=?
			AND current_action_id=? AND state_version=?`, terminal, bindingID, stamp, stamp, stamp,
			lifecycle.ProjectID, lifecycle.Role, lifecycle.ActorID, action.ID, lifecycle.StateVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		lifecycle.State, lifecycle.StateVersion, lifecycle.ActiveBindingID = terminal, lifecycle.StateVersion+1, bindingID
		return appendProjectActorControlEventTx(ctx, tx, lifecycle, "project_actor_lifecycle_projected",
			map[string]any{"action_id": action.ID, "operation": action.Operation, "binding_id": bindingID}, now)
	})
}

func projectActorBindingFromReceiptTx(ctx context.Context, tx *sql.Tx, lifecycle ProjectActorLifecycle,
	action ProjectActorLifecycleAction, id ProjectActorLifecycleIdentity, now time.Time) (string, error) {
	if action.Operation == "reattach" {
		if err := supersedeProjectActorBindingTx(ctx, tx, action, now); err != nil {
			return "", err
		}
	} else if current, err := activeDriverSessionBindingTx(ctx, tx, action.ProjectID, action.ActorID, action.Role); err == nil {
		if current.HostID == id.HostID && current.StoreID == id.StoreID && current.SessionID == id.SessionID &&
			current.PaneInstanceID == id.PaneInstanceID && current.AgentRunID == id.AgentRunID {
			return current.BindingID, nil
		}
		return "", ErrProjectActorActionStale
	} else if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	var epoch int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(binding_epoch),0)+1 FROM driver_session_bindings
		WHERE project_id=? AND worker_identity=? AND role=?`, action.ProjectID, action.ActorID, action.Role).Scan(&epoch); err != nil {
		return "", err
	}
	binding := DriverSessionBinding{ProjectID: action.ProjectID, WorkerIdentity: action.ActorID,
		Role: action.Role, BindingEpoch: epoch, HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerDomainID: id.TmuxServerDomainID, TmuxServerInstanceID: id.TmuxServerInstanceID,
		LifecycleOwnership: id.LifecycleOwnership, ExternalWatchID: action.ExternalWatchID,
		LifecycleKey: id.LifecycleKey, TargetEpoch: id.TargetEpoch, ProfileID: action.ProfileID,
		WorkspaceRootID: action.WorkspaceRootID, WorkspaceRelativePath: action.WorkspaceRelativePath,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID,
		Provider: id.Provider, ConversationID: id.ConversationID, ObservedAt: now}
	if err := binding.validate(); err != nil {
		return "", err
	}
	binding.BindingID = driverBindingID(binding, epoch)
	stamp := formatActorTime(now)
	_, err := tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
		(binding_id,project_id,worker_identity,role,seat_id,binding_epoch,state,host_id,store_id,
		 tmux_server_domain_id,tmux_server_instance_id,lifecycle_ownership,external_watch_id,
		 lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,
		 session_id,pane_instance_id,agent_run_id,provider,conversation_id,observed_at,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, binding.BindingID,
		binding.ProjectID, binding.WorkerIdentity, binding.Role, binding.SeatID, binding.BindingEpoch,
		binding.HostID, binding.StoreID, binding.TmuxServerDomainID, binding.TmuxServerInstanceID,
		binding.LifecycleOwnership, binding.ExternalWatchID, binding.LifecycleKey, binding.TargetEpoch,
		binding.ProfileID, binding.WorkspaceRootID, binding.WorkspaceRelativePath, binding.SessionID,
		binding.PaneInstanceID, binding.AgentRunID, binding.Provider, binding.ConversationID, stamp, stamp, stamp)
	return binding.BindingID, err
}

func supersedeProjectActorBindingTx(ctx context.Context, tx *sql.Tx, action ProjectActorLifecycleAction, now time.Time) error {
	if action.ExpectedBindingID == "" || action.ExpectedBindingEpoch < 1 {
		return ErrProjectActorActionStale
	}
	stamp := formatActorTime(now)
	res, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
		superseded_at=?,updated_at=? WHERE binding_id=? AND project_id=? AND worker_identity=?
		AND role=? AND binding_epoch=? AND state='active' AND host_id=? AND store_id=?
		AND tmux_server_domain_id=? AND tmux_server_instance_id=? AND session_id=?
		AND pane_instance_id=? AND agent_run_id=?`, stamp, stamp, action.ExpectedBindingID,
		action.ProjectID, action.ActorID, action.Role, action.ExpectedBindingEpoch,
		action.TargetHostID, action.TargetStoreID, action.TargetServerDomainID, action.TargetServerID,
		action.ExpectedSessionID, action.ExpectedPaneInstanceID, action.ExpectedAgentRunID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrProjectActorActionStale
	}
	return nil
}

// AcknowledgeProjectActorLifecycleAction is the post-projection CAS. Clearing
// current_action_id happens only after the terminal projection transaction, so
// a crash between projection and ack replays the same receipt instead of
// materializing another effect.
func (s *Store) AcknowledgeProjectActorLifecycleAction(ctx context.Context, id, owner string,
	epoch int64, now time.Time) error {
	if id == "" || owner == "" || epoch < 1 {
		return ErrProjectActorActionStale
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		action, err := projectActorActionByIDTx(ctx, tx, id)
		if err != nil || action.ActionEpoch != epoch || action.ClaimOwner != owner ||
			(action.State != "delivering" && action.State != "verifying") {
			return ErrProjectActorActionStale
		}
		res, err := tx.ExecContext(ctx, `UPDATE project_actor_lifecycle_actions SET state='acknowledged',
			acknowledged_at=?,claim_owner='',claim_deadline_at='',updated_at=?
			WHERE id=? AND action_epoch=? AND claim_owner=? AND state IN ('delivering','verifying')`,
			formatActorTime(now), formatActorTime(now), id, epoch, owner)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrProjectActorActionStale
		}
		_, err = tx.ExecContext(ctx, `UPDATE project_actor_lifecycles SET current_action_id='',updated_at=?
			WHERE project_id=? AND role=? AND actor_id=? AND current_action_id=?
			AND state IN ('active','stopped','released')`, formatActorTime(now), action.ProjectID,
			action.Role, action.ActorID, action.ID)
		return err
	})
}
