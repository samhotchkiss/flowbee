package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// memInbox is an in-memory Inbox for the HMAC/dedupe unit tests.
type memInbox struct {
	mu        sync.Mutex
	seen      map[string]bool
	processed map[string]bool
}

func newMemInbox() *memInbox {
	return &memInbox{seen: map[string]bool{}, processed: map[string]bool{}}
}

func (m *memInbox) RecordDelivery(_ context.Context, id, _ string, _ int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.seen[id] {
		return false, nil
	}
	m.seen[id] = true
	return true, nil
}

func (m *memInbox) MarkDeliveryProcessed(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.processed[id] = true
	return nil
}

// spyRefetcher records the PR numbers a verified webhook asked to refetch.
type spyRefetcher struct {
	mu     sync.Mutex
	calls  []int
	sweeps int
}

func (s *spyRefetcher) RefetchHint(_ context.Context, pr int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, pr)
	return true
}

func (s *spyRefetcher) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.calls) }

func (s *spyRefetcher) IntakeSweep(_ context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps++
	return true
}

func (s *spyRefetcher) sweepCount() int { s.mu.Lock(); defer s.mu.Unlock(); return s.sweeps }

const secret = "topsecret"

func post(t *testing.T, h http.Handler, delivery, event, body, sig string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks", strings.NewReader(body))
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestForgedSignatureRejected: an UNSIGNED or WRONG-signature webhook is 401'd and
// never touches the inbox or triggers a refetch (I-2). The endpoint is
// internet-reachable; it fails closed.
func TestForgedSignatureRejected(t *testing.T) {
	inbox, spy := newMemInbox(), &spyRefetcher{}
	h := New(secret, inbox, spy)
	body := `{"pull_request":{"number":7}}`

	// no signature.
	if rr := post(t, h, "d1", "pull_request", body, ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned: code=%d want 401", rr.Code)
	}
	// wrong signature (signed with the wrong secret).
	bad := Sign([]byte("wrong"), []byte(body))
	if rr := post(t, h, "d1", "pull_request", body, bad); rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged: code=%d want 401", rr.Code)
	}
	if spy.count() != 0 {
		t.Fatalf("forged webhook triggered %d refetches; want 0 (must never reach reconcile)", spy.count())
	}
	if len(inbox.seen) != 0 {
		t.Fatalf("forged webhook reached the inbox; must be rejected before write-ahead")
	}
}

// TestIssueLabeledTriggersIntakeSweep: a signed issues.labeled webhook triggers an
// intake sweep (event-driven adoption), while an issues event whose action can't
// introduce a new target (e.g. closed) does not — so the floor poll isn't the only way
// a labeled issue gets picked up.
func TestIssueLabeledTriggersIntakeSweep(t *testing.T) {
	inbox, spy := newMemInbox(), &spyRefetcher{}
	h := New(secret, inbox, spy)

	labeled := `{"action":"labeled","issue":{"number":42},"label":{"name":"flowbee:build"}}`
	rr := post(t, h, "d-lab", "issues", labeled, Sign([]byte(secret), []byte(labeled)))
	if rr.Code != http.StatusOK {
		t.Fatalf("labeled issue: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if spy.sweepCount() != 1 {
		t.Fatalf("issues.labeled must trigger 1 intake sweep, got %d", spy.sweepCount())
	}
	if spy.count() != 0 {
		t.Fatalf("an issue event must not hit the PR refetch path, got %d", spy.count())
	}

	// a `closed` issues action introduces no intake target -> no sweep.
	closed := `{"action":"closed","issue":{"number":42}}`
	post(t, h, "d-cls", "issues", closed, Sign([]byte(secret), []byte(closed)))
	if spy.sweepCount() != 1 {
		t.Fatalf("issues.closed must NOT sweep; sweepCount=%d want 1", spy.sweepCount())
	}
}

// TestReplayedDeliveryDeduped: a correctly-signed but REPLAYED delivery (same
// X-GitHub-Delivery) is deduped — recorded once, refetched once, the replay is a
// no-op (I-2). At worst the FIRST delivery triggers a refetch of real state.
func TestReplayedDeliveryDeduped(t *testing.T) {
	inbox, spy := newMemInbox(), &spyRefetcher{}
	h := New(secret, inbox, spy)
	body := `{"pull_request":{"number":7}}`
	sig := Sign([]byte(secret), []byte(body))

	rr1 := post(t, h, "dup-delivery", "pull_request", body, sig)
	if rr1.Code != http.StatusOK || !strings.Contains(rr1.Body.String(), `"refetched":true`) {
		t.Fatalf("first delivery: code=%d body=%s", rr1.Code, rr1.Body.String())
	}
	rr2 := post(t, h, "dup-delivery", "pull_request", body, sig)
	if rr2.Code != http.StatusOK || !strings.Contains(rr2.Body.String(), `"deduped":true`) {
		t.Fatalf("replay: code=%d body=%s want deduped", rr2.Code, rr2.Body.String())
	}
	if spy.count() != 1 {
		t.Fatalf("replayed delivery refetched %d times; want exactly 1 (dedupe)", spy.count())
	}
}

// TestValidWebhookTriggersTargetedRefetch: a fresh, correctly-signed webhook is
// recorded write-ahead, then triggers a TARGETED refetch of its PR — never a
// direct state change. The body's CLAIM is irrelevant; only the PR number routes.
func TestValidWebhookTriggersTargetedRefetch(t *testing.T) {
	inbox, spy := newMemInbox(), &spyRefetcher{}
	h := New(secret, inbox, spy)
	// a FORGED-CONTENT body (claims an approval) but correctly signed: the refetch
	// reads real state, so the lie cannot fast-track anything downstream.
	body := `{"action":"submitted","pull_request":{"number":42},"review":{"state":"approved"}}`
	sig := Sign([]byte(secret), []byte(body))

	rr := post(t, h, "fresh-1", "pull_request_review", body, sig)
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
	}
	if spy.count() != 1 || spy.calls[0] != 42 {
		t.Fatalf("refetch calls=%v want [42]", spy.calls)
	}
	if !inbox.processed["fresh-1"] {
		t.Fatalf("delivery not marked processed after refetch")
	}
}

// TestMissingDeliveryID: a webhook without X-GitHub-Delivery is rejected (no
// dedupe key). It must still pass HMAC first.
func TestMissingDeliveryID(t *testing.T) {
	h := New(secret, newMemInbox(), &spyRefetcher{})
	body := `{}`
	sig := Sign([]byte(secret), []byte(body))
	if rr := post(t, h, "", "ping", body, sig); rr.Code != http.StatusBadRequest {
		t.Fatalf("missing delivery: code=%d want 400", rr.Code)
	}
}

// TestEmptySecretFailsClosed: with no configured secret the handler rejects
// everything (fail closed).
func TestEmptySecretFailsClosed(t *testing.T) {
	h := New("", newMemInbox(), &spyRefetcher{})
	body := `{"pull_request":{"number":1}}`
	sig := Sign([]byte(""), []byte(body)) // even a "valid" sig over empty secret
	if rr := post(t, h, "d", "pull_request", body, sig); rr.Code != http.StatusUnauthorized {
		t.Fatalf("empty secret: code=%d want 401", rr.Code)
	}
}
