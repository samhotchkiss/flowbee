package alerting_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alerting"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestDurableAuthenticatedAlertRetriesThenAcknowledges(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO control_alerts
		(id,project_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES ('a1','default','review_dispatch_stalled','dedup-1','{"epic_id":"e1"}','pending',?,?)`, now.Format(time.RFC3339), now.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	calls := 0
	secret := "test-secret"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if r.Header.Get("X-Flowbee-Signature") != want || r.Header.Get("Idempotency-Key") != "dedup-1" {
			t.Errorf("missing authenticated idempotent headers")
		}
		calls++
		if calls == 1 {
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := alerting.Drainer{Store: st, Sink: alerting.WebhookSink{URL: srv.URL, Secret: secret}, Owner: "test", MaximumTries: 3}
	first, err := d.Tick(ctx, now)
	if err != nil || first.Retried != 1 {
		t.Fatalf("first tick=%+v err=%v", first, err)
	}
	var state string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='a1'`).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("alert not durable after failure: state=%s err=%v", state, err)
	}
	second, err := d.Tick(ctx, now.Add(time.Minute))
	if err != nil || second.Published != 1 || calls != 2 {
		t.Fatalf("second tick=%+v calls=%d err=%v", second, calls, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM control_alerts WHERE id='a1'`).Scan(&state); err != nil || state != "acknowledged" {
		t.Fatalf("alert not acknowledged: state=%s err=%v", state, err)
	}
}
