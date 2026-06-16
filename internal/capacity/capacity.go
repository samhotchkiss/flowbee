// Package capacity holds the F6 worker-capacity decision logic: per-model slot
// gating, ceiling-gated account selection with ROLLOVER, and the usage-report
// fold. It is a deterministic-core-style package (DESIGN §1.2 spirit): every
// function here is PURE over its value inputs — no clock, no randomness, no ID
// minter, no GitHub, no LLM. The instant a usage report carries is passed IN as a
// value (never read from a clock), so the same inputs always yield the same
// dispatch decision. internal/store wires these into the (serialized) lease claim;
// internal/api exposes the usage-report endpoint.
//
// The shape of the problem:
//   - A BOX advertises concurrency PER MODEL (claude:3, codex:3) — a slot budget
//     keyed by model_family, replacing the single max_concurrent_leases.
//   - A MODEL has named ACCOUNTS (per-model credentials, shared across boxes on
//     the same login). Usage is tracked PER ACCOUNT. Each account has a
//     ceiling_pct and an ordered preference_rank.
//   - DISPATCH is gated by the ceiling: a job for a model picks the lowest-rank
//     account that is BELOW its ceiling. If the preferred account is at/over its
//     ceiling (or rate-limited by a 429), dispatch ROLLS OVER to the next account;
//     if no account is below ceiling, the box WAITS (no dispatch).
package capacity

// Account is one named per-model credential as the selector sees it. UsagePct is
// the last-reported per-account usage (0..100+); CeilingPct is the dispatch gate;
// PreferenceRank orders the rollover chain (lower = preferred); RateLimited pins
// the account out of rotation after a 429 until a fresh sub-ceiling report clears
// it. All fields are plain values folded from the worker_accounts projection.
type Account struct {
	AccountID      string
	ModelFamily    string
	CeilingPct     int
	PreferenceRank int
	UsagePct       int
	RateLimited    bool
}

// AtCeiling reports whether the account is gated OUT of dispatch: either a 429
// pinned it (RateLimited) or its reported usage has reached/exceeded its ceiling.
// A non-positive ceiling is treated as "no ceiling" (never gates on usage alone),
// matching the operator intent of leaving ceiling unset = unlimited.
func (a Account) AtCeiling() bool {
	if a.RateLimited {
		return true
	}
	if a.CeilingPct <= 0 {
		return false
	}
	return a.UsagePct >= a.CeilingPct
}

// SelectAccount chooses the account a job for modelFamily should dispatch against:
// among the accounts for that model, the lowest preference_rank that is BELOW its
// ceiling (and not rate-limited). This is the ROLLOVER: if the preferred account
// is at/over ceiling, the next-ranked eligible account wins. Returns ok=false when
// EVERY account for the model is at/over ceiling — the box must WAIT (a capacity
// alarm surface, §C). Ties on rank break by AccountID for determinism. PURE.
func SelectAccount(accounts []Account, modelFamily string) (Account, bool) {
	var best Account
	found := false
	for _, a := range accounts {
		if a.ModelFamily != modelFamily {
			continue
		}
		if a.AtCeiling() {
			continue
		}
		if !found || less(a, best) {
			best = a
			found = true
		}
	}
	return best, found
}

// less orders two eligible accounts: lower preference_rank wins, ties broken by
// AccountID (lexicographic) for a total, deterministic order.
func less(a, b Account) bool {
	if a.PreferenceRank != b.PreferenceRank {
		return a.PreferenceRank < b.PreferenceRank
	}
	return a.AccountID < b.AccountID
}

// HasFreeSlot reports whether a box may start one more lease for modelFamily: the
// box's advertised per-model concurrency (maxSlots) must exceed the count of
// leases it currently holds for that model (active). PURE — the caller supplies
// both counts from the (serialized) projection. maxSlots <= 0 means the box does
// not run that model at all (no slot), so it can never start one.
func HasFreeSlot(maxSlots, active int) bool {
	if maxSlots <= 0 {
		return false
	}
	return active < maxSlots
}

// UsageReport is one per-account usage observation a box reports (POST
// /v1/workers/usage), best-effort from rate-limit headers / token counts, or
// IMMEDIATELY on a 429. UsagePct is the account's usage as a percent of its budget
// (0..100+). RateLimited is set when the report was triggered by a 429: it pins
// the account to a cool-down (gated out of dispatch) regardless of the percent.
type UsageReport struct {
	AccountID   string `json:"account_id"`
	ModelFamily string `json:"model_family,omitempty"`
	UsagePct    int    `json:"usage_pct"`
	RateLimited bool   `json:"rate_limited,omitempty"`
}

// FoldUsage applies a usage report onto an account's prior state, returning the
// new (UsagePct, RateLimited). A 429 report pins usage to at least 100% AND sets
// the rate-limited flag (the account is out until it cools). A normal report below
// the ceiling CLEARS a prior rate-limited flag (the account recovered) and adopts
// the reported percent. PURE: the new account state is a function of the prior
// state and the report only.
func FoldUsage(prior Account, r UsageReport) (usagePct int, rateLimited bool) {
	if r.RateLimited {
		pct := r.UsagePct
		if pct < 100 {
			pct = 100 // a 429 means the budget is effectively spent
		}
		return pct, true
	}
	// a fresh, non-429 report: adopt the percent and clear any prior 429 pin once
	// usage is back under the ceiling (recovery). If it is still at/over ceiling we
	// leave it gated by the percent alone (AtCeiling handles that).
	cleared := false
	if prior.RateLimited {
		// recovered only if the new percent is below the ceiling.
		if prior.CeilingPct <= 0 || r.UsagePct < prior.CeilingPct {
			cleared = true
		}
	}
	return r.UsagePct, prior.RateLimited && !cleared
}
