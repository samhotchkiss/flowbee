package alertingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestStoreAcceptorCommitsExactBodyAndControlAlertBeforeSignedAck(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)
	acceptor := StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now }}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman:default:1"
	body := storeAlertBody(t, "external-deadman-1", key, "default", "process unreachable")
	wantDigest := sha256.Sum256(body)
	wantSHA := hex.EncodeToString(wantDigest[:])

	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, signedRequest(http.MethodPost, body, key, testSecret))
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	var gotSHA, gotProject, gotAlertID, gotKind, alertState, alertPayload string
	var gotBody []byte
	if err := st.DB.QueryRowContext(ctx, `SELECT i.body_sha256,i.body,i.project_id,i.control_alert_id,
		i.envelope_kind,a.state,a.payload_json FROM control_alert_ingress_submissions i
		JOIN control_alerts a ON a.id=i.control_alert_id WHERE i.idempotency_key=?`, key).
		Scan(&gotSHA, &gotBody, &gotProject, &gotAlertID, &gotKind, &alertState, &alertPayload); err != nil {
		t.Fatal(err)
	}
	if gotSHA != wantSHA || !bytes.Equal(gotBody, body) || gotProject != "default" ||
		gotAlertID != "external-deadman-1" || gotKind != "external_deadman" || alertState != "pending" ||
		alertPayload != `{"message":"process unreachable"}` {
		t.Fatalf("durable ingress sha=%q body_equal=%v project=%q alert=%q kind=%q state=%q payload=%q",
			gotSHA, bytes.Equal(gotBody, body), gotProject, gotAlertID, gotKind, alertState, alertPayload)
	}
	var ingressCount, alertCount int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM control_alert_ingress_submissions),
		(SELECT COUNT(*) FROM control_alerts WHERE dedup_key=?)`, key).Scan(&ingressCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if ingressCount != 1 || alertCount != 1 {
		t.Fatalf("exact replay duplicated ingress=%d alerts=%d", ingressCount, alertCount)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE control_alert_ingress_submissions SET body=x'00'
		WHERE idempotency_key=?`, key); err == nil {
		t.Fatal("immutable ingress body was mutable")
	}
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM control_alert_ingress_submissions
		WHERE idempotency_key=?`, key); err == nil {
		t.Fatal("immutable ingress key/body binding was deletable")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE control_alerts SET payload_json='{}'
		WHERE id='external-deadman-1'`); err == nil {
		t.Fatal("ingress-bound control alert payload was mutable")
	}

	// Durable acceptance remains replayable even if project authorization later
	// closes; this is proof of the already-committed request, not new ingress.
	if _, err := st.DB.ExecContext(ctx, `UPDATE projects SET state='archived' WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	replay := httptest.NewRecorder()
	handler.ServeHTTP(replay, signedRequest(http.MethodPost, body, key, testSecret))
	if replay.Code != http.StatusAccepted {
		t.Fatalf("exact committed replay after archive status=%d body=%s", replay.Code, replay.Body.String())
	}
}

func TestStoreAcceptorChangedBodyConflictsAndUnauthorizedProjectIsNotAccepted(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	handler, err := New(Config{Secret: testSecret, Acceptor: StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now }}})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman:default:conflict"
	firstBody := storeAlertBody(t, "external-deadman-conflict", key, "default", "first")
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, signedRequest(http.MethodPost, firstBody, key, testSecret))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	changedBody := storeAlertBody(t, "external-deadman-conflict", key, "default", "changed")
	changed := httptest.NewRecorder()
	handler.ServeHTTP(changed, signedRequest(http.MethodPost, changedBody, key, testSecret))
	if changed.Code != http.StatusConflict || changed.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("changed replay status=%d signature=%q", changed.Code, changed.Header().Get("X-Flowbee-Signature"))
	}
	var gotBody []byte
	if err := st.DB.QueryRowContext(ctx, `SELECT body FROM control_alert_ingress_submissions
		WHERE idempotency_key=?`, key).Scan(&gotBody); err != nil || !bytes.Equal(gotBody, firstBody) {
		t.Fatalf("conflict mutated original body equal=%v err=%v", bytes.Equal(gotBody, firstBody), err)
	}

	missingKey := "deadman:missing:1"
	missingBody := storeAlertBody(t, "external-deadman-missing", missingKey, "missing", "unreachable")
	missing := httptest.NewRecorder()
	handler.ServeHTTP(missing, signedRequest(http.MethodPost, missingBody, missingKey, testSecret))
	if missing.Code != http.StatusServiceUnavailable || missing.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("unknown project status=%d signature=%q", missing.Code, missing.Header().Get("X-Flowbee-Signature"))
	}
	var missingRows int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alert_ingress_submissions
		WHERE project_id='missing'`).Scan(&missingRows); err != nil || missingRows != 0 {
		t.Fatalf("unauthorized project persisted rows=%d err=%v", missingRows, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE projects SET state='archived' WHERE id='default'`); err != nil {
		t.Fatal(err)
	}
	inactiveKey := "deadman:default:inactive"
	inactiveBody := storeAlertBody(t, "external-deadman-inactive", inactiveKey, "default", "inactive")
	inactive := httptest.NewRecorder()
	handler.ServeHTTP(inactive, signedRequest(http.MethodPost, inactiveBody, inactiveKey, testSecret))
	if inactive.Code != http.StatusServiceUnavailable || inactive.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("inactive authorized project status=%d signature=%q", inactive.Code, inactive.Header().Get("X-Flowbee-Signature"))
	}
}

func TestStoreAcceptorTransactionFailureRollsBackAlertAndRetryRecovers(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	handler, err := New(Config{Secret: testSecret, Acceptor: StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now }}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER test_ingress_crash BEFORE INSERT
		ON control_alert_ingress_submissions BEGIN SELECT RAISE(ABORT,'failpoint after alert insert'); END`); err != nil {
		t.Fatal(err)
	}
	key := "deadman:default:crash"
	body := storeAlertBody(t, "external-deadman-crash", key, "default", "database stalled")
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, signedRequest(http.MethodPost, body, key, testSecret))
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("failpoint status=%d signature=%q", failed.Code, failed.Header().Get("X-Flowbee-Signature"))
	}
	var ingressCount, alertCount int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM control_alert_ingress_submissions WHERE idempotency_key=?),
		(SELECT COUNT(*) FROM control_alerts WHERE dedup_key=?)`, key, key).
		Scan(&ingressCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if ingressCount != 0 || alertCount != 0 {
		t.Fatalf("failed transaction partially committed ingress=%d alert=%d", ingressCount, alertCount)
	}
	if _, err := st.DB.ExecContext(ctx, `DROP TRIGGER test_ingress_crash`); err != nil {
		t.Fatal(err)
	}
	retry := httptest.NewRecorder()
	handler.ServeHTTP(retry, signedRequest(http.MethodPost, body, key, testSecret))
	if retry.Code != http.StatusAccepted {
		t.Fatalf("retry status=%d body=%s", retry.Code, retry.Body.String())
	}
}

func TestStoreAcceptorNeverAdoptsPreexistingControlAlertIdentity(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 1, 30, 0, 0, time.UTC)
	stamp := now.Format(time.RFC3339Nano)
	key := "deadman:default:preexisting"
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO control_alerts
		(id,project_id,kind,dedup_key,payload_json,state,created_at,updated_at)
		VALUES ('preexisting-alert','default','internal_alert',?,'{}','pending',?,?)`, key, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	handler, err := New(Config{Secret: testSecret, Acceptor: StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now }}})
	if err != nil {
		t.Fatal(err)
	}
	body := storeAlertBody(t, "external-preexisting", key, "default", "must conflict")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedRequest(http.MethodPost, body, key, testSecret))
	if response.Code != http.StatusConflict || response.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("preexisting identity status=%d signature=%q", response.Code, response.Header().Get("X-Flowbee-Signature"))
	}
	var ingressCount int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alert_ingress_submissions`).Scan(&ingressCount); err != nil || ingressCount != 0 {
		t.Fatalf("preexisting identity was adopted: rows=%d err=%v", ingressCount, err)
	}
}

func TestStoreAcceptorRejectsCallerBodyHashMismatchWithoutMutation(t *testing.T) {
	st := testutil.NewStore(t)
	body := storeAlertBody(t, "external-deadman-hash", "deadman:default:hash", "default", "hash mismatch")
	err := (StoreAcceptor{Store: st, AuthorizedProjectID: "default"}).Accept(context.Background(), Submission{
		IdempotencyKey: "deadman:default:hash", BodySHA256: strings.Repeat("0", 64), Body: body,
	})
	if err == nil {
		t.Fatal("mismatched caller hash was accepted")
	}
	var rows int
	if queryErr := st.DB.QueryRow(`SELECT COUNT(*) FROM control_alert_ingress_submissions`).Scan(&rows); queryErr != nil || rows != 0 {
		t.Fatalf("hash mismatch persisted rows=%d err=%v", rows, queryErr)
	}
}

func TestStoreAcceptorConcurrentExactReplayCreatesOneBinding(t *testing.T) {
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	acceptor := StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now }}
	key := "deadman:default:parallel"
	body := storeAlertBody(t, "external-deadman-parallel", key, "default", "parallel replay")
	digest := sha256.Sum256(body)
	submission := Submission{IdempotencyKey: key, BodySHA256: hex.EncodeToString(digest[:]), Body: body}
	const attempts = 16
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- acceptor.Accept(context.Background(), submission)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent exact replay: %v", err)
		}
	}
	var ingressCount, alertCount int
	if err := st.DB.QueryRow(`SELECT
		(SELECT COUNT(*) FROM control_alert_ingress_submissions WHERE idempotency_key=?),
		(SELECT COUNT(*) FROM control_alerts WHERE dedup_key=?)`, key, key).
		Scan(&ingressCount, &alertCount); err != nil {
		t.Fatal(err)
	}
	if ingressCount != 1 || alertCount != 1 {
		t.Fatalf("parallel replay ingress=%d alerts=%d", ingressCount, alertCount)
	}
}

func TestStoreAcceptorHeartbeatAdvancesLeaseWithoutHumanAlert(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 4, 0, 0, 0, time.UTC)
	receivedAt := now
	acceptor := StoreAcceptor{Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return receivedAt }}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman-heartbeat:default:observer-a:1"
	body := storeHeartbeatBody(t, key, "default", "observer-a", "http://cp/healthz", 1, now)
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, signedRequest(http.MethodPost, body, key, testSecret))
		if response.Code != http.StatusAccepted {
			t.Fatalf("heartbeat attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
	}
	lease, fresh, err := st.ExternalWatchdogLeaseFresh(ctx, "default", "observer-a", now.Add(time.Minute), 2*time.Minute)
	if err != nil || !fresh || lease.LastSequence != 1 || lease.Target != "http://cp/healthz" {
		t.Fatalf("lease=%+v fresh=%v err=%v", lease, fresh, err)
	}
	var heartbeats, alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM external_watchdog_heartbeat_submissions),
		(SELECT COUNT(*) FROM control_alerts)`).Scan(&heartbeats, &alerts); err != nil {
		t.Fatal(err)
	}
	if heartbeats != 1 || alerts != 0 {
		t.Fatalf("heartbeat created notification or duplicated: heartbeats=%d alerts=%d", heartbeats, alerts)
	}
	receivedAt = now.Add(time.Minute)
	secondKey := "deadman-heartbeat:default:observer-a:2"
	secondBody := storeHeartbeatBody(t, secondKey, "default", "observer-a", "http://cp/healthz", 2, receivedAt)
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, signedRequest(http.MethodPost, secondBody, secondKey, testSecret))
	if second.Code != http.StatusAccepted {
		t.Fatalf("second heartbeat status=%d body=%s", second.Code, second.Body.String())
	}
	lease, fresh, err = st.ExternalWatchdogLeaseFresh(ctx, "default", "observer-a", receivedAt, 2*time.Minute)
	if err != nil || !fresh || lease.LastSequence != 2 {
		t.Fatalf("advanced lease=%+v fresh=%v err=%v", lease, fresh, err)
	}
	if _, fresh, err := st.ExternalWatchdogLeaseFresh(ctx, "default", "observer-a", receivedAt.Add(3*time.Minute), 2*time.Minute); err != nil || fresh {
		t.Fatalf("stale watchdog lease stayed ready: fresh=%v err=%v", fresh, err)
	}
}

func TestStoreAcceptorHeartbeatRejectsSpoofAndProjectMismatch(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 4, 30, 0, 0, time.UTC)
	handler, err := New(Config{Secret: testSecret, Acceptor: StoreAcceptor{
		Store: st, AuthorizedProjectID: "default", Now: func() time.Time { return now },
	}})
	if err != nil {
		t.Fatal(err)
	}
	firstKey := "deadman-heartbeat:default:observer-a:1"
	firstBody := storeHeartbeatBody(t, firstKey, "default", "observer-a", "http://cp/healthz", 1, now)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, signedRequest(http.MethodPost, firstBody, firstKey, testSecret))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first heartbeat status=%d body=%s", first.Code, first.Body.String())
	}

	spoofKey := "deadman-heartbeat:default:observer-b:2"
	spoofBody := storeHeartbeatBody(t, spoofKey, "default", "observer-b", "http://cp/healthz", 2, now.Add(time.Minute))
	spoof := httptest.NewRecorder()
	handler.ServeHTTP(spoof, signedRequest(http.MethodPost, spoofBody, spoofKey, testSecret))
	if spoof.Code != http.StatusConflict {
		t.Fatalf("spoof heartbeat status=%d body=%s", spoof.Code, spoof.Body.String())
	}

	mismatchKey := "deadman-heartbeat:default:observer-a:2"
	mismatchBody, err := json.Marshal(Envelope{FormatVersion: FormatVersion, ID: mismatchKey,
		DedupKey: mismatchKey, ProjectID: "default", Kind: ExternalWatchdogHeartbeatKind,
		Payload: mustMarshalHeartbeat(t, ExternalWatchdogHeartbeatPayload{
			FormatVersion: ExternalWatchdogHeartbeatFormatVersion, ProjectID: "other",
			WatchdogID: "observer-a", Target: "http://cp/healthz", Sequence: 2, ObservedAt: now.Add(time.Minute),
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatch := httptest.NewRecorder()
	handler.ServeHTTP(mismatch, signedRequest(http.MethodPost, mismatchBody, mismatchKey, testSecret))
	if mismatch.Code != http.StatusServiceUnavailable {
		t.Fatalf("payload project mismatch status=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
	lease, err := st.ExternalWatchdogLease(ctx, "default")
	if err != nil || lease.WatchdogID != "observer-a" || lease.LastSequence != 1 {
		t.Fatalf("spoof mutated lease=%+v err=%v", lease, err)
	}
}

func storeAlertBody(t *testing.T, id, key, projectID, message string) []byte {
	t.Helper()
	body, err := json.Marshal(Envelope{
		FormatVersion: FormatVersion, ID: id, DedupKey: key, ProjectID: projectID,
		Kind: "external_deadman", Payload: json.RawMessage(`{"message":` + mustJSON(t, message) + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func storeHeartbeatBody(t *testing.T, key, projectID, watchdogID, target string, sequence int64,
	observedAt time.Time) []byte {
	t.Helper()
	body, err := json.Marshal(Envelope{FormatVersion: FormatVersion, ID: key, DedupKey: key,
		ProjectID: projectID, Kind: ExternalWatchdogHeartbeatKind,
		Payload: mustMarshalHeartbeat(t, ExternalWatchdogHeartbeatPayload{
			FormatVersion: ExternalWatchdogHeartbeatFormatVersion, ProjectID: projectID,
			WatchdogID: watchdogID, Target: target, Sequence: sequence, ObservedAt: observedAt,
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustMarshalHeartbeat(t *testing.T, heartbeat ExternalWatchdogHeartbeatPayload) json.RawMessage {
	t.Helper()
	body, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	return body
}
