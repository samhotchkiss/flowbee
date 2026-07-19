// Package projectbreaker executes the mechanical recovery probes for the
// project/repository circuit breakers. The runner owns no scheduling policy:
// the durable store decides which probes are due and fences each claim with an
// owner, epoch, and lease. One RunOnce call is deliberately bounded and
// synchronous so serve can supervise it like every other reconciler.
package projectbreaker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

const reconcilerName = "project_breaker_probe"

// Store is the exact durable surface used by Runner. *store.Store satisfies it.
// Keeping this interface narrow makes crash and poison behavior testable without
// giving the executor authority over unrelated project state.
type Store interface {
	ReconcileDueProjectBreakerProbes(context.Context, string, time.Time, time.Duration, int) ([]store.ProjectBreakerProbe, error)
	GetProjectBreaker(context.Context, string, string) (store.ProjectBreaker, error)
	CompleteProjectBreakerProbe(context.Context, store.ProjectBreakerProbe, bool, store.ProjectBreakerRecoveryFact, string, time.Duration, time.Time) (store.ProjectBreaker, error)
	RecordReconcilerPoisonFact(context.Context, string, string, string, time.Time) error
	ResolveReconcilerPoisonFact(context.Context, string, string, time.Time) error
}

// DependencyProbe is the sole external dependency boundary. Implementations
// perform a read-only, project/repository-scoped mechanical check appropriate to
// FailureKind (for example, a GitHub API or required-check read). They must not
// alter Flowbee workflow state.
type DependencyProbe interface {
	Probe(context.Context, ProbeRequest) (ProbeResult, error)
}

type DependencyProbeFunc func(context.Context, ProbeRequest) (ProbeResult, error)

func (f DependencyProbeFunc) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	return f(ctx, req)
}

type ProbeRequest struct {
	ProjectID     string
	RepoID        string
	FailureKind   string
	FailureReason string
	FailureCount  int
	StateVersion  int
	ProbeEpoch    int
}

// ProbeResult separates a healthy dependency fact from an expected still-down
// observation. Err is reserved for a broken/malformed probe implementation and
// is quarantined as a poison fact. Recovered=true requires all evidence fields.
type ProbeResult struct {
	Recovered     bool
	EvidenceKind  string
	EvidenceRef   string
	ObservedAt    time.Time
	FailureReason string
	RetryAfter    time.Duration
}

type Config struct {
	Owner             string
	ClaimTTL          time.Duration
	FailureRetryAfter time.Duration
	Budget            int
	Now               func() time.Time
}

type ScopeOutcome struct {
	ProjectID string
	RepoID    string
	Epoch     int
	Recovered bool
	Reopened  bool
	Poisoned  bool
	Err       error
}

type Report struct {
	Claimed   int
	Recovered int
	Reopened  int
	Poisoned  int
	Outcomes  []ScopeOutcome
}

type Runner struct {
	Store  Store
	Probe  DependencyProbe
	Config Config
}

func (r Runner) now() time.Time {
	if r.Config.Now != nil {
		return r.Config.Now().UTC()
	}
	return time.Now().UTC()
}

func (r Runner) normalizedConfig() (Config, error) {
	cfg := r.Config
	cfg.Owner = strings.TrimSpace(cfg.Owner)
	if cfg.Owner == "" {
		return cfg, errors.New("project breaker runner requires owner")
	}
	if cfg.ClaimTTL <= 0 {
		cfg.ClaimTTL = time.Minute
	}
	if cfg.FailureRetryAfter <= 0 {
		cfg.FailureRetryAfter = time.Minute
	}
	if cfg.Budget == 0 {
		cfg.Budget = 25
	}
	if cfg.Budget < 1 || cfg.Budget > 1000 {
		return cfg, errors.New("project breaker runner budget must be between 1 and 1000")
	}
	return cfg, nil
}

// RunOnce claims and executes one bounded batch. Only failure to claim the batch
// is returned as a pass-level error. Every claimed scope is isolated: a panic,
// malformed response, or store error is reported/quarantined and processing
// continues with the next scope.
func (r Runner) RunOnce(ctx context.Context) (Report, error) {
	var report Report
	if r.Store == nil || r.Probe == nil {
		return report, errors.New("project breaker runner requires store and dependency probe")
	}
	cfg, err := r.normalizedConfig()
	if err != nil {
		return report, err
	}
	claimedAt := r.now()
	probes, err := r.Store.ReconcileDueProjectBreakerProbes(ctx, cfg.Owner, claimedAt, cfg.ClaimTTL, cfg.Budget)
	if err != nil {
		return report, fmt.Errorf("claim project breaker probes: %w", err)
	}
	report.Claimed = len(probes)
	for _, claim := range probes {
		outcome := r.runClaim(ctx, cfg, claim)
		report.Outcomes = append(report.Outcomes, outcome)
		if outcome.Recovered {
			report.Recovered++
		}
		if outcome.Reopened {
			report.Reopened++
		}
		if outcome.Poisoned {
			report.Poisoned++
		}
	}
	return report, nil
}

func (r Runner) runClaim(ctx context.Context, cfg Config, claim store.ProjectBreakerProbe) (out ScopeOutcome) {
	out = ScopeOutcome{ProjectID: claim.ProjectID, RepoID: claim.RepoID, Epoch: claim.Epoch}
	poisonKey := scopeKey(claim)
	now := r.now()
	breaker, err := r.Store.GetProjectBreaker(ctx, claim.ProjectID, claim.RepoID)
	if err != nil {
		return r.reopenPoison(ctx, cfg, claim, poisonKey, fmt.Errorf("load claimed breaker: %w", err), now)
	}
	request := ProbeRequest{
		ProjectID: claim.ProjectID, RepoID: claim.RepoID,
		FailureKind: breaker.FailureKind, FailureReason: breaker.Reason,
		FailureCount: breaker.FailureCount, StateVersion: claim.StateVersion,
		ProbeEpoch: claim.Epoch,
	}
	result, probeErr := callProbe(ctx, r.Probe, request)
	now = r.now()
	if probeErr != nil {
		return r.reopenPoison(ctx, cfg, claim, poisonKey, probeErr, now)
	}
	if err := validateResult(result, now); err != nil {
		return r.reopenPoison(ctx, cfg, claim, poisonKey, err, now)
	}
	if result.Recovered {
		_, err = r.Store.CompleteProjectBreakerProbe(ctx, claim, true, store.ProjectBreakerRecoveryFact{
			Kind: result.EvidenceKind, EvidenceRef: result.EvidenceRef, ObservedAt: result.ObservedAt,
		}, "", 0, now)
		if err != nil {
			out.Err = fmt.Errorf("complete recovered probe: %w", err)
			out.Poisoned = true
			_ = r.Store.RecordReconcilerPoisonFact(ctx, reconcilerName, poisonKey, out.Err.Error(), now)
			return out
		}
		out.Recovered = true
		_ = r.Store.ResolveReconcilerPoisonFact(ctx, reconcilerName, poisonKey, now)
		return out
	}
	_, err = r.Store.CompleteProjectBreakerProbe(ctx, claim, false, store.ProjectBreakerRecoveryFact{},
		result.FailureReason, result.RetryAfter, now)
	if err != nil {
		out.Err = fmt.Errorf("reopen failed probe: %w", err)
		out.Poisoned = true
		_ = r.Store.RecordReconcilerPoisonFact(ctx, reconcilerName, poisonKey, out.Err.Error(), now)
		return out
	}
	out.Reopened = true
	_ = r.Store.ResolveReconcilerPoisonFact(ctx, reconcilerName, poisonKey, now)
	return out
}

func (r Runner) reopenPoison(ctx context.Context, cfg Config, claim store.ProjectBreakerProbe, key string, cause error, now time.Time) ScopeOutcome {
	out := ScopeOutcome{ProjectID: claim.ProjectID, RepoID: claim.RepoID, Epoch: claim.Epoch, Poisoned: true, Err: cause}
	// Reopen first so the scope does not remain half-open until lease expiry. If
	// fencing rejects completion, the expired-claim path remains the recovery floor.
	_, completeErr := r.Store.CompleteProjectBreakerProbe(ctx, claim, false, store.ProjectBreakerRecoveryFact{},
		"mechanical probe unavailable", cfg.FailureRetryAfter, now)
	if completeErr != nil {
		out.Err = errors.Join(cause, fmt.Errorf("reopen poisoned probe: %w", completeErr))
	} else {
		out.Reopened = true
	}
	_ = r.Store.RecordReconcilerPoisonFact(ctx, reconcilerName, key, out.Err.Error(), now)
	return out
}

func callProbe(ctx context.Context, probe DependencyProbe, req ProbeRequest) (result ProbeResult, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("dependency probe panic: %v", recovered)
		}
	}()
	return probe.Probe(ctx, req)
}

func validateResult(result ProbeResult, now time.Time) error {
	result.EvidenceKind = strings.TrimSpace(result.EvidenceKind)
	result.EvidenceRef = strings.TrimSpace(result.EvidenceRef)
	result.FailureReason = strings.TrimSpace(result.FailureReason)
	if result.Recovered {
		if result.EvidenceKind == "" || result.EvidenceRef == "" || result.ObservedAt.IsZero() || result.ObservedAt.After(now) {
			return errors.New("recovered dependency probe lacks valid mechanical evidence")
		}
		return nil
	}
	if result.FailureReason == "" || result.RetryAfter <= 0 {
		return errors.New("failed dependency probe lacks reason or retry interval")
	}
	return nil
}

func scopeKey(claim store.ProjectBreakerProbe) string {
	repo := claim.RepoID
	if repo == "" {
		repo = "_project"
	}
	return "project:" + claim.ProjectID + "/repo:" + repo
}
