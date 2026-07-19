package multirepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	gh "github.com/samhotchkiss/flowbee/internal/github"
)

// RepositoryProbeFacts is a bounded, content-free mechanical projection of one
// exact repository BoardSweep. Fingerprint binds the complete projection below;
// it can be written to an append-only recovery ledger without retaining issue or
// PR prose. RepoID is the stable repos.id join, never owner/name inference.
type RepositoryProbeFacts struct {
	RepoID              string
	Fingerprint         string
	PullRequests        int
	Issues              int
	GreenPullRequests   int
	PendingPullRequests int
	FailedPullRequests  int
	TruncatedChecks     int
	RateLimit           int
	RateRemaining       int
}

type repositoryProbePR struct {
	Number    int
	HeadSHA   string
	BaseSHA   string
	CI        gh.CIState
	RealGreen bool
	Truncated bool
	Merged    bool
}

type repositoryProbeProjection struct {
	RepoID        string
	PullRequests  []repositoryProbePR
	IssueNumbers  []int
	RateLimit     int
	RateRemaining int
}

// ReadRepositoryProbe performs exactly one read-only BoardSweep through the
// GitHub client already bound to repoID. Unknown ids fail closed; there is no
// default-repository fallback and the GitHub Writer is never reachable here.
func (m *Manager) ReadRepositoryProbe(ctx context.Context, repoID string) (RepositoryProbeFacts, error) {
	loop, ok := m.loops[repoID]
	if !ok {
		return RepositoryProbeFacts{}, fmt.Errorf("unknown repository probe id %q", repoID)
	}
	snapshot, err := loop.client.BoardSweep(ctx)
	if err != nil {
		return RepositoryProbeFacts{}, fmt.Errorf("read repository %q dependency facts: %w", repoID, err)
	}
	projection := repositoryProbeProjection{
		RepoID: repoID, RateLimit: snapshot.RateLimit.Limit, RateRemaining: snapshot.RateLimit.Remaining,
	}
	facts := RepositoryProbeFacts{
		RepoID: repoID, PullRequests: len(snapshot.PullRequests), Issues: len(snapshot.Issues),
		RateLimit: snapshot.RateLimit.Limit, RateRemaining: snapshot.RateLimit.Remaining,
	}
	for _, pr := range snapshot.PullRequests {
		projection.PullRequests = append(projection.PullRequests, repositoryProbePR{
			Number: pr.Number, HeadSHA: pr.HeadRefOid, BaseSHA: pr.BaseRefOid, CI: pr.CIRollup,
			RealGreen: pr.CIHasRealSuccess, Truncated: pr.CheckContextsTruncated, Merged: pr.Merged,
		})
		switch {
		case pr.CIRollup == gh.CISuccess && pr.CIHasRealSuccess && !pr.CheckContextsTruncated:
			facts.GreenPullRequests++
		case pr.CIRollup == gh.CIFailure || pr.CIRollup == gh.CIError:
			facts.FailedPullRequests++
		default:
			facts.PendingPullRequests++
		}
		if pr.CheckContextsTruncated {
			facts.TruncatedChecks++
		}
	}
	for _, issue := range snapshot.Issues {
		projection.IssueNumbers = append(projection.IssueNumbers, issue.Number)
	}
	sort.Slice(projection.PullRequests, func(i, j int) bool {
		return projection.PullRequests[i].Number < projection.PullRequests[j].Number
	})
	sort.Ints(projection.IssueNumbers)
	payload, err := json.Marshal(projection)
	if err != nil {
		return RepositoryProbeFacts{}, fmt.Errorf("encode repository %q dependency facts: %w", repoID, err)
	}
	sum := sha256.Sum256(payload)
	facts.Fingerprint = hex.EncodeToString(sum[:])
	return facts, nil
}
