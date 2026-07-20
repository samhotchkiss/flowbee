package capacity_test

import (
	"slices"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
)

func healthyRoute(now time.Time) capacity.RouteObservation {
	return capacity.RouteObservation{
		Provider: "codex", AccountKey: "account-1", SeatID: "seat-1", HostID: "host-1",
		Enabled: true, SeatReady: true, IdentityMatches: true, CredentialLineageMatches: true,
		Source: "live_app_server", TrustState: "verified", FetchedAt: now,
		AccountFetchedAt: now, AccountTrustState: "verified", SeatMaximum: 2, AccountMaximum: 3,
		Windows: []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 40}},
	}
}

func TestV2RouteFailsClosedAtReadTime(t *testing.T) {
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	policy := capacity.RoutePolicy{FreshFor: 5 * time.Minute, ReservePct: 10}
	if got := capacity.DecideRoute(now, healthyRoute(now), policy); !got.Routable {
		t.Fatalf("healthy route held: %+v", got)
	}
	cases := []struct {
		name string
		edit func(*capacity.RouteObservation)
		want string
	}{
		{"cache", func(o *capacity.RouteObservation) { o.Source = "cache" }, capacity.HoldLiveSourceRequired},
		{"stale", func(o *capacity.RouteObservation) { o.FetchedAt = now.Add(-6 * time.Minute) }, capacity.HoldObservationStale},
		{"identity", func(o *capacity.RouteObservation) { o.IdentityMatches = false }, capacity.HoldIdentityMismatch},
		{"lineage", func(o *capacity.RouteObservation) { o.CredentialLineageMatches = false }, capacity.HoldCredentialMismatch},
		{"account stale", func(o *capacity.RouteObservation) { o.AccountFetchedAt = now.Add(-6 * time.Minute) }, capacity.HoldAccountProjectionStale},
		{"missing weekly", func(o *capacity.RouteObservation) { o.Windows = nil }, capacity.HoldRequiredWindowMissing},
		{"unknown", func(o *capacity.RouteObservation) { o.Windows[0].Known = false }, capacity.HoldWindowUnknown},
		{"reserve", func(o *capacity.RouteObservation) { o.Windows[0].Percent = 90 }, capacity.HoldReserveExhausted},
		{"seat full", func(o *capacity.RouteObservation) { o.SeatActive = 2 }, capacity.HoldSeatConcurrency},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observation := healthyRoute(now)
			tc.edit(&observation)
			got := capacity.DecideRoute(now, observation, policy)
			if got.Routable || !slices.Contains(got.Reasons, tc.want) {
				t.Fatalf("decision=%+v want %s", got, tc.want)
			}
		})
	}
}

func TestGrokAbsentPercentIsZeroOnlyWithValidatedActivePeriod(t *testing.T) {
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	observation := healthyRoute(now)
	observation.Provider = "grok"
	observation.Source = "live_billing"
	observation.BillingPeriodActive = true
	observation.Windows = []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: false}}
	if got := capacity.DecideRoute(now, observation, capacity.RoutePolicy{ReservePct: 10}); !got.Routable {
		t.Fatalf("documented Grok zero held: %+v", got)
	}
	observation.BillingPeriodActive = false
	if got := capacity.DecideRoute(now, observation, capacity.RoutePolicy{ReservePct: 10}); got.Routable || !slices.Contains(got.Reasons, capacity.HoldRequiredWindowMissing) {
		t.Fatalf("missing billing period routed: %+v", got)
	}
}
