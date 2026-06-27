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
//
// WindowTokens / BudgetTokens drive the PREEMPTIVE token-budget estimate (F6
// preemptive ceiling): codex exposes no live usage %, so the box reports the
// incremental tokens each run consumed and the server ACCUMULATES them over the
// account's reset window. UsagePct is then derived as WindowTokens/BudgetTokens —
// a real, rising percentage that crosses the ceiling (default 90) BEFORE the hard
// 429, so dispatch rolls over to a less-used account with headroom to spare. A
// non-positive budget disables the estimate (UsagePct then only moves on a 429,
// the legacy binary behavior).
type Account struct {
	AccountID      string
	ModelFamily    string
	CeilingPct     int
	PreferenceRank int
	UsagePct       int
	RateLimited    bool
	// WindowTokens is the cumulative tokens consumed in the CURRENT reset window
	// (shared across every box on this login). BudgetTokens is the window's total
	// token budget; UsagePct = WindowTokens/BudgetTokens*100 when budget > 0.
	WindowTokens int64
	BudgetTokens int64
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
// IMMEDIATELY on a 429. RateLimited is set when the report was triggered by a 429:
// it pins the account to a cool-down (gated out of dispatch) regardless of percent.
//
// Two reporting modes, not mutually exclusive:
//   - TokensDelta > 0: the box ran the agent and observed it consumed this many
//     tokens. The server ACCUMULATES the delta into the account's reset-window
//     bucket and DERIVES usage_pct from the budget (the preemptive estimate). This
//     is the codex path — codex emits token counts, not a live %.
//   - UsagePct > 0 (TokensDelta == 0): a direct percentage the box parsed from a
//     provider that exposes one (e.g. an "X% of usage limit" line). Adopted as-is.
//
// TokensDelta is INCREMENTAL (one run's tokens), never cumulative — the server owns
// the running total so per-login sharing across boxes folds into ONE bucket.
type UsageReport struct {
	AccountID   string `json:"account_id"`
	ModelFamily string `json:"model_family,omitempty"`
	UsagePct    int    `json:"usage_pct"`
	TokensDelta int64  `json:"tokens_delta,omitempty"`
	RateLimited bool   `json:"rate_limited,omitempty"`
}

// FoldWindowedUsage is the PREEMPTIVE token-budget fold (F6): it accumulates a
// report's incremental tokens into the account's reset-window bucket and DERIVES a
// rising usage percentage from the budget, so the ceiling gate (usage_pct >=
// ceiling_pct) trips with headroom to spare instead of only at the hard 429. PURE:
// the window-boundary decision is passed IN as windowReset (the store computes it
// from the clock), so this stays clock-free and deterministic.
//
//   - windowReset true => the prior window has elapsed: start a fresh bucket at
//     this report's delta (cumulative consumption resets, the account un-gates).
//   - a 429 (r.RateLimited) pins usage to >=100% AND the rate-limited flag, exactly
//     as the binary backstop did — it OVERRIDES the estimate so a provider that
//     429s before 90% is still caught (the estimate is best-effort, the 429 is truth).
//   - otherwise: add the delta, and if a budget is set derive usage_pct from it;
//     with no budget fall back to the directly reported UsagePct (provider-% path).
//
// Returns the new cumulative window tokens and the (usagePct, rateLimited) to persist.
func FoldWindowedUsage(prior Account, r UsageReport, windowReset bool, budgetTokens int64) (windowTokens int64, usagePct int, rateLimited bool) {
	base := prior.WindowTokens
	if windowReset {
		base = 0
	}
	delta := r.TokensDelta
	if delta < 0 {
		delta = 0
	}
	windowTokens = base + delta

	// a 429 is ground truth: pin to spent + rate-limited regardless of the estimate.
	if r.RateLimited {
		pct := r.UsagePct
		if pct < 100 {
			pct = 100
		}
		return windowTokens, pct, true
	}

	// derive the percentage. Prefer the token-budget estimate (codex path); fall
	// back to a directly reported provider-% when no budget is configured.
	switch {
	case budgetTokens > 0:
		usagePct = int(windowTokens * 100 / budgetTokens)
	default:
		usagePct = r.UsagePct
	}

	// recovery: clear a prior 429 pin only once the (reset-or-derived) usage is back
	// under the ceiling — matching the original FoldUsage recovery semantics.
	rateLimited = prior.RateLimited
	if rateLimited && (prior.CeilingPct <= 0 || usagePct < prior.CeilingPct) {
		rateLimited = false
	}
	return windowTokens, usagePct, rateLimited
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
