package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alertingress"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestFirstBootIngressHeartbeatConvergesDynamicReadinessWithoutHumanAlert(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "russ", Name: "Russ", State: "active", Priority: 100, SchedulerWeight: 1,
	}, now); err != nil {
		t.Fatal(err)
	}
	phase1Current := func() api.Phase1ProjectReadiness {
		_, fresh, err := st.ExternalWatchdogLeaseFresh(ctx, "russ", "observer-a", time.Now().UTC(), 2*time.Minute)
		if err != nil || !fresh {
			return api.Phase1ProjectReadiness{Required: true, ProjectID: "russ", Status: "held",
				Reason: "watchdog heartbeat missing", Holds: []string{"external_watchdog_lease_missing_or_stale"}}
		}
		return api.Phase1ProjectReadiness{Required: true, Available: true, ProjectID: "russ", Status: "ready"}
	}
	srv := api.New(st, clock.NewFake(now), ulid.NewMinter(nil), api.Config{
		Phase1ProjectCurrent: phase1Current,
	}, "test")
	health := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		srv.HealthHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		return recorder
	}
	if recorder := health(); recorder.Code != http.StatusServiceUnavailable ||
		!bytes.Contains(recorder.Body.Bytes(), []byte("external_watchdog_lease_missing_or_stale")) {
		t.Fatalf("first boot readiness code=%d body=%s", recorder.Code, recorder.Body.String())
	}

	secret := "ingress-secret"
	ingress, err := privateHandlerWithControlAlertIngress(srv.PrivateHandler(), secret, "russ", st)
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman-heartbeat:russ:observer-a:1"
	payload := alertingress.ExternalWatchdogHeartbeatPayload{
		FormatVersion: alertingress.ExternalWatchdogHeartbeatFormatVersion, ProjectID: "russ",
		WatchdogID: "observer-a", Target: "http://flowbee/healthz", Sequence: 1, ObservedAt: now,
	}
	body, err := json.Marshal(alertingress.Envelope{FormatVersion: alertingress.FormatVersion,
		ID: key, DedupKey: key, ProjectID: "russ", Kind: alertingress.ExternalWatchdogHeartbeatKind,
		Payload: mustJSONRaw(t, payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, alertingress.ControlAlertIngressPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	request.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	ingress.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("heartbeat ingress status=%d body=%s", response.Code, response.Body.String())
	}
	if lease, fresh, leaseErr := st.ExternalWatchdogLeaseFresh(ctx, "russ", "observer-a", time.Now().UTC(), 2*time.Minute); leaseErr != nil || !fresh {
		t.Fatalf("accepted heartbeat lease=%+v fresh=%v err=%v", lease, fresh, leaseErr)
	}
	if recorder := health(); recorder.Code != http.StatusOK ||
		!bytes.Contains(recorder.Body.Bytes(), []byte(`"phase1_project":{"required":true,"available":true`)) {
		t.Fatalf("post-heartbeat readiness code=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts`).Scan(&alerts); err != nil || alerts != 0 {
		t.Fatalf("heartbeat generated human alerts=%d err=%v", alerts, err)
	}
}

func mustJSONRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPrivateControlAlertIngressCommitsBeforeAcknowledgementAndPreservesBaseRoutes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "deadman-project", Name: "Deadman project", State: "active", Priority: 100, SchedulerWeight: 1,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	handler, err := privateHandlerWithControlAlertIngress(base, "ingress-secret", "deadman-project", st)
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"format_version":"flowbee.control-alert/v1","id":"deadman-alert-1","dedup_key":"deadman:deadman-project:1","project_id":"deadman-project","kind":"external_deadman","payload":{"state":"firing"}}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/control-alerts/ingress", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "deadman:deadman-project:1")
	mac := hmac.New(sha256.New, []byte("ingress-secret"))
	_, _ = mac.Write(body)
	req.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("ingress status=%d body=%s", rec.Code, rec.Body.String())
	}
	var submissions, alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alert_ingress_submissions
		WHERE idempotency_key='deadman:deadman-project:1'`).Scan(&submissions); err != nil || submissions != 1 {
		t.Fatalf("durable submissions=%d err=%v", submissions, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE id='deadman-alert-1' AND project_id='deadman-project' AND state='pending'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("durable alerts=%d err=%v", alerts, err)
	}

	fallback := httptest.NewRecorder()
	handler.ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/unrelated", nil))
	if fallback.Code != http.StatusNoContent {
		t.Fatalf("base route status=%d", fallback.Code)
	}
}

func TestPrivateControlAlertIngressIsNotMountedWithoutSecret(t *testing.T) {
	base := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler, err := privateHandlerWithControlAlertIngress(base, "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/control-alerts/ingress", nil))
	if rec.Code != http.StatusTeapot {
		t.Fatalf("disabled ingress must preserve base handler, status=%d", rec.Code)
	}
}
