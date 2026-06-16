// Package lease holds the fenced-lease types and the sentinel errors that drive
// HTTP status selection. It is a deterministic-core package (DESIGN §1.2): it
// imports no clock, randomness, ID minter, GitHub, or LLM package. The actual
// atomic-claim / heartbeat / release SQL lives in internal/store (the I/O seam);
// this package defines the value types those queries fill and the contract for
// fencing. `time.Time` is used only as a value here, never read from a clock.
package lease

import (
	"errors"
	"time"
)

// State is the lifecycle of a lease row (audit, §6.3).
type State string

const (
	StateActive  State = "active"
	StateExpired State = "expired"
	StateRevoked State = "revoked"
)

// Lease is one fenced claim on a job. Epoch is the fence: it is bumped on every
// claim AND every revocation (I-4), so a stale call (old epoch) is rejected.
type Lease struct {
	LeaseID     string
	JobID       string
	Epoch       int
	Identity    string
	ModelFamily string
	TTL         time.Duration
	GrantedAt   time.Time
	Deadline    time.Time // absolute Rung-3 cap; Flowbee-clock-only
	HBDue       time.Time
	State       State
}

// Disposition records why a lease ended (the leases.end_reason column).
type Disposition string

const (
	DispCompleted  Disposition = "completed"
	DispReleased   Disposition = "released"
	DispExpired    Disposition = "expired"
	DispRevoked    Disposition = "revoked"
	DispSuperseded Disposition = "superseded"
)

// ErrStaleEpoch means a fenced call carried a lease epoch that is no longer the
// live fence — the caller is a zombie. Mapped to HTTP 409 by the API layer.
var ErrStaleEpoch = errors.New("lease epoch stale")

// ErrLostRace means an atomic claim affected 0 rows: the job was not `ready`
// (another worker won, or it already advanced). Mapped to a 204 long-poll miss.
var ErrLostRace = errors.New("lost the claim race")
