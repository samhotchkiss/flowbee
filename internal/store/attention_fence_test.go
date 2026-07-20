package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestResolveAttentionFencedStaleEpoch proves the no-send resolve (dismiss/ack/escalate) is
// fenced IN-TX (m1): a stale supervisor epoch or a stale item_epoch is rejected with
// ErrStaleEpoch and leaves the item untouched, so a superseded incarnation can never
// dismiss/escalate an item another master now holds across an epoch boundary. Only the live
// leaseholder at the matching epoch resolves it.
func TestResolveAttentionFencedStaleEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	reg := registerMaster(t, st, ctx, "m", attnT0)
	upsertItem(t, st, ctx, store.AttentionItem{
		ID: "att1", Kind: "needs_input", EpicID: "e1", Repo: "r", Priority: 20, DedupKey: "e1:needs_input",
	}, attnT0)

	leased, err := st.LeaseAttention(ctx, reg.MasterID, reg.Epoch, 5, nil, time.Hour, attnT0.Add(time.Minute))
	if err != nil || len(leased) != 1 {
		t.Fatalf("lease: err=%v got %d items", err, len(leased))
	}
	it := leased[0]

	// stale SUPERVISOR epoch → fenced; item stays leased.
	if err := st.ResolveAttentionFenced(ctx, it.ID, reg.MasterID, reg.Epoch+99, it.ItemEpoch, "dismissed", attnT0.Add(2*time.Minute)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale supervisor epoch: want ErrStaleEpoch, got %v", err)
	}
	if after, _ := st.GetAttentionItem(ctx, it.ID); after.State != "leased" {
		t.Fatalf("a fenced resolve must not change state, got %q", after.State)
	}

	// stale ITEM epoch → fenced.
	if err := st.ResolveAttentionFenced(ctx, it.ID, reg.MasterID, reg.Epoch, it.ItemEpoch+1, "dismissed", attnT0.Add(3*time.Minute)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale item epoch: want ErrStaleEpoch, got %v", err)
	}

	// wrong leaseholder → fenced (a different master presenting its own epoch).
	other := registerMaster(t, st, ctx, "other", attnT0)
	if err := st.ResolveAttentionFenced(ctx, it.ID, other.MasterID, other.Epoch, it.ItemEpoch, "escalated", attnT0.Add(4*time.Minute)); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("wrong leaseholder: want ErrStaleEpoch, got %v", err)
	}

	// the live leaseholder at the matching epoch resolves it.
	if err := st.ResolveAttentionFenced(ctx, it.ID, reg.MasterID, reg.Epoch, it.ItemEpoch, "dismissed", attnT0.Add(5*time.Minute)); err != nil {
		t.Fatalf("valid fenced resolve: %v", err)
	}
	final, _ := st.GetAttentionItem(ctx, it.ID)
	if final.State != "resolved" || final.Resolution != "dismissed" {
		t.Fatalf("want resolved/dismissed, got %q/%q", final.State, final.Resolution)
	}
}
