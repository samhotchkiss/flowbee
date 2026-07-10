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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
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
	// ClosedUnmerged is true for a PR a human CLOSED without merging (GitHub state
	// CLOSED, not MERGED) — the signal that the change was rejected, so reconcile parks
	// the job instead of waiting on a merge that will never come.
	ClosedUnmerged bool
	CIRollup       CIState
	// CIHasRealSuccess is true iff at least one NON-skipped check actually concluded SUCCESS on
	// the head. GitHub's aggregate statusCheckRollup.state is SUCCESS even when EVERY check was
	// SKIPPED (e.g. a paths: filter excluded the changed files, or a hostile workflow edit) —
	// i.e. no test ran. The merge gate AND-s this with CIRollup==SUCCESS so an all-skipped PR is
	// never read as green and cannot mint a verdict / self-merge on tests that never executed.
	CIHasRealSuccess bool
	// FailingChecks names the checks that DEFINITIVELY failed at the head (CheckRun
	// FAILURE/TIMED_OUT/CANCELLED/STARTUP_FAILURE or legacy StatusContext FAILURE/ERROR).
	// Carried to a bounced build so the rebuild brief tells the agent EXACTLY which gate
	// to re-run + fix locally instead of a generic "CI was red". Empty when CI is green
	// or only pending.
	FailingChecks []string
	// FailingCheckURLs maps each failed check name to its actionable GitHub URL when
	// GitHub exposes one (CheckRun.detailsUrl or StatusContext.targetUrl).
	FailingCheckURLs map[string]string
	// PassedChecks names the checks that concluded SUCCESS at the head (CheckRun SUCCESS
	// or legacy StatusContext SUCCESS). Used to evaluate whether the repo's REQUIRED
	// status checks are green independently of the aggregate rollup — a PR can be
	// UNSTABLE/PENDING overall (a non-required cosmetic check) while every REQUIRED check
	// has passed, which is exactly when GitHub itself permits the merge.
	PassedChecks []string
	Labels       []string // read only to DETECT drift on Flowbee-owned renderings (§8.1.2)
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

// PRDiffer is the optional authoritative diff surface used by targeted adopted-PR
// review. The base/head SHAs are the resolved facts from PullRequest; implementations
// must return the diff for that exact pair or fail.
type PRDiffer interface {
	PullRequestDiff(ctx context.Context, number int, baseSHA, headSHA string) (string, error)
}

// BranchCIReader resolves the CI rollup at a branch HEAD — the integration branch's
// green/red state, for the green-main invariant (don't bounce a PR for a CI failure it
// merely inherited from a red main). A SEPARATE optional interface (reconcile type-asserts
// it) so test fakes that don't need it aren't forced to implement it; absence degrades to
// "main not red" (bounce as before).
type BranchCIReader interface {
	BranchCIState(ctx context.Context, branch string) (CIState, error)
}

// MergeUnsticker is the OPTIONAL GitHub surface for the merge_handoff un-stick driver (#214):
// read a PR's mergeable state and fast-forward a behind head so a reviewed, green PR stops
// rotting behind a base that keeps moving (the "each other merge pushes the waiting PRs
// further behind" cascade). SEPARATE + optional (the un-stick sweep type-asserts it, like
// BranchCIReader) so clients/fakes that don't need it aren't forced to implement it; absence
// simply disables the un-stick for that repo. It never MERGES — only fast-forwards — so it
// cannot land unreviewed code; it expands no autonomous-merge trust.
type MergeUnsticker interface {
	// PullMergeableState returns the PR's REST mergeable_state ("behind","clean","dirty",
	// "blocked","unstable","unknown",…). ok=false => no such PR. GitHub computes it
	// ASYNCHRONOUSLY, so a freshly-changed PR can read "unknown" until a later poll — the
	// caller acts ONLY on a definitive "behind".
	PullMergeableState(ctx context.Context, number int) (state string, ok bool, err error)
	// UpdateBranch fast-forwards the PR head with its base server-side (no local checkout):
	// PUT /pulls/{n}/update-branch (202 Accepted). Re-triggers CI. A 422 (a real conflict the
	// FF can't resolve, or already up to date) surfaces as an error the caller treats as
	// "skip" — never a force.
	UpdateBranch(ctx context.Context, number int) error
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
	// IssueComment posts a comment on an issue (or PR — same REST surface). This is
	// how a reviewer's findings + verdict are written into the GitHub issue so it is
	// the durable, human-readable record of the build's review history (build-list
	// §F). Workers never call this — the control plane is the sole GitHub writer (R4).
	IssueComment(ctx context.Context, number int, body string) error
	// SetLabels replaces the flowbee:* label set on a PR/issue (a rendering of
	// Domain-A stage; §8.2.1).
	SetLabels(ctx context.Context, number int, labels []string) error
	// CreateCheck emits a Flowbee-controlled status check at a SHA (e.g.
	// flowbee/review-valid@SHA, §8.5.3).
	CreateCheck(ctx context.Context, sha, name, conclusion string) error
	// EnqueueMergeQueue enqueues a PR to GitHub's native merge queue — how BOTH
	// merge arms physically merge (§5.4, §8.5). Workers never call this.
	EnqueueMergeQueue(ctx context.Context, number int, expectedHead string) error
	// ConvertToDraft transitions a PR back to draft — the compensation step that
	// never leaves a revoked zombie's PR ready-for-review (§6.5.4 draft-back, I-12).
	ConvertToDraft(ctx context.Context, number int) error
	// CancelCI cancels in-flight CI for a (revoked) epoch's pushed SHA — the
	// compensation step that stops a dead epoch's checks (§6.5.4, I-12). A best-effort
	// cancel: CI not cancellable is not an error.
	CancelCI(ctx context.Context, sha string) error
	// DeleteBranch deletes refs/heads/<branch> — the post-merge cleanup of a
	// flowbee/issue-N branch so the repo does not accumulate thousands of stale
	// flowbee/issue-* branches. Safe after a MERGE commit: the branch's commits stay
	// reachable from main, so only the ref is removed.
	DeleteBranch(ctx context.Context, branch string) error
	// BranchProtection reads the server-side protection on a branch (I-8, §9.6):
	// the orchestrator-independent backstop Flowbee asserts on startup.
	BranchProtection(ctx context.Context, branch string) (Protection, bool, error)
	// PutFile creates-or-updates a file on `branch` via the Contents API — one atomic
	// commit per call, onto the branch's current tip (no force-push, no fast-forward
	// race with concurrent merges). Used for the §F history archive (docs/history/*.md):
	// the durable, reconcile-first way to land a Flowbee-authored doc on the integration
	// branch. Idempotent: a re-PUT of byte-identical content is a no-op.
	PutFile(ctx context.Context, path string, content []byte, message, branch string) error
	// PutFiles creates-or-updates SEVERAL files in ONE commit on `branch` via the Git Data
	// API (tree -> commit -> fast-forward ref). The §F archive lands a job's card AND the
	// regenerated TOC together, so a merge adds a SINGLE archive commit, not one per file.
	// Idempotent: when the resulting tree is byte-identical to the branch tip's, it makes no
	// commit (a re-drain after a CP crash is a no-op).
	PutFiles(ctx context.Context, files map[string][]byte, message, branch string) error
}

// BranchProtectionReader is the narrow read surface the I-8 startup assertion
// consumes (§9.6). Both the Writer impls satisfy it.
type BranchProtectionReader interface {
	BranchProtection(ctx context.Context, branch string) (Protection, bool, error)
}

// RequiredChecksReader reads the required status-check contexts enforced on a branch from
// ANY source — modern rulesets included (BranchProtection only sees classic protection).
type RequiredChecksReader interface {
	BranchRequiredChecks(ctx context.Context, branch string) ([]string, error)
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

// rateLimitBackoff detects a GitHub rate-limit from response headers and returns how long to
// back off before retrying. GitHub signals it three ways, all covered here: a `Retry-After`
// header (the secondary/abuse limit), or `X-RateLimit-Remaining: 0` + `X-RateLimit-Reset:
// <unix>` (the primary 5000/hr limit) — the latter also present on a GraphQL 200 that
// carries a RATE_LIMITED error. Returns ok=false when the response is not rate-limited.
// Capped at 1h (a sane outage ceiling) and floored at 1s (never a busy-spin).
func rateLimitBackoff(h http.Header) (time.Duration, bool) {
	if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 {
			return clampBackoff(time.Duration(secs) * time.Second), true
		}
	}
	if h.Get("X-RateLimit-Remaining") == "0" {
		if reset := strings.TrimSpace(h.Get("X-RateLimit-Reset")); reset != "" {
			if unix, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return clampBackoff(time.Until(time.Unix(unix, 0))), true
			}
		}
	}
	return 0, false
}

func clampBackoff(d time.Duration) time.Duration {
	if d < time.Second {
		return time.Second
	}
	if d > time.Hour {
		return time.Hour
	}
	return d
}

// isGraphQLRateLimited reports whether a GraphQL errors array signals the shared rate limit
// — GitHub returns HTTP 200 with errors[].type == "RATE_LIMITED" (or a "rate limit" message),
// so the status-code path never sees it. This is why issues.create / mergeQueue.enqueue
// (both GraphQL mutations) were dead-lettered instead of backing off (russ #215).
func isGraphQLRateLimited(typ, msg string) bool {
	return strings.EqualFold(typ, "RATE_LIMITED") || strings.Contains(strings.ToLower(msg), "rate limit")
}

// ErrMergeConflict signals a PR that GitHub refuses to merge because it conflicts
// with its base (a 405 "merge conflicts" / "not mergeable") — a sibling merged into
// the same area after this change's verdict was minted. Distinct from a transient
// error: retrying the merge NEVER succeeds (it just loops). The sender routes the job
// to a conflict_resolver instead of re-queuing the merge.
var ErrMergeConflict = errors.New("pull request has merge conflicts (not mergeable)")

// ErrMergeBaseModified signals GitHub's 405 "Base branch was modified. Review and try
// the merge again." — optimistic concurrency, NOT a failure: the base moved between the
// mergeability check and the merge (a sibling PR merged first). It is RETRYABLE: once the
// base settles, re-merging succeeds. It is deliberately NOT an *ErrGitHub so the sender's
// Permanent() check treats it as transient (retry) rather than dead-lettering it.
var ErrMergeBaseModified = errors.New("merge base branch was modified (retryable)")

// ErrMergeRuleViolationPending signals GitHub's 405 "Repository rule violations found"
// merge refusal when the violated rule can clear without human intervention: a required
// status check is still "expected" (pending/not reported yet), or the branch must first be
// brought up to date with its base. It is deliberately NOT an *ErrGitHub so project-out
// treats it as retryable instead of dead-lettering a merge that raced ahead of CI.
var ErrMergeRuleViolationPending = errors.New("merge blocked by pending repository rule violation (retryable)")

// ErrMergeHeadModified signals GitHub's 409 "Head branch was modified. Review and try
// the merge again." — returned when the merge call pins an expected-head `sha` and the
// PR's live head no longer matches it. This is the SAFETY interlock against an
// approve-then-push race: the gate minted its verdict against the reviewed head, but a
// commit landed on the feature branch afterward. The sender invalidates the verdict,
// abandons the stale merge outbox row, and re-arms independent review — never a
// silent merge or a blind retry of the moved head.
var ErrMergeHeadModified = errors.New("merge head branch was modified after review")

// ErrGitHub is a non-2xx REST response carrying its status code, so the sender can
// distinguish a PERMANENT failure (a 4xx — a deleted branch/PR, a 422 validation, a
// 404 not-found: retrying NEVER succeeds) from a TRANSIENT one (a 5xx / network blip:
// GitHub will recover). Without this the outbox cannot tell a poison row from a brief
// outage and would either wedge forever (head-of-line) or dead-letter good work.
type ErrGitHub struct {
	StatusCode   int
	Method, Path string
	Body         string
}

func (e *ErrGitHub) Error() string {
	return fmt.Sprintf("rest %s %s: %d: %s", e.Method, e.Path, e.StatusCode, e.Body)
}

// Permanent reports whether retrying is futile: a 4xx client error (the request is
// malformed or the target is gone), EXCEPT 408 Request Timeout and 429 Too Many
// Requests, which are retried (429 is normally surfaced as ErrRetryAfter upstream).
func (e *ErrGitHub) Permanent() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500 &&
		e.StatusCode != http.StatusRequestTimeout && e.StatusCode != http.StatusTooManyRequests
}

// isMergeConflict reports whether a failed merge is an unmergeable-conflict 405 (vs a
// transient error). GitHub's messages are stable: "Pull Request has merge conflicts"
// and "Pull Request is not mergeable".
func isMergeConflict(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "405") &&
		(strings.Contains(s, "merge conflict") || strings.Contains(s, "not mergeable"))
}

// isBaseModified reports whether a failed merge is GitHub's retryable 405 "Base branch
// was modified. Review and try the merge again." (the base moved under a concurrent
// sibling merge) — distinct from an unmergeable conflict, which never succeeds on retry.
func isBaseModified(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "405") && strings.Contains(s, "base branch was modified")
}

// isMergeRuleViolationPending reports retryable branch-protection/ruleset 405s from the
// merge endpoint. GitHub returns these when Flowbee asked to merge before a required check
// reported, or when the repository requires the branch to be current with base. Those are
// not poison 4xxs; a later drain can succeed after CI reports or after update-branch.
func isMergeRuleViolationPending(err error) bool {
	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "405") || !strings.Contains(s, "repository rule violations found") {
		return false
	}
	return strings.Contains(s, "required status check") && strings.Contains(s, "is expected") ||
		isMergeRuleBehind(err)
}

// IsMergeRuleBehind reports the retryable ruleset variant where GitHub requires the PR
// branch to be up to date with its base before merging. Project-out uses this to trigger
// a best-effort update-branch before retrying.
func IsMergeRuleBehind(err error) bool {
	return isMergeRuleBehind(err)
}

func isMergeRuleBehind(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "branch is not up to date") ||
		strings.Contains(s, "must be up to date") ||
		strings.Contains(s, "behind")
}

// isHeadModified reports GitHub's 409 "Head branch was modified. Review and try the merge
// again." — returned when the merge `sha` interlock no longer matches the PR head (a
// commit landed on the feature branch after the gate reviewed it). Distinct from
// base-modified (a sibling moved main, retryable): the HEAD moving means the bytes that
// would merge are unreviewed, so this routes to the human gate, not a retry.
func isHeadModified(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "409") && strings.Contains(s, "head branch was modified")
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
	RESTBase string // REST base; defaults to https://api.github.com (overridable in tests)
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

// boardSweepQuery is the §8.1.1 batched read, fully paginated by BoardSweep (below).
// OPEN PRs and OPEN issues are paginated to exhaustion via $prCursor/$issueCursor — a
// busy repo (e.g. 100+ open issues) must NOT silently fall off a single first page, or
// reconcile would never see those items and intake would never adopt a stale-dated
// flowbee:adopt issue. MERGED PRs are a SEPARATE first-page-only connection (newest
// first) included on the first page only via $includeMerged: they are the recent-merge
// backstop for the merged->done transition (the webhook + targeted refetch are the
// primary signals), so paginating the entire merge history every sweep is neither
// needed nor affordable.
const boardSweepQuery = `
fragment prFields on PullRequest {
  number updatedAt isDraft merged mergedAt
  headRefOid baseRefOid
  mergeCommit { oid }
  commits(last:1) { nodes { commit { statusCheckRollup { state
    contexts(first:100) { nodes { __typename ... on CheckRun { name conclusion detailsUrl } ... on StatusContext { context state targetUrl } } } } } } }
  labels(first:20) { nodes { name } }
}
query BoardSweep($owner:String!, $repo:String!, $prCursor:String, $issueCursor:String, $includeMerged:Boolean!) {
  repository(owner:$owner, name:$repo) {
    pullRequests(first:50, after:$prCursor, states:[OPEN], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { ...prFields }
    }
    mergedPullRequests: pullRequests(first:50, states:[MERGED], orderBy:{field:UPDATED_AT, direction:DESC}) @include(if: $includeMerged) {
      nodes { ...prFields }
    }
    closedPullRequests: pullRequests(first:50, states:[CLOSED], orderBy:{field:UPDATED_AT, direction:DESC}) @include(if: $includeMerged) {
      nodes { ...prFields }
    }
    issues(first:50, after:$issueCursor, states:[OPEN], orderBy:{field:UPDATED_AT, direction:DESC}) {
      pageInfo { hasNextPage endCursor }
      nodes { number updatedAt title body labels(first:20){ nodes{ name } } }
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
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode graphql: %w", err)
	}
	if len(env.Errors) > 0 {
		// a GraphQL rate-limit is a 200 with errors[].type=RATE_LIMITED — back off until the
		// window resets (the same outbox park as a REST 429) instead of returning a generic
		// error that the drain retries blindly to the dead-letter backstop (russ #215).
		if isGraphQLRateLimited(env.Errors[0].Type, env.Errors[0].Message) {
			d, ok := rateLimitBackoff(resp.Header)
			if !ok {
				d = 60 * time.Second
			}
			return &ErrRetryAfter{RetryAfter: d}
		}
		return fmt.Errorf("graphql error: %s", env.Errors[0].Message)
	}
	return json.Unmarshal(env.Data, out)
}

// pageInfo is a GraphQL connection cursor (§8.1.1 pagination).
type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

// prNode is one pullRequests connection node (shared by the OPEN and MERGED selections
// via the prFields fragment).
type prNode struct {
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
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Typename   string `json:"__typename"`
							Name       string `json:"name"`       // CheckRun (Actions) check name
							Conclusion string `json:"conclusion"` // CheckRun (Actions)
							DetailsURL string `json:"detailsUrl"` // CheckRun URL
							Context    string `json:"context"`    // StatusContext (legacy) name
							State      string `json:"state"`      // StatusContext (legacy)
							TargetURL  string `json:"targetUrl"`  // StatusContext URL
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

// sweepData mirrors the boardSweepQuery shape.
type sweepData struct {
	Repository struct {
		PullRequests struct {
			PageInfo pageInfo `json:"pageInfo"`
			Nodes    []prNode `json:"nodes"`
		} `json:"pullRequests"`
		MergedPullRequests struct {
			Nodes []prNode `json:"nodes"`
		} `json:"mergedPullRequests"`
		ClosedPullRequests struct {
			Nodes []prNode `json:"nodes"`
		} `json:"closedPullRequests"`
		Issues struct {
			PageInfo pageInfo `json:"pageInfo"`
			Nodes    []struct {
				Number    int       `json:"number"`
				UpdatedAt time.Time `json:"updatedAt"`
				Title     string    `json:"title"`
				Body      string    `json:"body"`
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

// prFromNode maps a connection node to a Domain-B PullRequest fact.
func prFromNode(n prNode) PullRequest {
	pr := PullRequest{
		Number: n.Number, UpdatedAt: n.UpdatedAt, IsDraft: n.IsDraft,
		Merged: n.Merged, MergedAt: n.MergedAt,
		HeadRefOid: n.HeadRefOid, BaseRefOid: n.BaseRefOid,
	}
	if n.MergeCommit != nil {
		pr.MergeCommit = n.MergeCommit.Oid
	}
	if len(n.Commits.Nodes) > 0 && n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup
		pr.CIRollup = CIState(rollup.State)
		// a REAL success = at least one NON-skipped check actually concluded SUCCESS (a CheckRun
		// conclusion or a legacy StatusContext state). SKIPPED/NEUTRAL/missing don't count, so an
		// all-skipped rollup (which GitHub aggregates to SUCCESS) is NOT a real success.
		for _, c := range rollup.Contexts.Nodes {
			if (c.Typename == "CheckRun" && c.Conclusion == "SUCCESS") || (c.Typename == "StatusContext" && c.State == "SUCCESS") {
				pr.CIHasRealSuccess = true
				switch {
				case c.Typename == "CheckRun" && c.Name != "":
					pr.PassedChecks = append(pr.PassedChecks, c.Name)
				case c.Typename == "StatusContext" && c.Context != "":
					pr.PassedChecks = append(pr.PassedChecks, c.Context)
				}
			}
			// A SKIPPED or NEUTRAL required check is SATISFIED for GitHub's own merge gate — a
			// skipped required check (e.g. "Migration version guard" on a PR with no migration
			// changes) does NOT block merge (such PRs read mergeStateStatus=CLEAN). Count it as
			// passed so the required-checks gate matches GitHub. It is NOT a real success (no test
			// ran), so it does NOT set CIHasRealSuccess — the all-skipped-rollup guard still holds.
			if c.Typename == "CheckRun" && (c.Conclusion == "SKIPPED" || c.Conclusion == "NEUTRAL") && c.Name != "" {
				pr.PassedChecks = append(pr.PassedChecks, c.Name)
			}
			// collect the NAMES of definitively-failed checks so a bounced build can be told
			// exactly which gate to re-run + fix locally (not a generic "CI was red").
			switch {
			case c.Typename == "CheckRun" && (c.Conclusion == "FAILURE" || c.Conclusion == "TIMED_OUT" || c.Conclusion == "STARTUP_FAILURE" || c.Conclusion == "CANCELLED"):
				if c.Name != "" {
					pr.FailingChecks = append(pr.FailingChecks, c.Name)
					if c.DetailsURL != "" {
						if pr.FailingCheckURLs == nil {
							pr.FailingCheckURLs = make(map[string]string)
						}
						pr.FailingCheckURLs[c.Name] = c.DetailsURL
					}
				}
			case c.Typename == "StatusContext" && (c.State == "FAILURE" || c.State == "ERROR"):
				if c.Context != "" {
					pr.FailingChecks = append(pr.FailingChecks, c.Context)
					if c.TargetURL != "" {
						if pr.FailingCheckURLs == nil {
							pr.FailingCheckURLs = make(map[string]string)
						}
						pr.FailingCheckURLs[c.Context] = c.TargetURL
					}
				}
			}
		}
	}
	for _, l := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr
}

// accumulate folds one query page into snap, deduping by number (a connection re-read
// after its cursor is exhausted can return overlapping nodes). Merged PRs are taken only
// from the first page (where $includeMerged was true and the alias is populated).
func (d sweepData) accumulate(snap *BoardSnapshot, seenPR, seenIssue map[int]bool) {
	add := func(n prNode) {
		if seenPR[n.Number] {
			return
		}
		seenPR[n.Number] = true
		snap.PullRequests = append(snap.PullRequests, prFromNode(n))
	}
	for _, n := range d.Repository.PullRequests.Nodes {
		add(n)
	}
	for _, n := range d.Repository.MergedPullRequests.Nodes {
		add(n)
	}
	// PRs from the CLOSED connection were closed WITHOUT merging (states:[CLOSED]
	// excludes MERGED) — mark them so reconcile can park the rejected job.
	for _, n := range d.Repository.ClosedPullRequests.Nodes {
		if seenPR[n.Number] {
			continue
		}
		seenPR[n.Number] = true
		pr := prFromNode(n)
		pr.ClosedUnmerged = true
		snap.PullRequests = append(snap.PullRequests, pr)
	}
	for _, n := range d.Repository.Issues.Nodes {
		if seenIssue[n.Number] {
			continue
		}
		seenIssue[n.Number] = true
		iss := Issue{Number: n.Number, UpdatedAt: n.UpdatedAt, Title: n.Title, Body: n.Body}
		for _, l := range n.Labels.Nodes {
			iss.Labels = append(iss.Labels, l.Name)
		}
		snap.Issues = append(snap.Issues, iss)
	}
}

// sweepMaxPages bounds the pagination loop (50 items/page => up to 50k open PRs+issues).
// It is a runaway backstop set far beyond any real board; hitting it is logged, never
// silent, so a genuinely huge board surfaces instead of truncating invisibly.
const sweepMaxPages = 1000

// BoardSweep performs the §8.1.1 batched read, paginating OPEN PRs and OPEN issues to
// exhaustion. MERGED PRs are read once (first page, newest-first) as the recent-merge
// backstop. A single first page is NOT enough: a repo with >50 open issues would
// silently drop the tail, so reconcile would never see those PRs and intake would never
// adopt a flowbee:adopt issue whose update time had aged out of the first page.
func (c *RealClient) BoardSweep(ctx context.Context) (BoardSnapshot, error) {
	var snap BoardSnapshot
	seenPR, seenIssue := map[int]bool{}, map[int]bool{}
	var prCursor, issueCursor *string
	prMore, issueMore := true, true

	page := 0
	for ; (prMore || issueMore) && page < sweepMaxPages; page++ {
		var data sweepData
		if err := c.graphQL(ctx, boardSweepQuery, map[string]any{
			"owner": c.Owner, "repo": c.Repo,
			"prCursor": prCursor, "issueCursor": issueCursor,
			"includeMerged": page == 0, // recent merges only need fetching once
		}, &data); err != nil {
			return BoardSnapshot{}, err
		}
		data.accumulate(&snap, seenPR, seenIssue)
		if page == 0 {
			snap.RateLimit = RateLimit{
				Limit: data.RateLimit.Limit, Remaining: data.RateLimit.Remaining, ResetAt: data.RateLimit.ResetAt,
			}
		}
		// Advance each cursor to its page end regardless of hasNextPage so an exhausted
		// connection returns empty (not a re-read of page 1) while the other keeps paging.
		prMore = data.Repository.PullRequests.PageInfo.HasNextPage
		issueMore = data.Repository.Issues.PageInfo.HasNextPage
		if cur := data.Repository.PullRequests.PageInfo.EndCursor; cur != "" {
			prCursor = &cur
		}
		if cur := data.Repository.Issues.PageInfo.EndCursor; cur != "" {
			issueCursor = &cur
		}
	}
	if prMore || issueMore {
		log.Printf("github: BoardSweep hit page cap (%d pages) with more remaining (prMore=%v issueMore=%v) for %s/%s — board larger than the runaway backstop",
			sweepMaxPages, prMore, issueMore, c.Owner, c.Repo)
	}
	return snap, nil
}

// BranchCIState resolves the statusCheckRollup at a branch HEAD (the integration branch's
// green/red state) — the green-main signal. Returns "" (treated as not-red) when the branch
// has no rollup (no checks configured / unresolved), so a missing signal never WITHHOLDS a
// bounce (safe degradation to today's behavior).
func (c *RealClient) BranchCIState(ctx context.Context, branch string) (CIState, error) {
	const q = `query($owner:String!,$repo:String!,$ref:String!){
	  repository(owner:$owner,name:$repo){
	    ref(qualifiedName:$ref){ target{ ... on Commit{ statusCheckRollup{ state } } } }
	  }
	}`
	var out struct {
		Repository struct {
			Ref *struct {
				Target struct {
					StatusCheckRollup *struct {
						State string `json:"state"`
					} `json:"statusCheckRollup"`
				} `json:"target"`
			} `json:"ref"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, q, map[string]any{"owner": c.Owner, "repo": c.Repo, "ref": "refs/heads/" + branch}, &out); err != nil {
		return "", err
	}
	if out.Repository.Ref == nil || out.Repository.Ref.Target.StatusCheckRollup == nil {
		return "", nil
	}
	return CIState(out.Repository.Ref.Target.StatusCheckRollup.State), nil
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
      commits(last:1) { nodes { commit { statusCheckRollup { state
    contexts(first:100) { nodes { __typename ... on CheckRun { name conclusion detailsUrl } ... on StatusContext { context state targetUrl } } } } } } }
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
								State    string `json:"state"`
								Contexts struct {
									Nodes []struct {
										Typename   string `json:"__typename"`
										Name       string `json:"name"`
										Conclusion string `json:"conclusion"`
										DetailsURL string `json:"detailsUrl"`
										Context    string `json:"context"`
										State      string `json:"state"`
										TargetURL  string `json:"targetUrl"`
									} `json:"nodes"`
								} `json:"contexts"`
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
		rollup := n.Commits.Nodes[0].Commit.StatusCheckRollup
		pr.CIRollup = CIState(rollup.State)
		// a REAL success = at least one NON-skipped check actually concluded SUCCESS (a CheckRun
		// conclusion or a legacy StatusContext state). SKIPPED/NEUTRAL/missing don't count, so an
		// all-skipped rollup (which GitHub aggregates to SUCCESS) is NOT a real success.
		for _, c := range rollup.Contexts.Nodes {
			if (c.Typename == "CheckRun" && c.Conclusion == "SUCCESS") || (c.Typename == "StatusContext" && c.State == "SUCCESS") {
				pr.CIHasRealSuccess = true
				switch {
				case c.Typename == "CheckRun" && c.Name != "":
					pr.PassedChecks = append(pr.PassedChecks, c.Name)
				case c.Typename == "StatusContext" && c.Context != "":
					pr.PassedChecks = append(pr.PassedChecks, c.Context)
				}
			}
			// A SKIPPED or NEUTRAL required check is SATISFIED for GitHub's own merge gate — a
			// skipped required check (e.g. "Migration version guard" on a PR with no migration
			// changes) does NOT block merge (such PRs read mergeStateStatus=CLEAN). Count it as
			// passed so the required-checks gate matches GitHub. It is NOT a real success (no test
			// ran), so it does NOT set CIHasRealSuccess — the all-skipped-rollup guard still holds.
			if c.Typename == "CheckRun" && (c.Conclusion == "SKIPPED" || c.Conclusion == "NEUTRAL") && c.Name != "" {
				pr.PassedChecks = append(pr.PassedChecks, c.Name)
			}
			// collect the NAMES of definitively-failed checks so a bounced build can be told
			// exactly which gate to re-run + fix locally (not a generic "CI was red").
			switch {
			case c.Typename == "CheckRun" && (c.Conclusion == "FAILURE" || c.Conclusion == "TIMED_OUT" || c.Conclusion == "STARTUP_FAILURE" || c.Conclusion == "CANCELLED"):
				if c.Name != "" {
					pr.FailingChecks = append(pr.FailingChecks, c.Name)
					if c.DetailsURL != "" {
						if pr.FailingCheckURLs == nil {
							pr.FailingCheckURLs = make(map[string]string)
						}
						pr.FailingCheckURLs[c.Name] = c.DetailsURL
					}
				}
			case c.Typename == "StatusContext" && (c.State == "FAILURE" || c.State == "ERROR"):
				if c.Context != "" {
					pr.FailingChecks = append(pr.FailingChecks, c.Context)
					if c.TargetURL != "" {
						if pr.FailingCheckURLs == nil {
							pr.FailingCheckURLs = make(map[string]string)
						}
						pr.FailingCheckURLs[c.Context] = c.TargetURL
					}
				}
			}
		}
	}
	for _, l := range n.Labels.Nodes {
		pr.Labels = append(pr.Labels, l.Name)
	}
	return pr, true, nil
}

// PullRequestDiff returns GitHub's PR unified diff for the exact base/head pair the
// caller resolved. The PR diff endpoint handles stacked bases and fork heads under
// the original PR identity; the post-fetch SHA check prevents persisting a diff for
// a PR that moved during adoption.
func (c *RealClient) PullRequestDiff(ctx context.Context, number int, baseSHA, headSHA string) (string, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	base := c.RESTBase
	if base == "" {
		base = "https://api.github.com"
	}
	u := strings.TrimRight(base, "/") + "/repos/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.Repo) + "/pulls/" + strconv.Itoa(number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github.v3.diff")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ErrGitHub{StatusCode: resp.StatusCode, Method: http.MethodGet, Path: req.URL.Path, Body: string(raw)}
	}
	pr, ok, err := c.PullRequest(ctx, number)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("pr #%d disappeared while fetching diff", number)
	}
	if pr.BaseRefOid != baseSHA || pr.HeadRefOid != headSHA {
		return "", fmt.Errorf("pr #%d moved while fetching diff: base %s->%s head %s->%s",
			number, baseSHA, pr.BaseRefOid, headSHA, pr.HeadRefOid)
	}
	return string(raw), nil
}

// ── project-OUT writes (§8.2): the REST surface. Wired but unexercised in this
// environment (no App creds; e2e_github off by default) — Fake stands in for
// every test. The serialized sender (internal/project) is the only caller; this
// package performs no concurrency control of its own (§8.2.4 lives in the sender).

func (c *RealClient) restURL(path string) string {
	base := c.RESTBase
	if base == "" {
		base = "https://api.github.com"
	}
	return fmt.Sprintf("%s/repos/%s/%s%s", base, c.Owner, c.Repo, path)
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
		// 403/429 with a rate-limit signal (Retry-After OR X-RateLimit-Remaining:0 + Reset)
		// is a TEMPORARY outage, not a poison 4xx — back off until the window resets rather
		// than letting a 403 dead-letter or a 429 hammer the limit every drain (russ #215).
		if d, ok := rateLimitBackoff(resp.Header); ok {
			return &ErrRetryAfter{RetryAfter: d}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return &ErrRetryAfter{RetryAfter: 60 * time.Second} // 429 with no headers: default backoff
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ErrGitHub{StatusCode: resp.StatusCode, Method: method, Path: path, Body: string(raw)}
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
	if err != nil && isPRAlreadyExists(err) {
		// A 422 "A pull request already exists for <head>" means the PR is ALREADY open:
		// a prior attempt opened it, or an outbox re-send fired after a CP crash between
		// the create succeeding and the pr_number being recorded. Recover the existing
		// PR's number so the row consumes cleanly and the job gets its pr_number, instead
		// of dead-lettering an un-recordable 422 and stranding the job with no PR.
		if n, ok := c.openPRByHead(ctx, in.HeadRef); ok {
			return n, nil
		}
	}
	return out.Number, err
}

// openPRByHead returns the number of the single OPEN PR whose head ref is `head`
// (owner:branch), if any — the recovery lookup for an "already exists" 422.
func (c *RealClient) openPRByHead(ctx context.Context, head string) (int, bool) {
	var prs []struct {
		Number int `json:"number"`
	}
	q := fmt.Sprintf("/pulls?state=open&head=%s:%s", url.QueryEscape(c.Owner), url.QueryEscape(head))
	if err := c.rest(ctx, http.MethodGet, q, nil, &prs); err != nil {
		return 0, false
	}
	if len(prs) > 0 {
		return prs[0].Number, true
	}
	return 0, false
}

// isPRAlreadyExists reports whether a failed OpenPR is GitHub's 422 "a pull request
// already exists for this head" (idempotent re-open) rather than a different validation
// failure (e.g. a missing base branch), which must keep surfacing as an error.
func isPRAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "422") && strings.Contains(s, "already exists")
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

func (c *RealClient) IssueComment(ctx context.Context, number int, body string) error {
	return c.rest(ctx, http.MethodPost, fmt.Sprintf("/issues/%d/comments", number),
		map[string]any{"body": body}, nil)
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

// Preflight is a read-only deployment sanity check (used by `flowbee doctor`): can
// the token WRITE this repo, does the repo have CI (Flowbee merges only on green CI),
// and is the integration branch protected (autonomous merge then needs the token to
// satisfy the protection, or it must be off). These are the three misconfigs that
// otherwise silently stall a real deployment.
type Preflight struct {
	CanWrite        bool
	HasCI           bool
	CITriggersOnPR  bool // an active workflow triggers on pull_request (Flowbee gates on PR CI)
	BranchProtected bool
	TokenScopes     string // X-OAuth-Scopes: non-empty = a broadly-scoped CLASSIC PAT; empty = fine-grained / least-privilege
	// TokenScopesProbed is true only when the X-OAuth-Scopes header was actually read. A
	// FAILED probe leaves TokenScopes "" — which must NOT be reported as "least-privilege"
	// (a green-when-unknown false signal). doctor reports unknown/warn when this is false.
	TokenScopesProbed bool
}

// prTriggerRe matches the `pull_request` workflow trigger as a WHOLE word, so a naive
// substring no longer green-lights a workflow that only has `pull_request_target` /
// `pull_request_review` / `pull_request_comment` (different trigger semantics that do NOT
// put a status check on the bot's PR head) — the merge gate would never go green.
var prTriggerRe = regexp.MustCompile(`\bpull_request\b`)

func (c *RealClient) Preflight(ctx context.Context, branch string) (Preflight, error) {
	var pf Preflight
	var repo struct {
		Permissions struct {
			Admin    bool `json:"admin"`
			Maintain bool `json:"maintain"`
			Push     bool `json:"push"`
		} `json:"permissions"`
	}
	if err := c.rest(ctx, http.MethodGet, "", nil, &repo); err != nil {
		return pf, err // can't even read the repo — token/owner/repo wrong
	}
	pf.CanWrite = repo.Permissions.Push || repo.Permissions.Maintain || repo.Permissions.Admin
	// token scopes: a classic PAT reports its scopes in X-OAuth-Scopes (often far
	// broader than needed — repo, admin:org, delete_repo); a fine-grained PAT reports
	// none. Read the header off a raw repo GET.
	if req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, c.restURL(""), nil); rerr == nil {
		if tok, terr := c.Token(ctx); terr == nil {
			req.Header.Set("Authorization", "Bearer "+tok)
			if resp, derr := c.HTTP.Do(req); derr == nil {
				pf.TokenScopes = strings.TrimSpace(resp.Header.Get("X-OAuth-Scopes"))
				pf.TokenScopesProbed = true // distinguish "probed: none (fine-grained)" from "probe failed: unknown"
				_ = resp.Body.Close()
			}
		}
	}
	if branch == "" {
		branch = "main"
	}
	// CI: list workflows, and check at least one ACTIVE workflow triggers on
	// pull_request — a workflow that only runs on push leaves every PR without a
	// status check, so Flowbee (which gates the merge on green PR CI) never merges.
	var wf struct {
		Workflows []struct {
			Path  string `json:"path"`
			State string `json:"state"`
		} `json:"workflows"`
	}
	if err := c.rest(ctx, http.MethodGet, "/actions/workflows", nil, &wf); err == nil {
		for _, w := range wf.Workflows {
			if w.State != "active" {
				continue
			}
			pf.HasCI = true
			var content struct {
				Content string `json:"content"`
			}
			if err := c.rest(ctx, http.MethodGet, "/contents/"+w.Path+"?ref="+branch, nil, &content); err == nil {
				if raw, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(content.Content, "\n", "")); derr == nil && prTriggerRe.Match(raw) {
					pf.CITriggersOnPR = true
				}
			}
		}
	}
	// 404 on protection => not protected (the autonomous-merge-friendly default).
	if err := c.rest(ctx, http.MethodGet, "/branches/"+branch+"/protection", nil, nil); err == nil {
		pf.BranchProtected = true
	}
	return pf, nil
}

func (c *RealClient) EnqueueMergeQueue(ctx context.Context, number int, expectedHead string) error {
	// GitHub's native merge-queue is a GraphQL mutation that requires the repo to
	// have a merge queue configured. When it isn't, integrate the PR directly via
	// the merge API — the batch-size-1 integration the design's merge queue models
	// (one PR onto current main at a time). Flowbee only reaches here after its own
	// gate minted a verdict bound to green, reconciled CI, so this is the safe write.
	//
	// merge_method "merge" (NOT squash): the per-issue branch carries the full
	// node-by-node story — build, the reviewers' empty findings-commits, revisions —
	// and a merge commit keeps that whole trail REACHABLE from main (so you can see
	// how the change was built), while `git log --first-parent main` stays clean. A
	// squash would discard the history the per-issue-branch model exists to preserve.
	body := map[string]any{
		"merge_method": "merge",
	}
	if expectedHead != "" {
		// SHA interlock: GitHub merges ONLY if the PR head still equals the head the
		// gate reviewed. If a commit landed after approval, GitHub 409s rather than
		// merging the unreviewed head (see ErrMergeHeadModified). This atomic
		// GitHub check backs up reconcile-IN and catches a move that races the
		// final project-OUT call.
		body["sha"] = expectedHead
	}
	err := c.rest(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/merge", number), body, nil)
	if err != nil && isHeadModified(err) {
		// the reviewed head moved under us — do NOT merge, do NOT blind-retry (the head
		// is still the unreviewed one); surface the typed error so the sender invalidates
		// the stale approval and re-arms review.
		return fmt.Errorf("%w: %v", ErrMergeHeadModified, err)
	}
	if err != nil && isBaseModified(err) {
		// GitHub's 405 "Base branch was modified. Review and try the merge again." is
		// OPTIMISTIC-CONCURRENCY, not a failure: main moved between the mergeability check
		// and this merge (a sibling PR merged first — exactly what concurrent epic-child
		// merges produce). It is explicitly retryable. Return a TRANSIENT sentinel (NOT an
		// *ErrGitHub, whose Permanent() is true for any 4xx) so project-out RETRIES the
		// merge once the base settles instead of dead-lettering the loser to needs_human.
		return fmt.Errorf("%w: %v", ErrMergeBaseModified, err)
	}
	if err != nil && isMergeRuleViolationPending(err) {
		// A required-check "is expected" or up-to-date ruleset refusal is a race with
		// GitHub's own merge preconditions, not a bad request. Surface it as retryable so
		// the outbox waits for CI / update-branch instead of immediately escalating.
		return fmt.Errorf("%w: %v", ErrMergeRuleViolationPending, err)
	}
	if err != nil && isMergeConflict(err) {
		// A 405 "not mergeable" is ambiguous: a REAL conflict, GitHub still recomputing
		// mergeability after a sibling merge (transient), OR the PR is ALREADY MERGED — a
		// CP crash/restart between this GitHub call succeeding and the outbox row being
		// marked sent re-sends the merge, and GitHub 405s the merged PR. Distinguish by
		// the PR's actual state: if it is merged, the merge HAPPENED — return success so
		// the row consumes cleanly, instead of routing an already-merged PR to a resolver.
		if pr, ok, perr := c.PullRequest(ctx, number); perr == nil && ok && pr.Merged {
			return nil
		}
		// surface a real conflict as the typed error so the sender routes to a resolver
		// instead of re-queuing a merge that can never succeed.
		return fmt.Errorf("%w: %v", ErrMergeConflict, err)
	}
	return err
}

// PullMergeableState implements MergeUnsticker: GET /pulls/{n} for the REST mergeable_state
// (the only signal that distinguishes "behind base, blocked on up-to-date" from "clean"). A
// 404 means the PR is gone (ok=false), not an error.
func (c *RealClient) PullMergeableState(ctx context.Context, number int) (string, bool, error) {
	var out struct {
		MergeableState string `json:"mergeable_state"`
	}
	if err := c.rest(ctx, http.MethodGet, fmt.Sprintf("/pulls/%d", number), nil, &out); err != nil {
		var ghErr *ErrGitHub
		if errors.As(err, &ghErr) && ghErr.StatusCode == http.StatusNotFound {
			return "", false, nil
		}
		return "", false, err
	}
	return out.MergeableState, true, nil
}

// UpdateBranch implements MergeUnsticker: PUT /pulls/{n}/update-branch (server-side
// fast-forward of base into head). 202 Accepted => nil; a 422 conflict / already-up-to-date
// surfaces as ErrGitHub for the caller to skip.
func (c *RealClient) UpdateBranch(ctx context.Context, number int) error {
	return c.rest(ctx, http.MethodPut, fmt.Sprintf("/pulls/%d/update-branch", number), nil, nil)
}

// DeleteBranch deletes refs/heads/<branch>. A 404/422 (the ref is already gone, e.g.
// GitHub auto-deleted it on merge, or a re-drain) is success — the goal state (no
// branch) is reached either way, so the outbox row consumes cleanly rather than
// dead-lettering. Branch names with slashes (flowbee/issue-N) are valid ref paths.
func (c *RealClient) DeleteBranch(ctx context.Context, branch string) error {
	err := c.rest(ctx, http.MethodDelete, "/git/refs/heads/"+branch, nil, nil)
	if err != nil {
		var ghErr *ErrGitHub
		if errors.As(err, &ghErr) && (ghErr.StatusCode == http.StatusNotFound || ghErr.StatusCode == http.StatusUnprocessableEntity) {
			return nil
		}
	}
	return err
}

func (c *RealClient) PutFile(ctx context.Context, path string, content []byte, message, branch string) error {
	// fetch the current file's blob sha (an UPDATE needs it; a 404 means CREATE). Also
	// short-circuit if the content already matches — a re-drain after a CP crash must not
	// mint a redundant commit (idempotency).
	var cur struct {
		SHA     string `json:"sha"`
		Content string `json:"content"` // base64, may carry newlines
	}
	getErr := c.rest(ctx, http.MethodGet, "/contents/"+path+"?ref="+url.QueryEscape(branch), nil, &cur)
	if getErr == nil && cur.Content != "" {
		if existing, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(cur.Content, "\n", "")); derr == nil && bytes.Equal(existing, content) {
			return nil // unchanged: idempotent no-op
		}
	}
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if getErr == nil && cur.SHA != "" {
		body["sha"] = cur.SHA // update in place
	}
	return c.rest(ctx, http.MethodPut, "/contents/"+path, body, nil)
}

// PutFiles commits MULTIPLE files in one commit via the Git Data API: read the branch tip,
// build a tree (inline content) on top of its base tree, create a commit, fast-forward the
// ref. Reconcile-first like PutFile (no force-push). Idempotent: if the new tree equals the
// tip's tree (all files unchanged), it makes no commit — a re-drain after a crash is a no-op.
//
// The whole read-build-fast-forward is RETRIED on a non-fast-forward (a concurrent write moved
// the branch between the ref read and the ref update): a fast-forward update is fast-forward
// ONLY, so the loser must re-read the new tip and rebuild on it. Without the retry a concurrent
// merge/archive landing in the window 422'd this PERMANENTLY (a 4xx) and the archive was
// abandoned — the regression vs PutFile this replaced.
func (c *RealClient) PutFiles(ctx context.Context, files map[string][]byte, message, branch string) error {
	if len(files) == 0 {
		return nil
	}
	type treeEntry struct {
		Path    string `json:"path"`
		Mode    string `json:"mode"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	entries := make([]treeEntry, 0, len(files)) // content is fixed; only the base tip moves
	for path, content := range files {
		entries = append(entries, treeEntry{Path: path, Mode: "100644", Type: "blob", Content: string(content)})
	}

	const maxAttempts = 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		// 1) the branch tip commit + 2) its base tree.
		var ref struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := c.rest(ctx, http.MethodGet, "/git/ref/heads/"+url.PathEscape(branch), nil, &ref); err != nil {
			return fmt.Errorf("get ref %s: %w", branch, err)
		}
		parent := ref.Object.SHA
		var commit struct {
			Tree struct {
				SHA string `json:"sha"`
			} `json:"tree"`
		}
		if err := c.rest(ctx, http.MethodGet, "/git/commits/"+parent, nil, &commit); err != nil {
			return fmt.Errorf("get commit %s: %w", parent, err)
		}
		// 3) a new tree stacking the files on the CURRENT base tree (inline content).
		var tree struct {
			SHA string `json:"sha"`
		}
		if err := c.rest(ctx, http.MethodPost, "/git/trees",
			map[string]any{"base_tree": commit.Tree.SHA, "tree": entries}, &tree); err != nil {
			return fmt.Errorf("create tree: %w", err)
		}
		if tree.SHA == "" || tree.SHA == commit.Tree.SHA {
			return nil // unchanged tree: idempotent no-op (the content already matches the tip)
		}
		// 4) the commit + 5) fast-forward the ref.
		var newCommit struct {
			SHA string `json:"sha"`
		}
		if err := c.rest(ctx, http.MethodPost, "/git/commits",
			map[string]any{"message": message, "tree": tree.SHA, "parents": []string{parent}}, &newCommit); err != nil {
			return fmt.Errorf("create commit: %w", err)
		}
		err := c.rest(ctx, http.MethodPatch, "/git/refs/heads/"+url.PathEscape(branch),
			map[string]any{"sha": newCommit.SHA}, nil)
		if err == nil {
			return nil
		}
		// a non-fast-forward (the tip moved under us, 422) is retryable: re-read + rebuild.
		// GitHub also reports the lost FF race as 422 Unprocessable. Any other error is real.
		var ghErr *ErrGitHub
		if attempt < maxAttempts-1 && errors.As(err, &ghErr) && ghErr.StatusCode == http.StatusUnprocessableEntity {
			continue
		}
		return fmt.Errorf("update ref %s: %w", branch, err)
	}
	return fmt.Errorf("update ref %s: exhausted %d fast-forward attempts (branch kept moving)", branch, maxAttempts)
}

// prNodeQuery resolves a PR's GraphQL node ID (+ current draft state) from its
// number — convertPullRequestToDraft takes the node ID, not the number.
const prNodeQuery = `
query PRNode($owner:String!, $repo:String!, $number:Int!) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$number) { id isDraft }
  }
}`

// convertToDraftMutation flips an open PR back to draft (the M11 zombie compensation).
const convertToDraftMutation = `
mutation Draft($id:ID!) {
  convertPullRequestToDraft(input:{pullRequestId:$id}) {
    pullRequest { isDraft }
  }
}`

func (c *RealClient) ConvertToDraft(ctx context.Context, number int) error {
	// GitHub's REST API treats a PR's `draft` flag as READ-ONLY — a PATCH cannot toggle
	// it (it is silently ignored). Converting an open PR back to draft is only possible
	// via the GraphQL convertPullRequestToDraft mutation, which takes the PR's node ID
	// (not its number). Resolve the node ID, then mutate. This is the M11 zombie
	// compensation (§6.5.4): a revoked epoch's PR must never sit ready-for-review, so a
	// silent no-op here would leave a mergeable zombie behind.
	var idQ struct {
		Repository struct {
			PullRequest *struct {
				ID      string `json:"id"`
				IsDraft bool   `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphQL(ctx, prNodeQuery, map[string]any{
		"owner": c.Owner, "repo": c.Repo, "number": number,
	}, &idQ); err != nil {
		return fmt.Errorf("resolve pr %d node id: %w", number, err)
	}
	if idQ.Repository.PullRequest == nil || idQ.Repository.PullRequest.ID == "" {
		return fmt.Errorf("pr %d not found for draft-back", number)
	}
	if idQ.Repository.PullRequest.IsDraft {
		return nil // already draft — idempotent (the sender may retry the action)
	}
	var mut struct {
		ConvertPullRequestToDraft struct {
			PullRequest struct {
				IsDraft bool `json:"isDraft"`
			} `json:"pullRequest"`
		} `json:"convertPullRequestToDraft"`
	}
	if err := c.graphQL(ctx, convertToDraftMutation, map[string]any{
		"id": idQ.Repository.PullRequest.ID,
	}, &mut); err != nil {
		return fmt.Errorf("convert pr %d to draft: %w", number, err)
	}
	if !mut.ConvertPullRequestToDraft.PullRequest.IsDraft {
		return fmt.Errorf("pr %d still not draft after convertPullRequestToDraft", number)
	}
	return nil
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

// BranchRequiredChecks returns the required status-check contexts ENFORCED on a branch,
// covering modern repository RULESETS (not just classic branch protection). The
// /rules/branches/{branch} endpoint returns the effective active rules for the branch
// from every source; we extract the contexts from any required_status_checks rule. A repo
// like russ enforces via a ruleset and has NO classic branch protection (that API 404s),
// so BranchProtection alone reports zero required checks — this is the gap this closes.
// Returns nil (no error) when the branch has no required-check rule.
func (c *RealClient) BranchRequiredChecks(ctx context.Context, branch string) ([]string, error) {
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err := c.rest(ctx, http.MethodGet, fmt.Sprintf("/rules/branches/%s", branch), nil, &rules); err != nil {
		return nil, err
	}
	var checks []string
	for _, r := range rules {
		if r.Type != "required_status_checks" {
			continue
		}
		for _, sc := range r.Parameters.RequiredStatusChecks {
			if sc.Context != "" {
				checks = append(checks, sc.Context)
			}
		}
	}
	return checks, nil
}
