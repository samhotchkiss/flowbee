package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestBootstrapActionValidationIsClosedAndPayloadBound(t *testing.T) {
	payload := json.RawMessage(`{"operation":"ensure"}`)
	sum := sha256.Sum256(payload)
	base := BootstrapAction{FormatVersion: BootstrapActionFormat, BootstrapID: "bootstrap-russ",
		ProjectID: "russ", ActionID: "action-1", Kind: "actor_lifecycle",
		PayloadSHA256: "sha256:" + hex.EncodeToString(sum[:]), Payload: payload}
	if !validBootstrapAction(base) {
		t.Fatal("valid closed bootstrap action rejected")
	}
	for _, mutate := range []func(*BootstrapAction){
		func(a *BootstrapAction) { a.Kind = "raw_tmux" },
		func(a *BootstrapAction) { a.PayloadSHA256 = "" },
		func(a *BootstrapAction) { a.Payload = json.RawMessage(`{"operation":"adopt"}`) },
		func(a *BootstrapAction) { a.Payload = json.RawMessage(`null`) },
	} {
		item := base
		mutate(&item)
		if validBootstrapAction(item) {
			t.Fatalf("invalid action accepted: %+v", item)
		}
	}
}

func TestBootstrapActionStatusIsAuthenticatedAndPayloadRedacted(t *testing.T) {
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	payload := `{"secret":"must-not-leak"}`
	sum := sha256.Sum256([]byte(payload))
	if _, err := st.CommitBootstrapAction(context.Background(), store.BootstrapActionInput{
		ID: "action-status", BootstrapID: "bootstrap-russ", ProjectID: "russ", Kind: "project_upsert",
		PayloadJSON: payload, PayloadSHA256: "sha256:" + hex.EncodeToString(sum[:]),
	}, now); err != nil {
		t.Fatal(err)
	}
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{
		HumanAccess: auth.NewHumanAccess(nil, nil, nil, true),
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/bootstrap/actions/action-status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || raw["state"] != "pending" || raw["project_id"] != "russ" {
		t.Fatalf("status=%d body=%v", resp.StatusCode, raw)
	}
	if _, exists := raw["payload"]; exists || raw["payload_sha256"] != nil || raw["secret"] != nil {
		t.Fatalf("bootstrap status leaked payload material: %v", raw)
	}
}

type bootstrapIntakeFake struct {
	calls int
	keys  []string
}

func (f *bootstrapIntakeFake) CommitBootstrapAction(_ context.Context, action BootstrapAction, key string) (BootstrapActionReceipt, error) {
	f.calls++
	f.keys = append(f.keys, key)
	return BootstrapActionReceipt{FormatVersion: "flowbee.bootstrap-action-receipt/v1",
		ActionID: action.ActionID, ReceiptID: "receipt-" + action.ActionID, State: "pending"}, nil
}

func TestBootstrapActionHTTPRequiresExactIdempotencyAndInstalledIntake(t *testing.T) {
	st := testutil.NewStore(t)
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{
		HumanAccess: auth.NewHumanAccess(nil, nil, nil, true),
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	payload := json.RawMessage(`{"id":"russ","name":"Russ"}`)
	sum := sha256.Sum256(payload)
	action := BootstrapAction{FormatVersion: BootstrapActionFormat, BootstrapID: "bootstrap-russ",
		ProjectID: "russ", ActionID: "action-1", Kind: "project_upsert",
		PayloadSHA256: "sha256:" + hex.EncodeToString(sum[:]), Payload: payload}
	body, _ := json.Marshal(action)
	request := func(key string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/bootstrap/actions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := request("action-1"); resp.StatusCode != http.StatusServiceUnavailable {
		resp.Body.Close()
		t.Fatalf("nil intake status=%d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	fake := &bootstrapIntakeFake{}
	srv.SetBootstrapActionIntake(fake)
	if resp := request("different"); resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("mismatched key status=%d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	for i := 0; i < 2; i++ {
		resp := request("action-1")
		if resp.StatusCode != http.StatusAccepted {
			resp.Body.Close()
			t.Fatalf("replay status=%d", resp.StatusCode)
		}
		resp.Body.Close()
	}
	if fake.calls != 2 || len(fake.keys) != 2 || fake.keys[0] != "action-1" {
		t.Fatalf("intake calls=%d keys=%v", fake.calls, fake.keys)
	}
}
