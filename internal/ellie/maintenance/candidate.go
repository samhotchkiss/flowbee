package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

type SweepType string

const (
	SweepContradiction SweepType = "contradiction"
	SweepDedup         SweepType = "dedup"
	SweepReground      SweepType = "reground"
	SweepReflection    SweepType = "reflection"
)

type CandidateKind string

const (
	CandidatePair    CandidateKind = "pair"
	CandidateCluster CandidateKind = "cluster"
	CandidateMemory  CandidateKind = "memory"
)

type ResultStatus string

const (
	ResultSuccess       ResultStatus = "success"
	ResultNoOp          ResultStatus = "no_op"
	ResultRefusal       ResultStatus = "refusal"
	ResultNonActionable ResultStatus = "non_actionable"
	ResultFailed        ResultStatus = "failed"
	ResultCanceled      ResultStatus = "canceled"
	ResultTimedOut      ResultStatus = "timed_out"
)

type Member struct {
	ID          string `json:"id"`
	ContentHash string `json:"content_hash"`
}

type Candidate struct {
	Kind    CandidateKind `json:"kind"`
	Key     string        `json:"key"`
	Members []Member      `json:"members"`
}

func NewCandidate(kind CandidateKind, members []Member) (Candidate, error) {
	if err := validKind(kind); err != nil {
		return Candidate{}, err
	}
	if len(members) == 0 {
		return Candidate{}, errors.New("maintenance candidate requires at least one member")
	}
	if kind == CandidatePair && len(members) != 2 {
		return Candidate{}, fmt.Errorf("pair candidate requires 2 members, got %d", len(members))
	}
	if kind == CandidateMemory && len(members) != 1 {
		return Candidate{}, fmt.Errorf("memory candidate requires 1 member, got %d", len(members))
	}
	canon, err := CanonicalMembers(members)
	if err != nil {
		return Candidate{}, err
	}
	key, err := CandidateKey(kind, canon)
	if err != nil {
		return Candidate{}, err
	}
	return Candidate{Kind: kind, Key: key, Members: canon}, nil
}

func NormalizeCandidate(candidate Candidate) (Candidate, error) {
	if err := validKind(candidate.Kind); err != nil {
		return Candidate{}, err
	}
	normalized, err := NewCandidate(candidate.Kind, candidate.Members)
	if err != nil {
		return Candidate{}, err
	}
	if candidate.Key != "" && candidate.Key != normalized.Key {
		return Candidate{}, fmt.Errorf("maintenance candidate key %q does not match canonical key %q", candidate.Key, normalized.Key)
	}
	return normalized, nil
}

func Pair(a, b Member) (Candidate, error) {
	return NewCandidate(CandidatePair, []Member{a, b})
}

func Cluster(members ...Member) (Candidate, error) {
	return NewCandidate(CandidateCluster, members)
}

func Memory(member Member) (Candidate, error) {
	return NewCandidate(CandidateMemory, []Member{member})
}

func CanonicalMembers(members []Member) ([]Member, error) {
	out := append([]Member(nil), members...)
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	seen := make(map[string]bool, len(out))
	for _, m := range out {
		if m.ID == "" {
			return nil, errors.New("maintenance candidate member id is required")
		}
		if m.ContentHash == "" {
			return nil, fmt.Errorf("maintenance candidate member %q content_hash is required", m.ID)
		}
		if seen[m.ID] {
			return nil, fmt.Errorf("maintenance candidate member %q appears more than once", m.ID)
		}
		seen[m.ID] = true
	}
	return out, nil
}

func CandidateKey(kind CandidateKind, members []Member) (string, error) {
	if err := validKind(kind); err != nil {
		return "", err
	}
	canon, err := CanonicalMembers(members)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(canon))
	for _, m := range canon {
		ids = append(ids, m.ID)
	}
	blob, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(kind) + ":" + string(blob), nil
}

func ContentHashesMatch(candidate Candidate, completed []Member) bool {
	canon, err := CanonicalMembers(completed)
	if err != nil {
		return false
	}
	if len(candidate.Members) != len(canon) {
		return false
	}
	for i, m := range candidate.Members {
		if m.ID != canon[i].ID || m.ContentHash != canon[i].ContentHash {
			return false
		}
	}
	return true
}

func IsCompletedStatus(status ResultStatus) bool {
	switch status {
	case ResultSuccess, ResultNoOp, ResultRefusal, ResultNonActionable:
		return true
	default:
		return false
	}
}

func ValidSweepType(sweep SweepType) bool {
	switch sweep {
	case SweepContradiction, SweepDedup, SweepReground, SweepReflection:
		return true
	default:
		return false
	}
}

func validKind(kind CandidateKind) error {
	switch kind {
	case CandidatePair, CandidateCluster, CandidateMemory:
		return nil
	default:
		return fmt.Errorf("unknown maintenance candidate kind %q", kind)
	}
}

type CompletedChecker interface {
	MaintenanceCheckCompleted(ctx context.Context, storeID string, sweep SweepType, candidate Candidate) (bool, error)
}

type GateStats struct {
	SweepType           SweepType
	CandidatesGenerated int
	SkippedUnchanged    int
	SentToLLM           int
	LLMCalls            int
	CompletedPersisted  int
	FailedNotPersisted  int
}

func FilterEligible(ctx context.Context, checker CompletedChecker, storeID string, sweep SweepType, candidates []Candidate) ([]Candidate, GateStats, error) {
	if !ValidSweepType(sweep) {
		return nil, GateStats{}, fmt.Errorf("unknown maintenance sweep type %q", sweep)
	}
	stats := GateStats{SweepType: sweep, CandidatesGenerated: len(candidates)}
	eligible := make([]Candidate, 0, len(candidates))
	for _, c := range candidates {
		candidate, err := NormalizeCandidate(c)
		if err != nil {
			return nil, stats, err
		}
		done, err := checker.MaintenanceCheckCompleted(ctx, storeID, sweep, candidate)
		if err != nil {
			return nil, stats, err
		}
		if done {
			stats.SkippedUnchanged++
			continue
		}
		eligible = append(eligible, candidate)
	}
	stats.SentToLLM = len(eligible)
	return eligible, stats, nil
}

type CheckRecord struct {
	StoreID      string
	SweepType    SweepType
	Candidate    Candidate
	ResultStatus ResultStatus
	CheckedAt    time.Time
	SweepRunID   string
}

type CompletedRecorder interface {
	RecordMaintenanceCheck(ctx context.Context, check CheckRecord) (bool, error)
}

type CompletedLedger interface {
	CompletedChecker
	CompletedRecorder
}

type JudgeFunc func(ctx context.Context, candidate Candidate) (ResultStatus, error)

type RunOptions struct {
	StoreID    string
	SweepType  SweepType
	Candidates []Candidate
	Judge      JudgeFunc
	Now        func() time.Time
	SweepRunID string
}

func RunLLMSweep(ctx context.Context, ledger CompletedLedger, opts RunOptions) (GateStats, error) {
	if ledger == nil {
		return GateStats{}, errors.New("maintenance sweep ledger is required")
	}
	if opts.StoreID == "" {
		return GateStats{}, errors.New("maintenance sweep store_id is required")
	}
	if !ValidSweepType(opts.SweepType) {
		return GateStats{}, fmt.Errorf("unknown maintenance sweep type %q", opts.SweepType)
	}
	if opts.Judge == nil {
		return GateStats{}, errors.New("maintenance sweep judge is required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	eligible, stats, err := FilterEligible(ctx, ledger, opts.StoreID, opts.SweepType, opts.Candidates)
	if err != nil {
		return stats, err
	}
	for _, candidate := range eligible {
		stats.LLMCalls++
		status, err := opts.Judge(ctx, candidate)
		if err != nil {
			stats.FailedNotPersisted++
			return stats, fmt.Errorf("%s maintenance judge for %s: %w", opts.SweepType, candidate.Key, err)
		}
		if !IsCompletedStatus(status) {
			stats.FailedNotPersisted++
			continue
		}
		persisted, err := ledger.RecordMaintenanceCheck(ctx, CheckRecord{
			StoreID:      opts.StoreID,
			SweepType:    opts.SweepType,
			Candidate:    candidate,
			ResultStatus: status,
			CheckedAt:    now(),
			SweepRunID:   opts.SweepRunID,
		})
		if err != nil {
			return stats, fmt.Errorf("record %s maintenance check for %s: %w", opts.SweepType, candidate.Key, err)
		}
		if persisted {
			stats.CompletedPersisted++
		} else {
			stats.FailedNotPersisted++
		}
	}
	return stats, nil
}

func RequireCandidateKinds(sweep SweepType, candidates []Candidate, allowed ...CandidateKind) error {
	ok := make(map[CandidateKind]bool, len(allowed))
	for _, kind := range allowed {
		if err := validKind(kind); err != nil {
			return err
		}
		ok[kind] = true
	}
	for _, candidate := range candidates {
		if !ok[candidate.Kind] {
			return fmt.Errorf("%s sweep candidate %s has unsupported kind %q", sweep, candidate.Key, candidate.Kind)
		}
	}
	return nil
}
