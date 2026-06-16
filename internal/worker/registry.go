// Package worker holds the server-side worker registry/protocol helpers and a
// stub worker for tests. The control plane is the only DB client; the stub talks
// HTTP like a real worker. (Mode-A/Mode-B harnesses arrive in M5.)
package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// Registration is a worker's self-described enrollment (M1: attested := claimed).
type Registration struct {
	WorkerID     string   `json:"worker_id"`
	Identity     string   `json:"identity"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities"`
}

// RegisterResponse is returned by POST /v1/workers/register.
type RegisterResponse struct {
	WorkerID             string   `json:"worker_id"`
	LeaseTTLS            int      `json:"lease_ttl_s"`
	HeartbeatIntervalS   int      `json:"heartbeat_interval_s"`
	AttestedCapabilities []string `json:"attested_capabilities"`
	AttestationExpires   string   `json:"attestation_expires_at"`
}

// Registry upserts workers and (M1) attests their claimed capabilities verbatim.
type Registry struct {
	st                 *store.Store
	leaseTTLS          int
	heartbeatIntervalS int
}

func NewRegistry(st *store.Store, leaseTTLS, heartbeatIntervalS int) *Registry {
	return &Registry{st: st, leaseTTLS: leaseTTLS, heartbeatIntervalS: heartbeatIntervalS}
}

// Register upserts the worker row; attested := claimed in M1 (probing is M5).
func (r *Registry) Register(ctx context.Context, reg Registration, now time.Time) (RegisterResponse, error) {
	expires := now.Add(24 * time.Hour)
	caps, _ := json.Marshal(reg.Capabilities)
	_, err := r.st.DB.ExecContext(ctx, `
		INSERT INTO workers (worker_id, identity, host, claimed_capabilities, attested_capabilities,
		                     attestation_expires_at, registered_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))
		ON CONFLICT (worker_id) DO UPDATE SET
		    host = excluded.host,
		    claimed_capabilities = excluded.claimed_capabilities,
		    attested_capabilities = excluded.attested_capabilities,
		    attestation_expires_at = excluded.attestation_expires_at,
		    last_seen_at = datetime('now')`,
		reg.WorkerID, reg.Identity, reg.Host, string(caps), string(caps),
		expires.Format(time.RFC3339Nano))
	if err != nil {
		return RegisterResponse{}, err
	}
	return RegisterResponse{
		WorkerID:             reg.WorkerID,
		LeaseTTLS:            r.leaseTTLS,
		HeartbeatIntervalS:   r.heartbeatIntervalS,
		AttestedCapabilities: reg.Capabilities,
		AttestationExpires:   expires.Format(time.RFC3339Nano),
	}, nil
}
