package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestIsAccountGated: a rate-limited account is gated OUT of dispatch within the cooldown,
// auto-opens after it (so a gated account self-recovers at its window reset without a
// re-probe), and a clean report clears it immediately. Unknown/clean accounts never gate.
func TestIsAccountGated(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	t0 := time.Unix(1_000_000, 0)

	st.UpsertAccounts(ctx, []store.AccountSpec{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", CeilingPct: 90},
	}, t0)

	// unknown account / clean account => never gated.
	if g, _ := st.IsAccountGated(ctx, "nope", t0); g {
		t.Fatal("unknown account must not gate")
	}
	if g, _ := st.IsAccountGated(ctx, "codex:s@swh.me", t0); g {
		t.Fatal("a freshly-enrolled (not rate-limited) account must not gate")
	}
	if g, _ := st.IsAccountGated(ctx, "", t0); g {
		t.Fatal("empty account must not gate")
	}

	// report rate-limited => gated now, and still gated 10 min later (within cooldown).
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", UsagePct: 100, RateLimited: true},
	}, t0); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.IsAccountGated(ctx, "codex:s@swh.me", t0); !g {
		t.Fatal("a rate-limited account must gate within the cooldown")
	}
	if g, _ := st.IsAccountGated(ctx, "codex:s@swh.me", t0.Add(10*time.Minute)); !g {
		t.Fatal("still gated 10 min in (within 20-min cooldown)")
	}
	// past the cooldown => auto-opens (re-test), so a gated account isn't stuck forever.
	if g, _ := st.IsAccountGated(ctx, "codex:s@swh.me", t0.Add(25*time.Minute)); g {
		t.Fatal("must auto-open after the cooldown so a gated account re-tests")
	}

	// a fresh clean report clears it immediately.
	if _, err := st.RecordUsage(ctx, []capacity.UsageReport{
		{AccountID: "codex:s@swh.me", ModelFamily: "codex", UsagePct: 0, RateLimited: false},
	}, t0.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g, _ := st.IsAccountGated(ctx, "codex:s@swh.me", t0.Add(30*time.Minute)); g {
		t.Fatal("a clean report must clear the gate")
	}
}
