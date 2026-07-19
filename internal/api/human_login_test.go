package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestHumanLoginFragmentAndOneTimeSessionExchange(t *testing.T) {
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	clk := clock.NewFake(now)
	automation := auth.NewBearer([]byte("worker-bootstrap-secret"), []string{"sam"}, false)
	access := auth.NewHumanAccess([]byte(humanTestSecret), automation, map[string][]auth.HumanGrant{
		"sam": {{ProjectID: "default", Role: auth.HumanAdmin}},
	}, false)
	srv := api.New(st, clk, ulid.NewMinter(nil), api.Config{HumanAccess: access}, "human-login-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	page, err := ts.Client().Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	rawPage, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(rawPage), "location.hash") ||
		!strings.Contains(string(rawPage), "history.replaceState") || page.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("unsafe login bootstrap: status=%d headers=%v body=%s", page.StatusCode, page.Header, rawPage)
	}

	linkBody, _ := json.Marshal(map[string]string{"project_id": "default"})
	linkReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/human/login-links", bytes.NewReader(linkBody))
	linkReq.Header.Set("Content-Type", "application/json")
	linkReq.Header.Set("Authorization", "Bearer "+automation.Mint("sam"))
	linkResp, err := ts.Client().Do(linkReq)
	if err != nil {
		t.Fatal(err)
	}
	var link map[string]any
	if err := json.NewDecoder(linkResp.Body).Decode(&link); err != nil {
		t.Fatal(err)
	}
	_ = linkResp.Body.Close()
	path, _ := link["login_fragment_path"].(string)
	const prefix = "/login#token="
	if linkResp.StatusCode != http.StatusCreated || !strings.HasPrefix(path, prefix) {
		t.Fatalf("link status=%d body=%v", linkResp.StatusCode, link)
	}
	rawToken := strings.TrimPrefix(path, prefix)
	payload, _ := json.Marshal(map[string]string{"token": rawToken})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/human/session", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("exchange status=%d", resp.StatusCode)
	}
	var haveSession, haveCSRF bool
	for _, c := range resp.Cookies() {
		switch c.Name {
		case auth.HumanSessionCookie:
			haveSession = c.HttpOnly && c.SameSite == http.SameSiteStrictMode
		case "flowbee_csrf":
			haveCSRF = !c.HttpOnly && c.SameSite == http.SameSiteStrictMode && c.Value != ""
		}
	}
	if !haveSession || !haveCSRF {
		t.Fatalf("missing hardened session/csrf cookies: %v", resp.Cookies())
	}

	replay, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/human/session", bytes.NewReader(payload))
	replay.Header.Set("Content-Type", "application/json")
	replayed, err := ts.Client().Do(replay)
	if err != nil {
		t.Fatal(err)
	}
	_ = replayed.Body.Close()
	if replayed.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status=%d, want 401", replayed.StatusCode)
	}
}
