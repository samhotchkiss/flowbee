package projectbreaker

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/multirepo"
)

// ProjectRepos is the durable project-to-repository join used by a
// project-scoped breaker. The returned ids must be stable repos.id values.
type ProjectRepos interface {
	ProjectRepoIDs(context.Context, string, bool) ([]string, error)
}

// RepositoryFacts is the read-only multi-repo/GitHub boundary. It intentionally
// exposes no Writer method and therefore cannot mutate GitHub, Driver, or tmux.
type RepositoryFacts interface {
	ReadRepositoryProbe(context.Context, string) (multirepo.RepositoryProbeFacts, error)
}

// RepositoryActionFacts is the exact repo-scoped durable project-out surface.
// It proves action recovery from Flowbee's outbox ledger, never from a generic
// GitHub read or an agent/session assertion.
type RepositoryActionFacts interface {
	ReadRepositoryActionProbe(context.Context, string) (unresolvedFailures, maxAttempts int, fingerprint string, err error)
}

// MechanicalDependencyProbe turns repository-scoped GitHub reads into the
// evidence contract Runner persists. Project-scoped probes require every active
// repository to pass the same mechanical predicate; one broken repository keeps
// only that project open.
type MechanicalDependencyProbe struct {
	Projects     ProjectRepos
	Repositories RepositoryFacts
	Actions      RepositoryActionFacts
	Now          func() time.Time
	RetryAfter   time.Duration
}

func (p MechanicalDependencyProbe) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

func (p MechanicalDependencyProbe) retryAfter() time.Duration {
	if p.RetryAfter > 0 {
		return p.RetryAfter
	}
	return time.Minute
}

func (p MechanicalDependencyProbe) Probe(ctx context.Context, req ProbeRequest) (ProbeResult, error) {
	if p.Projects == nil || p.Repositories == nil || strings.TrimSpace(req.ProjectID) == "" {
		return ProbeResult{}, errors.New("mechanical dependency probe is not configured")
	}
	repoIDs := []string{strings.TrimSpace(req.RepoID)}
	if repoIDs[0] == "" {
		var err error
		repoIDs, err = p.Projects.ProjectRepoIDs(ctx, req.ProjectID, true)
		if err != nil {
			return ProbeResult{}, fmt.Errorf("list active repositories for project %q: %w", req.ProjectID, err)
		}
	}
	sort.Strings(repoIDs)
	if len(repoIDs) == 0 {
		return ProbeResult{FailureReason: "project has no active repository to probe", RetryAfter: p.retryAfter()}, nil
	}

	facts := make([]multirepo.RepositoryProbeFacts, 0, len(repoIDs))
	for _, repoID := range repoIDs {
		if strings.TrimSpace(repoID) == "" {
			return ProbeResult{}, errors.New("project repository join contains an empty repository id")
		}
		fact, err := p.Repositories.ReadRepositoryProbe(ctx, repoID)
		if err != nil {
			// A dependency read failure is an expected still-down observation, not
			// poison in the runner. The exact scope is reopened promptly and the
			// next project/repository claim remains isolated.
			return ProbeResult{FailureReason: fmt.Sprintf("repository %s dependency read unavailable", repoID), RetryAfter: p.retryAfter()}, nil
		}
		if fact.RepoID != repoID || fact.Fingerprint == "" {
			return ProbeResult{}, fmt.Errorf("repository %q returned malformed or cross-scoped facts", repoID)
		}
		facts = append(facts, fact)
	}

	if strings.TrimSpace(req.FailureKind) == "action_failure" {
		if p.Actions == nil {
			return ProbeResult{FailureReason: "project-out recovery probe is not configured", RetryAfter: p.retryAfter()}, nil
		}
		actionRefs := make([]string, 0, len(repoIDs))
		for _, repoID := range repoIDs {
			unresolved, _, fingerprint, err := p.Actions.ReadRepositoryActionProbe(ctx, repoID)
			if err != nil {
				return ProbeResult{FailureReason: fmt.Sprintf("repository %s project-out ledger unavailable", repoID), RetryAfter: p.retryAfter()}, nil
			}
			if unresolved != 0 || fingerprint == "" {
				return ProbeResult{FailureReason: fmt.Sprintf("repository %s still has %d unresolved project-out action(s)", repoID, unresolved), RetryAfter: p.retryAfter()}, nil
			}
			actionRefs = append(actionRefs, repoID+"@sha256:"+fingerprint)
		}
		return ProbeResult{Recovered: true, EvidenceKind: "project_outbox_health",
			EvidenceRef: "project:" + req.ProjectID + "/" + strings.Join(actionRefs, ","), ObservedAt: p.now()}, nil
	}

	recovered, reason := dependencyRecovered(req.FailureKind, facts)
	if !recovered {
		return ProbeResult{FailureReason: reason, RetryAfter: p.retryAfter()}, nil
	}
	refs := make([]string, 0, len(facts))
	for _, fact := range facts {
		refs = append(refs, fact.RepoID+"@sha256:"+fact.Fingerprint)
	}
	return ProbeResult{
		Recovered: true, EvidenceKind: "github_board_read",
		EvidenceRef: "project:" + req.ProjectID + "/" + strings.Join(refs, ","), ObservedAt: p.now(),
	}, nil
}

func dependencyRecovered(kind string, facts []multirepo.RepositoryProbeFacts) (bool, string) {
	switch strings.TrimSpace(kind) {
	case "github_error":
		// A successful authenticated BoardSweep is direct evidence that the
		// repository's GitHub read dependency has recovered.
		return true, ""
	case "ci_outage":
		for _, fact := range facts {
			if fact.GreenPullRequests < 1 {
				return false, "CI recovery is not yet proven by a real, complete green check set for repository " + fact.RepoID
			}
		}
		return true, ""
	case "merge_incident":
		// Read availability cannot prove that a write/effect seam recovered.
		// Those breakers close only after a later effect-specific mechanical
		// probe is added; fail closed rather than laundering a read receipt.
		return false, "GitHub read is healthy but effect recovery is not mechanically proven"
	case "policy_violation":
		return false, "policy violation requires a policy-specific mechanical recovery fact"
	default:
		return false, "unsupported project breaker failure kind"
	}
}
