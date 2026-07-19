package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestLegacyUpsertAccountLimitsCannotMutateWhileCapacityV2Enabled(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	err := st.UpsertAccountLimits(context.Background(), acctprobe.Result{
		Identity:   acctprobe.Identity{Provider: acctprobe.ProviderCodex, AccountKey: "legacy-account"},
		TrustState: acctprobe.TrustVerified,
		CapturedAt: time.Now(),
	}, time.Now())
	if !errors.Is(err, store.ErrLegacyCapacityWriterDisabled) {
		t.Fatalf("legacy writer err=%v", err)
	}
	var legacy, generations int
	_ = st.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM account_windows`).Scan(&legacy)
	_ = st.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM capacity_generations`).Scan(&generations)
	if legacy != 0 || generations != 0 {
		t.Fatalf("legacy writer mutated state: windows=%d generations=%d", legacy, generations)
	}
}
