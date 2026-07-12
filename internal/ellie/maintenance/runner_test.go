package maintenance

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunLLMSweepDoesNotPersistFailedCanceledOrTimedOut(t *testing.T) {
	ctx := context.Background()
	ledger := &memoryLedger{completed: map[string]Candidate{}}
	candidate := mustMemory(t, Member{ID: "m1", ContentHash: "h1"})
	statuses := []ResultStatus{ResultFailed, ResultCanceled, ResultTimedOut}

	for _, status := range statuses {
		stats, err := RunLLMSweep(ctx, ledger, RunOptions{
			StoreID:    "tenant-1",
			SweepType:  SweepReground,
			Candidates: []Candidate{candidate},
			Judge: func(context.Context, Candidate) (ResultStatus, error) {
				return status, nil
			},
			Now: func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
		})
		if err != nil {
			t.Fatalf("run %s: %v", status, err)
		}
		if stats.CompletedPersisted != 0 || stats.FailedNotPersisted != 1 {
			t.Fatalf("stats for %s=%+v, want failed-not-persisted only", status, stats)
		}
		done, err := ledger.MaintenanceCheckCompleted(ctx, "tenant-1", SweepReground, candidate)
		if err != nil {
			t.Fatalf("completed %s: %v", status, err)
		}
		if done {
			t.Fatalf("%s should not close the candidate ledger", status)
		}
	}
}

func TestRunLLMSweepDoesNotPersistJudgeErrors(t *testing.T) {
	ctx := context.Background()
	ledger := &memoryLedger{completed: map[string]Candidate{}}
	candidate := mustMemory(t, Member{ID: "m1", ContentHash: "h1"})

	stats, err := RunLLMSweep(ctx, ledger, RunOptions{
		StoreID:    "tenant-1",
		SweepType:  SweepReflection,
		Candidates: []Candidate{candidate},
		Judge: func(context.Context, Candidate) (ResultStatus, error) {
			return "", errors.New("llm unavailable")
		},
		Now: func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err == nil {
		t.Fatal("judge error should be returned")
	}
	if stats.LLMCalls != 1 || stats.CompletedPersisted != 0 || stats.FailedNotPersisted != 1 {
		t.Fatalf("stats=%+v, want one failed unpersisted LLM call", stats)
	}
	done, err := ledger.MaintenanceCheckCompleted(ctx, "tenant-1", SweepReflection, candidate)
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if done {
		t.Fatal("judge errors must not persist completed checks")
	}
}

func TestRunLLMSweepNormalizesCandidateBeforeJudgeAndLedger(t *testing.T) {
	ctx := context.Background()
	ledger := &memoryLedger{completed: map[string]Candidate{}}
	raw := Candidate{
		Kind: CandidatePair,
		Members: []Member{
			{ID: "b", ContentHash: "hb"},
			{ID: "a", ContentHash: "ha"},
		},
	}

	stats, err := RunLLMSweep(ctx, ledger, RunOptions{
		StoreID:    "tenant-1",
		SweepType:  SweepContradiction,
		Candidates: []Candidate{raw},
		Judge: func(_ context.Context, candidate Candidate) (ResultStatus, error) {
			if candidate.Key != `pair:["a","b"]` {
				t.Fatalf("judge saw non-canonical key %q", candidate.Key)
			}
			if candidate.Members[0].ID != "a" || candidate.Members[1].ID != "b" {
				t.Fatalf("judge saw non-canonical members %+v", candidate.Members)
			}
			return ResultSuccess, nil
		},
		Now: func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.LLMCalls != 1 || stats.CompletedPersisted != 1 {
		t.Fatalf("stats=%+v, want one judged and persisted candidate", stats)
	}
	done, err := ledger.MaintenanceCheckCompleted(ctx, "tenant-1", SweepContradiction, mustPair(t, Member{ID: "a", ContentHash: "ha"}, Member{ID: "b", ContentHash: "hb"}))
	if err != nil {
		t.Fatalf("completed: %v", err)
	}
	if !done {
		t.Fatal("canonical candidate should be completed after raw input was judged")
	}
}

type memoryLedger struct {
	completed map[string]Candidate
}

func (m *memoryLedger) MaintenanceCheckCompleted(_ context.Context, _ string, _ SweepType, candidate Candidate) (bool, error) {
	done, ok := m.completed[candidate.Key]
	return ok && ContentHashesMatch(candidate, done.Members), nil
}

func (m *memoryLedger) RecordMaintenanceCheck(_ context.Context, check CheckRecord) (bool, error) {
	if !IsCompletedStatus(check.ResultStatus) {
		return false, nil
	}
	m.completed[check.Candidate.Key] = check.Candidate
	return true, nil
}

func mustMemory(t *testing.T, member Member) Candidate {
	t.Helper()
	c, err := Memory(member)
	return mustCandidate(t, c, err)
}
