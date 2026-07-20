package store

import (
	"context"
	"database/sql"
	"time"
)

// AuthorizeProjectActorCredential is the live API authority check for a
// Flowbee-created Interactor or Orchestrator. Possessing a correctly signed
// token is insufficient: its immutable issuance id and generation must still
// name the exact active lifecycle, the credential must be installed and not
// revoked/expired, and that lifecycle's current exact Driver binding must remain
// authoritative. Stop, replacement, expiry, or binding supersession therefore
// fences the old token on the next request without a process restart.
func (s *Store) AuthorizeProjectActorCredential(ctx context.Context, identity, projectID, role,
	credentialID string, generation int64, now time.Time) bool {
	if identity == "" || projectID == "" || credentialID == "" || generation < 1 ||
		(role != DriverInteractorRole && role != DriverOrchestratorRole) {
		return false
	}
	freshFor := s.ManagedSessionDriverFreshFor
	if freshFor <= 0 {
		freshFor = 5 * time.Minute
	}
	authorized := false
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var bindingID, lifecycleKey string
		var targetEpoch int64
		err := tx.QueryRowContext(ctx, `SELECT l.active_binding_id,l.lifecycle_key,l.target_epoch
		FROM project_actor_lifecycles l JOIN project_actor_routes r
		  ON r.project_id=l.project_id AND r.role=l.role AND r.actor_id=l.actor_id
		WHERE l.project_id=? AND l.role=? AND l.actor_id=?
		  AND l.lifecycle_ownership='driver_managed' AND l.desired_state='active' AND l.state='active'
		  AND l.credential_envelope_ref=? AND l.credential_generation=?
		  AND l.credential_install_ref<>'' AND l.credential_expires_at>?
		  AND l.credential_revoked_at='' AND l.active_binding_id<>''
		  AND r.state='active' AND r.state_version=l.route_state_version`, projectID, role, identity,
			credentialID, generation, now.UTC().Format(rfc3339)).Scan(&bindingID, &lifecycleKey, &targetEpoch)
		if err != nil {
			return err
		}
		binding, err := activeDriverSessionBindingTx(ctx, tx, projectID, identity, role)
		if err != nil {
			return err
		}
		if binding.BindingID != bindingID || binding.LifecycleOwnership != "driver_managed" ||
			binding.LifecycleKey != lifecycleKey || binding.TargetEpoch != targetEpoch || targetEpoch != generation {
			return nil
		}
		authorized, err = currentDriverBinding(ctx, tx, binding, now, freshFor)
		return err
	})
	return err == nil && authorized
}

// IsRegisteredProjectActorIdentity prevents a legacy enrolled token or the
// loopback identity bypass from colliding with an actor name and falling through
// worker allowlist policy. Actor identities use only actor credential routes.
func (s *Store) IsRegisteredProjectActorIdentity(ctx context.Context, identity string) (bool, error) {
	if identity == "" {
		return false, nil
	}
	var n int
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_actor_routes WHERE actor_id=?`, identity).Scan(&n)
	return n > 0, err
}
