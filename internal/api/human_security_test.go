package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

const humanTestSecret = "01234567890123456789012345678901"

func humanAPIServer(t *testing.T, access *auth.HumanAccess) *httptest.Server {
	t.Helper()
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC))
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{HumanAccess: access}, "human-security-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	t.Cleanup(ts.Close)
	return ts
}

func signedHumanSession(t *testing.T, access *auth.HumanAccess, identity, csrf string) string {
	t.Helper()
	now := time.Now().UTC()
	token, err := access.MintSession(identity, "session-"+identity, csrf, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func humanRequest(t *testing.T, client *http.Client, method, url string, body any, token, csrf, key string) (*http.Response, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Flowbee-Actor", "attacker-spoof")
	if csrf != "" {
		req.Header.Set("X-Flowbee-CSRF", csrf)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	decoded := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp, decoded
}

func TestHumanDecisionAPIProjectRoleCSRFScopeAndArtifactFences(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"sam": {{ProjectID: "default", Role: auth.HumanAdmin}},
	}, false)
	ts := humanAPIServer(t, access)
	token := signedHumanSession(t, access, "sam", "csrf-sam")
	sha := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	create := map[string]any{
		"id": "secure-decision", "project_id": "default", "kind": "authorization",
		"title": "Authorize deployment", "prompt": "Authorize exact artifact.",
		"expected_response_kinds": []string{"approve"}, "route_to": "human",
		"subject_artifact_ref": "artifact://deploy/1", "subject_version": 1,
		"subject_sha256": sha,
	}
	resp, _ := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions", create,
		token, "", "create-without-csrf")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie mutation without CSRF status=%d want 403", resp.StatusCode)
	}
	resp, got := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+"/v1/decisions", create,
		token, "csrf-sam", "create-secure")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", resp.StatusCode, got)
	}
	if got["decision"].(map[string]any)["requested_by"] != "sam" {
		t.Fatalf("spoofed actor reached store: %v", got)
	}

	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/decisions?project_id=other", nil,
		token, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-project read status=%d want 403", resp.StatusCode)
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet, ts.URL+"/v1/decisions", nil, token, "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("project grant widened to portfolio status=%d want 403", resp.StatusCode)
	}

	base := map[string]any{
		"project_id": "default", "request_version": 1, "subject_version": 1,
		"subject_sha256": sha, "value": map[string]any{"approved": true},
	}
	base["authorization_scope"] = "project:*"
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/decisions/secure-decision/approve", base, token, "csrf-sam", "broad-scope")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("broader authorization scope status=%d want 403", resp.StatusCode)
	}
	base["authorization_scope"] = "project:default"
	base["subject_sha256"] = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/decisions/secure-decision/approve", base, token, "csrf-sam", "changed-artifact")
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Fatalf("changed artifact status=%d want 412", resp.StatusCode)
	}
	base["subject_sha256"] = sha
	resp, got = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/decisions/secure-decision/approve", base, token, "csrf-sam", "exact-artifact")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exact approval status=%d body=%v", resp.StatusCode, got)
	}
	response := got["response"].(map[string]any)
	if response["actor_id"] != "sam" || response["authorization_scope"] != "project:default" {
		t.Fatalf("stored authority is not exact authenticated scope: %v", response)
	}
}

func TestHumanPhase1RoutesFailClosedAndHonorRoleMatrix(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"viewer": {{ProjectID: "default", Role: auth.HumanViewer}},
	}, false)
	ts := humanAPIServer(t, access)

	resp, _ := humanRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/default/work-intents", nil, "", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous Phase-1 route status=%d want 401", resp.StatusCode)
	}
	token := signedHumanSession(t, access, "viewer", "csrf-viewer")
	resp, _ = humanRequest(t, ts.Client(), http.MethodGet,
		ts.URL+"/v1/projects/default/work-intents", nil, token, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("viewer read status=%d", resp.StatusCode)
	}
	create := map[string]any{
		"source_message_id": "m1", "source_message_version": 1,
		"interactor_incarnation_id": "i1", "title": "unauthorized write",
		"artifact_ref":    "artifact://intent/1",
		"artifact_sha256": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"intent_version":  1,
	}
	resp, _ = humanRequest(t, ts.Client(), http.MethodPost,
		ts.URL+"/v1/projects/default/work-intents", create, token, "csrf-viewer", "viewer-write")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer write status=%d want 403", resp.StatusCode)
	}
}

func TestDefaultHumanPostureAllowsLoopbackDevButFailsClosedOffLoopback(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC))
	h := api.New(st, clk, ulid.NewMinter(nil), api.Config{}, "default-human-posture").PrivateHandler()

	offBox := httptest.NewRequest(http.MethodGet, "http://flowbee/v1/decisions?project_id=default", nil)
	offBox.RemoteAddr = "100.64.0.9:4444"
	offBox.Header.Set("X-Flowbee-Actor", "self-asserted-admin")
	offResponse := httptest.NewRecorder()
	h.ServeHTTP(offResponse, offBox)
	if offResponse.Code != http.StatusUnauthorized {
		t.Fatalf("off-loopback default status=%d want 401", offResponse.Code)
	}

	local := httptest.NewRequest(http.MethodGet, "http://flowbee/v1/decisions?project_id=default", nil)
	local.RemoteAddr = "127.0.0.1:4444"
	localResponse := httptest.NewRecorder()
	h.ServeHTTP(localResponse, local)
	if localResponse.Code != http.StatusOK {
		t.Fatalf("loopback development status=%d body=%s", localResponse.Code, localResponse.Body.String())
	}
}
