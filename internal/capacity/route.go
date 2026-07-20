package capacity

import (
	"sort"
	"time"
)

// RouteObservation is the complete value input to the v2 fail-closed capacity
// predicate. It deliberately separates one physical seat from the coalesced
// provider-global account projection.
type RouteObservation struct {
	Provider, AccountKey, SeatID, HostID                          string
	Enabled, SeatReady, IdentityMatches, CredentialLineageMatches bool
	Source, TrustState                                            string
	FetchedAt, AccountFetchedAt, CooldownUntil                    time.Time
	AccountTrustState                                             string
	RateLimited                                                   bool
	SeatActive, SeatMaximum, AccountActive, AccountMaximum        int
	BillingPeriodActive                                           bool
	Windows                                                       []RouteWindow
}

type RouteWindow struct {
	Kind       string    `json:"kind"`
	Scope      string    `json:"scope,omitempty"`
	Display    string    `json:"display,omitempty"`
	Applicable bool      `json:"applicable"`
	Known      bool      `json:"known"`
	Percent    float64   `json:"percent"`
	ResetAt    time.Time `json:"reset_at,omitempty"`
}

type RoutePolicy struct {
	FreshFor   time.Duration
	ReservePct float64
}

type RouteDecision struct {
	Routable bool
	Reasons  []string
}

const (
	HoldSeatDisabled           = "seat_disabled"
	HoldSeatNotReady           = "seat_not_ready"
	HoldLiveSourceRequired     = "live_source_required"
	HoldTrustUnverified        = "trust_unverified"
	HoldObservationStale       = "observation_stale"
	HoldIdentityMismatch       = "identity_mismatch"
	HoldCredentialMismatch     = "credential_lineage_mismatch"
	HoldAccountProjectionStale = "account_projection_stale"
	HoldRequiredWindowMissing  = "required_window_missing"
	HoldWindowUnknown          = "window_unknown"
	HoldReserveExhausted       = "reserve_exhausted"
	HoldRateLimited            = "rate_limited"
	HoldCooldown               = "cooldown"
	HoldSeatConcurrency        = "seat_concurrency_exhausted"
	HoldAccountConcurrency     = "account_concurrency_exhausted"
	HoldProviderUnsupported    = "provider_unsupported"
)

// DecideRoute implements §9.7 as a total fail-closed predicate. Unknown is never
// zero except Grok's documented absent-percent + active-billing-period case.
func DecideRoute(now time.Time, observation RouteObservation, policy RoutePolicy) RouteDecision {
	if policy.FreshFor <= 0 {
		policy.FreshFor = 5 * time.Minute
	}
	if policy.ReservePct < 0 {
		policy.ReservePct = 0
	}
	if policy.ReservePct > 100 {
		policy.ReservePct = 100
	}
	reasons := map[string]bool{}
	add := func(reason string) { reasons[reason] = true }
	if !observation.Enabled {
		add(HoldSeatDisabled)
	}
	if !observation.SeatReady {
		add(HoldSeatNotReady)
	}
	liveSource := observation.Source == "live_app_server" || observation.Source == "live_billing"
	if !liveSource {
		add(HoldLiveSourceRequired)
	}
	if observation.TrustState != "verified" {
		add(HoldTrustUnverified)
	}
	if observation.FetchedAt.IsZero() || now.Sub(observation.FetchedAt) < 0 || now.Sub(observation.FetchedAt) > policy.FreshFor {
		add(HoldObservationStale)
	}
	if !observation.IdentityMatches {
		add(HoldIdentityMismatch)
	}
	if !observation.CredentialLineageMatches {
		add(HoldCredentialMismatch)
	}
	if observation.AccountTrustState != "verified" || observation.AccountFetchedAt.IsZero() ||
		now.Sub(observation.AccountFetchedAt) < 0 || now.Sub(observation.AccountFetchedAt) > policy.FreshFor {
		add(HoldAccountProjectionStale)
	}
	if observation.RateLimited {
		add(HoldRateLimited)
	}
	if !observation.CooldownUntil.IsZero() && now.Before(observation.CooldownUntil) {
		add(HoldCooldown)
	}
	if observation.SeatMaximum < 1 || observation.SeatActive >= observation.SeatMaximum {
		add(HoldSeatConcurrency)
	}
	if observation.AccountMaximum > 0 && observation.AccountActive >= observation.AccountMaximum {
		add(HoldAccountConcurrency)
	}

	threshold := 100 - policy.ReservePct
	requiredFound := false
	switch observation.Provider {
	case "codex":
		for _, window := range observation.Windows {
			if !window.Applicable {
				continue
			}
			if window.Kind == "weekly" || window.Kind == "weekly_all" {
				requiredFound = true
			}
			if !window.Known {
				add(HoldWindowUnknown)
				continue
			}
			if window.Percent >= threshold {
				add(HoldReserveExhausted)
			}
		}
		if !requiredFound {
			add(HoldRequiredWindowMissing)
		}
	case "grok":
		if !observation.BillingPeriodActive {
			add(HoldRequiredWindowMissing)
			break
		}
		requiredFound = true
		for _, window := range observation.Windows {
			if !window.Applicable {
				continue
			}
			// Grok defines absent creditUsagePercent as known 0 only when the
			// active billing period has independently validated.
			percent := window.Percent
			if !window.Known {
				percent = 0
			}
			if percent >= threshold {
				add(HoldReserveExhausted)
			}
		}
	default:
		add(HoldProviderUnsupported)
	}
	list := make([]string, 0, len(reasons))
	for reason := range reasons {
		list = append(list, reason)
	}
	sort.Strings(list)
	return RouteDecision{Routable: len(list) == 0, Reasons: list}
}
