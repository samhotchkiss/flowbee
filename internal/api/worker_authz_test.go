package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/worker"
)

func TestWorkerRegistrationBindsCredentialAndBlocksCapabilityEscalation(t *testing.T) {
	st := testutil.NewStore(t)
	now := time.Unix(1000, 0)
	authn := auth.NewBearer([]byte("server-secret"),
		[]string{"capacity-local", "reviewer-russ:grok"}, false)
	srv := api.New(st, clock.NewFake(now), ulid.NewMinter(nil), api.Config{
		Authenticator: authn,
		Allowlist: worker.Allowlist{Permit: map[string][]string{
			"capacity-local": {},
			"reviewer-russ":  {"role:code_reviewer", "model_family:grok"},
		}},
	}, "test")
	h := srv.PrivateHandler()
	token := authn.Mint("capacity-local")

	request := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/workers/register", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		h.ServeHTTP(rec, req)
		return rec
	}
	first := request(`{"identity":"","host":"collector","capabilities":[]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("bound registration status=%d body=%s", first.Code, first.Body.String())
	}
	var registered struct {
		WorkerID string `json:"worker_id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &registered); err != nil || registered.WorkerID == "" {
		t.Fatalf("registration response=%s err=%v", first.Body.String(), err)
	}

	escalation := request(`{"worker_id":"` + registered.WorkerID +
		`","identity":"reviewer-russ","capabilities":["role:code_reviewer","model_family:grok"]}`)
	if escalation.Code != http.StatusForbidden {
		t.Fatalf("cross-identity registration status=%d want 403 body=%s", escalation.Code, escalation.Body.String())
	}
	var identity, caps string
	if err := st.DB.QueryRowContext(context.Background(),
		`SELECT identity,attested_capabilities FROM workers WHERE worker_id=?`, registered.WorkerID).
		Scan(&identity, &caps); err != nil {
		t.Fatal(err)
	}
	if identity != "capacity-local" || caps != "null" {
		t.Fatalf("escalation changed durable worker: identity=%q caps=%s", identity, caps)
	}
}

func TestLoopbackBypassClientRegistrationBindsClaimedIdentity(t *testing.T) {
	st := testutil.NewStore(t)
	authn := auth.NewBearer([]byte("server-secret"), []string{"local-worker"}, true)
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		Authenticator: authn,
		Allowlist: worker.Allowlist{Permit: map[string][]string{
			"local-worker": {"role:eng_worker"},
		}},
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)

	registered, err := client.New(ts.URL).Register(context.Background(), client.Registration{
		Identity: "local-worker", Host: "localhost", Capabilities: []string{"role:eng_worker"},
	})
	if err != nil || len(registered.AttestedCapabilities) != 1 ||
		registered.AttestedCapabilities[0] != "role:eng_worker" {
		t.Fatalf("loopback registration=%+v err=%v", registered, err)
	}
	var identity string
	if err := st.DB.QueryRow(`SELECT identity FROM workers WHERE worker_id=?`, registered.WorkerID).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if identity != "local-worker" {
		t.Fatalf("loopback worker stored as %q", identity)
	}
}

func TestCapacityOnlyIdentityEndpointAuthorizationMatrix(t *testing.T) {
	st := testutil.NewStore(t)
	authn := auth.NewBearer([]byte("server-secret"), []string{"capacity-local", "builder"}, false)
	human := auth.NewHumanAccess([]byte("01234567890123456789012345678901"), authn, nil, false)
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{
		Authenticator: authn,
		HumanAccess:   human,
		Allowlist: worker.Allowlist{Permit: map[string][]string{
			"capacity-local": {},
			"builder":        {"role:eng_worker"},
		}},
	}, "test")
	h := srv.PrivateHandler()
	token := authn.Mint("capacity-local")

	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "capacity-authz-matrix")
		h.ServeHTTP(rec, req)
		return rec
	}
	allowed := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/workers/register", `{"host":"collector","capabilities":[]}`},
		{http.MethodPost, "/v1/workers/usage", `{"reports":[]}`},
		{http.MethodGet, "/v1/config", ""},
	}
	for _, endpoint := range allowed {
		if rec := request(endpoint.method, endpoint.path, endpoint.body); rec.Code != http.StatusOK {
			t.Errorf("allowed %s %s status=%d body=%s", endpoint.method, endpoint.path, rec.Code, rec.Body.String())
		}
	}

	denied := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/control/pause", `{}`},
		{http.MethodPost, "/v1/control/resume", `{}`},
		{http.MethodGet, "/v1/lease", ""},
		{http.MethodPost, "/v1/jobs/j/cancel", `{}`},
		{http.MethodPost, "/v1/jobs/j/requeue", `{}`},
		{http.MethodPost, "/v1/specs", `{"task":"x","acceptance":"y"}`},
		{http.MethodPost, "/v1/epics", `{}`},
		{http.MethodPost, "/v1/conversations/thread/messages/message/delivery", `{}`},
		{http.MethodPost, "/v1/masters/register", `{}`},
		{http.MethodPost, "/v1/decisions", `{"project_id":"default"}`},
	}
	for _, endpoint := range denied {
		rec := request(endpoint.method, endpoint.path, endpoint.body)
		if rec.Code != http.StatusForbidden {
			t.Errorf("capacity-only %s %s status=%d want 403 body=%s",
				endpoint.method, endpoint.path, rec.Code, rec.Body.String())
		}
	}
}
