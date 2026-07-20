package deadman_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alertingress"
	"github.com/samhotchkiss/flowbee/internal/deadman"
)

type mutableProbe struct{ observation deadman.Observation }

func (p *mutableProbe) Probe(context.Context) (deadman.Observation, error) { return p.observation, nil }

type recordingPublisher struct {
	notifications     []deadman.Notification
	heartbeats        []deadman.Heartbeat
	failures          int
	heartbeatFailures int
}

func (p *recordingPublisher) PublishHeartbeat(_ context.Context, heartbeat deadman.Heartbeat) error {
	p.heartbeats = append(p.heartbeats, heartbeat)
	if p.heartbeatFailures > 0 {
		p.heartbeatFailures--
		return errors.New("heartbeat receiver unavailable")
	}
	return nil
}

func TestHeartbeatSurvivesRestartWithStableSequenceAndNoHumanNotification(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	probe := &mutableProbe{observation: deadman.Observation{Healthy: true, HTTPStatus: http.StatusOK}}
	now := time.Date(2026, 7, 20, 6, 0, 0, 0, time.UTC)
	failing := &recordingPublisher{heartbeatFailures: 1}
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz",
		Probe: probe, Publisher: failing, HeartbeatPublisher: failing, Store: state,
		Now: func() time.Time { return now }}
	if report, err := runner.RunOnce(context.Background()); err == nil || report.IncidentStarted {
		t.Fatalf("failed heartbeat report=%+v err=%v", report, err)
	}
	if len(failing.heartbeats) != 1 || len(failing.notifications) != 0 {
		t.Fatalf("heartbeat failure emitted human notification: %+v %+v", failing.heartbeats, failing.notifications)
	}
	original, _ := json.Marshal(failing.heartbeats[0])

	succeeding := &recordingPublisher{}
	restarted := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz",
		Probe: probe, Publisher: succeeding, HeartbeatPublisher: succeeding, Store: state,
		Now: func() time.Time { return now.Add(time.Minute) }}
	if report, err := restarted.RunOnce(context.Background()); err != nil || report.IncidentStarted {
		t.Fatalf("restart heartbeat report=%+v err=%v", report, err)
	}
	retried, _ := json.Marshal(succeeding.heartbeats[0])
	if !bytes.Equal(original, retried) || succeeding.heartbeats[0].Sequence != 1 || len(succeeding.notifications) != 0 {
		t.Fatalf("restart changed heartbeat or notified human: first=%s retry=%s notifications=%+v",
			original, retried, succeeding.notifications)
	}

	now = now.Add(2 * time.Minute)
	if _, err := restarted.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(succeeding.heartbeats) != 2 || succeeding.heartbeats[1].Sequence != 2 {
		t.Fatalf("next interval heartbeat=%+v", succeeding.heartbeats)
	}
}

func (p *recordingPublisher) Publish(_ context.Context, n deadman.Notification) error {
	p.notifications = append(p.notifications, n)
	if p.failures > 0 {
		p.failures--
		return errors.New("receiver unavailable")
	}
	return nil
}

func TestIncidentSurvivesRestartWithoutDuplicateAndResolvesOnce(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	probe := &mutableProbe{observation: deadman.Observation{Reason: "process_unreachable", Detail: "connection refused"}}
	pub := &recordingPublisher{}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp:7001/healthz",
		Probe: probe, Publisher: pub, Store: state, Now: func() time.Time { return now }}
	first, err := runner.RunOnce(context.Background())
	if err != nil || !first.IncidentStarted || first.NotificationsSent != 1 {
		t.Fatalf("first pass=%+v err=%v", first, err)
	}
	if len(pub.notifications) != 1 || pub.notifications[0].Status != "firing" || pub.notifications[0].ProjectID != "russ" {
		t.Fatalf("notifications=%+v", pub.notifications)
	}
	incidentID := pub.notifications[0].Incident.ID

	// Constructing a new Runner models a watchdog process restart. The state
	// file, not process memory, prevents another firing event.
	restartedPub := &recordingPublisher{}
	restarted := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp:7001/healthz",
		Probe: probe, Publisher: restartedPub, Store: state, Now: func() time.Time { return now.Add(time.Minute) }}
	second, err := restarted.RunOnce(context.Background())
	if err != nil || second.IncidentStarted || len(restartedPub.notifications) != 0 {
		t.Fatalf("restart pass=%+v notifications=%+v err=%v", second, restartedPub.notifications, err)
	}

	probe.observation = deadman.Observation{Healthy: true, HTTPStatus: http.StatusOK}
	resolved, err := restarted.RunOnce(context.Background())
	if err != nil || !resolved.IncidentResolved || resolved.NotificationsSent != 1 {
		t.Fatalf("resolved pass=%+v err=%v", resolved, err)
	}
	if len(restartedPub.notifications) != 1 || restartedPub.notifications[0].Status != "resolved" ||
		restartedPub.notifications[0].Incident.ID != incidentID {
		t.Fatalf("resolved notifications=%+v", restartedPub.notifications)
	}
	persisted, err := state.Load()
	if err != nil || persisted.LastResolution == nil || persisted.LastResolution.IncidentID != incidentID {
		t.Fatalf("durable resolution=%+v err=%v", persisted.LastResolution, err)
	}
	last, err := restarted.RunOnce(context.Background())
	if err != nil || last.IncidentResolved || len(restartedPub.notifications) != 1 {
		t.Fatalf("post-recovery pass=%+v notifications=%+v err=%v", last, restartedPub.notifications, err)
	}
}

func TestFailedPublishRetriesImmutableKeyAndBodyAfterRestart(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	probe := &mutableProbe{observation: deadman.Observation{Reason: "control_plane_unhealthy", HTTPStatus: 503}}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	failing := &recordingPublisher{failures: 1}
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz", Probe: probe,
		Publisher: failing, Store: state, Now: func() time.Time { return now }}
	if _, err := runner.RunOnce(context.Background()); err == nil {
		t.Fatal("expected webhook failure")
	}
	if len(failing.notifications) != 1 {
		t.Fatalf("attempts=%d", len(failing.notifications))
	}
	original, _ := json.Marshal(failing.notifications[0])

	succeeding := &recordingPublisher{}
	restarted := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz", Probe: probe,
		Publisher: succeeding, Store: state, Now: func() time.Time { return now.Add(time.Minute) }}
	report, err := restarted.RunOnce(context.Background())
	if err != nil || report.IncidentStarted || report.NotificationsSent != 1 {
		t.Fatalf("retry pass=%+v err=%v", report, err)
	}
	retried, _ := json.Marshal(succeeding.notifications[0])
	if string(retried) != string(original) || succeeding.notifications[0].DedupKey != failing.notifications[0].DedupKey {
		t.Fatalf("retry mutated idempotent notification\nfirst=%s\nretry=%s", original, retried)
	}
}

func TestRecoveryDoesNotOvertakePendingFiringNotification(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	probe := &mutableProbe{observation: deadman.Observation{Reason: "process_unreachable"}}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	failing := &recordingPublisher{failures: 1}
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz", Probe: probe,
		Publisher: failing, Store: state, Now: func() time.Time { return now }}
	_, _ = runner.RunOnce(context.Background())

	probe.observation = deadman.Observation{Healthy: true, HTTPStatus: 200}
	pub := &recordingPublisher{}
	restarted := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz", Probe: probe,
		Publisher: pub, Store: state, Now: func() time.Time { return now.Add(time.Minute) }}
	report, err := restarted.RunOnce(context.Background())
	if err != nil || report.NotificationsSent != 2 {
		t.Fatalf("recovery pass=%+v err=%v", report, err)
	}
	if len(pub.notifications) != 2 || pub.notifications[0].Status != "firing" || pub.notifications[1].Status != "resolved" {
		t.Fatalf("notification order=%+v", pub.notifications)
	}
}

func TestWatchdogStateCannotReplayQueueIntoAnotherProject(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	notification := deadman.Notification{FormatVersion: "flowbee.deadman-alert/v1", ID: "old:firing",
		DedupKey: "old:firing", ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz",
		Status: "firing", Incident: deadman.Incident{ID: "old"}}
	if err := state.Save(deadman.State{Version: deadman.StateVersion, ProjectID: "russ",
		WatchdogID: "observer-a", Target: "http://cp/healthz",
		Pending: []deadman.PendingNotification{{Notification: notification}}}); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	runner := deadman.Runner{ProjectID: "mail", WatchdogID: "observer-a", Target: "http://cp/healthz",
		Probe: &mutableProbe{observation: deadman.Observation{Healthy: true}}, Publisher: pub, Store: state}
	if _, err := runner.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("project change error=%v", err)
	}
	if len(pub.notifications) != 0 {
		t.Fatalf("old project queue was replayed: %+v", pub.notifications)
	}
}

func TestVersionOneStateFailsClosedBecauseProjectIsUnbound(t *testing.T) {
	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	if err := state.Save(deadman.State{Version: 1, WatchdogID: "observer-a", Target: "http://cp/healthz"}); err != nil {
		t.Fatal(err)
	}
	pub := &recordingPublisher{}
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: "http://cp/healthz",
		Probe: &mutableProbe{observation: deadman.Observation{Healthy: true}}, Publisher: pub, Store: state}
	if _, err := runner.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "unsupported watchdog state version 1") {
		t.Fatalf("v1 state error=%v", err)
	}
	if len(pub.notifications) != 0 {
		t.Fatal("unbound v1 state reached publisher")
	}
}

func TestHTTPProbeClassifiesReconcilerOverdueAndUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"status":"degraded","reconciler_overdue":2,"reconciler_overdue_names":["review_handoff","driver_executor"]}`)
	}))
	probe := deadman.HTTPProbe{URL: srv.URL}
	obs, err := probe.Probe(context.Background())
	if err != nil || obs.Healthy || obs.Reason != "reconciler_overdue" || obs.ReconcilerOverdue != 2 || len(obs.ReconcilerOverdueIDs) != 2 {
		t.Fatalf("observation=%+v err=%v", obs, err)
	}
	srv.Close()
	obs, err = probe.Probe(context.Background())
	if err != nil || obs.Reason != "process_unreachable" {
		t.Fatalf("unreachable observation=%+v err=%v", obs, err)
	}
}

func TestHTTPProbeOnlyAllowsExactDriverControlDegradation(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "database unhealthy", body: `{"status":"degraded","db":false,"reconciler_overdue":0,"driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-003"}}`},
		{name: "reconciler summary missing", body: `{"status":"degraded","db":true,"driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-003"}}`},
		{name: "reconciler read failed", body: `{"status":"degraded","db":true,"reconciler_overdue":0,"reconciler_health_error":"locked","driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-003"}}`},
		{name: "different control fault", body: `{"status":"degraded","db":true,"reconciler_overdue":0,"driver_control":{"required":true,"available":false,"status":"token_invalid","gap":"GAP-FD-003"}}`},
		{name: "different contract gap", body: `{"status":"degraded","db":true,"reconciler_overdue":0,"driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-999"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, test.body)
			}))
			defer srv.Close()
			obs, err := (deadman.HTTPProbe{URL: srv.URL}).Probe(context.Background())
			if err != nil || obs.Healthy || obs.Reason == "" {
				t.Fatalf("observation=%+v err=%v", obs, err)
			}
		})
	}
}

func TestDriverControlDegradationDoesNotMaskLaterDeadmanEpisodes(t *testing.T) {
	var mu sync.Mutex
	mode := "control-only"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		current := mode
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		if current == "overdue" {
			_, _ = io.WriteString(w, `{"status":"degraded","db":true,"reconciler_overdue":1,"reconciler_overdue_names":["review_handoff"],"driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-003"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"degraded","db":true,"reconciler_overdue":0,"reconciler_overdue_names":[],"driver_control":{"required":true,"available":false,"status":"route_unavailable","gap":"GAP-FD-003"}}`)
	}))

	state := deadman.FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	pub := &recordingPublisher{}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	runner := deadman.Runner{ProjectID: "russ", WatchdogID: "observer-a", Target: srv.URL,
		Probe: deadman.HTTPProbe{URL: srv.URL}, Publisher: pub, Store: state,
		Now: func() time.Time { return now }}

	baseline, err := runner.RunOnce(context.Background())
	if err != nil || !baseline.Observation.Healthy || baseline.IncidentStarted || len(pub.notifications) != 0 {
		t.Fatalf("control-only baseline=%+v notifications=%+v err=%v", baseline, pub.notifications, err)
	}

	mu.Lock()
	mode = "overdue"
	mu.Unlock()
	now = now.Add(time.Minute)
	firing, err := runner.RunOnce(context.Background())
	if err != nil || !firing.IncidentStarted || firing.Observation.Reason != "reconciler_overdue" {
		t.Fatalf("overdue pass=%+v err=%v", firing, err)
	}
	firstID := firing.IncidentID
	if len(pub.notifications) != 1 || pub.notifications[0].Status != "firing" {
		t.Fatalf("notifications=%+v", pub.notifications)
	}

	// Continued observation of the same fault remains one episode.
	now = now.Add(time.Minute)
	repeated, err := runner.RunOnce(context.Background())
	if err != nil || repeated.IncidentStarted || len(pub.notifications) != 1 {
		t.Fatalf("repeated pass=%+v notifications=%+v err=%v", repeated, pub.notifications, err)
	}

	mu.Lock()
	mode = "control-only"
	mu.Unlock()
	now = now.Add(time.Minute)
	resolved, err := runner.RunOnce(context.Background())
	if err != nil || !resolved.IncidentResolved || resolved.IncidentID != firstID {
		t.Fatalf("resolved pass=%+v err=%v", resolved, err)
	}
	if len(pub.notifications) != 2 || pub.notifications[1].Status != "resolved" {
		t.Fatalf("notifications=%+v", pub.notifications)
	}
	now = now.Add(time.Minute)
	idempotent, err := runner.RunOnce(context.Background())
	if err != nil || idempotent.IncidentResolved || len(pub.notifications) != 2 {
		t.Fatalf("idempotent recovery=%+v notifications=%+v err=%v", idempotent, pub.notifications, err)
	}

	// A later process failure must start a new incident; the permanent Driver
	// route hold did not consume the sole nil->active transition.
	srv.Close()
	now = now.Add(time.Minute)
	unreachable, err := runner.RunOnce(context.Background())
	if err != nil || !unreachable.IncidentStarted || unreachable.Observation.Reason != "process_unreachable" || unreachable.IncidentID == firstID {
		t.Fatalf("unreachable pass=%+v first_id=%s err=%v", unreachable, firstID, err)
	}
	if len(pub.notifications) != 3 || pub.notifications[2].Status != "firing" {
		t.Fatalf("notifications=%+v", pub.notifications)
	}
}

func TestWebhookPublisherSignsExactImmutableBody(t *testing.T) {
	secret := "test-key"
	var received struct {
		FormatVersion string               `json:"format_version"`
		ID            string               `json:"id"`
		DedupKey      string               `json:"dedup_key"`
		ProjectID     string               `json:"project_id"`
		Kind          string               `json:"kind"`
		Payload       deadman.Notification `json:"payload"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if got := r.Header.Get("X-Flowbee-Signature"); got != wantSignature {
			t.Errorf("signature=%q want %q", got, wantSignature)
		}
		if err := json.Unmarshal(body, &received); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Idempotency-Key") != received.DedupKey {
			t.Errorf("idempotency key mismatch")
		}
		writeDeadmanReceiverAck(w, received.DedupKey, body, secret)
	}))
	defer srv.Close()
	n := deadman.Notification{FormatVersion: "flowbee.deadman-alert/v1", ID: "i:firing", DedupKey: "i:firing",
		ProjectID: "russ", WatchdogID: "w", Target: "t", Status: "firing", Incident: deadman.Incident{ID: "i"}}
	if err := (deadman.WebhookPublisher{URL: srv.URL, Secret: secret, ProjectID: "russ"}).Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if received.ID != n.ID || received.FormatVersion != "flowbee.control-alert/v1" ||
		received.ProjectID != "russ" || received.Kind != "external_deadman" || received.Payload.FormatVersion != n.FormatVersion {
		t.Fatalf("received=%+v", received)
	}
}

func TestWebhookPublisherRequiresExactMatchingProject(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	notification := deadman.Notification{FormatVersion: "flowbee.deadman-alert/v1", ID: "i:firing",
		DedupKey: "i:firing", ProjectID: "russ", WatchdogID: "w", Target: "t", Status: "firing"}
	for _, publisher := range []deadman.WebhookPublisher{
		{URL: srv.URL, Secret: "secret"},
		{URL: srv.URL, Secret: "secret", ProjectID: "mail"},
		{URL: srv.URL, Secret: "secret", ProjectID: "Russ/Main"},
	} {
		if err := publisher.Publish(context.Background(), notification); err == nil {
			t.Fatal("publisher accepted missing/mismatched project")
		}
	}
	if calls != 0 {
		t.Fatalf("invalid project publisher made %d HTTP call(s)", calls)
	}
}

func writeDeadmanReceiverAck(w http.ResponseWriter, key string, body []byte, secret string) {
	hash := sha256.Sum256(body)
	ackBody, _ := json.Marshal(alertingress.Acknowledgement{
		FormatVersion: alertingress.AckFormatVersion, Status: "accepted",
		IdempotencyKey: key, BodySHA256: hex.EncodeToString(hash[:]),
	})
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(ackBody)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(ackBody)
}

type deadmanAckTransport struct {
	secret string
	calls  int
	bodies [][]byte
}

func (d *deadmanAckTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	d.calls++
	d.bodies = append(d.bodies, append([]byte(nil), body...))
	if d.calls == 1 {
		return nil, errors.New("receiver response lost after acceptance")
	}
	recorder := httptest.NewRecorder()
	writeDeadmanReceiverAck(recorder, req.Header.Get("Idempotency-Key"), body, d.secret)
	return recorder.Result(), nil
}

func TestDeadmanPublisherRejectsProxy2xxAndReplaysExactBody(t *testing.T) {
	secret := "test-key"
	n := deadman.Notification{FormatVersion: "flowbee.deadman-alert/v1", ID: "i:firing", DedupKey: "i:firing",
		ProjectID: "russ", WatchdogID: "w", Target: "t", Status: "firing", Incident: deadman.Incident{ID: "i"}}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := (deadman.WebhookPublisher{URL: proxy.URL, Secret: secret, ProjectID: "russ"}).Publish(context.Background(), n); err == nil {
		proxy.Close()
		t.Fatal("proxy-generated empty 204 acknowledged dead-man notification")
	}
	proxy.Close()
	redirectedCalls := 0
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	if err := (deadman.WebhookPublisher{URL: redirector.URL, Secret: secret, ProjectID: "russ"}).Publish(context.Background(), n); err == nil {
		redirector.Close()
		t.Fatal("redirect unexpectedly acknowledged dead-man notification")
	}
	redirector.Close()
	if redirectedCalls != 0 {
		t.Fatalf("authenticated dead-man alert followed redirect: calls=%d", redirectedCalls)
	}

	transport := &deadmanAckTransport{secret: secret}
	publisher := deadman.WebhookPublisher{URL: "https://receiver.example.test/flowbee", Secret: secret, ProjectID: "russ",
		Client: &http.Client{Transport: transport}}
	if err := publisher.Publish(context.Background(), n); err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	if err := publisher.Publish(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if transport.calls != 2 || !bytes.Equal(transport.bodies[0], transport.bodies[1]) {
		t.Fatalf("dead-man retry changed immutable body: calls=%d bodies=%q", transport.calls, transport.bodies)
	}
}

func TestOwnerOnlySecretRejectsLooseModeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("key\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := deadman.ReadOwnerOnlySecret(path); err == nil {
		t.Fatal("expected loose permission rejection")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := deadman.ReadOwnerOnlySecret(path); err != nil || got != "key" {
		t.Fatalf("secret=%q err=%v", got, err)
	}
	link := filepath.Join(dir, "secret-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := deadman.ReadOwnerOnlySecret(link); err == nil {
		t.Fatal("expected symlink rejection")
	}
}

func TestStateStoreRejectsSymlinkAndOverlappingWriter(t *testing.T) {
	dir := t.TempDir()
	store := deadman.FileStore{Path: filepath.Join(dir, "state.json")}
	if err := store.Save(deadman.State{Version: deadman.StateVersion, ProjectID: "russ", WatchdogID: "w", Target: "t"}); err != nil {
		t.Fatal(err)
	}
	link := deadman.FileStore{Path: filepath.Join(dir, "state-link.json")}
	if err := os.Symlink(store.Path, link.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := link.Load(); err == nil {
		t.Fatal("expected state symlink rejection")
	}
	lock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if second, err := store.Lock(); err == nil {
		_ = second.Close()
		t.Fatal("expected overlapping watchdog rejection")
	}
}
