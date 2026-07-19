package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestLegacyCapacityFoldCannotWriteWhenV2Enabled(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if err := st.AddSeat(ctx, store.Seat{Box: "host-that-must-not-be-probed", AgentFamily: "codex", CodexHome: "/codex", Health: store.SeatReady}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER reject_legacy_capacity_insert BEFORE INSERT ON account_windows BEGIN SELECT RAISE(ABORT,'legacy writer ran'); END`); err != nil {
		t.Fatal(err)
	}
	foldSeatCapacity(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), st, now)
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_windows`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy account windows mutated: %d", count)
	}
}
