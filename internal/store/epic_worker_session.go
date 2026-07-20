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

const EpicWorkerBootstrapFormat = "flowbee.worker-bootstrap/v1"

type epicWorkerBootstrap struct {
	Format                    string                        `json:"format"`
	ProjectID                 string                        `json:"project_id"`
	EpicID                    string                        `json:"epic_id"`
	Role                      string                        `json:"role"`
	Family                    string                        `json:"model_family"`
	Repository                string                        `json:"repository"`
	Branch                    string                        `json:"branch"`
	SpecPath                  string                        `json:"spec_path"`
	Scope                     []string                      `json:"scope"`
	EpicSpecGoalFormat        string                        `json:"epic_spec_goal_format"`
	EpicSpecGoalUTF8          string                        `json:"epic_spec_goal_utf8"`
	EpicSpecGoalSHA256        string                        `json:"epic_spec_goal_sha256"`
	AdmissionContractSHA256   string                        `json:"admission_contract_sha256"`
	SourceArtifactSHA256      string                        `json:"source_artifact_sha256"`
	SourceCommitSHA           string                        `json:"source_commit_sha"`
	RoleCharter               string                        `json:"role_charter"`
	RoleCharterSHA256         string                        `json:"role_charter_sha256"`
	DisciplineKind            string                        `json:"discipline_kind"`
	DisciplineUTF8            string                        `json:"discipline_utf8"`
	DisciplineSHA256          string                        `json:"discipline_sha256"`
	ReferenceDocuments        []EpicWorkerReferenceDocument `json:"reference_documents"`
	ReferenceManifestSHA256   string                        `json:"reference_manifest_sha256"`
	ArtifactContextRef        string                        `json:"artifact_context_ref"`
	ArtifactHeadFenceRequired bool                          `json:"artifact_head_fence_required"`
	CredentialPolicyRef       string                        `json:"credential_policy_ref"`
	CredentialInstallRef      string                        `json:"credential_install_ref"`
	FlowbeeWorkerIdentity     string                        `json:"flowbee_worker_identity"`
}

func (s *Store) projectEpicReviewerLaunchTx(ctx context.Context, tx *sql.Tx,
	action BuilderLifecycleActionProjection, receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	if receipt.Operation != "ensure" || receipt.Status != "ensured" || action.TargetRole != DriverReviewerRole {
		return fmt.Errorf("reviewer launch not ensured: %s", receipt.Status)
	}
	id := receipt.IdentityAfter
	if id.HostID != action.TargetHostID || id.StoreID != action.TargetStoreID ||
		id.TmuxServerDomainID != action.TargetServerDomainID || id.TmuxServerInstanceID != action.TargetServerID ||
		id.LifecycleOwnership != "driver_managed" || id.LifecycleKey != action.LifecycleKey ||
		id.TargetEpoch != action.TargetEpoch || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return errors.New("Driver reviewer launch returned a different incarnation fence")
	}
	var payload struct {
		SeatID string `json:"seat_id"`
	}
	if err := json.Unmarshal([]byte(action.Payload), &payload); err != nil || payload.SeatID == "" {
		return errors.New("reviewer launch payload has no capacity seat")
	}
	var state, family, workerIdentity string
	if err := tx.QueryRowContext(ctx, `SELECT state,model_family,worker_identity
		FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role='reviewer'`,
		action.EpicID, action.ProjectID).Scan(&state, &family, &workerIdentity); err != nil {
		return err
	}
	if state == "active" {
		current, err := activeDriverSessionBindingTx(ctx, tx, action.ProjectID, workerIdentity, DriverReviewerRole)
		if err != nil {
			return err
		}
		if current.SessionID == id.SessionID && current.PaneInstanceID == id.PaneInstanceID &&
			current.AgentRunID == id.AgentRunID && current.TargetEpoch == id.TargetEpoch {
			return nil
		}
		return errors.New("reviewer launch replay found a different active incarnation")
	}
	if state != "ensure_pending" {
		return fmt.Errorf("reviewer launch projection found worker state %s", state)
	}
	if id.Provider != "" && family != id.Provider {
		return fmt.Errorf("reviewer provider %s does not match admitted family %s", id.Provider, family)
	}
	stamp := now.UTC().Format(rfc3339)
	binding := DriverSessionBinding{ProjectID: action.ProjectID, WorkerIdentity: workerIdentity,
		Role: DriverReviewerRole, SeatID: payload.SeatID, BindingEpoch: 1,
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
		bootstrap_state='route_pending',updated_at=? WHERE epic_id=? AND worker_role='reviewer'
		AND state='ensure_pending' AND ensure_action_id=?`, binding.BindingID, stamp,
		action.EpicID, action.ActionID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("reviewer launch lost immutable worker action fence")
	}
	return appendEpicControlEventTx(ctx, tx, action.ProjectID, action.EpicID,
		"reviewer_session_ensured", "awaiting_review_dispatch", "awaiting_review_dispatch", 0,
		"driver", `{"bootstrap_state":"route_pending"}`, now)
}

func ensureEpicWorkerStopIntentsTx(ctx context.Context, tx *sql.Tx, projectID, epicID string,
	now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT worker_role,worker_identity,state,ensure_action_id FROM epic_worker_sessions
		WHERE epic_id=? AND project_id=? ORDER BY worker_role`, epicID, projectID)
	if err != nil {
		return err
	}
	type worker struct{ role, identity, state, ensureActionID string }
	var workers []worker
	for rows.Next() {
		var w worker
		if err := rows.Scan(&w.role, &w.identity, &w.state, &w.ensureActionID); err != nil {
			rows.Close()
			return err
		}
		workers = append(workers, w)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	for _, w := range workers {
		if w.state == "stopped" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stop_pending',updated_at=?
			WHERE epic_id=? AND worker_role=? AND state<>'stopped'`, stamp, epicID, w.role); err != nil {
			return err
		}
		bindingRole := DriverBuilderRole
		if w.role == "reviewer" {
			bindingRole = DriverReviewerRole
		}
		binding, err := activeDriverSessionBindingTx(ctx, tx, projectID, w.identity, bindingRole)
		if errors.Is(err, sql.ErrNoRows) {
			// A worker with no committed Ensure action never had a workspace
			// preparation obligation and can close locally.  Every committed
			// Ensure action, including pending epoch-zero work, goes through the
			// durable no-effect classifier below so a prepared marker cannot leak.
			if w.ensureActionID == "" {
				if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stopped',
					state_due_at='',stopped_at=?,updated_at=? WHERE epic_id=? AND worker_role=?`,
					stamp, stamp, epicID, w.role); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='revoked',
					revoked_at=?,updated_at=? WHERE epic_id=? AND worker_role=? AND state<>'revoked'`,
					stamp, stamp, epicID, w.role); err != nil {
					return err
				}
				continue
			}
			created, noEffect, err := ensurePreEffectWorkspaceCleanupTx(ctx, tx, projectID, epicID,
				w.role, w.ensureActionID, now)
			if err != nil {
				return err
			}
			if created || noEffect {
				// The exact local cleanup action will remove the marker/worktree
				// before it projects stopped.  Never fabricate a Driver Stop for a
				// pre-effect Ensure, and never mark stopped before this cleanup.
				continue
			}
			continue // uncertain/delivered Ensure must reconcile before exact Stop.
		}
		if err != nil {
			return err
		}
		headSHA, baseSHA := "", ""
		sourceErr := tx.QueryRowContext(ctx, `SELECT head_sha,base_sha FROM epic_actions
			WHERE id=? AND epic_id=? AND project_id=?`, w.ensureActionID, epicID, projectID).
			Scan(&headSHA, &baseSHA)
		if errors.Is(sourceErr, sql.ErrNoRows) && w.role == "reviewer" {
			sourceErr = tx.QueryRowContext(ctx, `SELECT head_sha,base_sha FROM epic_deliveries
				WHERE epic_id=? AND project_id=?`, epicID, projectID).Scan(&headSHA, &baseSHA)
		} else if errors.Is(sourceErr, sql.ErrNoRows) {
			sourceErr = tx.QueryRowContext(ctx, `SELECT json_extract(bootstrap_payload,'$.source_commit_sha')
				FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role='builder'`,
				epicID, projectID).Scan(&baseSHA)
		}
		if sourceErr != nil {
			return fmt.Errorf("worker Stop immutable workspace source: %w", sourceErr)
		}
		if w.role == "reviewer" {
			if headSHA == "" {
				return errors.New("reviewer Stop has no immutable workspace head SHA")
			}
		} else {
			if !validGitObjectID(baseSHA) {
				return errors.New("builder Stop has no immutable workspace source SHA")
			}
		}
		dedup := strings.Join([]string{projectID, epicID, "worker_stop", w.role, binding.AgentRunID}, ":")
		idHash := sha256.Sum256([]byte(dedup))
		actionID := "worker-stop-" + hex.EncodeToString(idHash[:12])
		payload, _ := json.Marshal(map[string]string{"epic_id": epicID, "worker_role": w.role,
			"binding_id": binding.BindingID, "type": "worker_stop"})
		payloadHash := sha256.Sum256(payload)
		_, err = tx.ExecContext(ctx, `INSERT INTO epic_actions
			(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
			 executor_kind,target_role,target_host_id,target_store_id,target_server_domain_id,target_server_id,
			 lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,lease_id,lease_epoch,
			 recipient_session_id,recipient_pane_instance_id,recipient_agent_run_id,head_sha,base_sha,
			 next_attempt_at,created_at,updated_at)
			VALUES (?,?,?,'worker_stop','pending',0,?,?,?,'driver_lifecycle',
			 ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			actionID, projectID, epicID, dedup, string(payload), "sha256:"+hex.EncodeToString(payloadHash[:]),
			bindingRole, binding.HostID, binding.StoreID, binding.TmuxServerDomainID,
			binding.TmuxServerInstanceID, binding.LifecycleKey, binding.TargetEpoch, binding.ProfileID,
			binding.WorkspaceRootID, binding.WorkspaceRelativePath, "worker-stop:"+epicID+":"+w.role,
			binding.BindingEpoch, binding.SessionID, binding.PaneInstanceID, binding.AgentRunID,
			headSHA, baseSHA, stamp, stamp, stamp)
		if err != nil && !isUniqueConstraintErr(err) {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET stop_action_id=?,state_due_at=?,updated_at=?
			WHERE epic_id=? AND worker_role=? AND state='stop_pending'`, actionID,
			now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, epicID, w.role); err != nil {
			return err
		}
	}
	return nil
}

// ensurePreEffectWorkspaceCleanupTx materializes one Flowbee-local cleanup
// effect for an exact worker Ensure known not to have reached Driver.  It never
// infers no-effect from a retry/dead-letter state: the proof is either an
// unclaimed epoch-zero action or the immutable pre-effect certificate written
// by LifecycleRuntime before it invoked Driver.  A single persisted Driver
// receipt, including an uncertain one, defeats this path.
func ensurePreEffectWorkspaceCleanupTx(ctx context.Context, tx *sql.Tx, projectID, epicID, role,
	ensureActionID string, now time.Time) (created, certified bool, err error) {
	var state, executor, kind, targetRole, host, storeID, domain, server, lifecycle, profile, root, rel, head, base string
	var epoch, targetEpoch int64
	err = tx.QueryRowContext(ctx, `SELECT state,executor_kind,kind,action_epoch,target_role,target_host_id,
		target_store_id,target_server_domain_id,target_server_id,lifecycle_key,target_epoch,profile_id,
		workspace_root_id,workspace_relative_path,head_sha,base_sha
		FROM epic_actions WHERE id=? AND project_id=? AND epic_id=?`, ensureActionID, projectID, epicID).
		Scan(&state, &executor, &kind, &epoch, &targetRole, &host, &storeID, &domain, &server,
			&lifecycle, &targetEpoch, &profile, &root, &rel, &head, &base)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if executor != "driver_lifecycle" || !isWorkerEnsureKind(kind) || lifecycle == "" || root == "" || rel == "" {
		return false, false, nil
	}
	var receipts, certifiedRows int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_lifecycle_receipts WHERE action_id=?`,
		ensureActionID).Scan(&receipts); err != nil {
		return false, false, err
	}
	if receipts != 0 {
		return false, false, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_lifecycle_pre_effect_failures
		WHERE action_id=?`, ensureActionID).Scan(&certifiedRows); err != nil {
		return false, false, err
	}
	certified = (state == "pending" && epoch == 0) || certifiedRows == 1
	if !certified {
		return false, false, nil
	}
	// Fence a still-pending retry before materializing the local cleanup.  The
	// action epoch zero variant is known never to have crossed Driver; a
	// certificate variant is known to have failed before invocation.  Anything
	// else remains a visible uncertain hold above.
	stamp := now.UTC().Format(rfc3339)
	if state == "pending" {
		res, updateErr := tx.ExecContext(ctx, `UPDATE epic_actions SET state='cancelled_superseded',
			last_error='merged after certified pre-effect worker Ensure failure',updated_at=?
			WHERE id=? AND state='pending' AND action_epoch=?`, stamp, ensureActionID, epoch)
		if updateErr != nil {
			return false, false, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, false, nil
		}
	}
	if state != "pending" && state != "dead_letter" && state != "cancelled_superseded" {
		return false, false, nil
	}
	if targetRole == "" || host == "" || storeID == "" || domain == "" || server == "" || targetEpoch < 1 ||
		profile == "" || (role != "reviewer" && !validGitObjectID(base)) ||
		(role == "reviewer" && !validGitObjectID(head)) {
		return false, false, errors.New("certified pre-effect worker Ensure has incomplete immutable workspace authority")
	}
	dedup := strings.Join([]string{projectID, epicID, "worker_workspace_cleanup", role, ensureActionID}, ":")
	h := sha256.Sum256([]byte(dedup))
	actionID := "worker-workspace-cleanup-" + hex.EncodeToString(h[:12])
	payload, _ := json.Marshal(map[string]string{"type": "worker_workspace_cleanup", "epic_id": epicID,
		"worker_role": role, "ensure_action_id": ensureActionID})
	payloadHash := sha256.Sum256(payload)
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		executor_kind,target_role,target_host_id,target_store_id,target_server_domain_id,target_server_id,
		lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,head_sha,base_sha,
		next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'worker_workspace_cleanup','pending',0,?,?,?,'driver_lifecycle',
		?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, actionID, projectID, epicID, dedup, string(payload),
		"sha256:"+hex.EncodeToString(payloadHash[:]), targetRole, host, storeID, domain, server,
		lifecycle, targetEpoch, profile, root, rel, head, base, stamp, stamp, stamp)
	if err != nil {
		return false, false, err
	}
	// The action ID is deterministic.  The update is also the CAS that fences a
	// stale terminal reconciler from projecting stopped before the filesystem
	// cleanup completed.
	res, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET cleanup_action_id=?,state_due_at=?,updated_at=?
		WHERE epic_id=? AND project_id=? AND worker_role=? AND state='stop_pending'
		AND (cleanup_action_id='' OR cleanup_action_id=?)`, actionID,
		now.Add(10*time.Minute).UTC().Format(rfc3339), stamp, epicID, projectID, role, actionID)
	if err != nil {
		return false, false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, false, nil
	}
	return true, true, nil
}

func isWorkerEnsureKind(kind string) bool {
	switch kind {
	case "builder_launch", "builder_rework", "conflict_resolution", "reviewer_launch", "worker_recover":
		return true
	default:
		return false
	}
}

func projectEpicWorkerStopTx(ctx context.Context, tx *sql.Tx, action BuilderLifecycleActionProjection,
	receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	if receipt.Operation != "stop" || (receipt.Status != "stopped" && receipt.Status != "target_absent") ||
		receipt.AbsenceObservedAt == "" {
		return fmt.Errorf("worker stop lacks positive absence: %s", receipt.Status)
	}
	var payload struct {
		Role, BindingID string
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(action.Payload), &raw); err != nil {
		return err
	}
	payload.Role, payload.BindingID = raw["worker_role"], raw["binding_id"]
	if payload.Role == "" || payload.BindingID == "" {
		return errors.New("worker stop payload has no exact worker/binding identity")
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM epic_worker_sessions
		WHERE epic_id=? AND project_id=? AND worker_role=? AND stop_action_id=?`,
		action.EpicID, action.ProjectID, payload.Role, action.ActionID).Scan(&state); err != nil {
		return err
	}
	if state == "stopped" {
		return nil
	}
	if state != "stop_pending" {
		return fmt.Errorf("worker stop found state %s", state)
	}
	stamp := now.UTC().Format(rfc3339)
	res, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',
		superseded_at=?,updated_at=? WHERE binding_id=? AND project_id=? AND state='active'
		AND host_id=? AND store_id=? AND tmux_server_domain_id=? AND tmux_server_instance_id=?
		AND lifecycle_key=? AND target_epoch=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?`,
		stamp, stamp, payload.BindingID, action.ProjectID, action.TargetHostID, action.TargetStoreID,
		action.TargetServerDomainID, action.TargetServerID, action.LifecycleKey, action.TargetEpoch,
		action.RecipientSessionID, action.RecipientPaneInstanceID, action.RecipientAgentRunID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return errors.New("worker stop exact binding was superseded or replaced")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stopped',state_due_at='',stopped_at=?,
		updated_at=? WHERE epic_id=? AND worker_role=? AND stop_action_id=? AND state='stop_pending'`,
		stamp, stamp, action.EpicID, payload.Role, action.ActionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='revoked',
		revoked_at=?,updated_at=? WHERE epic_id=? AND worker_role=? AND state<>'revoked'`,
		stamp, stamp, action.EpicID, payload.Role); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, action.ProjectID, action.EpicID,
		"epic_worker_stopped", "cleanup_pending", "cleanup_pending", 0, "driver",
		fmt.Sprintf(`{"worker_role":%q,"remote_absence":true}`, payload.Role), now)
}

// projectEpicWorkerPreEffectWorkspaceCleanupTx is the local counterpart to an
// exact Driver Stop.  It accepts no Driver receipt: the preceding runtime step
// removed only the marker/worktree belonging to the immutable original Ensure.
// We recheck the durable no-effect proof here as a final transaction boundary
// before closing the worker and revoking its credential.
func projectEpicWorkerPreEffectWorkspaceCleanupTx(ctx context.Context, tx *sql.Tx,
	action BuilderLifecycleActionProjection, receipt BuilderLifecycleReceiptProjection, now time.Time) error {
	if receipt.Operation != "workspace_cleanup" || receipt.Status != "cleaned" ||
		receipt.ActionID != action.ActionID || receipt.ActionEpoch != action.Epoch {
		return errors.New("pre-effect workspace cleanup lacks exact local completion")
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(action.Payload), &raw); err != nil {
		return err
	}
	role, ensureActionID := raw["worker_role"], raw["ensure_action_id"]
	if (role != "builder" && role != "reviewer") || ensureActionID == "" {
		return errors.New("pre-effect workspace cleanup payload is incomplete")
	}
	var state, cleanupID, ensureID string
	if err := tx.QueryRowContext(ctx, `SELECT state,cleanup_action_id,ensure_action_id
		FROM epic_worker_sessions WHERE epic_id=? AND project_id=? AND worker_role=?`,
		action.EpicID, action.ProjectID, role).Scan(&state, &cleanupID, &ensureID); err != nil {
		return err
	}
	if state == "stopped" {
		return nil
	}
	if state != "stop_pending" || cleanupID != action.ActionID || ensureID != ensureActionID {
		return errors.New("pre-effect workspace cleanup lost worker action fence")
	}
	var receiptCount, certificateCount int
	var ensureState string
	var ensureEpoch int64
	if err := tx.QueryRowContext(ctx, `SELECT state,action_epoch FROM epic_actions
		WHERE id=? AND project_id=? AND epic_id=?`, ensureActionID, action.ProjectID, action.EpicID).
		Scan(&ensureState, &ensureEpoch); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_lifecycle_receipts WHERE action_id=?`,
		ensureActionID).Scan(&receiptCount); err != nil {
		return err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_lifecycle_pre_effect_failures WHERE action_id=?`,
		ensureActionID).Scan(&certificateCount); err != nil {
		return err
	}
	if receiptCount != 0 || !((ensureState == "cancelled_superseded" && ensureEpoch == 0) || certificateCount == 1) {
		return errors.New("pre-effect workspace cleanup no-effect certificate no longer holds")
	}
	stamp := now.UTC().Format(rfc3339)
	if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stopped',state_due_at='',
		stopped_at=?,updated_at=? WHERE epic_id=? AND project_id=? AND worker_role=?
		AND state='stop_pending' AND cleanup_action_id=?`, stamp, stamp, action.EpicID,
		action.ProjectID, role, action.ActionID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='revoked',revoked_at=?,updated_at=?
		WHERE epic_id=? AND worker_role=? AND state<>'revoked'`, stamp, stamp, action.EpicID, role); err != nil {
		return err
	}
	return appendEpicControlEventTx(ctx, tx, action.ProjectID, action.EpicID,
		"epic_worker_stopped", "cleanup_pending", "cleanup_pending", 0, "workspace",
		fmt.Sprintf(`{"worker_role":%q,"remote_absence":false,"pre_effect_workspace_cleanup":true}`, role), now)
}

type EpicWorkerStopReconcileResult struct{ Scanned, ActionsEnsured, Held int }

// ReconcileEpicWorkerStops is the delivery-agnostic merge shutdown backstop.
// Terminal artifact folding may return early forever after the merge fact; this
// independent loop therefore owns every non-stopped worker until an exact Stop
// is durable or an operator-visible hold exists.
func (s *Store) ReconcileEpicWorkerStops(ctx context.Context, now time.Time) (EpicWorkerStopReconcileResult, error) {
	var out EpicWorkerStopReconcileResult
	enabled := s.EnableEpicDedicatedWorkersV2
	if !enabled {
		var err error
		enabled, err = s.DurableEpicDedicatedWorkersV2(ctx)
		if err != nil {
			return out, err
		}
	}
	if !enabled {
		return out, nil
	}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT w.project_id,w.epic_id
			FROM epic_worker_sessions w JOIN epic_deliveries d ON d.epic_id=w.epic_id
			LEFT JOIN epic_artifacts a ON a.epic_id=w.epic_id
			WHERE w.state<>'stopped' AND (a.merged=1 OR d.state IN ('merged','cleanup_pending','complete'))`)
		if err != nil {
			return err
		}
		type item struct{ projectID, epicID string }
		var items []item
		for rows.Next() {
			var item item
			if err := rows.Scan(&item.projectID, &item.epicID); err != nil {
				rows.Close()
				return err
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range items {
			out.Scanned++
			before := 0
			_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id=?
				AND kind='worker_stop'`, item.epicID).Scan(&before)
			if err := ensureEpicWorkerStopIntentsTx(ctx, tx, item.projectID, item.epicID, now); err != nil {
				return err
			}
			after := 0
			_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions WHERE epic_id=?
				AND kind='worker_stop'`, item.epicID).Scan(&after)
			out.ActionsEnsured += after - before
			missing, err := tx.QueryContext(ctx, `SELECT worker_role,worker_identity,lifecycle_key,target_epoch
				FROM epic_worker_sessions WHERE epic_id=? AND state='stop_pending' AND stop_action_id=''
				AND cleanup_action_id=''`, item.epicID)
			if err != nil {
				return err
			}
			type unresolved struct {
				role, identity, lifecycle string
				epoch                     int64
			}
			var unresolvedWorkers []unresolved
			for missing.Next() {
				var worker unresolved
				if err := missing.Scan(&worker.role, &worker.identity, &worker.lifecycle, &worker.epoch); err != nil {
					missing.Close()
					return err
				}
				unresolvedWorkers = append(unresolvedWorkers, worker)
			}
			missing.Close()
			for _, worker := range unresolvedWorkers {
				out.Held++
				stamp := now.UTC().Format(rfc3339)
				if _, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='held',state_due_at=?,updated_at=?
					WHERE epic_id=? AND worker_role=? AND state='stop_pending'`,
					now.Add(5*time.Minute).UTC().Format(rfc3339), stamp, item.epicID, worker.role); err != nil {
					return err
				}
				dedup := "epic_worker_stop_unresolved:" + item.epicID + ":" + worker.role
				payload, _ := json.Marshal(map[string]any{"epic_id": item.epicID, "worker_role": worker.role,
					"worker_identity": worker.identity, "lifecycle_key": worker.lifecycle,
					"target_epoch": worker.epoch, "next_action": "resolve_exact_driver_presence"})
				attentionID := "worker-stop-unresolved-" + item.epicID + "-" + worker.role
				if _, err := tx.ExecContext(ctx, `INSERT INTO attention_items
					(id,project_id,kind,epic_id,repo,priority,state,dedup_key,blocking,evidence_json,
					 detail,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
					VALUES (?,?,'epic_worker_stop_unresolved',?,'',10,'open',?,1,?,
					 'merged epic worker has no exact active binding; Driver absence must be proven',1,?,?,?,?)
					ON CONFLICT DO NOTHING`, attentionID, item.projectID, item.epicID, dedup,
					string(payload), stamp, stamp, stamp, stamp); err != nil {
					return err
				}
				if err := ensureControlAlertTx(ctx, tx, item.projectID, item.epicID,
					"epic_worker_stop_unresolved", dedup, string(payload), now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return out, err
}

func epicWorkerCharter(role string) string {
	if role == "reviewer" {
		return "Independently review the authoritative PR diff at the Flowbee-supplied head/base fence; never build, merge, or accept transport success as a verdict. Return only mechanically attributable stage evidence."
	}
	return "Implement only the admitted epic scope on the assigned branch; never call GitHub directly, self-review, or treat terminal prose as stage evidence. Report progress through Flowbee-owned evidence."
}

// EpicWorkerDisplayName is the desired human-visible name. It is always project-qualified, including the default project,
// so moving an epic between project namespaces can never alias display intent.
// Driver route authority remains its stable identity tuple, never this name.
func EpicWorkerDisplayName(projectID, modelFamily, slug string) string {
	modelFamily = strings.ToLower(strings.TrimSpace(modelFamily))
	projectID = strings.ToLower(strings.TrimSpace(projectID))
	if projectID == "" {
		projectID = "default"
	}
	return "flowbee-worker-" + modelFamily + "-" + projectID + "-" + slug
}

// EpicWorkerLifecycleKey is opaque routing/lifecycle authority. It is derived
// only from immutable admission identity and role, never from a human-visible
// name, model label, project rename, or tmux name.
func EpicWorkerLifecycleKey(epicID, role string) string {
	h := sha256.Sum256([]byte(epicID + "\x00" + role))
	return "epic-worker-" + hex.EncodeToString(h[:16]) + "-" + role
}

func epicWorkerProfileID(family, role string) string {
	return strings.ToLower(strings.TrimSpace(family)) + "_" + role
}

func ReviewerDriverIdentity(epicID string) string { return "epic-reviewer:" + epicID }

// EpicWorkerFlowbeeIdentity is the bearer/attestation identity used at the
// worker API. It is deliberately separate from Driver's route identity and uses
// dots (never colons), matching the existing enrolled-identity grammar where a
// colon suffix is reserved for operator-bound model family.
func EpicWorkerFlowbeeIdentity(role, epicID string) string {
	return "epic-worker." + role + "." + epicID
}

func insertEpicWorkerSessionsTx(ctx context.Context, tx *sql.Tx, e EpicRun,
	reviewerFamily string, now time.Time) error {
	if reviewerFamily == "" || reviewerFamily == e.BuilderModelFamily {
		return fmt.Errorf("epic worker plan requires a distinct reviewer family: builder=%s reviewer=%s",
			e.BuilderModelFamily, reviewerFamily)
	}
	if e.WorkerBootstrapMaterials == nil {
		return errors.New("epic worker plan requires authoritative spec and discipline material")
	}
	materials, err := normalizeEpicWorkerBootstrapMaterials(*e.WorkerBootstrapMaterials)
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	workers := []struct {
		role, family, identity string
	}{
		{DriverBuilderRole, e.BuilderModelFamily, BuilderDriverIdentity(e.ID)},
		{"reviewer", reviewerFamily, ReviewerDriverIdentity(e.ID)},
	}
	for _, worker := range workers {
		charter := epicWorkerCharter(worker.role)
		charterHash := sha256.Sum256([]byte(charter))
		discipline := materials.BuilderDisciplineUTF8
		if worker.role == "reviewer" {
			discipline = materials.ReviewerDisciplineUTF8
		}
		flowbeeIdentity := EpicWorkerFlowbeeIdentity(worker.role, e.ID)
		credentialInstallRef := "flowbee://worker-credentials/" + e.ProjectID + "/" + e.ID + "/" + worker.role
		bootstrap := epicWorkerBootstrap{Format: EpicWorkerBootstrapFormat,
			ProjectID: e.ProjectID, EpicID: e.ID, Role: worker.role, Family: worker.family,
			Repository: e.Repo, Branch: e.Branch, SpecPath: e.FilePath, Scope: append([]string(nil), e.Scope...),
			EpicSpecGoalFormat: materials.GoalFormat, EpicSpecGoalUTF8: materials.EpicSpecGoalUTF8,
			EpicSpecGoalSHA256:      sha256String(materials.EpicSpecGoalUTF8),
			AdmissionContractSHA256: materials.AdmissionContractSHA256,
			SourceArtifactSHA256:    materials.SourceArtifactSHA256,
			SourceCommitSHA:         materials.SourceCommitSHA,
			RoleCharter:             charter, RoleCharterSHA256: "sha256:" + hex.EncodeToString(charterHash[:]),
			DisciplineKind: worker.role, DisciplineUTF8: discipline, DisciplineSHA256: sha256String(discipline),
			ReferenceDocuments:        append([]EpicWorkerReferenceDocument(nil), materials.ReferenceDocuments...),
			ReferenceManifestSHA256:   epicWorkerReferenceManifestHash(materials.ReferenceDocuments),
			ArtifactContextRef:        "flowbee://projects/" + e.ProjectID + "/epics/" + e.ID + "/artifact",
			ArtifactHeadFenceRequired: worker.role == "reviewer",
			// The ledger carries only an opaque, one-shot install reference and
			// refresh lineage. The resolved Flowbee worker bearer/certificate is
			// supplied out-of-band by the credential installer and never appears
			// in this manifest, an action, a Driver receipt, or a log. Workers still
			// receive no GitHub credential.
			CredentialPolicyRef:   "flowbee://credential-policies/worker-control-plane-only",
			CredentialInstallRef:  credentialInstallRef,
			FlowbeeWorkerIdentity: flowbeeIdentity}
		payload, err := json.Marshal(bootstrap)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(payload)
		displayName := EpicWorkerDisplayName(e.ProjectID, worker.family, e.Slug)
		lifecycleKey := EpicWorkerLifecycleKey(e.ID, worker.role)
		if _, err := tx.ExecContext(ctx, `INSERT INTO epic_worker_sessions
			(epic_id,project_id,worker_role,model_family,worker_identity,flowbee_identity,lifecycle_key,
			 display_name,state,target_epoch,bootstrap_format,bootstrap_payload,
			 bootstrap_sha256,bootstrap_state,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,'planned',1,?,?,?,'committed',?,?)`, e.ID, e.ProjectID,
			worker.role, worker.family, worker.identity, flowbeeIdentity, lifecycleKey, displayName, EpicWorkerBootstrapFormat,
			string(payload), "sha256:"+hex.EncodeToString(hash[:]), stamp, stamp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO epic_worker_credentials
			(epic_id,project_id,worker_role,flowbee_identity,install_ref,state,generation,
			 created_at,updated_at) VALUES (?,?,?,?,?,'planned',0,?,?)`, e.ID, e.ProjectID,
			worker.role, flowbeeIdentity, credentialInstallRef, stamp, stamp); err != nil {
			return err
		}
	}
	return nil
}

type EpicWorkerCredential struct {
	EpicID, ProjectID, WorkerRole, FlowbeeIdentity, InstallRef        string
	State, EnvelopeRef, PayloadSHA256, RefreshLineage, EnsureActionID string
	Generation                                                        int64
	IssuedAt, RefreshAfter, ExpiresAt, InstalledAt, RevokedAt         string
}

func scanEpicWorkerCredential(row *sql.Row) (EpicWorkerCredential, error) {
	var c EpicWorkerCredential
	err := row.Scan(&c.EpicID, &c.ProjectID, &c.WorkerRole, &c.FlowbeeIdentity,
		&c.InstallRef, &c.State, &c.Generation, &c.EnvelopeRef, &c.PayloadSHA256, &c.RefreshLineage,
		&c.EnsureActionID, &c.IssuedAt, &c.RefreshAfter, &c.ExpiresAt,
		&c.InstalledAt, &c.RevokedAt)
	return c, err
}

func epicWorkerCredentialTx(ctx context.Context, tx *sql.Tx, epicID, role string) (EpicWorkerCredential, error) {
	return scanEpicWorkerCredential(tx.QueryRowContext(ctx, `SELECT epic_id,project_id,worker_role,
		flowbee_identity,install_ref,state,generation,envelope_ref,payload_sha256,refresh_lineage,
		ensure_action_id,issued_at,refresh_after,expires_at,installed_at,revoked_at
		FROM epic_worker_credentials WHERE epic_id=? AND worker_role=?`, epicID, role))
}

func (s *Store) issueEpicWorkerCredentialTx(ctx context.Context, tx *sql.Tx, epicID, role,
	effectActionID string, now time.Time) (EpicWorkerCredential, error) {
	return s.issueEpicWorkerCredentialGenerationTx(ctx, tx, epicID, role, effectActionID, false, now)
}

// issueEpicWorkerCredentialGenerationTx is the only generation transition. A
// replacement is authorized only by the exact absence recovery transaction,
// which advances the lifecycle target epoch and commits the replacement action
// around this call. Ordinary callers remain unable to rewrite a committed
// Driver idempotency body.
func (s *Store) issueEpicWorkerCredentialGenerationTx(ctx context.Context, tx *sql.Tx, epicID, role,
	effectActionID string, allowReplacement bool, now time.Time) (EpicWorkerCredential, error) {
	current, err := epicWorkerCredentialTx(ctx, tx, epicID, role)
	if err != nil {
		return EpicWorkerCredential{}, err
	}
	if current.State == "revoked" {
		return EpicWorkerCredential{}, errors.New("revoked worker credential cannot be reissued")
	}
	// action_id is Driver's lifecycle idempotency key. Once it binds a
	// credential envelope, the exact generation/hash are immutable forever --
	// including after refresh/expiry and crash-uncertain recovery.
	if current.Generation > 0 && effectActionID != "" && current.EnsureActionID == effectActionID {
		return current, nil
	}
	if current.Generation > 0 && effectActionID != "" && current.EnsureActionID != effectActionID && !allowReplacement {
		return EpicWorkerCredential{}, errors.New("credential rotation requires a new lifecycle target epoch")
	}
	if allowReplacement && (current.Generation < 1 || effectActionID == "" || current.EnsureActionID == effectActionID) {
		return EpicWorkerCredential{}, errors.New("credential replacement requires a distinct lifecycle action")
	}
	generation := current.Generation + 1
	if effectActionID == "" {
		effectActionID = fmt.Sprintf("worker-credential-refresh:%s:%s:%d", epicID, role, generation)
	}
	envelopeRef := fmt.Sprintf("flowbee://credential-envelopes/%s/%s/%s/%d/%s",
		current.ProjectID, epicID, role, generation, effectActionID)
	lineageBytes := sha256.Sum256([]byte(strings.Join([]string{current.RefreshLineage,
		current.EnvelopeRef, current.FlowbeeIdentity, fmt.Sprint(generation)}, "\x00")))
	stamp := now.UTC().Format(rfc3339)
	// Driver v3 has no credential refresh operation. A live long-running epic
	// must not silently self-deauthorize after 24 hours, and replay/rebind must
	// preserve the exact immutable Ensure body. Durable generation, Stop,
	// replacement, binding, and revocation state are the per-request trust root;
	// the signed expiry is therefore practically non-expiring.
	expiresTime := projectActorCredentialPracticalExpiry
	refreshAt := expiresTime.Format(rfc3339)
	expiresAt := expiresTime.Format(rfc3339)
	if s.EpicWorkerCredentialMaterializer == nil {
		return EpicWorkerCredential{}, errors.New("worker credential envelope materializer unavailable")
	}
	payloadHash, err := s.EpicWorkerCredentialMaterializer(current.FlowbeeIdentity,
		current.ProjectID, role, envelopeRef, generation, expiresTime)
	if err != nil {
		return EpicWorkerCredential{}, fmt.Errorf("materialize worker credential envelope: %w", err)
	}
	if payloadHash == "" {
		return EpicWorkerCredential{}, errors.New("materialize worker credential envelope: empty hash")
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='issued',
		generation=?,envelope_ref=?,payload_sha256=?,refresh_lineage=?,ensure_action_id=?,issued_at=?,
		refresh_after=?,expires_at=?,installed_at='',updated_at=?
		WHERE epic_id=? AND worker_role=? AND generation=? AND ensure_action_id=? AND state<>'revoked'`, generation,
		envelopeRef, payloadHash, "sha256:"+hex.EncodeToString(lineageBytes[:]), effectActionID, stamp,
		refreshAt, expiresAt, stamp, epicID, role, current.Generation, current.EnsureActionID)
	if err != nil {
		return EpicWorkerCredential{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return EpicWorkerCredential{}, errors.New("worker credential generation changed concurrently")
	}
	return epicWorkerCredentialTx(ctx, tx, epicID, role)
}

// RotateEpicWorkerCredential deliberately refuses the old envelope-only
// rotation path. Driver v3 binds credential hash+epoch into the Ensure action;
// replacement must atomically create a new lifecycle action and advance both
// target and credential epochs. Until that replacement transition is active,
// expiry is a visible fail-closed hold rather than a changed-body replay.
func (s *Store) RotateEpicWorkerCredential(ctx context.Context, epicID, role string,
	now time.Time) (EpicWorkerCredential, error) {
	return EpicWorkerCredential{}, errors.New("credential replacement requires a new lifecycle action and higher target epoch")
}

// AuthorizeEpicWorkerCredential is the live worker-API revocation check. HMAC
// possession alone is insufficient: the exact credential id/generation must
// still be current, unexpired, and attached to a non-stopped worker session.
func (s *Store) AuthorizeEpicWorkerCredential(ctx context.Context, identity,
	projectID, role, credentialID string, generation int64, now time.Time) bool {
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_credentials c
		JOIN epic_worker_sessions w ON w.epic_id=c.epic_id AND w.worker_role=c.worker_role
		JOIN driver_session_bindings b ON b.binding_id=w.binding_id
		WHERE c.flowbee_identity=? AND c.project_id=? AND c.worker_role=?
		  AND w.flowbee_identity=c.flowbee_identity AND w.project_id=c.project_id
		  AND c.envelope_ref=? AND c.generation=?
		  AND c.state IN ('issued','installed') AND c.expires_at>? AND c.revoked_at=''
		  AND w.state='active' AND w.target_epoch=c.generation
		  AND b.state='active' AND b.project_id=w.project_id AND b.worker_identity=w.worker_identity
		  AND b.lifecycle_key=w.lifecycle_key AND b.target_epoch=w.target_epoch`, identity, projectID, role,
		credentialID, generation,
		now.UTC().Format(rfc3339)).Scan(&n)
	return err == nil && n == 1
}

// ensureEpicReviewerLaunchTx commits a dedicated reviewer Ensure before the
// native review job becomes claimable. The target is an operator-bound seat
// target, never inferred from a tmux name, cwd, or nearby shared reviewer.
func ensureEpicReviewerLaunchTx(ctx context.Context, s *Store, tx *sql.Tx, projectID, epicID,
	repo string, prNumber int, head, base string, now time.Time) (bool, error) {
	if repo == "" || prNumber < 1 || head == "" || base == "" {
		return false, errors.New("reviewer launch requires exact repository, PR, head, and base identity")
	}
	if err := requireExactlyTwoEpicWorkerPlansTx(ctx, s, tx, projectID, epicID, ""); err != nil {
		return false, fmt.Errorf("reviewer launch dedicated-worker invariant: %w", err)
	}
	var state, family, lifecycleKey, workerIdentity, bootstrapHash string
	var targetEpoch int64
	err := tx.QueryRowContext(ctx, `SELECT state,model_family,lifecycle_key,worker_identity,
		target_epoch,bootstrap_sha256 FROM epic_worker_sessions
		WHERE epic_id=? AND project_id=? AND worker_role='reviewer'`, epicID, projectID).
		Scan(&state, &family, &lifecycleKey, &workerIdentity, &targetEpoch, &bootstrapHash)
	if err != nil {
		return false, err
	}
	if state == "active" {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings
			WHERE project_id=? AND worker_identity=? AND role=? AND state='active'
			  AND lifecycle_key=? AND target_epoch=?`, projectID, workerIdentity,
			DriverReviewerRole, lifecycleKey, targetEpoch).Scan(&n); err != nil {
			return false, err
		}
		return n == 1, nil
	}
	if state == "ensure_pending" {
		return false, nil
	}
	if state == "stop_pending" || state == "stopped" {
		return false, fmt.Errorf("reviewer lifecycle is already stopping for epic %s", epicID)
	}

	type reviewerTarget struct{ seatID, hostID, storeID, domainID, serverID, profileID, rootID, relativeBase string }
	rows, queryErr := tx.QueryContext(ctx, `SELECT s.id,i.host_id,i.store_id,
		t.tmux_server_domain_id,t.tmux_server_instance_id,t.profile_id,
		t.workspace_root_id,t.workspace_relative_base
		FROM seats s
		JOIN builder_driver_targets t ON t.project_id=? AND t.seat_id=s.id AND t.enabled=1
		JOIN driver_instances i ON i.instance_ref=t.instance_ref AND i.state='live'
		WHERE s.enabled=1 AND s.agent_family=? ORDER BY s.id`, projectID, family)
	if queryErr != nil {
		return false, queryErr
	}
	var selected reviewerTarget
	var routeReasons []string
	for rows.Next() {
		var candidate reviewerTarget
		if err := rows.Scan(&candidate.seatID, &candidate.hostID, &candidate.storeID,
			&candidate.domainID, &candidate.serverID, &candidate.profileID,
			&candidate.rootID, &candidate.relativeBase); err != nil {
			rows.Close()
			return false, err
		}
		decision, err := capacityRouteForSeatQuery(ctx, tx, candidate.seatID, now, 5*time.Minute)
		if err != nil {
			rows.Close()
			return false, err
		}
		if candidate.profileID != epicWorkerProfileID(family, "reviewer") {
			routeReasons = append(routeReasons, candidate.seatID+"=profile_role_mismatch")
			continue
		}
		if decision.Routable && selected.seatID == "" {
			selected = candidate
		} else if !decision.Routable {
			routeReasons = append(routeReasons, candidate.seatID+"="+strings.Join(decision.Reasons, ","))
		}
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if selected.seatID == "" {
		detail := "no fresh exact Driver lifecycle target has reviewer capacity for family " + family
		if len(routeReasons) > 0 {
			detail += ": " + strings.Join(routeReasons, ";")
		}
		stamp := now.UTC().Format(rfc3339)
		if _, updateErr := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='held',updated_at=?
			WHERE epic_id=? AND worker_role='reviewer' AND state IN ('planned','held')`,
			stamp, epicID); updateErr != nil {
			return false, updateErr
		}
		payload, _ := json.Marshal(map[string]any{"epic_id": epicID, "role": "reviewer", "reason": detail})
		if alertErr := ensureControlAlertTx(ctx, tx, projectID, epicID,
			"reviewer_lifecycle_unavailable", "reviewer_lifecycle_unavailable:"+epicID,
			string(payload), now); alertErr != nil {
			return false, alertErr
		}
		return false, nil
	}
	seatID, hostID, storeID := selected.seatID, selected.hostID, selected.storeID
	domainID, serverID := selected.domainID, selected.serverID
	profileID, rootID, relativeBase := selected.profileID, selected.rootID, selected.relativeBase

	diffRef := fmt.Sprintf("flowbee://projects/%s/epics/%s/pulls/%d/diff/%s..%s",
		projectID, epicID, prNumber, base, head)
	payload, _ := json.Marshal(map[string]any{
		"type": "reviewer_launch", "project_id": projectID, "epic_id": epicID,
		"repo": repo, "pr_number": prNumber, "head_sha": head, "base_sha": base,
		"diff_reference": diffRef, "seat_id": seatID, "bootstrap_sha256": bootstrapHash,
	})
	if err := validateEpicWorkerLifecycleBootstrapSizeTx(ctx, tx, epicID, "reviewer", string(payload)); err != nil {
		return false, err
	}
	dedup := strings.Join([]string{"reviewer_launch", projectID, epicID, head, base}, ":")
	idHash := sha256.Sum256([]byte(dedup))
	actionID := "reviewer-launch-" + hex.EncodeToString(idHash[:12])
	payloadHash := sha256.Sum256(payload)
	stamp := now.UTC().Format(rfc3339)
	workspace := path.Join(relativeBase, projectID, epicID, "review")
	_, err = tx.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,action_epoch,dedup_key,payload_json,payload_sha256,
		 executor_kind,target_role,target_host_id,target_store_id,target_server_domain_id,target_server_id,
		 lifecycle_key,target_epoch,profile_id,workspace_root_id,workspace_relative_path,
		 lease_id,lease_epoch,head_sha,base_sha,next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,'reviewer_launch','pending',0,?,?,?,'driver_lifecycle','code_reviewer',
		?,?,?,?,?,1,?,?,?,?,1,?,?,?,?,?)`, actionID, projectID, epicID, dedup,
		string(payload), "sha256:"+hex.EncodeToString(payloadHash[:]), hostID, storeID,
		domainID, serverID, lifecycleKey, profileID, rootID, workspace,
		"reviewer-compute:"+epicID, head, base, stamp, stamp, stamp)
	if err != nil && !isUniqueConstraintErr(err) {
		return false, err
	}
	if _, err := s.issueEpicWorkerCredentialTx(ctx, tx, epicID, "reviewer", actionID, now); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='ensure_pending',
		seat_id=?,ensure_action_id=?,updated_at=? WHERE epic_id=? AND worker_role='reviewer'
		AND state IN ('planned','held') AND COALESCE(seat_id,'')=''`, seatID, actionID, stamp, epicID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("reviewer launch worker state changed concurrently")
	}
	return false, nil
}
