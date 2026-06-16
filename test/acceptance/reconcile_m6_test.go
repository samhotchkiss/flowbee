// M6 acceptance: GitHub reconcile-IN + webhook inbox + App-identity budget gauge,
// proven end-to-end against an in-memory fakeGitHub (BUILD.md §6.4 — no real creds,
// no network) over the real HTTP surfaces (private worker API + the public webhook
// listener).
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a sweep populates Domain-B columns to match a scripted repo;
//   - a forged/replayed webhook is rejected/deduped, at worst triggers a refetch of
//     real state (never fast-tracks a state);
//   - a new commit to an open PR -> job superseded + re-armed;
//   - the identity-budget gauge is live;
//   - reconcile-IN never writes a Domain-A field.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/webhook"
)

const webhookSecret = "m6-secret"

// m6Env wires the control plane (private API), a reconciler over a scriptable
// fakeGitHub, and the public webhook listener — the M6 topology, in-process.
type m6Env struct {
	st         *store.Store
	fake       *gh.Fake
	rec        *reconcile.Reconciler
	private    *httptest.Server
	webhookSrv *httptest.Server
}

func newM6Env(t *testing.T) *m6Env {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(1_000_000, 0))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 500 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "m6")
	fake := gh.NewFake()
	rec := reconcile.New(st, fake, clk, srv.Broker())
	wh := webhook.New(webhookSecret, st, rec)

	private := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(private.Close)
	whMux := http.NewServeMux()
	whMux.Handle("POST /webhooks", wh)
	whSrv := httptest.NewServer(whMux)
	t.Cleanup(whSrv.Close)

	return &m6Env{st: st, fake: fake, rec: rec, private: private, webhookSrv: whSrv}
}

// seedBoundBuild seeds a build job and binds it to a GitHub PR number.
func (e *m6Env) seedBoundBuild(t *testing.T, id string, pr int, base string) {
	t.Helper()
	ctx := context.Background()
	if _, err := e.st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: base,
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := e.st.BindPRNumber(ctx, id, pr); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

// postWebhook sends a webhook to the public listener with the given signature.
func (e *m6Env) postWebhook(t *testing.T, delivery, event, body, sig string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, e.webhookSrv.URL+"/webhooks", strings.NewReader(body))
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-GitHub-Event", event)
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	return resp
}

// TestM6_SweepPopulatesDomainBToMatchRepo: a sweep populates Domain-B columns to
// match the scripted fakeGitHub repo, and the budget gauge goes live (I-14).
func TestM6_SweepPopulatesDomainBToMatchRepo(t *testing.T) {
	e := newM6Env(t)
	ctx := context.Background()
	e.seedBoundBuild(t, "b1", 501, "base0")
	e.seedBoundBuild(t, "b2", 502, "base0")

	e.fake.SetPR(gh.PullRequest{Number: 501, HeadRefOid: "sha-a", BaseRefOid: "base0", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(100, 0)})
	e.fake.SetPR(gh.PullRequest{Number: 502, HeadRefOid: "sha-b", BaseRefOid: "base0", CIRollup: gh.CIFailure, IsDraft: true, UpdatedAt: time.Unix(101, 0)})
	e.fake.SetRateLimit(gh.RateLimit{Limit: 5000, Remaining: 4800, ResetAt: time.Unix(2_000_000, 0)})

	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	src := store.DBFactSource{DB: e.st.DB}
	f1, ok, _ := src.Facts(ctx, "b1")
	if !ok || f1.HeadSHA != "sha-a" || f1.BaseSHA != "base0" || !f1.CIGreen || !f1.PRExists || f1.PRNumber != 501 {
		t.Fatalf("b1 Domain-B columns mismatch repo: %+v", f1)
	}
	f2, _, _ := src.Facts(ctx, "b2")
	if f2.HeadSHA != "sha-b" || f2.CIGreen {
		t.Fatalf("b2 Domain-B columns mismatch repo: %+v", f2)
	}

	// the budget gauge is live over the private API (I-14).
	resp, err := http.Get(e.private.URL + "/v1/budget")
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	defer resp.Body.Close()
	var g store.RateLimitGauge
	_ = json.NewDecoder(resp.Body).Decode(&g)
	if g.Remaining != 4800 || g.Limit != 5000 {
		t.Fatalf("budget gauge not live: %+v", g)
	}
}

// TestM6_ForgedWebhookRejected: an UNSIGNED / WRONG-signature webhook is 401'd and
// never reaches the inbox or triggers any state change (I-2). The endpoint is
// internet-reachable; it fails closed.
func TestM6_ForgedWebhookRejected(t *testing.T) {
	e := newM6Env(t)
	ctx := context.Background()
	e.seedBoundBuild(t, "f1", 600, "base0")
	e.fake.SetPR(gh.PullRequest{Number: 600, HeadRefOid: "h", BaseRefOid: "base0", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(50, 0)})

	body := `{"action":"closed","pull_request":{"number":600,"merged":true}}` // a LIE: claims merged
	// unsigned.
	if resp := e.postWebhook(t, "forge-1", "pull_request", body, ""); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("unsigned webhook: code=%d want 401", resp.StatusCode)
	}
	// wrong-secret signature.
	bad := webhook.Sign([]byte("not-the-secret"), []byte(body))
	if resp := e.postWebhook(t, "forge-1", "pull_request", body, bad); resp.StatusCode != http.StatusUnauthorized {
		resp.Body.Close()
		t.Fatalf("forged webhook: code=%d want 401", resp.StatusCode)
	}

	// nothing was reconciled: no fake calls, no facts, job untouched.
	if calls := e.fake.Calls(); len(calls) != 0 {
		t.Fatalf("forged webhook reached GitHub: calls=%v", calls)
	}
	if seen, _ := e.st.DeliverySeen(ctx, "forge-1"); seen {
		t.Fatalf("forged webhook reached the inbox")
	}
	if j, _ := e.st.GetJob(ctx, "f1"); j.State == job.StateDone {
		t.Fatalf("forged 'merged' webhook fast-tracked the job to done")
	}
}

// TestM6_ForgedContentCannotFastTrack: a CORRECTLY-SIGNED but LYING webhook (claims
// the PR is approved/merged) is accepted, recorded, and triggers a TARGETED refetch
// — which reads the REAL state (still open, CI red) and CANNOT fast-track (§8.1.3,
// I-9). The strongest a forged delivery achieves is a refetch of the truth.
func TestM6_ForgedContentCannotFastTrack(t *testing.T) {
	e := newM6Env(t)
	ctx := context.Background()
	e.seedBoundBuild(t, "fc", 700, "base0")
	// the REAL state: open, CI red, NOT merged.
	e.fake.SetPR(gh.PullRequest{Number: 700, HeadRefOid: "h", BaseRefOid: "base0", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(60, 0)})

	body := `{"action":"submitted","pull_request":{"number":700},"review":{"state":"approved"}}`
	sig := webhook.Sign([]byte(webhookSecret), []byte(body))
	resp := e.postWebhook(t, "lie-1", "pull_request_review", body, sig)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed webhook: code=%d", resp.StatusCode)
	}
	var ack map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ack)
	if ack["refetched"] != true {
		t.Fatalf("signed webhook did not trigger a refetch: %v", ack)
	}
	// the refetch read REAL state: facts say un-merged, CI red. No fast-track.
	f, _, _ := store.DBFactSource{DB: e.st.DB}.Facts(ctx, "fc")
	if f.Merged || f.CIGreen {
		t.Fatalf("forged content fast-tracked facts: %+v", f)
	}
	if j, _ := e.st.GetJob(ctx, "fc"); j.State == job.StateDone {
		t.Fatalf("forged 'approved' review merged the job")
	}
	// the refetch path WENT THROUGH GitHub (the doorbell rang the real bell).
	calls := e.fake.Calls()
	if len(calls) != 1 || calls[0] != "PullRequest(700)" {
		t.Fatalf("expected one targeted refetch, got %v", calls)
	}
}

// TestM6_ReplayedWebhookDeduped: a correctly-signed but REPLAYED delivery (same
// X-GitHub-Delivery) is deduped — the replay is a no-op, the first triggered a
// single refetch (I-2). Replaying cannot multiply effects.
func TestM6_ReplayedWebhookDeduped(t *testing.T) {
	e := newM6Env(t)
	e.seedBoundBuild(t, "rp", 800, "base0")
	e.fake.SetPR(gh.PullRequest{Number: 800, HeadRefOid: "h", BaseRefOid: "base0", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(70, 0)})

	body := `{"pull_request":{"number":800}}`
	sig := webhook.Sign([]byte(webhookSecret), []byte(body))

	r1 := e.postWebhook(t, "same-id", "pull_request", body, sig)
	b1 := readBody(t, r1)
	if !strings.Contains(b1, `"refetched":true`) {
		t.Fatalf("first delivery body=%s", b1)
	}
	r2 := e.postWebhook(t, "same-id", "pull_request", body, sig)
	b2 := readBody(t, r2)
	if !strings.Contains(b2, `"deduped":true`) {
		t.Fatalf("replay not deduped: body=%s", b2)
	}
	// exactly one refetch hit GitHub despite two deliveries.
	calls := e.fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("replay multiplied refetches: %v", calls)
	}
}

// TestM6_NewCommitSupersedesAndRearms: a new commit (head SHA move) to an open PR
// whose job holds a SHA-bound verdict -> the job is superseded and re-armed to
// ready (I-5, §6.2.4) — driven entirely by reconcile-IN seeing the GitHub-owned
// SHA change, with the lease epoch bumped (a still-running worker is fenced).
func TestM6_NewCommitSupersedesAndRearms(t *testing.T) {
	e := newM6Env(t)
	ctx := context.Background()
	e.seedBoundBuild(t, "nc", 900, "base0")

	// initial reconcile: head h1, CI green.
	e.fake.SetPR(gh.PullRequest{Number: 900, HeadRefOid: "h1", BaseRefOid: "base0", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(80, 0)})
	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	// drive the job to mergeable with a verdict bound to (h1, base0), epoch 3.
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", "base0")
	vb, _ := json.Marshal(v)
	if _, err := e.st.DB.ExecContext(ctx,
		`UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1', lease_epoch=3 WHERE id='nc'`, string(vb)); err != nil {
		t.Fatalf("set mergeable: %v", err)
	}

	// a NEW COMMIT lands on the open PR: head moves h1 -> h2.
	e.fake.SetPR(gh.PullRequest{Number: 900, HeadRefOid: "h2", BaseRefOid: "base0", CIRollup: gh.CIPending, UpdatedAt: time.Unix(90, 0)})
	outs, err := e.rec.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	var superseded bool
	for _, o := range outs {
		if o.JobID == "nc" && o.Superseded {
			superseded = true
		}
	}
	if !superseded {
		t.Fatalf("new commit did not supersede the job: outs=%+v", outs)
	}
	j, _ := e.st.GetJob(ctx, "nc")
	if j.State != job.StateReady {
		t.Fatalf("not re-armed: state=%s want ready", j.State)
	}
	if j.Verdict != nil {
		t.Fatalf("SHA-bound verdict not invalidated on supersession")
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("not re-armed to eng_worker: role=%s", j.Role)
	}
	if j.LeaseEpoch != 4 {
		t.Fatalf("lease epoch=%d want 4 (revoked: a still-running worker is now fenced)", j.LeaseEpoch)
	}
}

// TestM6_ReconcileNeverWritesDomainAField: the keystone asserted end-to-end. A job
// carrying live Domain-A state (code_review, bound reviewer, bumped counters) is
// reconciled with a Domain-B fact change (CI flips green at the SAME head) — and
// EVERY Domain-A field is byte-identical afterward. reconcile-IN owns Domain B and
// nothing else (§3.4, I-1).
func TestM6_ReconcileNeverWritesDomainAField(t *testing.T) {
	e := newM6Env(t)
	ctx := context.Background()
	e.seedBoundBuild(t, "da", 950, "base0")

	e.fake.SetPR(gh.PullRequest{Number: 950, HeadRefOid: "hh", BaseRefOid: "base0", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(40, 0)})
	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	// give the job real Domain-A state worth protecting.
	if _, err := e.st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='code_review', role='code_reviewer', stage='review',
		       bounces=1, attempts=2, bound_lens='critical_reviewer', lease_epoch=7
		 WHERE id='da'`); err != nil {
		t.Fatalf("setup domain-A: %v", err)
	}
	before := domainASnap(t, e.st, "da")

	// CI flips green at the SAME head SHA: a pure Domain-B update.
	e.fake.SetPR(gh.PullRequest{Number: 950, HeadRefOid: "hh", BaseRefOid: "base0", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(45, 0)})
	if _, err := e.rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep2: %v", err)
	}

	after := domainASnap(t, e.st, "da")
	if before != after {
		t.Fatalf("reconcile-IN wrote a Domain-A field:\n before=%+v\n after =%+v", before, after)
	}
	f, _, _ := store.DBFactSource{DB: e.st.DB}.Facts(ctx, "da")
	if !f.CIGreen {
		t.Fatalf("Domain-B CI fact not updated by reconcile")
	}
}

type daSnap struct {
	State, Role, Stage, Lens, Verdict string
	Bounces, Attempts, Epoch          int
}

func domainASnap(t *testing.T, st *store.Store, id string) daSnap {
	t.Helper()
	var d daSnap
	if err := st.DB.QueryRowContext(context.Background(), `
		SELECT state, role, stage, COALESCE(bound_lens,''), COALESCE(verdict,''),
		       bounces, attempts, lease_epoch FROM jobs WHERE id = ?`, id).
		Scan(&d.State, &d.Role, &d.Stage, &d.Lens, &d.Verdict, &d.Bounces, &d.Attempts, &d.Epoch); err != nil {
		t.Fatalf("snap: %v", err)
	}
	return d
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return buf.String()
}
