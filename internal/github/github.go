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

// Issue is the Domain-B snapshot of one open issue from a BoardSweep. Title/Body
// are read so a flowbee:adopt opt-in can seed the single-issue flow's task context
// (F7); they are GitHub-owned facts, never written back by Flowbee.
type Issue struct {
	Number    int
	UpdatedAt time.Time
	Labels    []string
	Title     string
	Body      string
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

// OpenPRInput describes the draft PR Flowbee opens from a promoted epoch ref
// (§8.2.1, the canonical §7.3 PR-open trigger). The worker NEVER supplies a PR
// field — Flowbee opens the PR and stamps the returned number.
type OpenPRInput struct {
	Title   string
	Body    string
	HeadRef string // the Flowbee-promoted branch (ref name, not the epoch ref)
	BaseRef string
	Draft   bool
}

// CreateIssueInput describes an issue Flowbee materializes from a signed-off
// spec (§11, materialize_issues). project-OUT renders the body from spec.md.
type CreateIssueInput struct {
	Title  string
	Body   string
	Labels []string
}

// Writer is the project-OUT GitHub surface (§8.2): the ONLY writer to GitHub.
// Every method is a single outbound mutation the serialized sender performs (≤1
// in-flight, §8.2.4). The fake records every call (for the once-per-key audit
// assertion); the real impl bears the single installation token. Retry-After is
// surfaced as ErrRetryAfter so the sender can park the whole outbox.
type Writer interface {
	// OpenPR opens a (draft) PR and returns its GitHub-assigned number. Flowbee
	// stamps this number onto the job (Domain B owns PR existence, §3.4).
	OpenPR(ctx context.Context, in OpenPRInput) (number int, err error)
	// CreateIssue materializes a GitHub issue and returns its number (§11).
	CreateIssue(ctx context.Context, in CreateIssueInput) (number int, err error)
	// SetLabels replaces the flowbee:* label set on a PR/issue (a rendering of
	// Domain-A stage; §8.2.1).
	SetLabels(ctx context.Context, number int, labels []string) error
	// CreateCheck emits a Flowbee-controlled status check at a SHA (e.g.
	// flowbee/review-valid@SHA, §8.5.3).
	CreateCheck(ctx context.Context, sha, name, conclusion string) error
	// EnqueueMergeQueue enqueues a PR to GitHub's native merge queue — how BOTH
	// merge arms physically merge (§5.4, §8.5). Workers never call this.
	EnqueueMergeQueue(ctx context.Context, number int) error
	// ConvertToDraft transitions a PR back to draft — the compensation step that
	// never leaves a revoked zombie's PR ready-for-review (§6.5.4 draft-back, I-12).
	ConvertToDraft(ctx context.Context, number int) error
	// CancelCI cancels in-flight CI for a (revoked) epoch's pushed SHA — the
	// compensation step that stops a dead epoch's checks (§6.5.4, I-12). A best-effort
	// cancel: CI not cancellable is not an error.
	CancelCI(ctx context.Context, sha string) error
	// BranchProtection reads the server-side protection on a branch (I-8, §9.6):
	// the orchestrator-independent backstop Flowbee asserts on startup.
	BranchProtection(ctx context.Context, branch string) (Protection, bool, error)
}

// BranchProtectionReader is the narrow read surface the I-8 startup assertion
// consumes (§9.6). Both the Writer impls satisfy it.
type BranchProtectionReader interface {
	BranchProtection(ctx context.Context, branch string) (Protection, bool, error)
}

// Protection is the server-side branch-protection backstop (I-8, §9.6).
type Protection struct {
	RequirePR               bool
	RequiredReviews         int
	RequireCodeOwnerReview  bool
	DismissStale            bool
	RequiredChecks          []string
	NoForcePush             bool
	RequireDistinctReviewer bool // required review from an identity distinct from the author
}

// ErrRetryAfter signals a secondary/abuse rate limit (§8.2.4): the sender parks
// the WHOLE outbox for RetryAfter before draining again. It is authoritative.
type ErrRetryAfter struct{ RetryAfter time.Duration }

func (e *ErrRetryAfter) Error() string {
	return fmt.Sprintf("retry after %s", e.RetryAfter)
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

// ── project-OUT writes (§8.2): the REST surface. Wired but unexercised in this
// environment (no App creds; e2e_github off by default) — Fake stands in for
// every test. The serialized sender (internal/project) is the only caller; this
// package performs no concurrency control of its own (§8.2.4 lives in the sender).

func (c *RealClient) restURL(path string) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/%s%s", c.Owner, c.Repo, path)
}

// rest POSTs/PUTs a JSON body to a REST endpoint with the installation token and
// decodes the response into out. A 403/429 carrying Retry-After is surfaced as
// *ErrRetryAfter so the sender can park the outbox (§8.2.4).
func (c *RealClient) rest(ctx context.Context, method, path string, body any, out any) error {
	tok, err := c.Token(ctx)
	if err != nil {
		return fmt.Errorf("installation token: %w", err)
	}
	var buf io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.restURL(path), buf)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, perr := time.ParseDuration(ra + "s"); perr == nil {
				return &ErrRetryAfter{RetryAfter: secs}
			}
			return &ErrRetryAfter{RetryAfter: 60 * time.Second}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("rest %s %s: %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *RealClient) OpenPR(ctx context.Context, in OpenPRInput) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	err := c.rest(ctx, http.MethodPost, "/pulls", map[string]any{
		"title": in.Title, "body": in.Body, "head": in.HeadRef, "base": in.BaseRef, "draft": in.Draft,
	}, &out)
	return out.Number, err
}

func (c *RealClient) CreateIssue(ctx context.Context, in CreateIssueInput) (int, error) {
	var out struct {
		Number int `json:"number"`
	}
	err := c.rest(ctx, http.MethodPost, "/issues", map[string]any{
		"title": in.Title, "body": in.Body, "labels": in.Labels,
	}, &out)
	return out.Number, err
}

func (c *RealClient) SetLabels(ctx context.Context, number int, labels []string) error {
	return c.rest(ctx, http.MethodPut, fmt.Sprintf("/issues/%d/labels", number),
		map[string]any{"labels": labels}, nil)
}

func (c *RealClient) CreateCheck(ctx context.Context, sha, name, conclusion string) error {
	return c.rest(ctx, http.MethodPost, "/check-runs", map[string]any{
		"name": name, "head_sha": sha, "status": "completed", "conclusion": conclusion,
	}, nil)
}

func (c *RealClient) EnqueueMergeQueue(ctx context.Context, number int) error {
	// the merge-queue enqueue is a GraphQL mutation in production; the REST shim
	// here is a placeholder for the wired-but-unexercised real path.
	return c.rest(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/merge-queue", number), map[string]any{}, nil)
}

func (c *RealClient) ConvertToDraft(ctx context.Context, number int) error {
	// converting an open PR back to draft is a GraphQL mutation in production; the
	// REST shim here is a placeholder for the wired-but-unexercised real path.
	return c.rest(ctx, http.MethodPatch, fmt.Sprintf("/pulls/%d", number),
		map[string]any{"draft": true}, nil)
}

func (c *RealClient) CancelCI(ctx context.Context, sha string) error {
	// cancelling in-flight check runs at a SHA is a REST call per workflow run in
	// production; the shim is a placeholder. Best-effort — never fails the caller.
	return nil
}

func (c *RealClient) BranchProtection(ctx context.Context, branch string) (Protection, bool, error) {
	var out struct {
		RequiredPullRequestReviews *struct {
			RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
			RequireCodeOwnerReviews      bool `json:"require_code_owner_reviews"`
			DismissStaleReviews          bool `json:"dismiss_stale_reviews"`
		} `json:"required_pull_request_reviews"`
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
		AllowForcePushes struct {
			Enabled bool `json:"enabled"`
		} `json:"allow_force_pushes"`
	}
	err := c.rest(ctx, http.MethodGet, fmt.Sprintf("/branches/%s/protection", branch), nil, &out)
	if err != nil {
		return Protection{}, false, err
	}
	p := Protection{NoForcePush: !out.AllowForcePushes.Enabled}
	if out.RequiredPullRequestReviews != nil {
		p.RequirePR = true
		p.RequiredReviews = out.RequiredPullRequestReviews.RequiredApprovingReviewCount
		p.RequireCodeOwnerReview = out.RequiredPullRequestReviews.RequireCodeOwnerReviews
		p.DismissStale = out.RequiredPullRequestReviews.DismissStaleReviews
		p.RequireDistinctReviewer = p.RequiredReviews >= 1
	}
	if out.RequiredStatusChecks != nil {
		p.RequiredChecks = out.RequiredStatusChecks.Contexts
	}
	return p, true, nil
}
