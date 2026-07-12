package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEpicHostCRUD(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	if err := st.AddEpicHost(ctx, store.EpicHost{Name: "buncher", Note: "big box"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := st.AddEpicHost(ctx, store.EpicHost{Name: "buncher"}, now); !errors.Is(err, store.ErrEpicHostExists) {
		t.Fatalf("expected ErrEpicHostExists, got %v", err)
	}

	h, err := st.GetEpicHost(ctx, "buncher")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if h.Note != "big box" || !h.Enabled {
		t.Fatalf("unexpected row: %+v", h)
	}

	if _, err := st.GetEpicHost(ctx, "nope"); !errors.Is(err, store.ErrEpicHostNotFound) {
		t.Fatalf("expected ErrEpicHostNotFound, got %v", err)
	}

	if err := st.AddEpicHost(ctx, store.EpicHost{Name: "imac"}, now); err != nil {
		t.Fatalf("add second: %v", err)
	}
	all, err := st.ListEpicHosts(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list: %v %+v", err, all)
	}
	if all[0].Name != "buncher" || all[1].Name != "imac" {
		t.Fatalf("list not ordered by name: %+v", all)
	}

	if err := st.RemoveEpicHost(ctx, "imac"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := st.RemoveEpicHost(ctx, "imac"); !errors.Is(err, store.ErrEpicHostNotFound) {
		t.Fatalf("expected ErrEpicHostNotFound on double-remove, got %v", err)
	}
}
