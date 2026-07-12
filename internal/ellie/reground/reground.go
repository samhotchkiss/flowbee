package reground

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
		return maintenance.GateStats{}, errors.New("reground sweep candidate source is required")
	}
	return maintenance.RunLLMSweep(ctx, ledger, maintenance.RunOptions{
		StoreID:         opts.StoreID,
		SweepType:       maintenance.SweepReground,
		CandidateSource: requireRegroundCandidates(opts.CandidateSource),
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

func requireRegroundCandidates(source maintenance.CandidateSource) maintenance.CandidateSource {
	return func(ctx context.Context) ([]maintenance.Candidate, error) {
		candidates, err := source(ctx)
		if err != nil {
			return nil, err
		}
		if err := maintenance.RequireCandidateKinds(maintenance.SweepReground, candidates, maintenance.CandidateMemory, maintenance.CandidateCluster); err != nil {
			return nil, err
		}
		return candidates, nil
	}
}

func GateCandidates(ctx context.Context, ledger maintenance.CompletedLedger, storeID string, candidates []maintenance.Candidate) ([]maintenance.Candidate, maintenance.GateStats, error) {
	if err := maintenance.RequireCandidateKinds(maintenance.SweepReground, candidates, maintenance.CandidateMemory, maintenance.CandidateCluster); err != nil {
		return nil, maintenance.GateStats{}, err
	}
	return maintenance.FilterEligible(ctx, ledger, storeID, maintenance.SweepReground, candidates)
}

func fixedNow(t time.Time) func() time.Time {
	if t.IsZero() {
		return nil
	}
	return func() time.Time { return t }
}
