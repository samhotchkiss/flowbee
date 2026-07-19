package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
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

// driverEndpointControlState keeps authorization proofs separated by Driver
// isolation domain. Snapshot/Available are aggregate display helpers only;
// routing authority comes exclusively from AvailableFor's exact key.
type driverEndpointControlState struct {
	mu       sync.RWMutex
	expected map[string]driver.EndpointKey
	states   map[string]api.DriverControlReadiness
}

func newDriverEndpointControlState(endpoints []serveDriverEndpoint) *driverEndpointControlState {
	s := &driverEndpointControlState{
		expected: make(map[string]driver.EndpointKey, len(endpoints)),
		states:   make(map[string]api.DriverControlReadiness, len(endpoints)),
	}
	for _, endpoint := range endpoints {
		s.expected[endpoint.InstanceRef] = endpoint.Key
		s.states[endpoint.InstanceRef] = api.DriverControlReadiness{
			Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver endpoint capability has not been proven.",
		}
	}
	return s
}

func (s *driverEndpointControlState) Update(instanceRef string, next api.DriverControlReadiness) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.expected[instanceRef]; !ok {
		return
	}
	s.states[instanceRef] = normalizedDriverControlReadiness(next)
}

func (s *driverEndpointControlState) EndpointSnapshot(instanceRef string) api.DriverControlReadiness {
	if s == nil {
		return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver endpoint inventory is unavailable."}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if next, ok := s.states[instanceRef]; ok {
		return next
	}
	return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
		Reason: "Tmux Driver endpoint is not in the exact inventory."}
}

func (s *driverEndpointControlState) Snapshot() api.DriverControlReadiness {
	if s == nil {
		return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver endpoint inventory is unavailable."}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.expected) == 0 {
		return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver endpoint inventory is empty."}
	}
	refs := make([]string, 0, len(s.expected))
	for ref := range s.expected {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	failures := make([]string, 0)
	for _, ref := range refs {
		state, ok := s.states[ref]
		if !ok || !state.Available {
			reason := "capability has not been proven"
			if ok && state.Reason != "" {
				reason = state.Reason
			}
			failures = append(failures, fmt.Sprintf("%s: %s", ref, reason))
		}
	}
	if len(failures) != 0 {
		return api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver endpoint capability unavailable: " + strings.Join(failures, "; ")}
	}
	return api.DriverControlReadiness{Required: true, Available: true, Status: "ready",
		Reason: fmt.Sprintf("Tmux Driver authenticated control-principal origin ready on %d exact endpoint(s).", len(refs))}
}

func (s *driverEndpointControlState) Available() bool { return s.Snapshot().Available }

// AvailableFor proves control-origin capability for exactly one configured
// host/store/tmux-server domain. It has no host-only, store-only, domain-only,
// or single-default fallback.
func (s *driverEndpointControlState) AvailableFor(key driver.EndpointKey) bool {
	if s == nil || key.HostID == "" || key.StoreID == "" || key.TmuxServerDomainID == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for ref, expected := range s.expected {
		if expected == key {
			state, ok := s.states[ref]
			return ok && state.Available
		}
	}
	return false
}

func probeDriverEndpointControlState(ctx context.Context, state *driverEndpointControlState, db *sql.DB,
	resolver *driver.EndpointResolver, endpoint serveDriverEndpoint,
) api.DriverControlReadiness {
	if err := rejectSyntheticDriverControlBinding(ctx, db); err != nil {
		next := api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003", Reason: err.Error()}
		state.Update(endpoint.InstanceRef, next)
		return next
	}
	readiness, err := resolver.ControlReadiness(ctx, endpoint.Key)
	if err != nil {
		next := api.DriverControlReadiness{Required: true, Status: "route_unavailable", Gap: "GAP-FD-003",
			Reason: "Tmux Driver exact endpoint capability is unavailable or unauthorized: " + err.Error()}
		state.Update(endpoint.InstanceRef, next)
		return next
	}
	next := api.DriverControlReadiness{Required: true, Available: true, Status: "ready",
		Reason: "Tmux Driver authenticated control-principal origin ready for " + readiness.Capability.PrincipalID + "."}
	state.Update(endpoint.InstanceRef, next)
	return next
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
