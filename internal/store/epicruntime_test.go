package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestSetEpicRuntimeStateAndSeatBinding(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: "frob", Repo: "acme/russ", FilePath: "epics/2026-07-16-frob.md",
		Host: "buncher", Branch: "epic/frob", TmuxName: "epic-frob", Agent: "codex",
	}, now); err != nil {
		t.Fatalf("add epic: %v", err)
	}

	// runtime facts (a supervision pass).
	rs := store.EpicRuntimeState{ContextPct: 62.5, PaneState: "working", AuthState: "ok", LastCommitAt: now.Format(time.RFC3339Nano)}
	if err := st.SetEpicRuntimeState(ctx, "frob", rs, now); err != nil {
		t.Fatalf("runtime: %v", err)
	}
	// seat binding (the launch gate).
	if err := st.SetEpicSeatBinding(ctx, "frob", "acc-1", "buncher|/home/ops/.codex", "codex", now); err != nil {
		t.Fatalf("binding: %v", err)
	}
	if err := st.SetEpicExplainerPath(ctx, "frob", "epics/frob-explainer.html", now); err != nil {
		t.Fatalf("explainer: %v", err)
	}

	e, err := st.GetEpicRun(ctx, "frob")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.ContextPct != 62.5 || e.PaneState != "working" || e.AuthState != "ok" {
		t.Fatalf("runtime not persisted: %+v", e)
	}
	if e.AccountKey != "acc-1" || e.SeatID != "buncher|/home/ops/.codex" || e.BuilderModelFamily != "codex" {
		t.Fatalf("binding not persisted: %+v", e)
	}
	if e.ExplainerPath != "epics/frob-explainer.html" {
		t.Fatalf("explainer not persisted: %+v", e)
	}
	// status ingestion and runtime writes must not clobber each other.
	if e.Branch != "epic/frob" || e.Agent != "codex" {
		t.Fatalf("runtime write clobbered launch fields: %+v", e)
	}
}

func TestEpicRuntimeDefaultsUnknownContext(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "z", Repo: "r", FilePath: "epics/z.md", TmuxName: "epic-z", Agent: "claude"}, now); err != nil {
		t.Fatalf("add: %v", err)
	}
	e, err := st.GetEpicRun(ctx, "z")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// a fresh epic's context_pct defaults to the -1 unknown sentinel, not 0.
	if e.ContextPct != store.ContextPctUnknown {
		t.Fatalf("expected unknown context (-1), got %v", e.ContextPct)
	}
}

func TestSetEpicRuntimeStateMissing(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	if err := st.SetEpicRuntimeState(ctx, "ghost", store.EpicRuntimeState{}, time.Now()); !errors.Is(err, store.ErrEpicRunNotFound) {
		t.Fatalf("expected ErrEpicRunNotFound, got %v", err)
	}
}
