package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// usageFor returns one account's usage row from AllAccountUsage.
func usageFor(t *testing.T, st *store.Store, accountID string) store.AccountUsageRow {
	t.Helper()
	rows, err := st.AllAccountUsage(context.Background())
	if err != nil {
		t.Fatalf("AllAccountUsage: %v", err)
	}
	for _, r := range rows {
		if r.AccountID == accountID {
			return r
		}
	}
	t.Fatalf("account %s not found in usage rows", accountID)
	return store.AccountUsageRow{}
}

// TestPreemptiveCeilingAccumulates proves the F6 preemptive cutoff end-to-end at the
// store: per-run token reports ACCUMULATE into the shared per-account bucket over the
// reset window, the derived usage_pct rises and crosses the 90 ceiling (AtCeiling) so
// the lease gate rolls dispatch over BEFORE any 429 — then a window reset zeroes the
// bucket and the account un-gates (per-login sharing resumes).
func TestPreemptiveCeilingAccumulates(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	// budget 1000 tokens / 5h window (the gate trips at 900 = 90%).
	st.AccountBudgetTokens = 1000
	st.AccountWindow = 5 * time.Hour

	if err := st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:busy", ModelFamily: "codex", CeilingPct: 90},
	}, time.Unix(0, 0)); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	base := time.Unix(1_000, 0)
	// three runs in the same window: 400, 300, 250 = 950 cumulative tokens.
	report := func(at time.Time, tokens int64) []string {
		ac, err := st.RecordUsage(ctx, []capacity.UsageReport{
			{AccountID: "codex:busy", ModelFamily: "codex", TokensDelta: tokens},
		}, at)
		if err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
		return ac
	}

	report(base, 400)
	if u := usageFor(t, st, "codex:busy"); u.UsagePct != 40 || u.AtCeiling {
		t.Fatalf("after 400/1000: pct=%d atCeiling=%v want 40,false", u.UsagePct, u.AtCeiling)
	}
	report(base.Add(time.Minute), 300)
	if u := usageFor(t, st, "codex:busy"); u.UsagePct != 70 || u.AtCeiling {
		t.Fatalf("after 700/1000: pct=%d atCeiling=%v want 70,false", u.UsagePct, u.AtCeiling)
	}
	// crossing 90: 950/1000 = 95% -> AtCeiling -> the lease gate rolls over (preemptive).
	atCeiling := report(base.Add(2*time.Minute), 250)
	u := usageFor(t, st, "codex:busy")
	if u.UsagePct != 95 || !u.AtCeiling {
		t.Fatalf("after 950/1000: pct=%d atCeiling=%v want 95,true", u.UsagePct, u.AtCeiling)
	}
	if len(atCeiling) != 1 || atCeiling[0] != "codex:busy" {
		t.Fatalf("RecordUsage should surface the at-ceiling account, got %v", atCeiling)
	}
	if _, ok := capacity.SelectAccount([]capacity.Account{
		{AccountID: "codex:busy", ModelFamily: "codex", CeilingPct: 90, UsagePct: u.UsagePct},
	}, "codex"); ok {
		t.Fatal("a 95%-used account must be ineligible for dispatch (preemptive cutoff)")
	}

	// a report AFTER the window elapses zeroes the bucket -> account un-gates.
	report(base.Add(6*time.Hour), 100)
	if u := usageFor(t, st, "codex:busy"); u.UsagePct != 10 || u.AtCeiling {
		t.Fatalf("after window reset: pct=%d atCeiling=%v want 10,false", u.UsagePct, u.AtCeiling)
	}
}

// TestPreemptiveSharingAcrossBoxes proves per-login sharing: two boxes on the SAME
// codex login report against the SAME account_id and their tokens fold into ONE
// bucket — so the shared login crosses the ceiling on their COMBINED consumption.
func TestPreemptiveSharingAcrossBoxes(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	st.AccountBudgetTokens = 1000
	st.AccountWindow = 5 * time.Hour
	if err := st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:gpt", ModelFamily: "codex", CeilingPct: 90},
	}, time.Unix(0, 0)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Unix(2_000, 0)
	// buncher reports 500, imac reports 450 — same login, combined 950 -> 95%.
	for _, tok := range []int64{500, 450} {
		if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
			{AccountID: "codex:gpt", ModelFamily: "codex", TokensDelta: tok},
		}, now); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
		now = now.Add(time.Second)
	}
	if u := usageFor(t, st, "codex:gpt"); u.UsagePct != 95 || !u.AtCeiling {
		t.Fatalf("shared login: pct=%d atCeiling=%v want 95,true", u.UsagePct, u.AtCeiling)
	}
}

// TestPreemptiveRateLimitBackstop proves the 429 backstop is preserved: a hard
// rate-limit report pins the account out even when its token-budget estimate is far
// below the ceiling (the estimate is best-effort; the 429 is ground truth).
func TestPreemptiveRateLimitBackstop(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	st.AccountBudgetTokens = 1_000_000 // huge budget: the estimate alone never gates
	st.AccountWindow = 5 * time.Hour
	if err := st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:s", ModelFamily: "codex", CeilingPct: 90},
	}, time.Unix(0, 0)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Unix(3_000, 0)
	// a tiny token report with a 429 flag: estimate ~0%, but the backstop gates it.
	atCeiling, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:s", ModelFamily: "codex", TokensDelta: 10, RateLimited: true},
	}, now)
	if err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if len(atCeiling) != 1 {
		t.Fatalf("a 429 should put the account at ceiling, got %v", atCeiling)
	}
	u := usageFor(t, st, "codex:s")
	if !u.RateLimited || !u.AtCeiling || u.UsagePct < 100 {
		t.Fatalf("429 backstop: rl=%v atCeiling=%v pct=%d want true,true,>=100", u.RateLimited, u.AtCeiling, u.UsagePct)
	}
}

// TestPerAccountBudgetOverride proves a per-account budget_tokens override wins over
// the store default: a bigger-quota login carries more tokens before it gates.
func TestPerAccountBudgetOverride(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	st.AccountBudgetTokens = 1000 // default
	st.AccountWindow = 5 * time.Hour
	if err := st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:big", ModelFamily: "codex", CeilingPct: 90, BudgetTokens: 10000},
	}, time.Unix(0, 0)); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Unix(4_000, 0)
	// 950 tokens would be 95% on the default budget, but only 9% on the 10k override.
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:big", ModelFamily: "codex", TokensDelta: 950},
	}, now); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	if u := usageFor(t, st, "codex:big"); u.UsagePct != 9 || u.AtCeiling {
		t.Fatalf("override budget: pct=%d atCeiling=%v want 9,false", u.UsagePct, u.AtCeiling)
	}
}
