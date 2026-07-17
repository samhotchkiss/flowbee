package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestSeatCRUD(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	claude := store.Seat{Box: "buncher", AgentFamily: "claude", AccountKey: "acc-1", ConfigDir: "/home/ops/.claude-pearl", ExtraEnv: map[string]string{"FOO": "bar"}}
	if err := st.AddSeat(ctx, claude, now); err != nil {
		t.Fatalf("add claude seat: %v", err)
	}
	// re-adding the same box+dir is a dup (not an upsert).
	if err := st.AddSeat(ctx, claude, now); !errors.Is(err, store.ErrSeatExists) {
		t.Fatalf("expected ErrSeatExists, got %v", err)
	}

	got, err := st.GetSeat(ctx, "buncher|claude|/home/ops/.claude-pearl")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.AgentFamily != "claude" || got.AccountKey != "acc-1" || got.Health != store.SeatUnreachable {
		t.Fatalf("unexpected seat: %+v", got)
	}
	if got.ExtraEnv["FOO"] != "bar" || !got.Enabled {
		t.Fatalf("env/enabled: %+v", got)
	}
	// max_concurrent defaults to 1 (one-box-one-epic preserved) when not specified.
	if got.MaxConcurrent != 1 {
		t.Fatalf("expected default max_concurrent=1, got %d", got.MaxConcurrent)
	}

	// a codex seat on the same box is a distinct row (different dir).
	if err := st.AddSeat(ctx, store.Seat{Box: "buncher", AgentFamily: "codex", CodexHome: "/home/ops/.codex"}, now); err != nil {
		t.Fatalf("add codex seat: %v", err)
	}
	// a grok seat reuses config_dir for its GROK_HOME; the seatID folds the family in.
	grok := store.Seat{Box: "buncher", AgentFamily: "grok", AccountKey: "acc-g", ConfigDir: "/home/ops/.grok"}
	if err := st.AddSeat(ctx, grok, now); err != nil {
		t.Fatalf("add grok seat: %v", err)
	}
	gotGrok, err := st.GetSeat(ctx, "buncher|grok|/home/ops/.grok")
	if err != nil {
		t.Fatalf("get grok: %v", err)
	}
	if gotGrok.AgentFamily != "grok" || gotGrok.Ident() != "/home/ops/.grok" {
		t.Fatalf("unexpected grok seat: %+v (ident=%q)", gotGrok, gotGrok.Ident())
	}
	all, err := st.ListSeats(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("list: n=%d err=%v", len(all), err)
	}
	// grok is selectable by family in ListReadySeats.
	if err := st.UpdateSeatHealth(ctx, gotGrok.ID, store.SeatReady, "weekly 5%", now); err != nil {
		t.Fatalf("grok health: %v", err)
	}
	if ready, err := st.ListReadySeats(ctx, "grok"); err != nil || len(ready) != 1 || ready[0].AgentFamily != "grok" {
		t.Fatalf("expected 1 ready grok seat, got %+v err=%v", ready, err)
	}
}

// TestSeatMaxConcurrent covers the 0031 per-seat concurrent-epic cap: an explicit cap
// round-trips through AddSeat, an unset/zero cap normalizes to 1 (never an unbounded box),
// SetSeatMaxConcurrent updates it (the operational "make this a 2-wide codex seat" write),
// a cap below 1 is rejected, and an unknown seat is ErrSeatNotFound.
func TestSeatMaxConcurrent(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// a codex box registered 2-wide round-trips the cap.
	codex := store.Seat{Box: "codexbox", AgentFamily: "codex", CodexHome: "/home/ops/.codex", MaxConcurrent: 2}
	if err := st.AddSeat(ctx, codex, now); err != nil {
		t.Fatalf("add codex 2-wide: %v", err)
	}
	if cb, err := st.GetSeat(ctx, codex.ComposeID()); err != nil || cb.MaxConcurrent != 2 {
		t.Fatalf("expected codex max_concurrent=2, got %+v (err %v)", cb, err)
	}

	// an unset/zero cap normalizes to 1 (never a cap-0 seat that refuses every launch).
	zero := store.Seat{Box: "zerobox", AgentFamily: "claude", ConfigDir: "/home/ops/.claude", MaxConcurrent: 0}
	if err := st.AddSeat(ctx, zero, now); err != nil {
		t.Fatalf("add zerobox: %v", err)
	}
	if zb, _ := st.GetSeat(ctx, zero.ComposeID()); zb.MaxConcurrent != 1 {
		t.Fatalf("expected zerobox cap normalized to 1, got %d", zb.MaxConcurrent)
	}

	// SetSeatMaxConcurrent bumps it later (turn the zerobox claude seat into a 3-wide seat).
	if err := st.SetSeatMaxConcurrent(ctx, zero.ComposeID(), 3, now); err != nil {
		t.Fatalf("set max concurrent: %v", err)
	}
	if zb, _ := st.GetSeat(ctx, zero.ComposeID()); zb.MaxConcurrent != 3 {
		t.Fatalf("expected zerobox cap updated to 3, got %d", zb.MaxConcurrent)
	}

	// a cap below 1 is rejected (that's a seat rm, not a cap); an unknown seat is not found.
	if err := st.SetSeatMaxConcurrent(ctx, zero.ComposeID(), 0, now); err == nil {
		t.Fatal("expected max_concurrent < 1 to be rejected")
	}
	if err := st.SetSeatMaxConcurrent(ctx, "ghost|codex|/nope", 2, now); !errors.Is(err, store.ErrSeatNotFound) {
		t.Fatalf("expected ErrSeatNotFound setting cap on unknown seat, got %v", err)
	}
}

func TestSeatValidation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()

	cases := []struct {
		name string
		seat store.Seat
	}{
		{"unknown family", store.Seat{Box: "b", AgentFamily: "gpt", CodexHome: "/x"}},
		{"claude without config_dir", store.Seat{Box: "b", AgentFamily: "claude"}},
		{"claude with codex_home", store.Seat{Box: "b", AgentFamily: "claude", ConfigDir: "/a", CodexHome: "/b"}},
		{"codex without codex_home", store.Seat{Box: "b", AgentFamily: "codex"}},
		{"grok without config_dir", store.Seat{Box: "b", AgentFamily: "grok"}},
		{"grok with codex_home", store.Seat{Box: "b", AgentFamily: "grok", ConfigDir: "/g", CodexHome: "/x"}},
		{"grok argv-unsafe config_dir", store.Seat{Box: "b", AgentFamily: "grok", ConfigDir: "/a b"}},
		{"argv-unsafe box", store.Seat{Box: "-oProxyCommand=x", AgentFamily: "codex", CodexHome: "/x"}},
		{"argv-unsafe config_dir", store.Seat{Box: "b", AgentFamily: "claude", ConfigDir: "/a b"}},
		{"bad env key", store.Seat{Box: "b", AgentFamily: "codex", CodexHome: "/x", ExtraEnv: map[string]string{"9BAD": "v"}}},
		{"argv-unsafe env value", store.Seat{Box: "b", AgentFamily: "codex", CodexHome: "/x", ExtraEnv: map[string]string{"OK": "a b"}}},
	}
	for _, c := range cases {
		if err := st.AddSeat(ctx, c.seat, now); err == nil {
			t.Errorf("%s: expected rejection, got nil", c.name)
		}
	}
}

func TestListReadySeatsAndHealth(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	if err := st.AddSeat(ctx, store.Seat{Box: "b1", AgentFamily: "claude", ConfigDir: "/c1"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddSeat(ctx, store.Seat{Box: "b2", AgentFamily: "claude", ConfigDir: "/c2"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddSeat(ctx, store.Seat{Box: "b3", AgentFamily: "codex", CodexHome: "/c3"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}

	// nothing ready yet (all unreachable).
	ready, err := st.ListReadySeats(ctx, "claude")
	if err != nil || len(ready) != 0 {
		t.Fatalf("expected 0 ready, n=%d err=%v", len(ready), err)
	}

	if err := st.UpdateSeatHealth(ctx, "b1|claude|/c1", store.SeatReady, "weekly 30%", now); err != nil {
		t.Fatalf("health b1: %v", err)
	}
	if err := st.UpdateSeatHealth(ctx, "b2|claude|/c2", store.SeatLimitCritical, "weekly 96%", now); err != nil {
		t.Fatalf("health b2: %v", err)
	}
	if err := st.UpdateSeatHealth(ctx, "b3|codex|/c3", store.SeatReady, "ok", now); err != nil {
		t.Fatalf("health b3: %v", err)
	}

	ready, err = st.ListReadySeats(ctx, "claude")
	if err != nil {
		t.Fatalf("ready: %v", err)
	}
	if len(ready) != 1 || ready[0].ID != "b1|claude|/c1" {
		t.Fatalf("expected only b1 ready for claude, got %+v", ready)
	}

	if err := st.UpdateSeatHealth(ctx, "nope", store.SeatReady, "", now); !errors.Is(err, store.ErrSeatNotFound) {
		t.Fatalf("expected ErrSeatNotFound, got %v", err)
	}
	if err := st.UpdateSeatHealth(ctx, "b1|claude|/c1", "bogus", "", now); err == nil {
		t.Fatal("expected invalid health rejection")
	}
}

func TestSetSeatAccountKey(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.AddSeat(ctx, store.Seat{Box: "b1", AgentFamily: "codex", CodexHome: "/c1"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.SetSeatAccountKey(ctx, "b1|codex|/c1", "acct-xyz", now); err != nil {
		t.Fatalf("set key: %v", err)
	}
	got, _ := st.GetSeat(ctx, "b1|codex|/c1")
	if got.AccountKey != "acct-xyz" {
		t.Fatalf("account key not set: %+v", got)
	}
}
