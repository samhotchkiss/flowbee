package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacitycollector"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

const defaultCapacityCollectorInterval = 2 * time.Minute

type capacityCollectorRuntime struct {
	Seats           capacitycollector.SeatSource
	Fleet           capacitycollector.FleetService
	NewGenerationID func() string
}

func (r capacityCollectorRuntime) readiness(ctx context.Context) ([]capacitycollector.Seat, error) {
	if r.Seats == nil || r.NewGenerationID == nil {
		return nil, errors.New("capacity collector runtime is incomplete")
	}
	seats, err := r.Seats.EnabledCapacitySeats(ctx)
	if err != nil {
		return nil, fmt.Errorf("list enabled capacity seats: %w", err)
	}
	if err := capacitycollector.ValidateFleetReadiness(r.Fleet, seats); err != nil {
		return nil, err
	}
	return seats, nil
}

func (r capacityCollectorRuntime) collect(ctx context.Context, now time.Time) (store.CapacityGeneration, error) {
	seats, err := r.readiness(ctx)
	if err != nil {
		return store.CapacityGeneration{}, err
	}
	id := r.NewGenerationID()
	if id == "" {
		return store.CapacityGeneration{}, errors.New("capacity generation minter returned an empty id")
	}
	return r.Fleet.CollectAndCommit(ctx, id, seats, now.UTC())
}

// runCapacityCollectorLoop is the deterministic periodic activation seam. Serve
// owns the ticker while tests can supply exact ticks; closing the channel or
// cancelling the process context stops collection without an extra final probe.
func runCapacityCollectorLoop(ctx context.Context, ticks <-chan time.Time, collect func(time.Time)) {
	if ticks == nil || collect == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now, ok := <-ticks:
			if !ok {
				return
			}
			collect(now)
		}
	}
}

func newProductionCapacityCollector(st *store.Store, cfg config.Config, localHostID, collectorID string) (capacityCollectorRuntime, error) {
	localHostID, collectorID = strings.TrimSpace(localHostID), strings.TrimSpace(collectorID)
	if localHostID == "" || collectorID == "" {
		return capacityCollectorRuntime{}, errors.New("capacity routing v2 requires FLOWBEE_CAPACITY_LOCAL_HOST_ID and FLOWBEE_CAPACITY_COLLECTOR_ID")
	}
	if !capacityCollectorEnrolled(cfg.EnrolledIdentities, collectorID) {
		return capacityCollectorRuntime{}, fmt.Errorf("capacity collector identity %q is not in FLOWBEE_ENROLLED_IDENTITIES", collectorID)
	}
	service := &capacitycollector.Service{
		Probe: capacitycollector.AcctProbe{}, Backoff: capacitycollector.SQLBackoffStore{DB: st.DB},
	}
	client, err := capacitycollector.NewEnrolledLocalHostClient(localHostID, collectorID, true, service)
	if err != nil {
		return capacityCollectorRuntime{}, err
	}
	fleet, err := capacitycollector.NewFleetService([]capacitycollector.HostEnrollment{{
		HostID: localHostID, CollectorID: collectorID, Authenticated: true, Client: client,
	}}, st, 1)
	if err != nil {
		return capacityCollectorRuntime{}, err
	}
	return capacityCollectorRuntime{
		Seats: capacitycollector.SQLSeatSource{DB: st.DB}, Fleet: fleet,
		NewGenerationID: func() string { return "capacity-" + ulid.New() },
	}, nil
}

func capacityCollectorEnrolled(entries []string, identity string) bool {
	for _, entry := range entries {
		// The existing worker-auth enrollment syntax optionally appends a model
		// family after ':'. Collector identities use the same operator-owned list
		// but never accept a self-declared family as authority.
		if enrolled, _, _ := strings.Cut(strings.TrimSpace(entry), ":"); enrolled == identity {
			return true
		}
	}
	return false
}

func capacityCollectorInterval() (time.Duration, error) {
	raw := strings.TrimSpace(envOr("FLOWBEE_CAPACITY_COLLECT_INTERVAL", defaultCapacityCollectorInterval.String()))
	interval, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse FLOWBEE_CAPACITY_COLLECT_INTERVAL: %w", err)
	}
	// Scheduler freshness is five minutes. Leave at least thirty seconds for the
	// supervised-loop watchdog to report a missed collection before routing ages
	// out, instead of creating periodic silent fail-closed gaps by configuration.
	if interval <= 0 || interval > 4*time.Minute {
		return 0, fmt.Errorf("FLOWBEE_CAPACITY_COLLECT_INTERVAL must be >0 and <=4m (got %s)", interval)
	}
	return interval, nil
}
