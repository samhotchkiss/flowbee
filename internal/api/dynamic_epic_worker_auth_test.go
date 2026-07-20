package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

func insertDynamicEpicWorker(t *testing.T, st *store.Store, epicID, role, family,
	credentialID string, expiresAt, now time.Time) string {
	t.Helper()
	ctx := context.Background()
	identity := store.EpicWorkerFlowbeeIdentity(role, epicID)
	driverIdentity := "driver:" + epicID + ":" + role
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epics
		(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
		VALUES (?,?,?,'launching','default',?,?,?,?)`, epicID, "", "epics/"+epicID+".md",
		epicID, "dynamic-auth:"+epicID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_worker_sessions
		(epic_id,project_id,worker_role,model_family,worker_identity,flowbee_identity,
		 lifecycle_key,display_name,state,target_epoch,bootstrap_payload,bootstrap_sha256,
		 created_at,updated_at)
		VALUES (?,'default',?,?,?,?,?,?,'active',1,'{}','sha256:fixture',?,?)`, epicID,
		role, family, driverIdentity, identity, "lifecycle:"+epicID+":"+role,
		"worker-"+epicID+"-"+role, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_worker_credentials
		(epic_id,project_id,worker_role,flowbee_identity,install_ref,state,generation,
		 envelope_ref,issued_at,refresh_after,expires_at,created_at,updated_at)
		VALUES (?,'default',?,?,?,'issued',1,?,?,?,?,?,?)`, epicID, role, identity,
		"flowbee://worker-credentials/default/"+epicID+"/"+role, credentialID, stamp,
		now.Add(30*time.Minute).UTC().Format(time.RFC3339Nano),
		expiresAt.UTC().Format(time.RFC3339Nano), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	driverRole := store.DriverBuilderRole
	provider := "codex"
	if role == "reviewer" {
		driverRole, provider = store.DriverReviewerRole, "grok"
	}
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: driverIdentity, Role: driverRole,
		HostID: "dynamic-host", StoreID: "dynamic-store-" + epicID,
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "dynamic-server-" + epicID,
		LifecycleOwnership: "driver_managed", LifecycleKey: "lifecycle:" + epicID + ":" + role,
		TargetEpoch: 1, ProfileID: provider + "_" + role, WorkspaceRootID: "dynamic-root",
		WorkspaceRelativePath: "repos/" + epicID, SessionID: "dynamic-session-" + epicID,
		PaneInstanceID: "dynamic-pane-" + epicID, AgentRunID: "dynamic-run-" + epicID,
		Provider: provider, ObservedAt: now,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_sessions SET binding_id=?
		WHERE epic_id=? AND worker_role=?`, binding.BindingID, epicID, role); err != nil {
		t.Fatal(err)
	}
	return identity
}

func dynamicWorkerServer(t *testing.T, st *store.Store, clk *clock.Fake,
	authn auth.Authenticator) *httptest.Server {
	t.Helper()
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL:     time.Minute,
		LongPollWait: 20 * time.Millisecond,
		LeaseTTLS:    60, HeartbeatIntervalS: 30,
		Authenticator: authn,
		// This is the production posture: no permissive fallback and no static
		// enrollment for the per-epic identities created after serve starts.
		Allowlist: worker.Allowlist{Permit: map[string][]string{}},
	}, "dynamic-worker-auth")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	return ts
}

func TestProductionDynamicEpicWorkerAuthoritySurvivesRestartAndRevokesImmediately(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	clk := clock.NewFake(now)
	expires := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	builderCredential, reviewerCredential := "credential-builder-v1", "credential-reviewer-v1"
	builderID := insertDynamicEpicWorker(t, st, "dynamic-builder", "builder", "codex",
		builderCredential, expires, now)
	reviewerID := insertDynamicEpicWorker(t, st, "dynamic-reviewer", "reviewer", "grok",
		reviewerCredential, expires, now)

	secret := []byte("production-dynamic-worker-test-secret")
	bearer := auth.NewBearer(secret, nil, false).WithNow(clk.Now).
		WithCredentialVerifier(func(claims auth.CredentialClaims, observedAt time.Time) bool {
			return st.AuthorizeEpicWorkerCredential(ctx, claims.Identity, claims.ProjectID,
				claims.WorkerRole, claims.CredentialID, claims.Generation, observedAt)
		})
	builderToken := bearer.MintCredential(builderID, "default", "builder", builderCredential, 1, expires)
	reviewerToken := bearer.MintCredential(reviewerID, "default", "reviewer", reviewerCredential, 1, expires)
	ts := dynamicWorkerServer(t, st, clk, bearer)
	builderClient := client.NewWithToken(ts.URL, builderToken)
	reviewerClient := client.NewWithToken(ts.URL, reviewerToken)

	spoofBody, err := json.Marshal(client.Registration{Identity: reviewerID, Host: "local",
		Capabilities: []string{"role:code_reviewer"}})
	if err != nil {
		t.Fatal(err)
	}
	spoofRequest, err := http.NewRequestWithContext(ctx, http.MethodPost,
		ts.URL+"/v1/workers/register", bytes.NewReader(spoofBody))
	if err != nil {
		t.Fatal(err)
	}
	spoofRequest.Header.Set("Authorization", "Bearer "+builderToken)
	spoofRequest.Header.Set("Content-Type", "application/json")
	spoofResponse, err := builderClient.HTTP.Do(spoofRequest)
	if err != nil {
		t.Fatal(err)
	}
	if spoofResponse.StatusCode != http.StatusForbidden {
		_ = spoofResponse.Body.Close()
		t.Fatalf("registration identity spoof status=%d want 403", spoofResponse.StatusCode)
	}
	_ = spoofResponse.Body.Close()

	register := func(c *client.Client, identity string, claims []string) client.RegisterResponse {
		t.Helper()
		resp, err := c.Register(ctx, client.Registration{Identity: identity, Host: "local",
			Capabilities: claims})
		if err != nil {
			t.Fatalf("register %s: %v", identity, err)
		}
		sort.Strings(resp.AttestedCapabilities)
		return resp
	}
	allClaims := []string{"role:eng_worker", "role:code_reviewer", "model_family:codex", "model_family:grok"}
	builderRegistration := register(builderClient, builderID, allClaims)
	reviewerRegistration := register(reviewerClient, reviewerID, allClaims)
	if want := []string{"model_family:codex", "role:eng_worker"}; !reflect.DeepEqual(builderRegistration.AttestedCapabilities, want) {
		t.Fatalf("builder spoofed authority: got %v want %v", builderRegistration.AttestedCapabilities, want)
	}
	if want := []string{"model_family:grok", "role:code_reviewer"}; !reflect.DeepEqual(reviewerRegistration.AttestedCapabilities, want) {
		t.Fatalf("reviewer spoofed authority: got %v want %v", reviewerRegistration.AttestedCapabilities, want)
	}

	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "dynamic-build-job", Kind: job.KindBuild,
		Flow: "build", Stage: "build", Role: job.RoleEngWorker, BaseSHA: "base",
		RequiredCapabilities: []string{"role:eng_worker", "model_family:codex"}, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "dynamic-review-job", Kind: job.KindBuild,
		Flow: "build", Stage: "review", Role: job.RoleCodeReviewer, BaseSHA: "base",
		RequiredCapabilities: []string{"role:code_reviewer", "model_family:grok"}, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs
		SET state='review_pending', head_sha='review-head', base_sha='review-base'
		WHERE id='dynamic-review-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO domain_b_facts
		(job_id, pr_exists, pr_number, head_sha, base_sha, ci_green, merged, updated_at)
		VALUES ('dynamic-review-job', 1, 42, 'review-head', 'review-base', 1, 0, ?)`, now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	// The family query is deliberately spoofed. Registry authority clamps it back
	// to codex, and the builder can see only the build role.
	if grant, ok, err := builderClient.LeaseDryRun(ctx, builderID, "grok", "eng_worker"); err != nil || !ok || grant.JobID != "dynamic-build-job" || grant.Context.ModelFamily != "codex" {
		t.Fatalf("builder lease: grant=%+v ok=%v err=%v", grant, ok, err)
	}
	if _, ok, err := builderClient.LeaseDryRun(ctx, builderID, "codex", "code_reviewer"); err != nil || ok {
		t.Fatalf("builder escaped into review role: ok=%v err=%v", ok, err)
	}
	if grant, ok, err := reviewerClient.LeaseDryRun(ctx, reviewerID, "codex", "code_reviewer"); err != nil || !ok || grant.JobID != "dynamic-review-job" || grant.Context.ModelFamily != "grok" {
		t.Fatalf("reviewer lease: grant=%+v ok=%v err=%v", grant, ok, err)
	}
	if _, ok, err := reviewerClient.LeaseDryRun(ctx, reviewerID, "grok", "eng_worker"); err != nil || ok {
		t.Fatalf("reviewer escaped into builder role: ok=%v err=%v", ok, err)
	}

	// A fresh server/Registry instance must derive the same authority from durable
	// worker state, without adding either identity to the static config allowlist.
	ts.Close()
	ts = dynamicWorkerServer(t, st, clk, bearer)
	builderClient = client.NewWithToken(ts.URL, builderToken)
	reviewerClient = client.NewWithToken(ts.URL, reviewerToken)
	restarted := register(builderClient, builderID, allClaims)
	if restarted.WorkerID != builderRegistration.WorkerID ||
		!reflect.DeepEqual(restarted.AttestedCapabilities, []string{"model_family:codex", "role:eng_worker"}) {
		t.Fatalf("restart authority=%+v original=%+v", restarted, builderRegistration)
	}
	clk.Advance(48 * time.Hour)
	if after24h := register(builderClient, builderID, allClaims); after24h.WorkerID != builderRegistration.WorkerID {
		t.Fatalf("active exact worker self-deauthorized after 24h: %+v", after24h)
	}

	leaseStatus := func(c *client.Client) int {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/lease?role=eng_worker", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	assertUnauthorized := func(c *client.Client, label string) {
		t.Helper()
		if status := leaseStatus(c); status != http.StatusUnauthorized {
			t.Fatalf("%s status=%d want 401", label, status)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET expires_at=?
		WHERE epic_id='dynamic-builder' AND worker_role='builder'`, now.Add(-time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(builderClient, "durably expired credential")

	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='revoked',revoked_at=?,updated_at=?
		WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`, now.Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(reviewerClient, "revoked credential")

	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET state='issued',revoked_at='',updated_at=?
		WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if status := leaseStatus(reviewerClient); status != http.StatusNoContent {
		t.Fatalf("restored credential status=%d want 204 before worker stop", status)
	}
	var reviewerBindingID string
	if err := st.DB.QueryRowContext(ctx, `SELECT binding_id FROM epic_worker_sessions
		WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`).Scan(&reviewerBindingID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded',updated_at=?
		WHERE binding_id=?`, now.Format(time.RFC3339Nano), reviewerBindingID); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(reviewerClient, "superseded exact worker binding")
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET state='active',superseded_at='',updated_at=?
		WHERE binding_id=?`, now.Format(time.RFC3339Nano), reviewerBindingID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET generation=2,
		envelope_ref='credential-reviewer-v2',updated_at=?
		WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(reviewerClient, "old generation after worker replacement")
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_credentials SET generation=1,
		envelope_ref=?,updated_at=? WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`,
		reviewerCredential, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_worker_sessions SET state='stopped',stopped_at=?,updated_at=?
		WHERE epic_id='dynamic-reviewer' AND worker_role='reviewer'`, now.Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	assertUnauthorized(reviewerClient, "stopped worker")
}
