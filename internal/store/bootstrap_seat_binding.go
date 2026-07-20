package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

// BootstrapSeatBinding is the indivisible project admission fact for one local
// worker seat. Capacity identity without an exact Driver target is not ready,
// and a Driver target without its authenticated capacity identity is not
// schedulable, so bootstrap persists both in one transaction.
type BootstrapSeatBinding struct {
	ProjectID string
	Seat      Seat
	Capacity  CapacitySeatIdentity
	Target    BuilderDriverTarget
}

func (s *Store) BindBootstrapSeat(ctx context.Context, in BootstrapSeatBinding, now time.Time) error {
	if in.Seat.MaxConcurrent < 1 {
		in.Seat.MaxConcurrent = 1
	}
	if strings.TrimSpace(in.ProjectID) == "" || in.Capacity.SeatID == "" ||
		in.Target.ProjectID != in.ProjectID || in.Target.SeatID != in.Capacity.SeatID ||
		in.Seat.ComposeID() != in.Capacity.SeatID {
		return errors.New("bootstrap seat binding requires one exact project and seat identity")
	}
	if err := validateSeat(&in.Seat); err != nil {
		return err
	}
	if in.Capacity.HostID == "" || in.Capacity.AccountKey == "" || in.Capacity.CredentialLineage == "" ||
		in.Capacity.ReservePct < 0 || in.Capacity.ReservePct > 100 || in.Capacity.AccountMaximum < 1 {
		return errors.New("bootstrap seat binding requires valid capacity identity")
	}
	for name, value := range map[string]string{
		"instance_ref": in.Target.InstanceRef, "tmux_server_domain_id": in.Target.TmuxServerDomainID,
		"tmux_server_instance_id": in.Target.TmuxServerInstanceID, "profile_id": in.Target.ProfileID,
		"workspace_root_id": in.Target.WorkspaceRootID, "workspace_relative_base": in.Target.WorkspaceRelativeBase,
	} {
		if strings.TrimSpace(value) == "" || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("bootstrap seat Driver target requires %s", name)
		}
	}
	clean := path.Clean(in.Target.WorkspaceRelativeBase)
	if clean != in.Target.WorkspaceRelativeBase || clean == "." || strings.HasPrefix(clean, "../") || path.IsAbs(clean) {
		return errors.New("bootstrap seat Driver workspace base must be a clean relative path")
	}
	if !in.Target.Enabled {
		return errors.New("bootstrap seat Driver target must be enabled")
	}

	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := ensureExactBootstrapSeatTx(ctx, tx, in.Seat, now); err != nil {
			return err
		}
		var otherProjects int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM builder_driver_targets
			WHERE seat_id=? AND project_id<>? AND enabled=1`, in.Capacity.SeatID, in.ProjectID).Scan(&otherProjects); err != nil {
			return err
		}
		var host, account, lineage string
		var reserve float64
		var accountMaximum int
		if err := tx.QueryRowContext(ctx, `SELECT expected_host_id,expected_account_key,expected_credential_lineage,
			capacity_reserve_pct,account_max_concurrent FROM seats WHERE id=?`, in.Capacity.SeatID).
			Scan(&host, &account, &lineage, &reserve, &accountMaximum); err != nil {
			return err
		}
		exactIdentity := host == in.Capacity.HostID && account == in.Capacity.AccountKey &&
			lineage == in.Capacity.CredentialLineage
		exactCapacity := exactIdentity && reserve == in.Capacity.ReservePct && accountMaximum == in.Capacity.AccountMaximum
		if otherProjects > 0 && !exactCapacity {
			return errors.New("seat identity is shared with another project and cannot be rebound")
		}
		stamp := now.UTC().Format(rfc3339)
		if otherProjects == 0 && !exactCapacity {
			if _, err := tx.ExecContext(ctx, `UPDATE seats SET expected_host_id=?,expected_account_key=?,
				expected_credential_lineage=?,capacity_reserve_pct=?,account_max_concurrent=?,updated_at=? WHERE id=?`,
				in.Capacity.HostID, in.Capacity.AccountKey, in.Capacity.CredentialLineage,
				in.Capacity.ReservePct, in.Capacity.AccountMaximum, stamp, in.Capacity.SeatID); err != nil {
				return err
			}
		}

		var driverHost, driverDomain, driverOwnership string
		if err := tx.QueryRowContext(ctx, `SELECT host_id,tmux_server_domain_id,tmux_server_ownership
			FROM driver_instances WHERE instance_ref=? AND state='live'`, in.Target.InstanceRef).
			Scan(&driverHost, &driverDomain, &driverOwnership); err != nil {
			return fmt.Errorf("bootstrap seat Driver instance is not live: %w", err)
		}
		if driverHost != in.Capacity.HostID || driverDomain != in.Target.TmuxServerDomainID ||
			driverOwnership != "managed_dedicated" {
			return errors.New("bootstrap seat target does not match the exact managed Driver domain")
		}

		var instanceRef, domain, serverID, profile, root, base string
		var enabled int
		err := tx.QueryRowContext(ctx, `SELECT instance_ref,tmux_server_domain_id,tmux_server_instance_id,
			profile_id,workspace_root_id,workspace_relative_base,enabled FROM builder_driver_targets
			WHERE project_id=? AND seat_id=?`, in.ProjectID, in.Capacity.SeatID).
			Scan(&instanceRef, &domain, &serverID, &profile, &root, &base, &enabled)
		if err == nil {
			if instanceRef != in.Target.InstanceRef || domain != in.Target.TmuxServerDomainID ||
				serverID != in.Target.TmuxServerInstanceID || profile != in.Target.ProfileID ||
				root != in.Target.WorkspaceRootID || base != in.Target.WorkspaceRelativeBase || enabled != 1 {
				return errors.New("bootstrap seat target is already bound to different immutable routing authority")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO builder_driver_targets
			(project_id,seat_id,instance_ref,tmux_server_domain_id,tmux_server_instance_id,profile_id,
			 workspace_root_id,workspace_relative_base,enabled,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,1,?,?)`, in.ProjectID, in.Capacity.SeatID, in.Target.InstanceRef,
			in.Target.TmuxServerDomainID, in.Target.TmuxServerInstanceID, in.Target.ProfileID,
			in.Target.WorkspaceRootID, in.Target.WorkspaceRelativeBase, stamp, stamp)
		return err
	})
}

func ensureExactBootstrapSeatTx(ctx context.Context, tx *sql.Tx, seat Seat, now time.Time) error {
	id := seat.ComposeID()
	var got Seat
	var enabled int
	var envJSON string
	err := tx.QueryRowContext(ctx, seatSelect+` WHERE id=?`, id).Scan(&got.ID, &got.Box, &got.AgentFamily,
		&got.AccountKey, &got.ConfigDir, &got.CodexHome, &envJSON, &got.MaxConcurrent, &enabled,
		&got.Health, &got.HealthDetail, &got.LastProbeAt, &got.CreatedAt, &got.UpdatedAt)
	if err == nil {
		got.Enabled, got.ExtraEnv = enabled != 0, unmarshalEnv(envJSON)
		wantEnv, envErr := marshalEnv(seat.ExtraEnv)
		if envErr != nil {
			return envErr
		}
		if got.Box != seat.Box || got.AgentFamily != seat.AgentFamily || got.ConfigDir != seat.ConfigDir ||
			got.CodexHome != seat.CodexHome || got.MaxConcurrent != seat.MaxConcurrent || !got.Enabled || envJSON != wantEnv {
			return errors.New("bootstrap seat is already registered with different immutable runtime identity")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	envJSON, err = marshalEnv(seat.ExtraEnv)
	if err != nil {
		return err
	}
	stamp := now.UTC().Format(rfc3339)
	health := seat.Health
	if health == "" {
		health = SeatUnreachable
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO seats
		(id,box,agent_family,account_key,config_dir,codex_home,extra_env_json,max_concurrent,enabled,
		 health,health_detail,last_probe_at,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,1,?,?,?,?,?)`,
		id, seat.Box, seat.AgentFamily, seat.AccountKey, seat.ConfigDir, seat.CodexHome, envJSON,
		seat.MaxConcurrent, health, seat.HealthDetail, seat.LastProbeAt, stamp, stamp)
	return err
}
