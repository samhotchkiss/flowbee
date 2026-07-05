package contradiction

import (
	"context"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
)

func Sweep(ctx context.Context, ledger maintenance.CompletedLedger, storeID string, candidates []maintenance.Candidate, judge maintenance.JudgeFunc, now time.Time, sweepRunID string) (maintenance.GateStats, error) {
	if err := maintenance.RequireCandidateKinds(maintenance.SweepContradiction, candidates, maintenance.CandidatePair); err != nil {
		return maintenance.GateStats{}, err
	}
	return maintenance.RunLLMSweep(ctx, ledger, maintenance.RunOptions{
		StoreID:    storeID,
		SweepType:  maintenance.SweepContradiction,
		Candidates: candidates,
		Judge:      judge,
		Now:        fixedNow(now),
		SweepRunID: sweepRunID,
	})
}

func fixedNow(t time.Time) func() time.Time {
	if t.IsZero() {
		return nil
	}
	return func() time.Time { return t }
}
