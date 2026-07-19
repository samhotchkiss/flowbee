package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	// DriverControlIdentity is the stable Flowbee-owned sender principal used for
	// control-plane → managed-session routes. It is resolved from the durable
	// binding registry; a review claimant may never inject sender coordinates.
	DriverControlIdentity  = "flowbee-control"
	DriverControlRole      = "flowbee_control"
	DriverReviewerRole     = "code_reviewer"
	DriverBuilderRole      = "builder"
	DriverInteractorRole   = "interactor"
	DriverOrchestratorRole = "orchestrator"
)

func BuilderDriverIdentity(epicID string) string { return "epic-builder:" + epicID }

var ErrDriverSessionBindingMissing = errors.New("exact Driver session binding is missing")
var ErrDriverControlOriginUnavailable = errors.New("GAP-FD-003: authenticated non-session Driver control origin is unavailable")

// DriverSessionBinding is a durable, exact Driver incarnation. Raw tmux names,
// pane numbers, CWDs, PIDs, provider text, and wall-clock proximity are
// deliberately absent. Re-observing a different store, pane, or agent run mints
// a successor epoch instead of silently moving old authority.
type DriverSessionBinding struct {
	BindingID             string
	ProjectID             string
	WorkerIdentity        string
	Role                  string
	BindingEpoch          int64
	HostID                string
	StoreID               string
	TmuxServerInstanceID  string
	LifecycleKey          string
	TargetEpoch           int64
	ProfileID             string
	WorkspaceRootID       string
	WorkspaceRelativePath string
	SessionID             string
	PaneInstanceID        string
	AgentRunID            string
	Provider              string
	ConversationID        string
	ObservedAt            time.Time
}

func (b DriverSessionBinding) validate() error {
	if b.ProjectID == "" {
		b.ProjectID = "default"
	}
	for name, value := range map[string]string{
		"project_id": b.ProjectID, "worker_identity": b.WorkerIdentity, "role": b.Role,
		"host_id": b.HostID, "store_id": b.StoreID,
		"tmux_server_instance_id": b.TmuxServerInstanceID, "lifecycle_key": b.LifecycleKey,
		"profile_id": b.ProfileID, "workspace_root_id": b.WorkspaceRootID,
		"workspace_relative_path": b.WorkspaceRelativePath, "session_id": b.SessionID,
		"pane_instance_id": b.PaneInstanceID, "agent_run_id": b.AgentRunID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("Driver session binding requires %s", name)
		}
	}
	if b.TargetEpoch < 1 {
		return errors.New("Driver session binding requires positive target_epoch")
	}
	return nil
}

func sameDriverBinding(a, b DriverSessionBinding) bool {
	return a.ProjectID == b.ProjectID && a.WorkerIdentity == b.WorkerIdentity && a.Role == b.Role &&
		a.HostID == b.HostID && a.StoreID == b.StoreID &&
		a.TmuxServerInstanceID == b.TmuxServerInstanceID && a.LifecycleKey == b.LifecycleKey &&
		a.TargetEpoch == b.TargetEpoch && a.ProfileID == b.ProfileID &&
		a.WorkspaceRootID == b.WorkspaceRootID && a.WorkspaceRelativePath == b.WorkspaceRelativePath &&
		a.SessionID == b.SessionID && a.PaneInstanceID == b.PaneInstanceID &&
		a.AgentRunID == b.AgentRunID && a.Provider == b.Provider && a.ConversationID == b.ConversationID
}

func driverBindingID(b DriverSessionBinding, epoch int64) string {
	material := fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s",
		b.ProjectID, b.WorkerIdentity, b.Role, epoch, b.StoreID, b.SessionID,
		b.PaneInstanceID, b.AgentRunID, b.LifecycleKey)
	h := sha256.Sum256([]byte(material))
	return "driver-binding-" + hex.EncodeToString(h[:12])
}

// UpsertDriverSessionBinding activates one exact incarnation. Exact replays are
// no-ops. A changed incarnation supersedes, but never overwrites, its predecessor.
// Binding changes also release a prior missing-binding hold for a fresh claim
// attempt; the claim transaction will immediately restore the hold if the route
// is still incomplete.
func (s *Store) UpsertDriverSessionBinding(ctx context.Context, in DriverSessionBinding, now time.Time) (DriverSessionBinding, error) {
	if in.ProjectID == "" {
		in.ProjectID = "default"
	}
	if in.ObservedAt.IsZero() {
		in.ObservedAt = now
	}
	if err := in.validate(); err != nil {
		return DriverSessionBinding{}, err
	}
	var out DriverSessionBinding
	err := s.tx(ctx, func(tx *sql.Tx) error {
		current, err := activeDriverSessionBindingTx(ctx, tx, in.ProjectID, in.WorkerIdentity, in.Role)
		hadCurrent := err == nil
		switch {
		case err == nil && sameDriverBinding(current, in):
			out = current
			return nil
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return err
		}

		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(binding_epoch),0)+1
			FROM driver_session_bindings WHERE project_id=? AND worker_identity=? AND role=?`,
			in.ProjectID, in.WorkerIdentity, in.Role).Scan(&epoch); err != nil {
			return err
		}
		stamp := now.UTC().Format(rfc3339)
		if hadCurrent {
			if _, err := tx.ExecContext(ctx, `UPDATE driver_session_bindings
				SET state='superseded',superseded_at=?,updated_at=?
				WHERE binding_id=? AND state='active'`, stamp, stamp, current.BindingID); err != nil {
				return err
			}
		}
		in.BindingEpoch = epoch
		in.BindingID = driverBindingID(in, epoch)
		_, err = tx.ExecContext(ctx, `INSERT INTO driver_session_bindings
			(binding_id,project_id,worker_identity,role,binding_epoch,state,host_id,store_id,
			 tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
			 workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
			 conversation_id,observed_at,created_at,updated_at)
			VALUES (?,?,?,?,?,'active',?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.BindingID,
			in.ProjectID, in.WorkerIdentity, in.Role, epoch, in.HostID, in.StoreID,
			in.TmuxServerInstanceID, in.LifecycleKey, in.TargetEpoch, in.ProfileID,
			in.WorkspaceRootID, in.WorkspaceRelativePath, in.SessionID, in.PaneInstanceID,
			in.AgentRunID, in.Provider, in.ConversationID, in.ObservedAt.UTC().Format(rfc3339), stamp, stamp)
		if err != nil {
			return err
		}
		// A newly observed exact binding is a real recovery fact. It permits one
		// fresh claim attempt, but does not itself claim work or create an action.
		// Release each projection hold with its own state-version fence + ledger
		// event; a still-incomplete directional route is held again by the claim.
		rows, err := tx.QueryContext(ctx, `SELECT epic_id,state_version FROM epic_deliveries
			WHERE project_id=? AND state='review_queued' AND hold_kind='review_session_unbound'
			  AND ?=1`, in.ProjectID, b2i(s.HasDriverControlOrigin()))
		if err != nil {
			return err
		}
		type heldDelivery struct {
			epicID  string
			version int
		}
		var held []heldDelivery
		for rows.Next() {
			var item heldDelivery
			if err := rows.Scan(&item.epicID, &item.version); err != nil {
				rows.Close()
				return err
			}
			held = append(held, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, item := range held {
			res, err := tx.ExecContext(ctx, `UPDATE epic_deliveries SET
				state_version=state_version+1,hold_kind='',hold_reason='',return_state='',
				state_due_at=?,fact_progress_at=?,updated_at=?
				WHERE epic_id=? AND state='review_queued' AND hold_kind='review_session_unbound'
				  AND state_version=?`, now.Add(10*time.Minute).UTC().Format(rfc3339), stamp,
				stamp, item.epicID, item.version)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				continue
			}
			if err := appendEpicControlEventTx(ctx, tx, in.ProjectID, item.epicID,
				"review_binding_hold_released", "review_queued", "review_queued",
				item.version+1, "driver", `{"reason":"session_binding_observed"}`, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO control_events
			(project_id,epic_id,kind,actor_kind,actor_id,payload_json,created_at)
			VALUES (?,'','driver_session_binding_activated','driver',?,json_object(
			'binding_id',?,'role',?,'binding_epoch',?,'store_id',?,'session_id',?,
			'pane_instance_id',?,'agent_run_id',?),?)`, in.ProjectID, in.WorkerIdentity,
			in.BindingID, in.Role, epoch, in.StoreID, in.SessionID, in.PaneInstanceID,
			in.AgentRunID, stamp); err != nil {
			return err
		}
		out = in
		return nil
	})
	return out, err
}

// ActiveDriverSessionBinding returns only an exact, currently authoritative
// binding. Superseded incarnations remain queryable as history but never route.
func (s *Store) ActiveDriverSessionBinding(ctx context.Context, projectID, workerIdentity, role string) (DriverSessionBinding, error) {
	if projectID == "" {
		projectID = "default"
	}
	return activeDriverSessionBindingRow(s.DB.QueryRowContext(ctx, `SELECT
		binding_id,project_id,worker_identity,role,binding_epoch,host_id,store_id,
		tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		conversation_id,observed_at
		FROM driver_session_bindings WHERE project_id=? AND worker_identity=? AND role=? AND state='active'`,
		projectID, workerIdentity, role))
}

type bindingScanner interface{ Scan(...any) error }

func activeDriverSessionBindingTx(ctx context.Context, tx *sql.Tx, projectID, workerIdentity, role string) (DriverSessionBinding, error) {
	return activeDriverSessionBindingRow(tx.QueryRowContext(ctx, `SELECT
		binding_id,project_id,worker_identity,role,binding_epoch,host_id,store_id,
		tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		workspace_relative_path,session_id,pane_instance_id,agent_run_id,provider,
		conversation_id,observed_at
		FROM driver_session_bindings WHERE project_id=? AND worker_identity=? AND role=? AND state='active'`,
		projectID, workerIdentity, role))
}

func activeDriverSessionBindingRow(row bindingScanner) (DriverSessionBinding, error) {
	var b DriverSessionBinding
	var observed string
	err := row.Scan(&b.BindingID, &b.ProjectID, &b.WorkerIdentity, &b.Role, &b.BindingEpoch,
		&b.HostID, &b.StoreID, &b.TmuxServerInstanceID, &b.LifecycleKey, &b.TargetEpoch,
		&b.ProfileID, &b.WorkspaceRootID, &b.WorkspaceRelativePath, &b.SessionID,
		&b.PaneInstanceID, &b.AgentRunID, &b.Provider, &b.ConversationID, &observed)
	if err != nil {
		return DriverSessionBinding{}, err
	}
	b.ObservedAt, _ = time.Parse(rfc3339, observed)
	return b, nil
}
