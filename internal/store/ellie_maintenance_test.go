package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEllieMaintenanceLedgerSkipsSameHashesAndReopensChangedHashes(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ab := mustMaintPair(t,
		maintenance.Member{ID: "a", ContentHash: "ha"},
		maintenance.Member{ID: "b", ContentHash: "hb"},
	)

	persisted, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
		StoreID: "tenant-1", SweepType: maintenance.SweepContradiction,
		Candidate: ab, ResultStatus: maintenance.ResultSuccess, CheckedAt: at,
		SweepRunID: "sweep-1",
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !persisted {
		t.Fatal("completed success check should be persisted")
	}
	done, err := st.MaintenanceCheckCompleted(ctx, "tenant-1", maintenance.SweepContradiction, ab)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if !done {
		t.Fatal("same pair with same hashes should be skipped")
	}

	changed := mustMaintPair(t,
		maintenance.Member{ID: "a", ContentHash: "ha"},
		maintenance.Member{ID: "b", ContentHash: "hb2"},
	)
	done, err = st.MaintenanceCheckCompleted(ctx, "tenant-1", maintenance.SweepContradiction, changed)
	if err != nil {
		t.Fatalf("completed changed: %v", err)
	}
	if done {
		t.Fatal("changed hash should be eligible")
	}
}

func TestEllieMaintenanceLedgerTreatsRefusalAndNoOpAsCompleted(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	candidates := []maintenance.Candidate{
		mustMaintPair(t,
			maintenance.Member{ID: "a", ContentHash: "ha"},
			maintenance.Member{ID: "b", ContentHash: "hb"},
		),
		mustMaintCluster(t,
			maintenance.Member{ID: "a", ContentHash: "ha"},
			maintenance.Member{ID: "b", ContentHash: "hb"},
			maintenance.Member{ID: "c", ContentHash: "hc"},
		),
	}
	statuses := []maintenance.ResultStatus{maintenance.ResultRefusal, maintenance.ResultNoOp}

	for i, c := range candidates {
		persisted, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
			StoreID: "tenant-1", SweepType: maintenance.SweepDedup,
			Candidate: c, ResultStatus: statuses[i], CheckedAt: at.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if !persisted {
			t.Fatalf("status %s should be persisted", statuses[i])
		}
		done, err := st.MaintenanceCheckCompleted(ctx, "tenant-1", maintenance.SweepDedup, c)
		if err != nil {
			t.Fatalf("completed %d: %v", i, err)
		}
		if !done {
			t.Fatalf("status %s should skip unchanged dedup candidate", statuses[i])
		}
	}
}

func TestEllieMaintenanceLedgerDoesNotPersistFailedCanceledOrTimedOut(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	c := mustMaintMemory(t, maintenance.Member{ID: "m1", ContentHash: "h1"})

	for _, status := range []maintenance.ResultStatus{maintenance.ResultFailed, maintenance.ResultCanceled, maintenance.ResultTimedOut} {
		persisted, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
			StoreID: "tenant-1", SweepType: maintenance.SweepReground,
			Candidate: c, ResultStatus: status, CheckedAt: at,
		})
		if err != nil {
			t.Fatalf("record %s: %v", status, err)
		}
		if persisted {
			t.Fatalf("%s should not be persisted as a completed check", status)
		}
	}

	var n int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM ellie_maintenance_checks`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("failed/canceled/timed-out checks persisted %d rows, want 0", n)
	}
}

func TestEllieMaintenanceZeroWriteSecondPassSendsNoLLMWork(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	candidates := []maintenance.Candidate{
		mustMaintPair(t,
			maintenance.Member{ID: "a", ContentHash: "ha"},
			maintenance.Member{ID: "b", ContentHash: "hb"},
		),
		mustMaintCluster(t,
			maintenance.Member{ID: "a", ContentHash: "ha"},
			maintenance.Member{ID: "b", ContentHash: "hb"},
			maintenance.Member{ID: "c", ContentHash: "hc"},
		),
		mustMaintMemory(t, maintenance.Member{ID: "a", ContentHash: "ha"}),
	}
	sweeps := []maintenance.SweepType{
		maintenance.SweepContradiction,
		maintenance.SweepDedup,
		maintenance.SweepReground,
		maintenance.SweepReflection,
	}

	for _, sweep := range sweeps {
		for _, c := range candidates {
			if sweep == maintenance.SweepContradiction && c.Kind != maintenance.CandidatePair {
				continue
			}
			if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
				StoreID: "tenant-1", SweepType: sweep, Candidate: c,
				ResultStatus: maintenance.ResultNonActionable, CheckedAt: at,
			}); err != nil {
				t.Fatalf("record %s/%s: %v", sweep, c.Kind, err)
			}
		}
	}

	totalLLM := 0
	for _, sweep := range sweeps {
		eligible, stats, err := maintenance.FilterEligible(ctx, st, "tenant-1", sweep, candidatesForSweep(sweep, candidates))
		if err != nil {
			t.Fatalf("filter %s: %v", sweep, err)
		}
		if len(eligible) != 0 || stats.SentToLLM != 0 {
			t.Fatalf("%s sent unchanged work to LLM: eligible=%v stats=%+v", sweep, eligible, stats)
		}
		totalLLM += stats.SentToLLM
	}
	if totalLLM != 0 {
		t.Fatalf("zero-write second pass sent %d LLM candidates, want 0", totalLLM)
	}
}

func TestEllieMaintenanceChangedMemberOnlyReopensAffectedCandidates(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	at := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	ab := mustMaintPair(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "b", ContentHash: "hb"})
	ac := mustMaintPair(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "c", ContentHash: "hc"})
	bc := mustMaintPair(t, maintenance.Member{ID: "b", ContentHash: "hb"}, maintenance.Member{ID: "c", ContentHash: "hc"})
	for _, c := range []maintenance.Candidate{ab, ac, bc} {
		if _, err := st.RecordEllieMaintenanceCheck(ctx, store.EllieMaintenanceCheck{
			StoreID: "tenant-1", SweepType: maintenance.SweepContradiction, Candidate: c,
			ResultStatus: maintenance.ResultSuccess, CheckedAt: at,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	abChanged := mustMaintPair(t, maintenance.Member{ID: "a", ContentHash: "ha"}, maintenance.Member{ID: "b", ContentHash: "hb2"})
	bcChanged := mustMaintPair(t, maintenance.Member{ID: "b", ContentHash: "hb2"}, maintenance.Member{ID: "c", ContentHash: "hc"})
	eligible, stats, err := maintenance.FilterEligible(ctx, st, "tenant-1", maintenance.SweepContradiction, []maintenance.Candidate{abChanged, ac, bcChanged})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if stats.SkippedUnchanged != 1 || stats.SentToLLM != 2 {
		t.Fatalf("stats=%+v, want one unchanged skip and two LLM candidates", stats)
	}
	got := map[string]bool{}
	for _, c := range eligible {
		got[c.Key] = true
	}
	if !got[abChanged.Key] || !got[bcChanged.Key] || got[ac.Key] {
		t.Fatalf("eligible keys=%v, want only candidates involving changed b", got)
	}
}

func candidatesForSweep(sweep maintenance.SweepType, candidates []maintenance.Candidate) []maintenance.Candidate {
	var out []maintenance.Candidate
	for _, c := range candidates {
		if sweep == maintenance.SweepContradiction && c.Kind != maintenance.CandidatePair {
			continue
		}
		out = append(out, c)
	}
	return out
}

func mustMaintCandidate(t *testing.T, c maintenance.Candidate, err error) maintenance.Candidate {
	t.Helper()
	if err != nil {
		t.Fatalf("candidate: %v", err)
	}
	return c
}

func mustMaintPair(t *testing.T, a, b maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Pair(a, b)
	return mustMaintCandidate(t, c, err)
}

func mustMaintCluster(t *testing.T, members ...maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Cluster(members...)
	return mustMaintCandidate(t, c, err)
}

func mustMaintMemory(t *testing.T, member maintenance.Member) maintenance.Candidate {
	t.Helper()
	c, err := maintenance.Memory(member)
	return mustMaintCandidate(t, c, err)
}
