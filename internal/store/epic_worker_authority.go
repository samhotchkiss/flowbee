package store

import (
	"context"
	"fmt"
	"time"
)

// EpicWorkerCapabilities resolves the scheduling authority for a Flowbee-created
// per-epic worker identity. The bool distinguishes a managed identity whose
// credential is currently inactive from a legacy identity which should fall back
// to the operator's static allowlist.
//
// This is deliberately evaluated at every registration and lease/role check.
// Persisted worker attestations are only an audit/cache: credential rotation,
// expiry, revocation, or a verified worker Stop removes authority immediately.
func (s *Store) EpicWorkerCapabilities(ctx context.Context, identity string,
	now time.Time) (capabilities []string, managed bool, err error) {
	if identity == "" {
		return nil, false, nil
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT w.worker_role,w.model_family,w.state,
		COALESCE(c.state,''),COALESCE(c.generation,0),COALESCE(c.envelope_ref,''),
		COALESCE(c.expires_at,'')
		FROM epic_worker_sessions w
		LEFT JOIN epic_worker_credentials c
		  ON c.epic_id=w.epic_id AND c.worker_role=w.worker_role
		 AND c.flowbee_identity=w.flowbee_identity AND c.project_id=w.project_id
		WHERE w.flowbee_identity=?`, identity)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	type authority struct {
		role, family, workerState, credentialState, envelopeRef, expiresAt string
		generation                                                         int64
	}
	var found []authority
	for rows.Next() {
		var a authority
		if err := rows.Scan(&a.role, &a.family, &a.workerState, &a.credentialState,
			&a.generation, &a.envelopeRef, &a.expiresAt); err != nil {
			return nil, false, err
		}
		found = append(found, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(found) == 0 {
		return nil, false, nil
	}
	if len(found) != 1 {
		return nil, true, fmt.Errorf("epic worker identity %q is not globally unique", identity)
	}
	a := found[0]
	if (a.workerState != "ensure_pending" && a.workerState != "active") ||
		(a.credentialState != "issued" && a.credentialState != "installed") ||
		a.generation < 1 || a.envelopeRef == "" {
		return nil, true, nil
	}
	expiresAt, parseErr := time.Parse(rfc3339, a.expiresAt)
	if parseErr != nil || !now.Before(expiresAt) {
		return nil, true, nil
	}
	roleCapability := ""
	switch a.role {
	case "builder":
		roleCapability = "role:eng_worker"
	case "reviewer":
		roleCapability = "role:code_reviewer"
	default:
		return nil, true, nil
	}
	if a.family == "" {
		return nil, true, nil
	}
	return []string{roleCapability, "model_family:" + a.family}, true, nil
}
