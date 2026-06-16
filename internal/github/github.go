// Package github is Flowbee's SINGLE GitHub caller (R4): the control plane is the
// only DB client and the only process that ever speaks to GitHub. Workers never
// reach this package. It exposes a narrow Client interface over the two loops'
// needs — the batched BoardSweep read (reconcile-IN, §8.1.1) and the rate-limit
// gauge — plus the App installation identity (one ToS-clean bucket, I-14, §8.3).
//
// Two implementations satisfy Client:
//   - RealClient: a genuine GitHub caller (GraphQL over stdlib net/http, bearing
//     the single installation token). It is wired but NEVER exercised in this
//     environment — there are no App creds and the e2e_github smoke is off by
//     default. All tests use Fake.
//   - Fake (fake.go): an in-memory, scriptable stub that records every call. ALL
//     reconcile-IN tests run against it (BUILD.md §6.4).
//
// This package is NOT part of the deterministic core (archcheck forbids the core
// from importing it): it does network I/O and reads a clock for token rotation.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// CIState is the GitHub statusCheckRollup state at a PR head (Domain B, §3.1).
type CIState string

const (
	CISuccess CIState = "SUCCESS"
	CIPending CIState = "PENDING"
	CIFailure CIState = "FAILURE"
	CIError   CIState = "ERROR"
	CINone    CIState = "" // no checks reported
)

// PullRequest is the Domain-B snapshot of one PR from a BoardSweep (§8.1.1). Only
// the GitHub-OWNED facts are carried — Flowbee owns everything else (§3.4).
type PullRequest struct {
	Number      int
	UpdatedAt   time.Time
	IsDraft     bool
	Merged      bool
	MergedAt    time.Time
	HeadRefOid  string // Domain-B: head SHA
	BaseRefOid  string // Domain-B: base SHA
	MergeCommit string // Domain-B: merge commit SHA (terminal fact)
	CIRollup    CIState
	Labels      []string // read only to DETECT drift on Flowbee-owned renderings (§8.1.2)
}

// Issue is the Domain-B snapshot of one open issue from a BoardSweep.
type Issue struct {
	Number    int
	UpdatedAt time.Time
	Labels    []string
}

// RateLimit is the single installation token's budget (I-14): one bucket to watch.
type RateLimit struct {
	Limit     int
	Remaining int
	ResetAt   time.Time
}

// BoardSnapshot is the result of one batched BoardSweep over the whole board.
type BoardSnapshot struct {
	PullRequests []PullRequest
	Issues       []Issue
	RateLimit    RateLimit
}

// Client is the narrow GitHub surface reconcile-IN consumes. The real and fake
// implementations are interchangeable; reconcile-IN never branches on which.
type Client interface {
	// BoardSweep performs the one batched GraphQL read of the whole board
	// (§8.1.1). The real impl paginates; the fake returns a scripted snapshot.
	BoardSweep(ctx context.Context) (BoardSnapshot, error)
	// PullRequest fetches a single PR's Domain-B facts (the targeted refetch a
	// webhook hint triggers, §8.1.3). ok=false means "no such open/merged PR".
	PullRequest(ctx context.Context, number int) (PullRequest, bool, error)
}

// RealClient is the genuine GitHub caller. It is CGO-free (stdlib net/http only)
// and bears the single installation token (I-14). It is wired but unexercised in
// this environment (no creds; e2e_github off by default) — Fake stands in for
// every test.
type RealClient struct {
	Owner string
	Repo  string
	// Token provides the installation token, rotating it as needed (ghinstallation
	// in production). A function so the long-lived token can be refreshed without a
	// new Client.
	Token func(ctx context.Context) (string, error)
	// HTTP is the client used for both the GraphQL and REST endpoints. Serialized
	// outbound writes (the §8.2.4 concurrency cap) live in project-OUT, not here.
	HTTP     *http.Client
	Endpoint string // GraphQL endpoint; defaults to https://api.github.com/graphql
}

// NewRealClient builds a RealClient with sane defaults.
func NewRealClient(owner, repo string, token func(ctx context.Context) (string, error)) *RealClient {
	return &RealClient{
		Owner:    owner,
		Repo:     repo,
		Token:    token,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Endpoint: "https://api.github.com/graphql",
	}
}

// boardSweepQuery is the §8.1.1 batched read. Pagination cursors are threaded in
// by the caller; this MVP fetches the first page (the sweep is the floor, and the
// test repo is small) — full pagination is a mechanical extension.
const boardSweepQuery = `
query BoardSweep($owner:String!, $repo:String!, $prCursor:String, $issueCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequests(first:50, after:$prCursor, states:[OPEN, MERGED], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes {
        number updatedAt isDraft merged mergedAt
        headRefOid baseRefOid
        mergeCommit { oid }
        commits(last:1) { nodes { commit { statusCheckRollup { state } } } }
        labels(first:20) { nodes { name } }
      }
    }
    issues(first:50, after:$issueCursor, states:[OPEN], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { number updatedAt labels(first:20){ nodes{ name } } }
    }
  }
  rateLimit { limit cost remaining resetAt }
}`

// graphQL POSTs a query and decodes the data into out.
func (c *RealClient) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return fmt.Errorf("installation token: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graphql %d: %s", resp.StatusCode, string(raw))
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode graphql: %w", err)
	}
	if len(env.Errors) > 0 {
		return fmt.Errorf("graphql error: %s", env.Errors[0].Message)
	}
	return json.Unmarshal(env.Data, out)
}

// sweepData mirrors the boardSweepQuery shape.
type sweepData struct {
	Repository struct {
		PullRequests struct {
			Nodes []struct {
				Number      int       `json:"number"`
				UpdatedAt   time.Time `json:"updatedAt"`
				IsDraft     bool      `json:"isDraft"`
				Merged      bool      `json:"merged"`
				MergedAt    time.Time `json:"mergedAt"`
				HeadRefOid  string    `json:"headRefOid"`
				BaseRefOid  string    `json:"baseRefOid"`
				MergeCommit *struct {
					Oid string `json:"oid"`
				} `json:"mergeCommit"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
				Labels struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"pullRequests"`
		Issues struct {
			Nodes []struct {
				Number    int       `json:"number"`
				UpdatedAt time.Time `json:"updatedAt"`
				Labels    struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"nodes"`
		} `json:"issues"`
	} `json:"repository"`
	RateLimit struct {
		Limit     int       `json:"limit"`
		Remaining int       `json:"remaining"`
		ResetAt   time.Time `json:"resetAt"`
	} `json:"rateLimit"`
}

func (d sweepData) toSnapshot() BoardSnapshot {
	var snap BoardSnapshot
	for _, n := range d.Repository.PullRequests.Nodes {
		pr := PullRequest{
			Number: n.Number, UpdatedAt: n.UpdatedAt, IsDraft: n.IsDraft,
			Merged: n.Merged, MergedAt: n.MergedAt,
			HeadRefOid: n.HeadRefOid, BaseRefOid: n.BaseRefOid,
		}
		if n.MergeCommit != nil {
			pr.MergeCommit = n.MergeCommit.Oid
		}
		if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
			pr.CIRollup = CIState(n.Commits.Nodes[0].Commit.StatusCheckRollup.State)
		}
		for _, l := range n.Labels.Nodes {
			pr.Labels = append(pr.Labels, l.Name)
		}
		snap.PullRequests = append(snap.PullRequests, pr)
	}
	for _, n := range d.Repository.Issues.Nodes {
		iss := Issue{Number: n.Number, UpdatedAt: n.UpdatedAt}
		for _, l := range n.Labels.Nodes {
			iss.Labels = append(iss.Labels, l.Name)
		}
		snap.Issues = append(snap.Issues, iss)
	}
	snap.RateLimit = RateLimit{
		Limit: d.RateLimit.Limit, Remaining: d.RateLimit.Remaining, ResetAt: d.RateLimit.ResetAt,
	}
	return snap
}

// BoardSweep performs the batched read (§8.1.1).
func (c *RealClient) BoardSweep(ctx context.Context) (BoardSnapshot, error) {
	var data sweepData
	if err := c.graphQL(ctx, boardSweepQuery, map[string]any{
		"owner": c.Owner, "repo": c.Repo, "prCursor": nil, "issueCursor": nil,
	}, &data); err != nil {
		return BoardSnapshot{}, err
	}
	return data.toSnapshot(), nil
}

// PullRequest fetches one PR's Domain-B facts (the targeted refetch, §8.1.3). It
// reuses the same fragment shape as the sweep so a webhook and a sweep converge to
// the SAME reconciled fact through the SAME code path.
const pullRequestQuery = `
query PR($owner:String!, $repo:String!, $number:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) {
      number updatedAt isDraft merged mergedAt
      headRefOid baseRefOid
      mergeCommit { oid }
      commits(last:1) { nodes { commit { statusCheckRollup { state } } } }
      labels(first:20) { nodes { name } }
    }
  }
  rateLimit { limit cost remaining resetAt }
}`

func (c *RealClient) PullRequest(ctx context.Context, number int) (PullRequest, bool, error) {
	var data struct {
		Repository struct {
			PullRequest *struct {
				Number      int       `json:"number"`
				UpdatedAt   time.Time `json:"updatedAt"`
				IsDraft     bool      `json:"isDraft"`
				Merged      bool      `json:"merged"`
				MergedAt    time.Time `json:"mergedAt"`
				HeadRefOid  string    `json:"headRefOid"`
				BaseRefOid  string    `json:"baseRefOid"`
				MergeCommit *struct {
					Oid string `json:"oid"`
				} `json:"mergeCommit"`
				Commits struct {
					Nodes []struct {
						Commit struct {
							StatusCheckRollup *struct {
								State string `json:"state"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
				Labels struct {
					Nodes []struct {
						Name string `json:"name"`
					} `json:"nodes"`
				} `json:"labels"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, pullRequestQuery, map[string]any{
		"owner": c.Owner, "repo": c.Repo, "number": number,
	}, &data); err != nil {
		return PullRequest{}, false, err
	}
	n := data.Repository.PullRequest
	if n == nil {
		return PullRequest{}, false, nil
	}
	pr := PullRequest{
		Number: n.Number, UpdatedAt: n.UpdatedAt, IsDraft: n.IsDraft,
		Merged: n.Merged, MergedAt: n.MergedAt,
		HeadRefOid: n.HeadRefOid, BaseRefOid: n.BaseRefOid,
	}
	if n.MergeCommit != nil {
		pr.MergeCommit = n.MergeCommit.Oid
	}
	if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		pr.CIRollup = CIState(n.Commits.Nodes[0].Commit.StatusCheckRollup.State)
	}
	for _, l := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr, true, nil
}
