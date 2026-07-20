package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/api"
)

func TestBootstrapAPIClientCarriesExactAuthAndIdempotency(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v1/bootstrap/actions" || r.Header.Get("Idempotency-Key") != "action-1" ||
			r.Header.Get("X-Flowbee-CSRF") != "csrf" || r.Header.Get("Cookie") != "flowbee_human_session=session" {
			t.Fatalf("request path=%q headers=%v", r.URL.Path, r.Header)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(api.BootstrapActionReceipt{FormatVersion: "flowbee.bootstrap-action-receipt/v1",
			ActionID: "action-1", ReceiptID: "receipt-1", State: "pending"})
	}))
	defer server.Close()
	client := bootstrapAPIClient{BaseURL: server.URL, SessionCookie: "flowbee_human_session=session",
		CSRFToken: "csrf", Client: server.Client()}
	action := api.BootstrapAction{FormatVersion: api.BootstrapActionFormat, BootstrapID: "bootstrap-russ",
		ProjectID: "russ", ActionID: "action-1", Kind: "project_upsert", PayloadSHA256: "sha256:abc",
		Payload: json.RawMessage(`{"project_id":"russ"}`)}
	for i := 0; i < 2; i++ {
		receipt, err := client.Commit(context.Background(), action)
		if err != nil || receipt.ReceiptID != "receipt-1" {
			t.Fatalf("Commit() = %+v, %v", receipt, err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestBootstrapAPIClientLoadsOwnerOnlyAutomationBearerAndSkipsCSRF(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.token")
	const secret = "automation.secret-material-that-must-not-be-printed"
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bootstrapAutomationTokenFileEnv, path)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Flowbee-CSRF") != "" ||
			r.Header.Get("Cookie") != "" {
			t.Fatalf("automation request headers = %v", r.Header)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(api.BootstrapActionReceipt{FormatVersion: bootstrapActionReceiptFormat,
			ActionID: "action-1", ReceiptID: "receipt-1", State: "pending"})
	}))
	defer server.Close()
	client, err := bootstrapAPIClientFromConfiguredBearer(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Commit(context.Background(), api.BootstrapAction{FormatVersion: api.BootstrapActionFormat,
		BootstrapID: "bootstrap-russ", ProjectID: "russ", ActionID: "action-1", Kind: "project_upsert",
		PayloadSHA256: "sha256:abc", Payload: json.RawMessage(`{"project_id":"russ"}`)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBootstrapAPIClientRejectsTrailingOrUnknownReceiptState(t *testing.T) {
	response := `{"format_version":"flowbee.bootstrap-action-receipt/v1","action_id":"action-1","receipt_id":"receipt-1","state":"pending"}{}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	client := bootstrapAPIClient{BaseURL: server.URL, Bearer: "secret", Client: server.Client()}
	action := api.BootstrapAction{ActionID: "action-1"}
	if _, err := client.Commit(context.Background(), action); err == nil {
		t.Fatal("trailing receipt document was accepted")
	}
	response = `{"format_version":"flowbee.bootstrap-action-receipt/v1","action_id":"action-1","receipt_id":"receipt-1","state":"complete"}`
	if _, err := client.Commit(context.Background(), action); err == nil {
		t.Fatal("unknown receipt state was accepted")
	}
}

func TestBootstrapAutomationBearerFileFailsClosedOnLooseOrSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bootstrap.token")
	if err := os.WriteFile(path, []byte("secret"), 0o640); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bootstrapAutomationTokenFileEnv, path)
	if _, err := bootstrapAPIClientFromConfiguredBearer("http://127.0.0.1:7070", http.DefaultClient); err == nil {
		t.Fatal("group-readable automation bearer was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "bootstrap-link.token")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bootstrapAutomationTokenFileEnv, link)
	if _, err := bootstrapAPIClientFromConfiguredBearer("http://127.0.0.1:7070", http.DefaultClient); err == nil {
		t.Fatal("symlinked automation bearer was accepted")
	}
}

func TestBootstrapAPIErrorNeverPrintsAutomationBearer(t *testing.T) {
	const secret = "automation.secret-material-that-must-not-be-printed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer server.Close()
	client := bootstrapAPIClient{BaseURL: server.URL, Bearer: secret, Client: server.Client()}
	_, err := client.Commit(context.Background(), api.BootstrapAction{FormatVersion: api.BootstrapActionFormat,
		BootstrapID: "bootstrap-russ", ProjectID: "russ", ActionID: "action-1", Kind: "project_upsert",
		PayloadSHA256: "sha256:abc", Payload: json.RawMessage(`{"project_id":"russ"}`)})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("secret-bearing error = %v", err)
	}
}

func TestBootstrapAPIClientFailsClosedWithoutHumanAuth(t *testing.T) {
	client := bootstrapAPIClient{Client: http.DefaultClient}
	if _, err := client.Commit(context.Background(), api.BootstrapAction{}); err == nil {
		t.Fatal("incomplete auth was accepted")
	}
}

func TestBootstrapAPIClientRejectsMixedBearerAndBrowserOrigins(t *testing.T) {
	client := bootstrapAPIClient{BaseURL: "http://127.0.0.1", Bearer: "bearer",
		SessionCookie: "flowbee_human_session=session", CSRFToken: "csrf", Client: http.DefaultClient}
	if _, err := client.Commit(context.Background(), api.BootstrapAction{}); err == nil {
		t.Fatal("mixed automation/browser credentials were accepted")
	}
}

func TestBootstrapAPIClientStatusUsesBearerAndRejects202AsCompletion(t *testing.T) {
	const secret = "automation-secret"
	state := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/bootstrap/actions/action-1" ||
			r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Flowbee-CSRF") != "" {
			t.Fatalf("status request path=%q headers=%v", r.URL.Path, r.Header)
		}
		w.WriteHeader(state)
		_ = json.NewEncoder(w).Encode(api.BootstrapActionStatus{FormatVersion: "flowbee.bootstrap-action-status/v1",
			ActionID: "action-1", ProjectID: "russ", State: "succeeded"})
	}))
	defer server.Close()
	client := bootstrapAPIClient{BaseURL: server.URL, Bearer: secret, Client: server.Client()}
	status, err := client.Status(context.Background(), "action-1")
	if err != nil || status.State != "succeeded" {
		t.Fatalf("Status()=%+v, %v", status, err)
	}
	state = http.StatusAccepted
	if _, err := client.Status(context.Background(), "action-1"); err == nil {
		t.Fatal("202 response was treated as completed status")
	}
}
