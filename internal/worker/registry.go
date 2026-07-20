// Package worker holds the server-side worker registry/protocol helpers and a
// stub worker for tests. The control plane is the only DB client; the stub talks
// HTTP like a real worker.
//
// M5 adds real ATTESTATION (DESIGN §7.2, §9.4.1): a worker submits CLAIMED
// capabilities plus a handshake (arch/os); the registry returns the ATTESTED
// subset — only those caps gate scheduler matching. Two attestation checks:
//   - role:* / model_family:* / tool:* are attested against an enrolled-identity
//     ALLOWLIST (an unenrolled identity claiming role:code_reviewer cannot
//     rubber-stamp its own builds, §9.4.1);
//   - arch:* / os:* are attested against the worker's submitted handshake (the
//     arch-lottery fix: a worker can't claim arch:arm64 from an x86 box).
//
// An unattested capability is dropped and therefore never matched at lease time.
package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// ErrWorkerIDReassignment means a registration tried to reuse a durable
// worker_id that belongs to another authenticated identity. Worker IDs are
// stable row keys, never bearer capabilities that may be transferred.
var ErrWorkerIDReassignment = errors.New("worker_id belongs to another identity")

// Registration is a worker's self-described enrollment. claimed capabilities are
// ATTESTED by the server (M5: against the allowlist + handshake); only the
// attested subset gates matching.
type Registration struct {
	WorkerID     string   `json:"worker_id"`
	Identity     string   `json:"identity"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities"` // CLAIMED
	// Handshake fields the registry attests arch:*/os:* against (§7.2).
	Arch string `json:"arch"`
	OS   string `json:"os"`
	// F6 capacity advertisement. ModelSlots is the box's PER-MODEL concurrency
	// (claude:3, codex:3) replacing the single max_concurrent_leases. Weight is the
	// per-box distribution bias (default 1). Accounts are the named per-model
	// credentials (the rollover chain) this box can dispatch against. All optional:
	// an empty advertisement keeps the legacy single-slot, accountless behavior.
	ModelSlots map[string]int   `json:"model_slots,omitempty"`
	Weight     int              `json:"weight,omitempty"`
	Accounts   []AccountSpecMsg `json:"accounts,omitempty"`
}

// AccountSpecMsg is one named per-model account advertised at registration (§C):
// a credential with a ceiling and an ordered preference (the rollover chain).
type AccountSpecMsg struct {
	AccountID      string `json:"account_id"`
	ModelFamily    string `json:"model_family"`
	CeilingPct     int    `json:"ceiling_pct"`
	PreferenceRank int    `json:"preference_rank"`
}

// RegisterResponse is returned by POST /v1/workers/register.
type RegisterResponse struct {
	WorkerID             string   `json:"worker_id"`
	LeaseTTLS            int      `json:"lease_ttl_s"`
	HeartbeatIntervalS   int      `json:"heartbeat_interval_s"`
	AttestedCapabilities []string `json:"attested_capabilities"`
	AttestationExpires   string   `json:"attestation_expires_at"`
}

// Allowlist is the enrolled-identity attestation policy (§9.4.1). It maps an
// identity to the role/model_family/tool capabilities it is permitted to attest.
// An identity absent from the allowlist (in Open mode) is treated as enrolled
// for whatever it claims — convenient for the single-operator dev box and for
// tests; production enrolls explicitly. arch:* / os:* are NEVER taken from the
// allowlist — they are attested only against the handshake.
type Allowlist struct {
	// Open, when true, attests any role/model_family/tool claim (dev default).
	// When false, only identities present in Permit may attest, and only the
	// capabilities listed for them.
	Open bool
	// Permit maps identity -> the set of permitted role/model_family/tool caps.
	Permit map[string][]string
}

// OpenAllowlist is the permissive dev/test default: every claimed role/family/
// tool is attested; arch/os still gated by the handshake.
func OpenAllowlist() Allowlist { return Allowlist{Open: true} }

// permits reports whether identity may attest capability cap (a role/family/tool).
func (a Allowlist) permits(identity, cap string) bool {
	if a.Open {
		return true
	}
	for _, c := range a.Permit[identity] {
		if c == cap {
			return true
		}
	}
	return false
}

// hasRoleAuthority distinguishes a scheduling worker from an enrolled
// capacity-only principal. Open mode preserves the development/test behavior.
func (a Allowlist) hasRoleAuthority(identity string) bool {
	if a.Open {
		return true
	}
	for _, capability := range a.Permit[identity] {
		if strings.HasPrefix(capability, "role:") && capability != "role:" {
			return true
		}
	}
	return false
}

// attest filters claimed capabilities to the attested subset for an identity,
// given the worker's handshake (arch/os). It is a PURE function of its inputs.
//   - arch:* / os:* : attested iff the value matches the handshake.
//   - role:* / model_family:* / tool:* : attested iff the allowlist permits it.
//   - anything else : dropped (unknown capability shapes don't gate matching).
func (a Allowlist) attest(identity string, claimed []string, arch, osName string) []string {
	var out []string
	for _, c := range claimed {
		switch {
		case strings.HasPrefix(c, "arch:"):
			if arch != "" && c == "arch:"+arch {
				out = append(out, c)
			}
		case strings.HasPrefix(c, "os:"):
			if osName != "" && c == "os:"+osName {
				out = append(out, c)
			}
		case strings.HasPrefix(c, "role:"), strings.HasPrefix(c, "model_family:"), strings.HasPrefix(c, "tool:"):
			if a.permits(identity, c) {
				out = append(out, c)
			}
		case strings.HasPrefix(c, "model:"):
			// model:<backend> is a SELF-DECLARED, display-only tag (which model the worker
			// actually runs, e.g. codex) — there is no allowlist or handshake to verify it,
			// and NO job ever requires it, so it never gates a lease (no spoofing risk).
			// Attest it as-is so it surfaces on the roster + in `flowbee status`. If it ever
			// becomes a matching axis, gate it like role:/model_family: above.
			out = append(out, c)
		default:
			// unknown shape: not attested, never matched.
		}
	}
	return out
}

// Registry upserts workers and attests their claimed capabilities (§7.2).
type Registry struct {
	st                 *store.Store
	leaseTTLS          int
	heartbeatIntervalS int
	allow              Allowlist
}

func NewRegistry(st *store.Store, leaseTTLS, heartbeatIntervalS int, allow Allowlist) *Registry {
	return &Registry{st: st, leaseTTLS: leaseTTLS, heartbeatIntervalS: heartbeatIntervalS, allow: allow}
}

// Register upserts the worker row, persisting BOTH the claimed set (audit) and
// the attested subset (what the scheduler matches). The attestation expiry makes
// a stale capability dormant rather than silently matched.
func (r *Registry) Register(ctx context.Context, reg Registration, now time.Time) (RegisterResponse, error) {
	expires := now.Add(24 * time.Hour)
	allow, err := r.currentAllowlist(ctx, reg.Identity, now)
	if err != nil {
		return RegisterResponse{}, err
	}
	attested := allow.attest(reg.Identity, reg.Capabilities, reg.Arch, reg.OS)
	claimedJSON, _ := json.Marshal(reg.Capabilities)
	attestedJSON, _ := json.Marshal(attested)
	result, err := r.st.DB.ExecContext(ctx, `
		INSERT INTO workers (worker_id, identity, host, arch, os,
		                     claimed_capabilities, attested_capabilities,
		                     attestation_expires_at, registered_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (worker_id) DO UPDATE SET
		    host = excluded.host,
		    arch = excluded.arch,
		    os = excluded.os,
		    claimed_capabilities = excluded.claimed_capabilities,
		    attested_capabilities = excluded.attested_capabilities,
		    attestation_expires_at = excluded.attestation_expires_at,
		    last_seen_at = excluded.last_seen_at
		WHERE workers.identity = excluded.identity`,
		reg.WorkerID, reg.Identity, reg.Host, reg.Arch, reg.OS,
		string(claimedJSON), string(attestedJSON),
		expires.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return RegisterResponse{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return RegisterResponse{}, err
	} else if affected != 1 {
		return RegisterResponse{}, ErrWorkerIDReassignment
	}

	// F6: persist the box's per-model concurrency + distribution weight + named
	// accounts (the rollover chain). All optional — an empty advertisement leaves
	// the legacy single-slot, accountless behavior intact.
	if len(reg.ModelSlots) > 0 || reg.Weight > 0 {
		if err := r.st.SetWorkerModelSlots(ctx, reg.WorkerID, reg.ModelSlots, reg.Weight, now); err != nil {
			return RegisterResponse{}, err
		}
	}
	if len(reg.Accounts) > 0 {
		specs := make([]store.AccountSpec, 0, len(reg.Accounts))
		for _, a := range reg.Accounts {
			specs = append(specs, store.AccountSpec{
				AccountID: a.AccountID, ModelFamily: a.ModelFamily,
				CeilingPct: a.CeilingPct, PreferenceRank: a.PreferenceRank,
			})
		}
		if err := r.st.UpsertAccounts(ctx, specs, now); err != nil {
			return RegisterResponse{}, err
		}
	}

	return RegisterResponse{
		WorkerID:             reg.WorkerID,
		LeaseTTLS:            r.leaseTTLS,
		HeartbeatIntervalS:   r.heartbeatIntervalS,
		AttestedCapabilities: attested,
		AttestationExpires:   expires.Format(time.RFC3339Nano),
	}, nil
}

// AttestedFor returns the capability set that is authoritative at lease time.
// Persisted attestations are audit/cache material only: every read checks their
// expiry and re-applies the registry's current policy to the original claims and
// stored arch/OS handshake. This makes a restart from an old open policy to a
// strict production policy fail closed before the worker re-registers.
func (r *Registry) AttestedFor(ctx context.Context, identity string, now time.Time) ([]string, error) {
	var claimed, expires, arch, osName string
	err := r.st.DB.QueryRowContext(ctx,
		`SELECT claimed_capabilities,attestation_expires_at,arch,os
		 FROM workers WHERE identity = ?`, identity).Scan(&claimed, &expires, &arch, &osName)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expires)
	if err != nil || !now.Before(expiresAt) {
		return nil, nil
	}
	var claims []string
	if err := json.Unmarshal([]byte(claimed), &claims); err != nil {
		return nil, err
	}
	allow, err := r.currentAllowlist(ctx, identity, now)
	if err != nil {
		return nil, err
	}
	return allow.attest(identity, claims, arch, osName), nil
}

// HasRoleAuthority reports whether the current operator policy grants an
// identity any worker role. It is used to keep capacity-only credentials off
// control-plane and work-mutation endpoints.
func (r *Registry) HasRoleAuthority(ctx context.Context, identity string, now time.Time) (bool, error) {
	allow, err := r.currentAllowlist(ctx, identity, now)
	if err != nil {
		return false, err
	}
	return allow.hasRoleAuthority(identity), nil
}

// ModelFamilyFor returns the current authority-bound model family when the
// policy grants exactly one. It prevents an ephemeral worker from changing the
// anti-affinity family merely by changing a lease query parameter.
func (r *Registry) ModelFamilyFor(ctx context.Context, identity string, now time.Time) (string, bool, error) {
	allow, err := r.currentAllowlist(ctx, identity, now)
	if err != nil {
		return "", false, err
	}
	var family string
	for _, capability := range allow.Permit[identity] {
		if !strings.HasPrefix(capability, "model_family:") {
			continue
		}
		candidate := strings.TrimPrefix(capability, "model_family:")
		if candidate == "" || family != "" && family != candidate {
			return "", false, nil
		}
		family = candidate
	}
	return family, family != "", nil
}

func (r *Registry) currentAllowlist(ctx context.Context, identity string, now time.Time) (Allowlist, error) {
	capabilities, managed, err := r.st.EpicWorkerCapabilities(ctx, identity, now)
	if err != nil {
		return Allowlist{}, err
	}
	if !managed {
		return r.allow, nil
	}
	return Allowlist{Permit: map[string][]string{identity: capabilities}}, nil
}

// TouchLastSeen records a worker's most-recent contact (heartbeat liveness for
// the roster's stale-hb badge, §12.6.2). Best-effort by identity.
func (r *Registry) TouchLastSeen(ctx context.Context, identity string, now time.Time) {
	_, _ = r.st.DB.ExecContext(ctx,
		`UPDATE workers SET last_seen_at = ? WHERE identity = ?`, now.Format(time.RFC3339Nano), identity)
}
