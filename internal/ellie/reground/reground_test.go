package reground

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestRunGatesUnchangedMemoryBeforeJudge(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	memory := mustMemory(t, maintenance.Member{ID: "a", ContentHash: "ha"})
	if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
		StoreID:      "tenant-1",
		SweepType:    maintenance.SweepReground,
		Candidate:    memory,
		ResultStatus: maintenance.ResultNonActionable,
		CheckedAt:    at,
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	stats, err := Run(ctx, st, Options{
		StoreID: "tenant-1",
		CandidateSource: func(context.Context) ([]maintenance.Candidate, error) {
			return []maintenance.Candidate{memory}, nil
		},
		Judge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
			t.Fatal("judge must not be called for unchanged completed reground candidate")
			return maintenance.ResultSuccess, nil
		},
		Now: at,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.SkippedUnchanged != 1 || stats.LLMCalls != 0 || stats.SentToLLM != 0 {
		t.Fatalf("stats=%+v, want one skipped unchanged and zero LLM calls", stats)
	}
}

func TestRunRechecksMemoryAfterHashChanges(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	oldMemory := mustMemory(t, maintenance.Member{ID: "a", ContentHash: "ha"})
	changedMemory := mustMemory(t, maintenance.Member{ID: "a", ContentHash: "ha2"})
	if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
		StoreID:      "tenant-1",
		SweepType:    maintenance.SweepReground,
		Candidate:    oldMemory,
		ResultStatus: maintenance.ResultNonActionable,
		CheckedAt:    at,
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	calls := 0
	stats, err := Run(ctx, st, Options{
		StoreID: "tenant-1",
		CandidateSource: func(context.Context) ([]maintenance.Candidate, error) {
			return []maintenance.Candidate{changedMemory}, nil
		},
		Judge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
			calls++
			return maintenance.ResultNonActionable, nil
		},
		Now: at.Add(time.Second),
	})
	if err != nil {
		t.Fatalf("run changed: %v", err)
	}
	if calls != 1 || stats.LLMCalls != 1 || stats.CompletedPersisted != 1 {
		t.Fatalf("calls=%d stats=%+v, want changed hash judged and persisted", calls, stats)
	}
}

func mustMemory(t *testing.T, member maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Memory(member)
	if err != nil {
		t.Fatalf("memory: %v", err)
	}
	return c
}
