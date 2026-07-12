package capacity

import "testing"

func TestAtCeiling(t *testing.T) {
	cases := []struct {
		name string
		a    Account
		want bool
	}{
		{"under ceiling", Account{CeilingPct: 90, UsagePct: 50}, false},
		{"at ceiling", Account{CeilingPct: 90, UsagePct: 90}, true},
		{"over ceiling", Account{CeilingPct: 90, UsagePct: 95}, true},
		{"no ceiling never gates", Account{CeilingPct: 0, UsagePct: 99}, false},
		{"rate limited gates regardless", Account{CeilingPct: 90, UsagePct: 5, RateLimited: true}, true},
	}
	for _, c := range cases {
		if got := c.a.AtCeiling(); got != c.want {
			t.Errorf("%s: AtCeiling=%v want %v", c.name, got, c.want)
		}
	}
}

// TestSelectAccountRollover proves the core F6 behavior: among a model's accounts,
// the lowest-rank account BELOW its ceiling wins; when the preferred is at/over
// ceiling the choice ROLLS OVER to the fallback; when all are at/over ceiling the
// dispatch must wait (ok=false).
func TestSelectAccountRollover(t *testing.T) {
	primary := Account{AccountID: "claude-primary", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 0}
	fallback := Account{AccountID: "claude-fallback", ModelFamily: "claude", CeilingPct: 90, PreferenceRank: 1}
	codex := Account{AccountID: "codex-a", ModelFamily: "codex", CeilingPct: 90, PreferenceRank: 0}

	// both under ceiling -> the preferred (rank 0) wins.
	a, ok := SelectAccount([]Account{fallback, primary, codex}, "claude")
	if !ok || a.AccountID != "claude-primary" {
		t.Fatalf("preferred selection: got %q ok=%v want claude-primary", a.AccountID, ok)
	}

	// preferred at ceiling -> rolls over to the fallback.
	hot := primary
	hot.UsagePct = 92
	a, ok = SelectAccount([]Account{hot, fallback, codex}, "claude")
	if !ok || a.AccountID != "claude-fallback" {
		t.Fatalf("rollover: got %q ok=%v want claude-fallback", a.AccountID, ok)
	}

	// both claude accounts at ceiling -> must wait (no eligible account).
	hotFb := fallback
	hotFb.UsagePct = 99
	_, ok = SelectAccount([]Account{hot, hotFb, codex}, "claude")
	if ok {
		t.Fatalf("all-at-ceiling: expected ok=false (wait)")
	}

	// a rate-limited preferred (429) also rolls over even when its percent is low.
	rl := primary
	rl.RateLimited = true
	a, ok = SelectAccount([]Account{rl, fallback}, "claude")
	if !ok || a.AccountID != "claude-fallback" {
		t.Fatalf("429 rollover: got %q ok=%v want claude-fallback", a.AccountID, ok)
	}

	// model isolation: a codex job never picks a claude account.
	a, ok = SelectAccount([]Account{hot, hotFb, codex}, "codex")
	if !ok || a.AccountID != "codex-a" {
		t.Fatalf("model isolation: got %q ok=%v want codex-a", a.AccountID, ok)
	}
}

func TestHasFreeSlot(t *testing.T) {
	if !HasFreeSlot(3, 2) {
		t.Error("3 slots, 2 active: should have a free slot")
	}
	if HasFreeSlot(3, 3) {
		t.Error("3 slots, 3 active: should be full")
	}
	if HasFreeSlot(0, 0) {
		t.Error("0 slots: box does not run the model")
	}
}

// TestFoldUsage proves the report fold: a 429 pins usage to >=100% and sets the
// rate-limited flag; a fresh sub-ceiling report clears a prior 429 (recovery).
func TestFoldUsage(t *testing.T) {
	// a normal report adopts the percent, no rate-limit.
	pct, rl := FoldUsage(Account{CeilingPct: 90}, UsageReport{UsagePct: 60})
	if pct != 60 || rl {
		t.Fatalf("normal report: got pct=%d rl=%v want 60,false", pct, rl)
	}

	// a 429 report pins usage to 100% and sets rate-limited even if percent is low.
	pct, rl = FoldUsage(Account{CeilingPct: 90}, UsageReport{UsagePct: 5, RateLimited: true})
	if pct < 100 || !rl {
		t.Fatalf("429 report: got pct=%d rl=%v want >=100,true", pct, rl)
	}

	// a fresh sub-ceiling report clears a prior 429 pin (recovery).
	pct, rl = FoldUsage(Account{CeilingPct: 90, RateLimited: true}, UsageReport{UsagePct: 40})
	if pct != 40 || rl {
		t.Fatalf("recovery: got pct=%d rl=%v want 40,false", pct, rl)
	}

	// a fresh-but-still-over-ceiling report keeps it gated by percent (rl stays).
	pct, rl = FoldUsage(Account{CeilingPct: 90, RateLimited: true}, UsageReport{UsagePct: 95})
	if pct != 95 || !rl {
		t.Fatalf("still-hot: got pct=%d rl=%v want 95,true", pct, rl)
	}
}

// TestFoldWindowedUsage proves the PREEMPTIVE token-budget fold (F6): incremental
// token reports ACCUMULATE over the window and derive a rising usage_pct that
// crosses the ceiling BEFORE the hard 429; a window reset zeroes the bucket; and
// the 429 backstop still pins the account out regardless of the estimate.
func TestFoldWindowedUsage(t *testing.T) {
	const budget = 1000

	// first report in a fresh window: 200/1000 = 20%.
	wt, pct, rl := FoldWindowedUsage(Account{CeilingPct: 90}, UsageReport{TokensDelta: 200}, true, budget)
	if wt != 200 || pct != 20 || rl {
		t.Fatalf("fresh: got wt=%d pct=%d rl=%v want 200,20,false", wt, pct, rl)
	}

	// accumulate onto a prior window: prior 200 + 700 = 900 -> 90% (AT ceiling).
	prior := Account{CeilingPct: 90, WindowTokens: 200, BudgetTokens: budget}
	wt, pct, rl = FoldWindowedUsage(prior, UsageReport{TokensDelta: 700}, false, budget)
	if wt != 900 || pct != 90 || rl {
		t.Fatalf("accumulate: got wt=%d pct=%d rl=%v want 900,90,false", wt, pct, rl)
	}
	// the derived account is now AtCeiling -> gated out (the preemptive cutoff).
	if !(Account{CeilingPct: 90, UsagePct: pct}).AtCeiling() {
		t.Fatal("900/1000 tokens should put the account at the 90% ceiling (preemptive gate)")
	}

	// a window RESET zeroes the bucket even with a big prior: 50 tokens -> 5%, un-gated.
	hot := Account{CeilingPct: 90, WindowTokens: 950, BudgetTokens: budget}
	wt, pct, rl = FoldWindowedUsage(hot, UsageReport{TokensDelta: 50}, true, budget)
	if wt != 50 || pct != 5 || rl {
		t.Fatalf("reset: got wt=%d pct=%d rl=%v want 50,5,false", wt, pct, rl)
	}
	if (Account{CeilingPct: 90, UsagePct: pct}).AtCeiling() {
		t.Fatal("after window reset the account should be below ceiling (sharing resumes)")
	}

	// the 429 BACKSTOP: even at 10% estimated usage, a 429 pins >=100% + rate_limited.
	wt, pct, rl = FoldWindowedUsage(Account{CeilingPct: 90, WindowTokens: 50, BudgetTokens: budget},
		UsageReport{TokensDelta: 50, RateLimited: true}, false, budget)
	if pct < 100 || !rl {
		t.Fatalf("429 backstop: got pct=%d rl=%v want >=100,true", pct, rl)
	}
	if wt != 100 {
		t.Fatalf("429 still accumulates tokens: got wt=%d want 100", wt)
	}

	// recovery from a 429 once back under ceiling (e.g. after a window reset).
	_, pct, rl = FoldWindowedUsage(Account{CeilingPct: 90, RateLimited: true, BudgetTokens: budget},
		UsageReport{TokensDelta: 100}, true, budget)
	if pct != 10 || rl {
		t.Fatalf("recovery: got pct=%d rl=%v want 10,false", pct, rl)
	}

	// no budget configured => fall back to a directly reported provider-%.
	_, pct, rl = FoldWindowedUsage(Account{CeilingPct: 90}, UsageReport{UsagePct: 73}, false, 0)
	if pct != 73 || rl {
		t.Fatalf("provider-%% fallback: got pct=%d rl=%v want 73,false", pct, rl)
	}
}
