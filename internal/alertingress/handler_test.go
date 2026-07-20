package alertingress

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

const testSecret = "receiver-test-secret"

type bindingAcceptor struct {
	mu     sync.Mutex
	hashes map[string]string
	calls  int
}

func (a *bindingAcceptor) Accept(_ context.Context, submission Submission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if existing, ok := a.hashes[submission.IdempotencyKey]; ok {
		if existing != submission.BodySHA256 {
			return ErrIdempotencyConflict
		}
		return nil
	}
	a.hashes[submission.IdempotencyKey] = submission.BodySHA256
	return nil
}

func alertBody(t *testing.T, key, message string) []byte {
	t.Helper()
	body, err := json.Marshal(Envelope{
		FormatVersion: FormatVersion,
		ID:            "alert-1",
		DedupKey:      key,
		ProjectID:     "russ",
		Kind:          "external_deadman",
		Payload:       json.RawMessage(`{"message":` + mustJSON(t, message) + `}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func mustJSON(t *testing.T, value string) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func signedRequest(method string, body []byte, key, secret string) *http.Request {
	req := httptest.NewRequest(method, "https://flowbee.example.test/alerts", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	return req
}

func TestExactReplayReturnsSameSignedDurableAck(t *testing.T) {
	acceptor := &bindingAcceptor{hashes: map[string]string{}}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	body := alertBody(t, "deadman:russ:1", "process unreachable")
	hash := sha256.Sum256(body)
	wantHash := hex.EncodeToString(hash[:])

	var firstBody []byte
	var firstSignature string
	for attempt := 0; attempt < 2; attempt++ {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, signedRequest(http.MethodPost, body, "deadman:russ:1", testSecret))
		if response.Code != http.StatusAccepted {
			t.Fatalf("attempt %d status=%d body=%s", attempt, response.Code, response.Body.String())
		}
		ackBody := bytes.Clone(response.Body.Bytes())
		signature := response.Header().Get("X-Flowbee-Signature")
		if err := ValidateAcknowledgement(ackBody, signature, testSecret, "deadman:russ:1", wantHash); err != nil {
			t.Fatalf("attempt %d ack is not deadman-compatible: %v", attempt, err)
		}
		if attempt == 0 {
			firstBody, firstSignature = ackBody, signature
		} else if !bytes.Equal(firstBody, ackBody) || firstSignature != signature {
			t.Fatal("exact replay did not return the same key/body-bound acknowledgement")
		}
	}
	if acceptor.calls != 2 {
		t.Fatalf("acceptor calls=%d want 2 (acceptor owns replay proof)", acceptor.calls)
	}
}

func TestValidateAcknowledgementRejectsFalseGreenResponses(t *testing.T) {
	const key = "deadman:russ:false-green"
	const bodyHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	validBody, err := json.Marshal(Acknowledgement{
		FormatVersion: AckFormatVersion, Status: "accepted", IdempotencyKey: key, BodySHA256: bodyHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	sign := func(body []byte) string {
		mac := hmac.New(sha256.New, []byte(testSecret))
		_, _ = mac.Write(body)
		return "sha256=" + hex.EncodeToString(mac.Sum(nil))
	}
	unknownField := append(bytes.TrimSuffix(bytes.Clone(validBody), []byte("}")), []byte(`,"provider":"fallback"}`)...)
	for _, tc := range []struct {
		name, signature, key, hash string
		body                       []byte
	}{
		{name: "empty", signature: sign(nil), key: key, hash: bodyHash},
		{name: "unsigned", body: validBody, signature: "", key: key, hash: bodyHash},
		{name: "unknown field", body: unknownField, signature: sign(unknownField), key: key, hash: bodyHash},
		{name: "wrong key", body: validBody, signature: sign(validBody), key: key + "-other", hash: bodyHash},
		{name: "wrong hash", body: validBody, signature: sign(validBody), key: key, hash: strings.Repeat("f", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateAcknowledgement(tc.body, tc.signature, testSecret, tc.key, tc.hash); err == nil {
				t.Fatal("false-green acknowledgement was accepted")
			}
		})
	}
}

func TestChangedBodyForSameKeySurfacesAcceptorConflict(t *testing.T) {
	acceptor := &bindingAcceptor{hashes: map[string]string{}}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman:russ:conflict"
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, signedRequest(http.MethodPost, alertBody(t, key, "first"), key, testSecret))
	if first.Code != http.StatusAccepted {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	conflict := httptest.NewRecorder()
	handler.ServeHTTP(conflict, signedRequest(http.MethodPost, alertBody(t, key, "changed"), key, testSecret))
	if conflict.Code != http.StatusConflict {
		t.Fatalf("conflict status=%d want 409 body=%s", conflict.Code, conflict.Body.String())
	}
	if conflict.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatal("conflicting body received a signed durable acknowledgement")
	}
}

func TestBadSignatureAndContentNeverReachAcceptor(t *testing.T) {
	acceptor := &bindingAcceptor{hashes: map[string]string{}}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman:russ:invalid"
	body := alertBody(t, key, "invalid")

	badSignature := signedRequest(http.MethodPost, body, key, "wrong-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, badSignature)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("bad signature status=%d want 401", response.Code)
	}

	badContentType := signedRequest(http.MethodPost, body, key, testSecret)
	badContentType.Header.Set("Content-Type", "text/plain")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, badContentType)
	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("bad content type status=%d want 415", response.Code)
	}

	invalidEnvelope := []byte(`{"format_version":"flowbee.control-alert/v1","id":"a","dedup_key":"deadman:russ:invalid","project_id":"russ","kind":"external_deadman","payload":{},"unknown":true}`)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, signedRequest(http.MethodPost, invalidEnvelope, key, testSecret))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unknown content status=%d want 400", response.Code)
	}
	if acceptor.calls != 0 {
		t.Fatalf("invalid requests reached acceptor %d time(s)", acceptor.calls)
	}
}

func TestOversizeBodyIsRejectedBeforeSignatureOrAcceptance(t *testing.T) {
	acceptor := &bindingAcceptor{hashes: map[string]string{}}
	handler, err := New(Config{Secret: testSecret, Acceptor: acceptor})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("x"), maxBodyBytes+1)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, signedRequest(http.MethodPost, body, "oversize", testSecret))
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d want 413", response.Code)
	}
	if acceptor.calls != 0 || response.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatal("oversize request reached durable acceptance or received an acknowledgement")
	}
}

func TestNoAcknowledgementBeforeDurableAcceptorSuccess(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	handler, err := New(Config{Secret: testSecret, Acceptor: AcceptorFunc(func(context.Context, Submission) error {
		close(started)
		<-release
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	body := alertBody(t, "deadman:russ:blocking", "blocking")
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(response, signedRequest(http.MethodPost, body, "deadman:russ:blocking", testSecret))
	}()
	<-started
	if response.Header().Get("X-Flowbee-Signature") != "" || response.Body.Len() != 0 {
		t.Fatal("handler acknowledged before acceptor reported durable success")
	}
	close(release)
	<-done
	if response.Code != http.StatusAccepted || response.Header().Get("X-Flowbee-Signature") == "" {
		t.Fatalf("durable success response status=%d signature=%q", response.Code, response.Header().Get("X-Flowbee-Signature"))
	}
}

func TestAcceptorFailureAndNonPOSTNeverProduceAckOrRedirect(t *testing.T) {
	handler, err := New(Config{Secret: testSecret, Acceptor: AcceptorFunc(func(context.Context, Submission) error {
		return errors.New("storage unavailable")
	})})
	if err != nil {
		t.Fatal(err)
	}
	key := "deadman:russ:failed"
	body := alertBody(t, key, "failed")
	failed := httptest.NewRecorder()
	handler.ServeHTTP(failed, signedRequest(http.MethodPost, body, key, testSecret))
	if failed.Code != http.StatusServiceUnavailable || failed.Header().Get("X-Flowbee-Signature") != "" {
		t.Fatalf("failed acceptor response status=%d signature=%q", failed.Code, failed.Header().Get("X-Flowbee-Signature"))
	}

	nonPOST := httptest.NewRecorder()
	handler.ServeHTTP(nonPOST, signedRequest(http.MethodGet, body, key, testSecret))
	if nonPOST.Code != http.StatusMethodNotAllowed || nonPOST.Header().Get("Location") != "" || nonPOST.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("non-POST response status=%d location=%q allow=%q", nonPOST.Code,
			nonPOST.Header().Get("Location"), nonPOST.Header().Get("Allow"))
	}
}
