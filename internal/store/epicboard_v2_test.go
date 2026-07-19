package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEpicDigestAdvancesOnDeliveryOnlyMutation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 4, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "digest-v2", Repo: "repo", Branch: "epic/digest"}, 1, now); err != nil {
		t.Fatal(err)
	}
	before, err := st.EpicDigestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET updated_at=? WHERE epic_id='digest-v2'`, now.Add(time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	after, err := st.EpicDigestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after <= before {
		t.Fatalf("delivery mutation did not advance digest: before=%d after=%d", before, after)
	}
}
