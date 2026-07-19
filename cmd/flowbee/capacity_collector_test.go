package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacitycollector"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type staticCapacitySeats struct{ seats []capacitycollector.Seat }

func (s staticCapacitySeats) EnabledCapacitySeats(context.Context) ([]capacitycollector.Seat, error) {
	return append([]capacitycollector.Seat(nil), s.seats...), nil
}

type runtimeHostClient struct{ calls int }

func (c *runtimeHostClient) Collect(_ context.Context, generationID string, identity capacitycollector.Identity, seats []capacitycollector.Seat, now time.Time) (store.CapacityGeneration, error) {
	c.calls++
	gen := store.CapacityGeneration{ID: generationID, StartedAt: now.UTC()}
	for _, seat := range seats {
		gen.ExpectedSeatIDs = append(gen.ExpectedSeatIDs, seat.ID)
		gen.Observations = append(gen.Observations, store.CapacitySeatObservation{
			ObservationID: "observation-" + seat.ID, SeatID: seat.ID, HostID: seat.HostID,
			CollectorID: identity.ID,
		})
	}
	return gen, nil
}

type runtimeCommitter struct {
	calls int
	last  store.CapacityGeneration
}

func (c *runtimeCommitter) CommitCapacityGeneration(_ context.Context, generation store.CapacityGeneration, _ time.Time) error {
	c.calls++
	c.last = generation
	return nil
}

func TestCapacityCollectorRuntimeInvokesAtomicFleetService(t *testing.T) {
	host := &runtimeHostClient{}
	sink := &runtimeCommitter{}
	fleet, err := capacitycollector.NewFleetService([]capacitycollector.HostEnrollment{{
		HostID: "host-a", CollectorID: "collector-a", Authenticated: true, Client: host,
	}}, sink, 1)
	if err != nil {
		t.Fatal(err)
	}
	seat := capacitycollector.Seat{ID: "seat-a", HostID: "host-a", Provider: "codex",
		ConfigHome: "/home/codex", ExpectedAccountKey: "account", ExpectedCredentialLineage: "lineage"}
	runtime := capacityCollectorRuntime{Seats: staticCapacitySeats{[]capacitycollector.Seat{seat}}, Fleet: fleet,
		NewGenerationID: func() string { return "generation-a" }}
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	got, err := runtime.collect(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if host.calls != 1 || sink.calls != 1 || got.ID != "generation-a" || sink.last.ID != got.ID {
		t.Fatalf("host=%d commits=%d got=%+v last=%+v", host.calls, sink.calls, got, sink.last)
	}
}

func TestProductionCapacityCollectorRequiresEnrollmentAndLocalSeats(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := newProductionCapacityCollector(st, config.Config{}, "host-a", "collector-a"); err == nil {
		t.Fatal("unenrolled collector accepted")
	}
	cfg := config.Config{EnrolledIdentities: []string{"collector-a"}}
	runtime, err := newProductionCapacityCollector(st, cfg, "host-a", "collector-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.readiness(ctx); err == nil {
		t.Fatal("capacity v2 with zero seats passed readiness")
	}
	seat := store.Seat{Box: "remote-box", AgentFamily: "codex", CodexHome: "/home/codex"}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: seat.ID,
		HostID: "host-a", AccountKey: "account", CredentialLineage: "lineage", ReservePct: 10}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.readiness(ctx); !errors.Is(err, capacitycollector.ErrRemoteTransportUnavailable) {
		t.Fatalf("remote seat readiness err=%v", err)
	}
}

func TestProductionCapacityCollectorAcceptsFullyBoundLocalSeat(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 30, 0, 0, time.UTC)
	seat := store.Seat{AgentFamily: "codex", CodexHome: "/home/local-codex"}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{
		SeatID: seat.ID, HostID: "control-host", AccountKey: "account",
		CredentialLineage: "lineage", ReservePct: 10,
	}, now); err != nil {
		t.Fatal(err)
	}
	runtime, err := newProductionCapacityCollector(st,
		config.Config{EnrolledIdentities: []string{"collector-local:operations"}},
		"control-host", "collector-local")
	if err != nil {
		t.Fatal(err)
	}
	seats, err := runtime.readiness(ctx)
	if err != nil || len(seats) != 1 || !seats[0].Local || seats[0].HostID != "control-host" {
		t.Fatalf("local readiness seats=%+v err=%v", seats, err)
	}
}

func TestCapacityCollectorLoopRunsEveryProvidedTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time, 2)
	done := make(chan struct{})
	var got []time.Time
	go func() {
		runCapacityCollectorLoop(ctx, ticks, func(now time.Time) { got = append(got, now) })
		close(done)
	}()
	first := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	second := first.Add(2 * time.Minute)
	ticks <- first
	ticks <- second
	close(ticks)
	<-done
	if len(got) != 2 || !got[0].Equal(first) || !got[1].Equal(second) {
		t.Fatalf("periodic collection ticks=%v", got)
	}
}

func TestCapacityCollectorIntervalStaysInsideFreshnessWindow(t *testing.T) {
	t.Setenv("FLOWBEE_CAPACITY_COLLECT_INTERVAL", "90s")
	if got, err := capacityCollectorInterval(); err != nil || got != 90*time.Second {
		t.Fatalf("interval=%s err=%v", got, err)
	}
	t.Setenv("FLOWBEE_CAPACITY_COLLECT_INTERVAL", "5m")
	if _, err := capacityCollectorInterval(); err == nil {
		t.Fatal("cadence equal to freshness boundary accepted")
	}
	t.Setenv("FLOWBEE_CAPACITY_COLLECT_INTERVAL", "4m")
	if got, err := capacityCollectorInterval(); err != nil || got != 4*time.Minute {
		t.Fatalf("safe maximum cadence=%s err=%v", got, err)
	}
}

func TestCapacityCollectorEnrollmentAcceptsExistingFamilySuffixSyntax(t *testing.T) {
	if !capacityCollectorEnrolled([]string{"collector-a:operations", "worker-b"}, "collector-a") {
		t.Fatal("existing enrolled identity suffix syntax not recognized")
	}
	if capacityCollectorEnrolled([]string{"collector-b"}, "collector-a") {
		t.Fatal("wrong collector identity accepted")
	}
}
