// Package ledger is the event-sourced spine (DESIGN §6, EVENT-SOURCED). It is a
// deterministic-core package (§1.2): no clock, no randomness, no ID minter, no
// GitHub/LLM imports. Append (the I/O write) lives in internal/store; this
// package defines the Event record and the PURE Fold: Fold(events) == jobs row.
//
// Events carry RESOLVED facts (a clock-derived deadline is recorded in the
// lease_claimed event, never recomputed at fold time), so Fold reads no clock.
package ledger

import (
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
)

// EventKind enumerates the ledger event kinds (M1 subset).
type EventKind string

const (
	KindJobCreated     EventKind = "job_created"
	KindLeaseClaimed   EventKind = "lease_claimed"
	KindWorkerStarted  EventKind = "worker_started"
	KindHeartbeat      EventKind = "heartbeat"
	KindResultAccepted EventKind = "result_accepted"
	KindLeaseReleased  EventKind = "lease_released"
	KindStateChanged   EventKind = "state_changed"
)

// Event is one appended ledger row. Payload holds kind-specific RESOLVED facts as
// already-decoded values (not raw JSON) so Fold stays pure and total.
type Event struct {
	JobID      string
	JobSeq     int // per-job ordinal (1,2,3,…)
	Kind       EventKind
	FromState  job.State
	ToState    job.State
	LeaseEpoch int
	Actor      string
	CreatedAt  time.Time // resolved at append time; recorded, never recomputed
	Payload    Payload
}

// Payload carries the resolved, kind-specific facts a fold needs. Only the fields
// relevant to an event's kind are set; the rest are zero.
type Payload struct {
	// job_created
	Kind     job.Kind
	Flow     string
	Stage    string
	Role     job.Role
	BaseSHA  string
	Priority int

	// lease_claimed
	LeaseID          string
	BoundIdentity    string
	BoundModelFamily string

	// counter deltas (lease_released / state_changed)
	AttemptsDelta int
}

// Fold replays events into the jobs projection. PURE: no clock, no RNG, no I/O.
// Fold(events) == the jobs row the store maintains incrementally.
func Fold(events []Event) (job.Job, error) {
	var j job.Job
	for _, e := range events {
		switch e.Kind {
		case KindJobCreated:
			j.ID = e.JobID
			j.Kind = e.Payload.Kind
			j.Flow = e.Payload.Flow
			j.Stage = e.Payload.Stage
			j.Role = e.Payload.Role
			j.BaseSHA = e.Payload.BaseSHA
			j.Priority = e.Payload.Priority
			j.State = e.ToState
			// M1 default counters mirror the migration defaults.
			j.MaxAttempts = 5
			j.MaxBounces = 3
		case KindLeaseClaimed:
			j.State = e.ToState
			j.LeaseEpoch = e.LeaseEpoch
			j.LeaseID = e.Payload.LeaseID
			j.BoundIdentity = e.Payload.BoundIdentity
			j.BoundModelFamily = e.Payload.BoundModelFamily
		case KindWorkerStarted:
			j.State = e.ToState
		case KindHeartbeat:
			// liveness only; no projection field changes in M1.
		case KindResultAccepted:
			j.State = e.ToState
			// the lease is released on result: clear live lease columns, keep epoch.
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindLeaseReleased:
			j.State = e.ToState
			j.Attempts += e.Payload.AttemptsDelta
			j.LeaseID = ""
			j.BoundIdentity = ""
			j.BoundModelFamily = ""
		case KindStateChanged:
			j.State = e.ToState
		}
		j.JobSeq = e.JobSeq
	}
	return j, nil
}
