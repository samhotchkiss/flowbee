package contradiction

import (
	"context"
	"errors"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ellie/maintenance"
)

type Options struct {
	StoreID         string
	CandidateSource maintenance.CandidateSource
	Judge           maintenance.JudgeFunc
	Now             time.Time
	SweepRunID      string
}

func Run(ctx context.Context, ledger maintenance.CompletedLedger, opts Options) (maintenance.GateStats, error) {
	if opts.CandidateSource == nil {
		return maintenance.GateStats{}, errors.New("contradiction sweep candidate source is required")
	}
	return maintenance.RunLLMSweep(ctx, ledger, maintenance.RunOptions{
		StoreID:         opts.StoreID,
		SweepType:       maintenance.SweepContradiction,
		CandidateSource: requirePairs(opts.CandidateSource),
		Judge:           opts.Judge,
		Now:             fixedNow(opts.Now),
		SweepRunID:      opts.SweepRunID,
	})
}

func Sweep(ctx context.Context, ledger maintenance.CompletedLedger, storeID string, candidates []maintenance.Candidate, judge maintenance.JudgeFunc, now time.Time, sweepRunID string) (maintenance.GateStats, error) {
	return Run(ctx, ledger, Options{
		StoreID:         storeID,
		CandidateSource: maintenance.StaticCandidateSource(candidates),
		Judge:           judge,
		Now:             now,
		SweepRunID:      sweepRunID,
	})
}

func requirePairs(source maintenance.CandidateSource) maintenance.CandidateSource {
	return func(ctx context.Context) ([]maintenance.Candidate, error) {
		candidates, err := source(ctx)
		if err != nil {
			return nil, err
		}
		if err := maintenance.RequireCandidateKinds(maintenance.SweepContradiction, candidates, maintenance.CandidatePair); err != nil {
			return nil, err
		}
		return candidates, nil
	}
}

func GateCandidates(ctx context.Context, ledger maintenance.CompletedLedger, storeID string, candidates []maintenance.Candidate) ([]maintenance.Candidate, maintenance.GateStats, error) {
	if err := maintenance.RequireCandidateKinds(maintenance.SweepContradiction, candidates, maintenance.CandidatePair); err != nil {
		return nil, maintenance.GateStats{}, err
	}
	return maintenance.FilterEligible(ctx, ledger, storeID, maintenance.SweepContradiction, candidates)
}

func fixedNow(t time.Time) func() time.Time {
	if t.IsZero() {
		return nil
	}
	return func() time.Time { return t }
}
