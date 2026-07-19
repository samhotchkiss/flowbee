package capacitycollector

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

var (
	ErrCollectorNotEnrolled = errors.New("capacity collector is not enrolled for host")
	// ErrRemoteTransportUnavailable is returned only as an activation/readiness
	// failure. Flowbee intentionally does not substitute SSH, raw tmux, worker
	// usage reports, or a private one-off HTTP protocol for the missing live probe
	// operation.
	ErrRemoteTransportUnavailable = errors.New("authenticated remote live capacity collector transport is unavailable")
)

// HostEnrollment is operator-owned transport authority. Authenticated must be
// derived from an enrolled local process boundary or a future authenticated
// transport peer; it must never be copied from a report body.
type HostEnrollment struct {
	HostID, CollectorID string
	Authenticated       bool
	Client              HostClient
}

// EnrolledLocalHostClient is the production adapter for seats whose provider
// config home is on the control-plane host. Its authority comes from explicit
// serve configuration and enrollment; callers cannot change the bound host or
// collector identity at Collect time.
type EnrolledLocalHostClient struct {
	hostID, collectorID string
	delegate            HostClient
}

func NewEnrolledLocalHostClient(hostID, collectorID string, enrolled bool, delegate HostClient) (*EnrolledLocalHostClient, error) {
	if hostID == "" || collectorID == "" || !enrolled || delegate == nil {
		return nil, ErrCollectorNotEnrolled
	}
	return &EnrolledLocalHostClient{hostID: hostID, collectorID: collectorID, delegate: delegate}, nil
}

func (c *EnrolledLocalHostClient) Collect(ctx context.Context, generationID string, identity Identity, seats []Seat, now time.Time) (store.CapacityGeneration, error) {
	if c == nil || c.delegate == nil || !identity.Authenticated ||
		identity.ID != c.collectorID || identity.HostID != c.hostID {
		return store.CapacityGeneration{}, ErrCollectorNotEnrolled
	}
	if err := c.ValidateSeats(seats); err != nil {
		return store.CapacityGeneration{}, err
	}
	// Reconstruct the identity from the immutable enrollment rather than passing
	// through caller-owned fields. The local delegate therefore sees the same
	// trust shape a future authenticated remote adapter must provide.
	bound := Identity{ID: c.collectorID, HostID: c.hostID, Authenticated: true}
	return c.delegate.Collect(ctx, generationID, bound, seats, now)
}

func (c *EnrolledLocalHostClient) ValidateSeats(seats []Seat) error {
	for _, seat := range seats {
		if seat.HostID != c.hostID {
			return fmt.Errorf("seat %s targets host %s through collector for host %s", seat.ID, seat.HostID, c.hostID)
		}
		if !seat.Local || !filepath.IsAbs(seat.ConfigHome) {
			return fmt.Errorf("%w for seat %s on host %s (GAP-FD-002)", ErrRemoteTransportUnavailable, seat.ID, seat.HostID)
		}
	}
	return nil
}

// NewFleetService validates and materializes the host maps consumed by the
// existing atomic FleetService. It rejects a missing client before any provider
// call or generation commit can occur.
func NewFleetService(enrollments []HostEnrollment, committer GenerationCommitter, concurrency int) (FleetService, error) {
	if committer == nil {
		return FleetService{}, errors.New("capacity generation committer is required")
	}
	fleet := FleetService{Hosts: map[string]HostClient{}, Collectors: map[string]Identity{}, Committer: committer, Concurrency: concurrency}
	collectorIDs := map[string]string{}
	for _, enrollment := range enrollments {
		if enrollment.HostID == "" || enrollment.CollectorID == "" || !enrollment.Authenticated || enrollment.Client == nil {
			return FleetService{}, ErrCollectorNotEnrolled
		}
		if _, duplicate := fleet.Hosts[enrollment.HostID]; duplicate {
			return FleetService{}, fmt.Errorf("duplicate capacity collector enrollment for host %s", enrollment.HostID)
		}
		if priorHost := collectorIDs[enrollment.CollectorID]; priorHost != "" {
			return FleetService{}, fmt.Errorf("capacity collector %s is bound to both %s and %s", enrollment.CollectorID, priorHost, enrollment.HostID)
		}
		collectorIDs[enrollment.CollectorID] = enrollment.HostID
		fleet.Hosts[enrollment.HostID] = enrollment.Client
		fleet.Collectors[enrollment.HostID] = Identity{ID: enrollment.CollectorID, HostID: enrollment.HostID, Authenticated: true}
	}
	if len(fleet.Hosts) == 0 {
		return FleetService{}, ErrCollectorNotEnrolled
	}
	return fleet, nil
}

// ValidateFleetReadiness proves every enabled seat has a complete immutable
// identity expectation and an authenticated HostClient. This is called before
// active v2 readiness and again before each generation, so a newly added remote
// or unbound seat fails closed rather than disappearing from the fold.
func ValidateFleetReadiness(fleet FleetService, seats []Seat) error {
	if len(seats) == 0 {
		return errors.New("capacity routing v2 requires at least one enabled seat")
	}
	ordered := append([]Seat(nil), seats...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seatIDs, homes := map[string]bool{}, map[string]string{}
	byHost := map[string][]Seat{}
	for _, seat := range ordered {
		if seat.ID == "" || seat.HostID == "" || seat.Provider == "" || seat.ConfigHome == "" ||
			seat.ExpectedAccountKey == "" || seat.ExpectedCredentialLineage == "" {
			return fmt.Errorf("capacity seat %q has incomplete host/account/lineage enrollment", seat.ID)
		}
		if seatIDs[seat.ID] {
			return fmt.Errorf("duplicate capacity seat %s", seat.ID)
		}
		seatIDs[seat.ID] = true
		homeKey := seat.HostID + "\x00" + seat.Provider + "\x00" + seat.ConfigHome
		if prior := homes[homeKey]; prior != "" {
			return fmt.Errorf("canonical config home %s is registered by both %s and %s", seat.ConfigHome, prior, seat.ID)
		}
		homes[homeKey] = seat.ID
		client, identity := fleet.Hosts[seat.HostID], fleet.Collectors[seat.HostID]
		if client == nil || identity.ID == "" || identity.HostID != seat.HostID || !identity.Authenticated {
			return fmt.Errorf("%w for host %s (GAP-FD-002)", ErrRemoteTransportUnavailable, seat.HostID)
		}
		byHost[seat.HostID] = append(byHost[seat.HostID], seat)
	}
	for hostID, hostSeats := range byHost {
		if validator, ok := fleet.Hosts[hostID].(interface{ ValidateSeats([]Seat) error }); ok {
			if err := validator.ValidateSeats(hostSeats); err != nil {
				return err
			}
		}
	}
	return nil
}
