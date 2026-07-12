package maintenance_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/contradiction"
	"github.com/samhotchkiss/flowbee/internal/ellie/dedup"
	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
	"github.com/samhotchkiss/flowbee/internal/ellie/reflection"
	"github.com/samhotchkiss/flowbee/internal/ellie/reground"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEllieLLMSweepsUseContentHashLedgerGate(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

	memberA := maintenance.Member{ID: "a", ContentHash: "ha"}
	memberB := maintenance.Member{ID: "b", ContentHash: "hb"}
	memberC := maintenance.Member{ID: "c", ContentHash: "hc"}
	memberAChanged := maintenance.Member{ID: "a", ContentHash: "ha2"}

	pairAB := mustPair(t, memberA, memberB)
	pairAC := mustPair(t, memberA, memberC)
	pairABChanged := mustPair(t, memberAChanged, memberB)
	pairACChanged := mustPair(t, memberAChanged, memberC)
	clusterABC := mustCluster(t, memberA, memberB, memberC)
	clusterABCChanged := mustCluster(t, memberAChanged, memberB, memberC)
	memoryA := mustMemory(t, memberA)
	memoryB := mustMemory(t, memberB)
	memoryAChanged := mustMemory(t, memberAChanged)

	cases := []struct {
		name       string
		sweepType  maintenance.SweepType
		first      []maintenance.Candidate
		changed    []maintenance.Candidate
		firstJudge maintenance.JudgeFunc
		run        func([]maintenance.Candidate, maintenance.JudgeFunc) (maintenance.GateStats, error)
	}{
		{
			name:      "contradiction",
			sweepType: maintenance.SweepContradiction,
			first:     []maintenance.Candidate{pairAB, pairAC},
			changed:   []maintenance.Candidate{pairABChanged, pairACChanged},
			firstJudge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
				return maintenance.ResultSuccess, nil
			},
			run: func(candidates []maintenance.Candidate, judge maintenance.JudgeFunc) (maintenance.GateStats, error) {
				return contradiction.Sweep(ctx, st, "tenant-1", candidates, judge, at, "contradiction-run")
			},
		},
		{
			name:      "dedup",
			sweepType: maintenance.SweepDedup,
			first:     []maintenance.Candidate{pairAB, clusterABC},
			changed:   []maintenance.Candidate{pairABChanged, clusterABCChanged},
			firstJudge: func(_ context.Context, candidate maintenance.Candidate) (maintenance.ResultStatus, error) {
				if candidate.Kind == maintenance.CandidateCluster {
					return maintenance.ResultNoOp, nil
				}
				return maintenance.ResultRefusal, nil
			},
			run: func(candidates []maintenance.Candidate, judge maintenance.JudgeFunc) (maintenance.GateStats, error) {
				return dedup.Sweep(ctx, st, "tenant-1", candidates, judge, at, "dedup-run")
			},
		},
		{
			name:      "reground",
			sweepType: maintenance.SweepReground,
			first:     []maintenance.Candidate{memoryA, memoryB, clusterABC},
			changed:   []maintenance.Candidate{memoryAChanged, memoryB, clusterABCChanged},
			firstJudge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
				return maintenance.ResultNonActionable, nil
			},
			run: func(candidates []maintenance.Candidate, judge maintenance.JudgeFunc) (maintenance.GateStats, error) {
				return reground.Sweep(ctx, st, "tenant-1", candidates, judge, at, "reground-run")
			},
		},
		{
			name:      "reflection",
			sweepType: maintenance.SweepReflection,
			first:     []maintenance.Candidate{memoryA, memoryB, clusterABC},
			changed:   []maintenance.Candidate{memoryAChanged, memoryB, clusterABCChanged},
			firstJudge: func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
				return maintenance.ResultNonActionable, nil
			},
			run: func(candidates []maintenance.Candidate, judge maintenance.JudgeFunc) (maintenance.GateStats, error) {
				return reflection.Sweep(ctx, st, "tenant-1", candidates, judge, at, "reflection-run")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			firstCalls := 0
			firstStats, err := tc.run(tc.first, func(ctx context.Context, candidate maintenance.Candidate) (maintenance.ResultStatus, error) {
				firstCalls++
				return tc.firstJudge(ctx, candidate)
			})
			if err != nil {
				t.Fatalf("first sweep: %v", err)
			}
			if firstCalls != len(tc.first) || firstStats.CompletedPersisted != len(tc.first) {
				t.Fatalf("first calls=%d stats=%+v, want %d completed", firstCalls, firstStats, len(tc.first))
			}

			secondCalls := 0
			secondStats, err := tc.run(tc.first, func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
				secondCalls++
				return maintenance.ResultSuccess, nil
			})
			if err != nil {
				t.Fatalf("second sweep: %v", err)
			}
			if secondCalls != 0 || secondStats.SentToLLM != 0 || secondStats.LLMCalls != 0 {
				t.Fatalf("zero-write second pass called LLM: calls=%d stats=%+v", secondCalls, secondStats)
			}
			if secondStats.SkippedUnchanged != len(tc.first) {
				t.Fatalf("second stats=%+v, want all candidates skipped unchanged", secondStats)
			}

			changedCalls := 0
			changedStats, err := tc.run(tc.changed, func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
				changedCalls++
				return maintenance.ResultSuccess, nil
			})
			if err != nil {
				t.Fatalf("changed sweep: %v", err)
			}
			wantChangedCalls := len(tc.changed)
			if tc.name == "reground" || tc.name == "reflection" {
				wantChangedCalls = 2
			}
			if changedCalls != wantChangedCalls || changedStats.SentToLLM != wantChangedCalls {
				t.Fatalf("changed calls=%d stats=%+v, want %d calls for candidates involving changed memory", changedCalls, changedStats, wantChangedCalls)
			}
		})
	}
}

func TestSweepPackagesDoNotBypassPreLLMGateForSeededLedgers(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	pair := mustPair(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "b", ContentHash: "hb"})
	cluster := mustCluster(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "b", ContentHash: "hb"})
	memory := mustMemory(t, maintenance.Member{ID: "a", ContentHash: "ha"})

	seeded := []struct {
		sweep     maintenance.SweepType
		candidate maintenance.Candidate
		status    maintenance.ResultStatus
	}{
		{maintenance.SweepContradiction, pair, maintenance.ResultSuccess},
		{maintenance.SweepDedup, pair, maintenance.ResultRefusal},
		{maintenance.SweepDedup, cluster, maintenance.ResultNoOp},
		{maintenance.SweepReground, memory, maintenance.ResultNonActionable},
		{maintenance.SweepReflection, memory, maintenance.ResultNonActionable},
	}
	for _, row := range seeded {
		if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
			StoreID:      "tenant-1",
			SweepType:    row.sweep,
			Candidate:    row.candidate,
			ResultStatus: row.status,
			CheckedAt:    at,
		}); err != nil {
			t.Fatalf("seed %s/%s: %v", row.sweep, row.candidate.Key, err)
		}
	}

	failJudge := func(context.Context, maintenance.Candidate) (maintenance.ResultStatus, error) {
		t.Fatal("judge must not be called for unchanged completed candidates")
		return maintenance.ResultSuccess, nil
	}
	if _, err := contradiction.Sweep(ctx, st, "tenant-1", []maintenance.Candidate{pair}, failJudge, at, ""); err != nil {
		t.Fatalf("contradiction: %v", err)
	}
	if _, err := dedup.Sweep(ctx, st, "tenant-1", []maintenance.Candidate{pair, cluster}, failJudge, at, ""); err != nil {
		t.Fatalf("dedup: %v", err)
	}
	if _, err := reground.Sweep(ctx, st, "tenant-1", []maintenance.Candidate{memory}, failJudge, at, ""); err != nil {
		t.Fatalf("reground: %v", err)
	}
	if _, err := reflection.Sweep(ctx, st, "tenant-1", []maintenance.Candidate{memory}, failJudge, at, ""); err != nil {
		t.Fatalf("reflection: %v", err)
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

func mustMemory(t *testing.T, member maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Memory(member)
	if err != nil {
		t.Fatalf("memory: %v", err)
	}
	return c
}
