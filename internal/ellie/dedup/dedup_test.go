package dedup

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestRunGatesUnchangedRefusalAndNoOpBeforeJudge(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	pair := mustPair(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "b", ContentHash: "hb"})
	cluster := mustCluster(t,
		maintenance.Member{ID: "a", ContentHash: "ha"},
		maintenance.Member{ID: "b", ContentHash: "hb"},
		maintenance.Member{ID: "c", ContentHash: "hc"},
	)
	for _, row := range []struct {
		candidate maintenance.Candidate
		status    maintenance.ResultStatus
	}{
		{pair, maintenance.ResultRefusal},
		{cluster, maintenance.ResultNoOp},
	} {
		if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
			StoreID:      "tenant-1",
			SweepType:    maintenance.SweepDedup,
			Candidate:    row.candidate,
			ResultStatus: row.status,
			CheckedAt:    at,
		}); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}

	stats, err := Run(ctx, st, Options{
		StoreID: "tenant-1",
		CandidateSource: func(context.Context) ([]maintenance.Candidate, error) {
			return []maintenance.Candidate{pair, cluster}, nil
		},
		Judge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
			t.Fatal("judge must not be called for unchanged completed dedup candidate")
			return maintenance.ResultSuccess, nil
		},
		Now: at,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.SkippedUnchanged != 2 || stats.LLMCalls != 0 || stats.SentToLLM != 0 {
		t.Fatalf("stats=%+v, want two skipped unchanged and zero LLM calls", stats)
	}
}

func TestRunRechecksClusterAfterMemberHashChanges(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	oldCluster := mustCluster(t,
		maintenance.Member{ID: "a", ContentHash: "ha"},
		maintenance.Member{ID: "b", ContentHash: "hb"},
	)
	changedCluster := mustCluster(t,
		maintenance.Member{ID: "a", ContentHash: "ha"},
		maintenance.Member{ID: "b", ContentHash: "hb2"},
	)
	if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
		StoreID:      "tenant-1",
		SweepType:    maintenance.SweepDedup,
		Candidate:    oldCluster,
		ResultStatus: maintenance.ResultNoOp,
		CheckedAt:    at,
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}

	calls := 0
	stats, err := Run(ctx, st, Options{
		StoreID: "tenant-1",
		CandidateSource: func(context.Context) ([]maintenance.Candidate, error) {
			return []maintenance.Candidate{changedCluster}, nil
		},
		Judge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
			calls++
			return maintenance.ResultNoOp, nil
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

func mustPair(t *testing.T, a, b maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Pair(a, b)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return c
}

func mustCluster(t *testing.T, members ...maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Cluster(members...)
	if err != nil {
		t.Fatalf("cluster: %v", err)
	}
	return c
}
