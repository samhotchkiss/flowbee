package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDurableEpicReviewHandoffV2DefaultsLegacyAndPersists(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	if enabled, err := st.DurableEpicReviewHandoffV2(ctx); err != nil || enabled {
		t.Fatalf("unset activation = %v, err=%v; want legacy", enabled, err)
	}
	if err := st.SetDurableEpicReviewHandoffV2(ctx, true); err != nil {
		t.Fatal(err)
	}
	if enabled, err := st.DurableEpicReviewHandoffV2(ctx); err != nil || !enabled {
		t.Fatalf("persisted activation = %v, err=%v; want v2", enabled, err)
	}
	if err := st.SetDurableEpicReviewHandoffV2(ctx, false); err != nil {
		t.Fatal(err)
	}
	if enabled, err := st.DurableEpicReviewHandoffV2(ctx); err != nil || enabled {
		t.Fatalf("explicit rollback activation = %v, err=%v; want legacy", enabled, err)
	}
}

func TestDurableEpicReviewHandoffV2CorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO flowbee_meta(key,value)
		VALUES ('runtime_epic_review_handoff_v2','maybe')`); err != nil {
		t.Fatal(err)
	}
	enabled, err := st.DurableEpicReviewHandoffV2(ctx)
	if err == nil || !enabled {
		t.Fatalf("corrupt activation = %v, err=%v; want fail-closed v2 plus error", enabled, err)
	}
}

func TestLiveDriverControlOriginGateOverridesStartupSnapshot(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableDriverControlOrigin = true
	available := false
	st.DriverControlOriginGate = func() bool { return available }
	if st.HasDriverControlOrigin() {
		t.Fatal("revoked live capability must override a ready startup snapshot")
	}
	available = true
	if !st.HasDriverControlOrigin() {
		t.Fatal("restored exact capability did not reopen the live gate")
	}
}
