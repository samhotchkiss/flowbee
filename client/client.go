// Package client is the reusable Mode-B worker client (also the MCP-shim surface
// from M5). It speaks the §7.2 worker HTTP protocol and never touches the DB —
// the architecture test asserts the worker subcommands can't import the store.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
	// BearerToken is the signed per-worker token (DESIGN §7.6) presented on every
	// call as Authorization: Bearer. Empty on a loopback dev client (the server's
	// loopback bypass accepts it); REQUIRED for a non-loopback listener.
	BearerToken string
	// Model is the worker's ACTUAL backend/model label (e.g. "codex", "sonnet") sent on
	// every lease so the server can record it on the bound event for the §F card. Display
	// only; empty omits the param (older/unlabeled workers just show no model on the card).
	Model string
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: http.DefaultClient}
}

// NewWithToken builds a client that authenticates with a signed bearer token —
// the cross-box (non-loopback) path (§7.6).
func NewWithToken(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, HTTP: http.DefaultClient, BearerToken: token}
}

// authHeader sets Authorization: Bearer when a token is configured.
func (c *Client) authHeader(req *http.Request) {
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	}
}

// Registration mirrors the server-side enrollment payload. Arch/OS are the
// attestation handshake (§7.2): the server attests arch:*/os:* claims against
// them.
type Registration struct {
	WorkerID     string   `json:"worker_id"`
	Identity     string   `json:"identity"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities"`
	Arch         string   `json:"arch,omitempty"`
	OS           string   `json:"os,omitempty"`
	// F6 capacity advertisement: per-model concurrency (claude:3, codex:3), the
	// per-box distribution weight, and the named per-model accounts (rollover chain).
	ModelSlots map[string]int   `json:"model_slots,omitempty"`
	Weight     int              `json:"weight,omitempty"`
	Accounts   []AccountSpecMsg `json:"accounts,omitempty"`
}

// AccountSpecMsg is one named per-model account advertised at registration (F6):
// a credential with a ceiling_pct gate and an ordered preference (rollover chain).
type AccountSpecMsg struct {
	AccountID      string `json:"account_id"`
	ModelFamily    string `json:"model_family"`
	CeilingPct     int    `json:"ceiling_pct"`
	PreferenceRank int    `json:"preference_rank"`
	// BudgetTokens optionally overrides the fleet-wide per-account token budget the
	// preemptive usage ceiling derives usage_pct from (F6). 0 = use the server default.
	BudgetTokens int64 `json:"budget_tokens,omitempty"`
}

// UsageReport is one per-account usage observation a box reports to POST
// /v1/workers/usage (after each run, immediate on a 429). RateLimited marks a
// 429-triggered report (pins the account out of dispatch until it cools).
//
// TokensDelta is the INCREMENTAL tokens the just-finished run consumed: the control
// plane accumulates it into the account's reset-window bucket and derives a rising
// usage_pct from the budget (the F6 PREEMPTIVE ceiling — codex emits token counts,
// not a live %). UsagePct is used directly only when a provider exposes a real
// percentage (no TokensDelta); for codex it stays 0 and the server derives it.
type UsageReport struct {
	AccountID   string `json:"account_id"`
	ModelFamily string `json:"model_family,omitempty"`
	UsagePct    int    `json:"usage_pct"`
	TokensDelta int64  `json:"tokens_delta,omitempty"`
	RateLimited bool   `json:"rate_limited,omitempty"`
}

// ReportUsage posts per-account usage to the control plane (F6). The control plane
// folds it into the shared per-account buckets the ceiling gate reads at dispatch.
func (c *Client) ReportUsage(ctx context.Context, reports []UsageReport) (int, error) {
	var out map[string]any
	return c.postJSONStatus(ctx, "/v1/workers/usage", nil,
		map[string]any{"reports": reports}, &out)
}

type RegisterResponse struct {
	WorkerID             string   `json:"worker_id"`
	LeaseTTLS            int      `json:"lease_ttl_s"`
	HeartbeatIntervalS   int      `json:"heartbeat_interval_s"`
	AttestedCapabilities []string `json:"attested_capabilities"`
	AttestationExpires   string   `json:"attestation_expires_at"`
}

func (c *Client) Register(ctx context.Context, reg Registration) (RegisterResponse, error) {
	var out RegisterResponse
	if err := c.postJSON(ctx, "/v1/workers/register", nil, reg, &out); err != nil {
		return out, err
	}
	return out, nil
}

// LeaseGrant is the §7.2 lease envelope.
type LeaseGrant struct {
	JobID        string `json:"job_id"`
	Kind         string `json:"kind"`
	Role         string `json:"role"`
	BaseSHA      string `json:"base_sha"`
	LeaseID      string `json:"lease_id"`
	LeaseEpoch   int    `json:"lease_epoch"`
	LeaseTTLS    int    `json:"lease_ttl_s"`
	Deadline     string `json:"lease_deadline"`
	DryRun       bool   `json:"dry_run,omitempty"`
	Provisioning string `json:"provisioning"`
	MirrorPath   string `json:"mirror_path"`
	PushTarget   string `json:"push_target"`

	SpecContentHash string `json:"spec_content_hash"`
	SpecVersion     int    `json:"spec_version"`

	// Context is the F1 self-contained context block (§B): resolved identity +
	// task/spec/acceptance/base_sha/prior_verdict. The harness writes Task/Spec/
	// Acceptance into the worktree and exports them as env so any agent CLI reads
	// the task without knowing Flowbee. Nil for an old server (back-compat).
	Context *LeaseContext `json:"context,omitempty"`
}

// LeaseContext mirrors the server's F1 context block (kept self-contained so the
// worker client imports no internal package). Every field is a RESOLVED fact: the
// worker acts AS Identity and cannot choose its own (fenced by the server).
type LeaseContext struct {
	Identity           string         `json:"identity"`
	ModelFamily        string         `json:"model_family,omitempty"`
	Lens               string         `json:"lens,omitempty"`
	Role               string         `json:"role"`
	BaseSHA            string         `json:"base_sha,omitempty"`
	Task               string         `json:"task,omitempty"`
	Spec               string         `json:"spec,omitempty"`
	AcceptanceCriteria string         `json:"acceptance_criteria,omitempty"`
	PriorVerdict       map[string]any `json:"prior_verdict,omitempty"`
	// PriorReviewFindings is the most recent code-review's changes-requested findings,
	// carried to a rebuild so the agent fixes what was flagged (§F compounding memory).
	PriorReviewFindings string `json:"prior_review_findings,omitempty"`
	// CIFailures names the checks that failed CI on the prior attempt (newline-separated,
	// e.g. "Architecture and guardrail lints\ngolangci-lint"), carried to a rebuild so the
	// brief tells the agent the exact gate to re-run + fix rather than rebuilding blind.
	CIFailures string `json:"ci_failures,omitempty"`
	// StuckHint is the Rung-E advisor's note (0024) carried into a build re-armed out of
	// needs_human by the advisor — "what was tried / try this" for fresh-context re-entry.
	StuckHint string `json:"stuck_hint,omitempty"`
	// Diff is the eng_worker's build patch, shipped to a code_reviewer so its agent
	// can judge the actual change (the review harness writes it to .flowbee/diff.patch
	// + $FLOWBEE_DIFF). Empty for non-review roles.
	Diff string `json:"diff,omitempty"`
	// DiffEmpty is true when the control plane authoritatively computed an empty PR
	// diff. It distinguishes an empty adopted PR from a legacy missing diff.
	DiffEmpty bool `json:"diff_empty,omitempty"`
	// CIReady is true when the reconciled facts are green (PR exists, CI green): a
	// code_reviewer should only judge once CI is green, else its approval can't mint
	// and it bounces — so the harness skips+releases until this is true.
	CIReady bool `json:"ci_ready,omitempty"`
	// IssueBranch is the per-issue branch every node commits to (flowbee/issue-N).
	// The worker-push harness fetches it, commits its work (a builder's change, a
	// reviewer's empty findings-commit) on top, and pushes it back — so the branch
	// history is the node-by-node story. Empty when the job has no bound issue yet.
	IssueBranch string `json:"issue_branch,omitempty"`
	// Rebuild is true when this build job has been bounced before (a prior attempt
	// FAILED CI or a reviewer requested changes). The harness brief then tells the
	// agent the prior change is already in the worktree and to FIX the build/lint/test
	// failures (run the linter/tests) rather than re-submit the same thing — otherwise
	// a CI-failing change just loops to needs_human with no feedback.
	Rebuild bool `json:"rebuild,omitempty"`
	// Conflict is true for a conflict_resolver lease (resolving_conflict): the worktree
	// is at the CURRENT main (a sibling merged a change to the same area since this work
	// was built), and Diff carries this job's ORIGINAL intended change. The harness brief
	// tells the agent to re-apply that intent on top of the current code, reconciling it
	// with the sibling change — NOT to blindly re-run the original task (whose target may
	// no longer exist, which is the conflict). Without this the resolver re-runs the build
	// task and "produces no changes".
	Conflict bool `json:"conflict,omitempty"`
	// RepoURL is the git clone/push URL for THIS job's repo (F9 multi-repo). A
	// fungible worker leases jobs across repos, so the control plane tells it which
	// repo each job belongs to — the worker-push harness clones/fetches/pushes here
	// (with its own git credential), and derives its local mirror path per repo. Empty
	// in single-repo deployments (the worker falls back to its configured --repo-url).
	RepoURL string `json:"repo_url,omitempty"`
}

// Lease long-polls for a lease. ok=false means a 204 (no work this round).
func (c *Client) Lease(ctx context.Context, identity, family, role string) (LeaseGrant, bool, error) {
	return c.LeaseWithLens(ctx, identity, family, role, "")
}

// LeaseWithLens long-polls carrying the worker's lens (the §5.5 distinct-lens
// anti-affinity input for spec_review). ok=false means a 204.
func (c *Client) LeaseWithLens(ctx context.Context, identity, family, role, lens string) (LeaseGrant, bool, error) {
	return c.leaseWithLens(ctx, identity, family, role, lens, false)
}

// LeaseDryRun returns the grant that would be offered without claiming the job.
func (c *Client) LeaseDryRun(ctx context.Context, identity, family, role string) (LeaseGrant, bool, error) {
	return c.leaseWithLens(ctx, identity, family, role, "", true)
}

func (c *Client) leaseWithLens(ctx context.Context, identity, family, role, lens string, dryRun bool) (LeaseGrant, bool, error) {
	q := url.Values{}
	q.Set("identity", identity)
	q.Set("model_family", family)
	if c.Model != "" {
		q.Set("model", c.Model)
	}
	// F6 capacity: declare this worker's agent login so dispatch can gate it OUT when the
	// account is rate-limited (the per-account ceiling). Empty/unset => never gated.
	if acct := os.Getenv("FLOWBEE_ACCOUNT"); acct != "" {
		q.Set("account_id", acct)
	}
	if role != "" {
		q.Set("role", role)
	}
	if lens != "" {
		q.Set("lens", lens)
	}
	if dryRun {
		q.Set("dry_run", "1")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/lease?"+q.Encode(), nil)
	if err != nil {
		return LeaseGrant{}, false, err
	}
	c.authHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return LeaseGrant{}, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return LeaseGrant{}, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return LeaseGrant{}, false, statusErr(resp)
	}
	var g LeaseGrant
	if err := json.NewDecoder(resp.Body).Decode(&g); err != nil {
		return LeaseGrant{}, false, err
	}
	return g, true, nil
}

// Bundle fetches the read-only git bundle of a job's base SHA (F3, §7.4 mode (a)):
// the credential-less cross-box provisioning channel. The worker clones a working
// tree from the returned bytes WITHOUT any GitHub credential and returns only a
// diff; Flowbee performs every git write. Returns the raw bundle bytes.
func (c *Client) Bundle(ctx context.Context, jobID string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/jobs/"+jobID+"/bundle", nil)
	if err != nil {
		return nil, err
	}
	c.authHeader(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, statusErr(resp)
	}
	return io.ReadAll(resp.Body)
}

// Heartbeat sends a bare fenced heartbeat. status is the HTTP status (409 = stale).
func (c *Client) Heartbeat(ctx context.Context, jobID string, epoch int) (directive string, status int, err error) {
	return c.HeartbeatWith(ctx, jobID, epoch, HeartbeatObs{})
}

// HeartbeatObs carries the §10 liveness observations a worker reports (all HINTS,
// I-13) + the two §10.6 fast-path flags. The zero value is a bare ping.
type HeartbeatObs struct {
	AgentHealth   string `json:"agent_health,omitempty"`
	Rung1Class    string `json:"rung1_class,omitempty"`
	AwaitingInput bool   `json:"awaiting_input,omitempty"`
	AgentExited   bool   `json:"agent_exited,omitempty"`
	// M10 cost report (§6.7, I-15): the {tokens_in, tokens_out, $} DELTA since the
	// last heartbeat. $ is MICRO-USD ($1.00 = 1_000_000) so the meter is exact. A
	// delta crossing the per-job ceiling escalates the job to needs_human and the
	// directive comes back `cancel`.
	TokensInDelta  int64 `json:"tokens_in_delta,omitempty"`
	TokensOutDelta int64 `json:"tokens_out_delta,omitempty"`
	MicroUSDDelta  int64 `json:"micro_usd_delta,omitempty"`
}

// HeartbeatWith sends a fenced heartbeat carrying liveness observations. A `cancel`
// directive (§10.6 fast-path, or a two-rung kill that already revoked) tells the
// worker to stop. status is the HTTP status (409 = stale).
func (c *Client) HeartbeatWith(ctx context.Context, jobID string, epoch int, obs HeartbeatObs) (directive string, status int, err error) {
	var out struct {
		Directive string `json:"directive"`
	}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/heartbeat", epochHeader(epoch), obs, &out)
	return out.Directive, st, err
}

// ResultResponse is the result POST reply.
type ResultResponse struct {
	Accepted bool   `json:"accepted"`
	JobState string `json:"job_state"`
}

// Result posts a fenced, idempotent work-product result. status is the HTTP
// status (409 = stale).
func (c *Client) Result(ctx context.Context, jobID string, epoch int, idemKey string, body any) (ResultResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out ResultResponse
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/result", h, body, &out)
	return out, st, err
}

// ReviewResponse is the code-review gate reply.
type ReviewResponse struct {
	Accepted bool   `json:"accepted"`
	JobState string `json:"job_state"`
	Verdict  string `json:"verdict"`
	Minted   bool   `json:"minted"`
}

// Review posts a fenced code-review result: the reviewer's verdict CLAIM +
// requested disposition (untrusted; the server's gate decides from reconciled
// facts, I-9). status is the HTTP status (409 = stale).
func (c *Client) Review(ctx context.Context, jobID string, epoch int, idemKey, verdict, disposition, notes, headSHA string) (ReviewResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out ReviewResponse
	body := map[string]string{"verdict": verdict, "disposition": disposition, "notes": notes}
	// head_sha is the issue-branch HEAD the reviewer just advanced with its empty findings-
	// commit (empty when it pushed nothing). Reported so the server can track the move and an
	// N>1 consensus panel's accumulate round isn't superseded by the reviewer's own commit.
	if headSHA != "" {
		body["head_sha"] = headSHA
	}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/review", h, body, &out)
	return out, st, err
}

// SpecSubmit posts the spec_author's draft prose (§11.6). Flowbee commits it and
// computes the content hash; the response carries the hash the reviewer will bind
// to. status is the HTTP status (409 = stale).
func (c *Client) SpecSubmit(ctx context.Context, jobID string, epoch int, specMarkdown string, version int) (hash string, vers int, status int, err error) {
	var out struct {
		Accepted        bool   `json:"accepted"`
		SpecContentHash string `json:"spec_content_hash"`
		SpecVersion     int    `json:"spec_version"`
	}
	st, e := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/spec", epochHeader(epoch),
		map[string]any{"spec_markdown": specMarkdown, "version": version}, &out)
	return out.SpecContentHash, out.SpecVersion, st, e
}

// SpecReviewResponse is the spec gate reply.
type SpecReviewResponse struct {
	Accepted    bool   `json:"accepted"`
	JobState    string `json:"job_state"`
	Minted      bool   `json:"minted"`
	Superseded  bool   `json:"superseded"`
	Amended     bool   `json:"amended"`      // F4: amended in place + signed off (no author bounce)
	NeedsDesign bool   `json:"needs_design"` // F4: design fork -> needs_design
}

// SpecReview posts a fenced spec-review verdict CLAIM + sub-checks + the hash it
// judged (§11.5). The server's gate decides from the CURRENT bytes (I-9). status
// is the HTTP status (409 = stale).
func (c *Client) SpecReview(ctx context.Context, jobID string, epoch int, idemKey, decision, bindsTo string, meetsStyle, meetsReq bool) (SpecReviewResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out SpecReviewResponse
	body := map[string]any{
		"decision": decision, "binds_to": bindsTo,
		"meets_style": meetsStyle, "meets_requirements": meetsReq,
	}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/spec-review", h, body, &out)
	return out, st, err
}

// SpecReviewAmend is the F4 issue-review AMEND-in-place result: the reviewer judges
// the spec sub-standard and supplies the AMENDED prose rather than bouncing to the
// author. Flowbee commits the bytes (computes the hash) and mints a sign-off bound to
// the amended hash. Issue-review never bounces to the author.
func (c *Client) SpecReviewAmend(ctx context.Context, jobID string, epoch int, idemKey, bindsTo, amendedMarkdown string, amendedVersion int) (SpecReviewResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out SpecReviewResponse
	body := map[string]any{
		"decision": "amended", "binds_to": bindsTo,
		"amended_spec_markdown": amendedMarkdown, "amended_version": amendedVersion,
	}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/spec-review", h, body, &out)
	return out, st, err
}

// SpecReviewNeedsDesign is the F4 design-fork escalation: the reviewer flags that
// the spec needs human DESIGN input (issue-review cannot resolve it by amending).
// The job parks in needs_design (surfaced on /v1/needs-input).
func (c *Client) SpecReviewNeedsDesign(ctx context.Context, jobID string, epoch int, idemKey, bindsTo string) (SpecReviewResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out SpecReviewResponse
	body := map[string]any{"decision": "needs_design", "binds_to": bindsTo}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/spec-review", h, body, &out)
	return out, st, err
}

// Release posts a fenced release. status is the HTTP status (409 = stale).
func (c *Client) Release(ctx context.Context, jobID string, epoch int) (status int, err error) {
	var out map[string]bool
	return c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/release", epochHeader(epoch), nil, &out)
}

// Requeue re-arms a stranded job (escalated to needs_human from a now-fixed
// transient failure) for a fresh attempt: reset attempts/bounces, back to ready.
func (c *Client) Requeue(ctx context.Context, jobID string, force bool) (status int, err error) {
	var out map[string]string
	path := "/v1/jobs/" + jobID + "/requeue"
	if force {
		path += "?force=true"
	}
	return c.postJSONStatus(ctx, path, nil, nil, &out)
}

func (c *Client) Cancel(ctx context.Context, jobID string, force bool) (status int, err error) {
	var out map[string]string
	path := "/v1/jobs/" + jobID + "/cancel"
	if force {
		path += "?force=true"
	}
	return c.postJSONStatus(ctx, path, nil, nil, &out)
}

// AdoptPR imports a pre-existing PR (one Flowbee did not originate) into the named
// repo's review pipeline via POST /v1/adopt. Returns the new/re-armed adopted job id,
// whether the PR was already tracked (idempotent no-op), whether an existing adopted
// job was re-armed after a SHA move, and the HTTP status. repo may be "" when exactly
// one repo is registered; with 2+ repos the server requires it.
func (c *Client) AdoptPR(ctx context.Context, repo string, prNumber int) (jobID string, alreadyTracked bool, rearmed bool, status int, err error) {
	var out struct {
		JobID          string `json:"job_id"`
		AlreadyTracked bool   `json:"already_tracked"`
		Rearmed        bool   `json:"rearmed"`
	}
	st, e := c.postJSONStatus(ctx, "/v1/adopt", nil,
		map[string]any{"repo": repo, "pr": prNumber}, &out)
	return out.JobID, out.AlreadyTracked, out.Rearmed, st, e
}

// SpecRequest is the /v1/specs intake payload (the planner front door): a work item a
// spec_author drafts into a spec. Repo defaults to the primary registered repo.
type SpecRequest struct {
	Task       string `json:"task"`
	Title      string `json:"title,omitempty"`
	Acceptance string `json:"acceptance,omitempty"`
	Repo       string `json:"repo,omitempty"`
}

// CreateSpec POSTs a work item to /v1/specs; the control plane seeds a spec job that a
// spec_author drafts, an issue-reviewer signs off, and which then materializes a GitHub
// issue -> a build. Returns the spec job id + its initial state.
func (c *Client) CreateSpec(ctx context.Context, req SpecRequest) (jobID, state string, err error) {
	var out struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	st, err := c.postJSONStatus(ctx, "/v1/specs", nil, req, &out)
	if err != nil {
		return "", "", err
	}
	if st != 200 {
		return "", "", fmt.Errorf("create spec: status %d", st)
	}
	return out.JobID, out.State, nil
}

// ReleaseNoPenalty re-arms WITHOUT burning an attempt — for a non-failure abandon
// (the worker built fine but lost a fast-forward race to a branch move). Keeps the
// attempt budget for genuine build failures so re-validation churn can't escalate a
// good change to needs_human.
func (c *Client) ReleaseNoPenalty(ctx context.Context, jobID string, epoch int) (status int, err error) {
	var out map[string]bool
	return c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/release?keep=1", epochHeader(epoch), nil, &out)
}

// ReleaseFailed posts a release that BURNS an attempt even for a penalty-free gate state —
// for a genuine reviewer failure (the agent produced no parseable verdict). It lets a
// persistently-broken reviewer escalate to needs_human after max_attempts instead of
// churning claim↔release, the review-path analogue of the build's no-output abandon.
func (c *Client) ReleaseFailed(ctx context.Context, jobID string, epoch int) (status int, err error) {
	var out map[string]bool
	return c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/release?fail=1", epochHeader(epoch), nil, &out)
}

// ReportRebaseConflict tells the control plane that this build's branch patch does NOT
// apply onto the granted base (a real conflict with merged work). The control plane
// diverts the job to a conflict_resolver — which re-applies the supplied branch change
// onto current main + resolves the markers — instead of the build looping to needs_human.
// baseSHA is the base the rebase targeted (the conflicting head); diff is the branch's
// own change for the resolver to re-apply. A stale epoch returns 409.
func (c *Client) ReportRebaseConflict(ctx context.Context, jobID string, epoch int, baseSHA, diff string) (status int, err error) {
	var out map[string]bool
	body := map[string]string{"base_sha": baseSHA, "diff": diff}
	return c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/rebase-conflict", epochHeader(epoch), body, &out)
}

func epochHeader(epoch int) map[string]string {
	return map[string]string{"X-Lease-Epoch": strconv.Itoa(epoch)}
}

func (c *Client) postJSON(ctx context.Context, path string, headers map[string]string, body, out any) error {
	_, err := c.postJSONStatus(ctx, path, headers, body, out)
	return err
}

// Pause tells the dispatcher to stop handing out new work. An empty repo pauses dispatch
// GLOBALLY ("pause everything"); a repo id parks just that repo (other repos keep flowing).
// Running jobs are never interrupted. Idempotent.
func (c *Client) Pause(ctx context.Context, repo string) error {
	return c.postJSON(ctx, "/v1/control/pause", nil, map[string]string{"repo": repo}, nil)
}

// Resume is the inverse of Pause (resume global dispatch or a single repo).
func (c *Client) Resume(ctx context.Context, repo string) error {
	return c.postJSON(ctx, "/v1/control/resume", nil, map[string]string{"repo": repo}, nil)
}

func (c *Client) postJSONStatus(ctx context.Context, path string, headers map[string]string, body, out any) (int, error) {
	var buf io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, buf)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authHeader(req)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	// Auth failures are surfaced as errors: a worker that is not an enrolled
	// identity (§7.6) must SEE the rejection, not silently treat it as a no-op.
	// (Fencing 409s are NOT errors here — callers branch on the returned status.)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return resp.StatusCode, statusErr(resp)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func statusErr(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, bytes.TrimSpace(b))
}
