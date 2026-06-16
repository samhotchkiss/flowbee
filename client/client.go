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
}

func New(baseURL string) *Client {
	return &Client{BaseURL: baseURL, HTTP: http.DefaultClient}
}

// Registration mirrors the server-side enrollment payload.
type Registration struct {
	WorkerID     string   `json:"worker_id"`
	Identity     string   `json:"identity"`
	Host         string   `json:"host"`
	Capabilities []string `json:"capabilities"`
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
	JobID      string `json:"job_id"`
	Kind       string `json:"kind"`
	Role       string `json:"role"`
	BaseSHA    string `json:"base_sha"`
	LeaseID    string `json:"lease_id"`
	LeaseEpoch int    `json:"lease_epoch"`
	LeaseTTLS  int    `json:"lease_ttl_s"`
	Deadline   string `json:"lease_deadline"`
}

// Lease long-polls for a lease. ok=false means a 204 (no work this round).
func (c *Client) Lease(ctx context.Context, identity, family, role string) (LeaseGrant, bool, error) {
	q := url.Values{}
	q.Set("identity", identity)
	q.Set("model_family", family)
	if role != "" {
		q.Set("role", role)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/lease?"+q.Encode(), nil)
	if err != nil {
		return LeaseGrant{}, false, err
	}
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

// Heartbeat sends a fenced heartbeat. status is the HTTP status (409 = stale).
func (c *Client) Heartbeat(ctx context.Context, jobID string, epoch int) (directive string, status int, err error) {
	var out struct {
		Directive string `json:"directive"`
	}
	st, err := c.postJSONStatus(ctx, "/v1/jobs/"+jobID+"/heartbeat", epochHeader(epoch), nil, &out)
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
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
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
