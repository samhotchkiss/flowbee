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

// TestAddEpicHostRejectsArgvHostileNames: a registered host name flows into
// `flowbee epic start`'s ssh/tmux launch argv, so the same argv-safety gate the
// goal-session registry applies (leading '-' = ssh option injection; whitespace/
// control chars = argv splitting) must make such a name unregistrable (review F6).
func TestAddEpicHostRejectsArgvHostileNames(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	for _, name := range []string{
		"-oProxyCommand=evil", // ssh option injection
		"-box",                // any leading dash
		"box name",            // space splits argv
		"box\tname",           // tab
		"box\nname",           // newline
		"box\x01name",         // control char
	} {
		if err := st.AddEpicHost(ctx, store.EpicHost{Name: name}, now); err == nil {
			t.Errorf("AddEpicHost(%q) should be rejected as argv-hostile", name)
		}
	}
	// a normal hostname still registers.
	if err := st.AddEpicHost(ctx, store.EpicHost{Name: "feller.local"}, now); err != nil {
		t.Fatalf("a benign hostname must register: %v", err)
	}
}
