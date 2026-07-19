package capacitycollector

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type identityCaptureClient struct {
	called   int
	identity Identity
}

func (c *identityCaptureClient) Collect(_ context.Context, generationID string, identity Identity, seats []Seat, now time.Time) (store.CapacityGeneration, error) {
	c.called++
	c.identity = identity
	observations := make([]store.CapacitySeatObservation, len(seats))
	ids := make([]string, len(seats))
	for i, seat := range seats {
		ids[i] = seat.ID
		observations[i] = store.CapacitySeatObservation{ObservationID: "o-" + seat.ID,
			SeatID: seat.ID, HostID: seat.HostID, CollectorID: identity.ID}
	}
	return store.CapacityGeneration{ID: generationID, ExpectedSeatIDs: ids,
		StartedAt: now.UTC(), Observations: observations}, nil
}

func completeSeat(id, host string) Seat {
	return Seat{ID: id, HostID: host, Provider: "codex", ConfigHome: "/home/" + id,
		ExpectedAccountKey: "account-" + id, ExpectedCredentialLineage: "lineage-" + id, Local: true}
}

func TestEnrolledLocalHostClientFencesCallerIdentity(t *testing.T) {
	delegate := &identityCaptureClient{}
	client, err := NewEnrolledLocalHostClient("host-a", "collector-a", true, delegate)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seat := completeSeat("seat-a", "host-a")
	if _, err := client.Collect(context.Background(), "g", Identity{ID: "collector-a", HostID: "host-a", Authenticated: true}, []Seat{seat}, now); err != nil {
		t.Fatal(err)
	}
	if delegate.called != 1 || delegate.identity != (Identity{ID: "collector-a", HostID: "host-a", Authenticated: true}) {
		t.Fatalf("delegate identity=%+v calls=%d", delegate.identity, delegate.called)
	}
	for _, forged := range []Identity{
		{ID: "collector-a", HostID: "host-a"},
		{ID: "collector-b", HostID: "host-a", Authenticated: true},
		{ID: "collector-a", HostID: "host-b", Authenticated: true},
	} {
		if _, err := client.Collect(context.Background(), "g2", forged, []Seat{seat}, now); !errors.Is(err, ErrCollectorNotEnrolled) {
			t.Fatalf("forged identity %+v err=%v", forged, err)
		}
	}
	if delegate.called != 1 {
		t.Fatalf("forged request reached provider delegate: calls=%d", delegate.called)
	}
}

func TestFleetReadinessFailsClosedForUnboundAndRemoteHost(t *testing.T) {
	delegate := &identityCaptureClient{}
	local, err := NewEnrolledLocalHostClient("local-host", "collector-local", true, delegate)
	if err != nil {
		t.Fatal(err)
	}
	fleet, err := NewFleetService([]HostEnrollment{{HostID: "local-host", CollectorID: "collector-local", Authenticated: true, Client: local}}, &fakeCommitter{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateFleetReadiness(fleet, []Seat{completeSeat("local", "local-host")}); err != nil {
		t.Fatal(err)
	}
	unbound := completeSeat("unbound", "local-host")
	unbound.ExpectedCredentialLineage = ""
	if err := ValidateFleetReadiness(fleet, []Seat{unbound}); err == nil {
		t.Fatal("unbound seat passed readiness")
	}
	if err := ValidateFleetReadiness(fleet, []Seat{completeSeat("remote", "remote-host")}); !errors.Is(err, ErrRemoteTransportUnavailable) {
		t.Fatalf("remote readiness err=%v", err)
	}
}

func TestSQLSeatSourceReturnsOnlySanitizedBoundExpectations(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	seat := store.Seat{AgentFamily: "claude", ConfigDir: "/provider/home",
		ExtraEnv: map[string]string{"FLOWBEE_EXPECTED_ORG_FINGERPRINT": "org-digest", "SECRET_TOKEN": "must-not-escape"}}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: seat.ID,
		HostID: "local-host", AccountKey: "account-a", CredentialLineage: "lineage-a",
		ReservePct: 10, AccountMaximum: 1}, now); err != nil {
		t.Fatal(err)
	}
	got, err := (SQLSeatSource{DB: st.DB}).EnabledCapacitySeats(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("seats=%+v err=%v", got, err)
	}
	if got[0].HostID != "local-host" || got[0].ConfigHome != "/provider/home" ||
		got[0].ExpectedAccountKey != "account-a" || got[0].ExpectedCredentialLineage != "lineage-a" ||
		got[0].ExpectedOrgFingerprint != "org-digest" || !got[0].Local {
		t.Fatalf("sanitized seat=%+v", got[0])
	}
}

func TestFleetServiceConstructionRejectsDuplicateOrUnauthenticatedEnrollment(t *testing.T) {
	delegate := &identityCaptureClient{}
	if _, err := NewFleetService([]HostEnrollment{{HostID: "h", CollectorID: "c", Client: delegate}}, &fakeCommitter{}, 1); !errors.Is(err, ErrCollectorNotEnrolled) {
		t.Fatalf("unauthenticated enrollment err=%v", err)
	}
	_, err := NewFleetService([]HostEnrollment{
		{HostID: "h", CollectorID: "c1", Authenticated: true, Client: delegate},
		{HostID: "h", CollectorID: "c2", Authenticated: true, Client: delegate},
	}, &fakeCommitter{}, 1)
	if err == nil {
		t.Fatal("duplicate host enrollment accepted")
	}
}
