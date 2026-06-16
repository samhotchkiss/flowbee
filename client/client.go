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
	"strconv"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
	// BearerToken is the signed per-worker token (DESIGN §7.6) presented on every
	// call as Authorization: Bearer. Empty on a loopback dev client (the server's
	// loopback bypass accepts it); REQUIRED for a non-loopback listener.
	BearerToken string
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
}

// Lease long-polls for a lease. ok=false means a 204 (no work this round).
func (c *Client) Lease(ctx context.Context, identity, family, role string) (LeaseGrant, bool, error) {
	return c.LeaseWithLens(ctx, identity, family, role, "")
}

// LeaseWithLens long-polls carrying the worker's lens (the §5.5 distinct-lens
// anti-affinity input for spec_review). ok=false means a 204.
func (c *Client) LeaseWithLens(ctx context.Context, identity, family, role, lens string) (LeaseGrant, bool, error) {
	q := url.Values{}
	q.Set("identity", identity)
	q.Set("model_family", family)
	if role != "" {
		q.Set("role", role)
	}
	if lens != "" {
		q.Set("lens", lens)
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
func (c *Client) Review(ctx context.Context, jobID string, epoch int, idemKey, verdict, disposition string) (ReviewResponse, int, error) {
	h := epochHeader(epoch)
	if idemKey != "" {
		h["Idempotency-Key"] = idemKey
	}
	var out ReviewResponse
	body := map[string]string{"verdict": verdict, "disposition": disposition}
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
	Accepted   bool   `json:"accepted"`
	JobState   string `json:"job_state"`
	Minted     bool   `json:"minted"`
	Superseded bool   `json:"superseded"`
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

// Release posts a fenced release. status is the HTTP status (409 = stale).
func (c *Client) Release(ctx context.Context, jobID string, epoch int) (status int, err error) {
	var out map[string]bool
	return c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/release", epochHeader(epoch), nil, &out)
}

func epochHeader(epoch int) map[string]string {
	return map[string]string{"X-Lease-Epoch": strconv.Itoa(epoch)}
}

func (c *Client) postJSON(ctx context.Context, path string, headers map[string]string, body, out any) error {
	_, err := c.postJSONStatus(ctx, path, headers, body, out)
	return err
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
