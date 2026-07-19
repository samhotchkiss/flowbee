package main

import (
	"context"
	"database/sql"
	"sync/atomic"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/driver"
)

// driverControlState is the live authorization fence shared by projections,
// executors, and operator read models. Atomic replacement means a failed
// capability probe closes all new message work as one process-wide event.
type driverControlState struct {
	value atomic.Pointer[api.DriverControlReadiness]
}

func newDriverControlState(initial api.DriverControlReadiness) *driverControlState {
	s := &driverControlState{}
	s.Update(initial)
	return s
}

func (s *driverControlState) Update(next api.DriverControlReadiness) {
	next = normalizedDriverControlReadiness(next)
	s.value.Store(&next)
}

func (s *driverControlState) Snapshot() api.DriverControlReadiness {
	if s == nil || s.value.Load() == nil {
		return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver control-principal capability has not been proven."}
	}
	return *s.value.Load()
}

func (s *driverControlState) Available() bool { return s.Snapshot().Available }

func normalizedDriverControlReadiness(r api.DriverControlReadiness) api.DriverControlReadiness {
	if !r.Required {
		r.Available = false
		if r.Status == "" {
			r.Status = "disabled"
		}
		return r
	}
	if r.Available {
		return api.DriverControlReadiness{Required: true, Available: true, Status: "ready"}
	}
	if r.Status == "" {
		r.Status = "route_unavailable"
	}
	return r
}

// probeDriverControlState performs both binding invariants and Driver's exact
// v2.4 dual proof (/v2/meta feature plus authenticated capability endpoint).
// Probe failures are readiness state, not loop failures: observation,
// lifecycle, and receipt verification must continue while new sends are held.
func probeDriverControlState(ctx context.Context, state *driverControlState, db *sql.DB, port driver.DriverPort) api.DriverControlReadiness {
	if err := rejectSyntheticDriverControlBinding(ctx, db); err != nil {
		next := api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003", Reason: err.Error()}
		state.Update(next)
		return next
	}
	next := driverControlReadiness(ctx, true, port)
	state.Update(next)
	return next
}
