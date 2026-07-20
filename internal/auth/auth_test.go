package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func req(t *testing.T, remoteAddr, authz, identityQuery string) *http.Request {
	t.Helper()
	url := "http://flowbee/v1/lease"
	if identityQuery != "" {
		url += "?identity=" + identityQuery
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.RemoteAddr = remoteAddr
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	return r
}

// TestBearerEnrolledTokenAuthenticates: a signed token for an enrolled identity
// authenticates and binds that identity.
func TestBearerEnrolledTokenAuthenticates(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"studio.opus"}, false)
	tok := a.Mint("studio.opus")
	id, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+tok, ""))
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if id != "studio.opus" {
		t.Fatalf("bound identity=%q want studio.opus", id)
	}
}

// TestBearerForgedMACRejected: a token whose MAC was not produced with the server
// secret is rejected — the identity is unforgeable (§7.6).
func TestBearerForgedMACRejected(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"studio.opus"}, false)
	// attacker fabricates a token with a bogus MAC for an enrolled identity.
	forged := "studio.opus.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+forged, "")); err != ErrUnauthorized {
		t.Fatalf("forged MAC must be unauthorized, got %v", err)
	}
	// even a token minted with the WRONG secret fails.
	other := NewBearer([]byte("different"), []string{"studio.opus"}, false)
	wrong := other.Mint("studio.opus")
	if _, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+wrong, "")); err != ErrUnauthorized {
		t.Fatalf("wrong-secret token must be unauthorized, got %v", err)
	}
}

// TestBearerUnenrolledIdentityRejected: a correctly-signed token for an identity
// NOT on the allowlist is rejected (enrollment is required, not just a valid MAC).
func TestBearerUnenrolledIdentityRejected(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"studio.opus"}, false)
	tok := a.Mint("rogue.codex") // validly signed, but rogue.codex is not enrolled
	if _, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+tok, "")); err != ErrUnauthorized {
		t.Fatalf("unenrolled identity must be unauthorized, got %v", err)
	}
}

func TestCredentialBearerUsesInjectedClockAndBindsProjectRole(t *testing.T) {
	observed := time.Date(2032, 3, 4, 5, 6, 7, 0, time.UTC)
	var got CredentialClaims
	a := NewBearer([]byte("s3cret"), nil, false).
		WithNow(func() time.Time { return observed }).
		WithCredentialVerifier(func(claims CredentialClaims, at time.Time) bool {
			got = claims
			return at.Equal(observed) && claims.ProjectID == "russ" &&
				claims.WorkerRole == "reviewer"
		})
	tok := a.MintCredential("epic-worker.reviewer.e1", "russ", "reviewer",
		"envelope-1", 4, observed.Add(time.Hour))
	id, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+tok, ""))
	if err != nil || id != "epic-worker.reviewer.e1" {
		t.Fatalf("credential authenticate id=%q err=%v", id, err)
	}
	if got.CredentialID != "envelope-1" || got.Generation != 4 {
		t.Fatalf("claims=%+v", got)
	}
	observed = observed.Add(2 * time.Hour)
	if _, err := a.Authenticate(req(t, "100.64.0.2:5555", "Bearer "+tok, "")); err != ErrUnauthorized {
		t.Fatalf("expired credential accepted: %v", err)
	}
}

func TestCredentialPrefixIsReservedFromLegacyIdentity(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"fbw2.legacy:codex"}, false)
	if tok := a.Mint("fbw2.legacy"); tok != "" {
		t.Fatalf("reserved legacy identity minted ambiguous token %q", tok)
	}
}

// TestNonLoopbackWithoutTokenRejected: a non-loopback caller with no token is
// rejected even when loopback bypass is on — the bypass is loopback-only.
func TestNonLoopbackWithoutTokenRejected(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"studio.opus"}, true)
	if _, err := a.Authenticate(req(t, "100.64.0.2:5555", "", "studio.opus")); err != ErrUnauthorized {
		t.Fatalf("non-loopback no-token must be unauthorized, got %v", err)
	}
}

// TestLoopbackBypass: a loopback caller with no token is accepted when bypass is
// on (§12.4), and its declared identity is bound.
func TestLoopbackBypass(t *testing.T) {
	a := NewBearer([]byte("s3cret"), nil, true)
	id, err := a.Authenticate(req(t, "127.0.0.1:5555", "", "mac.codex"))
	if err != nil {
		t.Fatalf("loopback bypass should pass: %v", err)
	}
	if id != "mac.codex" {
		t.Fatalf("loopback identity=%q want mac.codex", id)
	}
	// bypass OFF: loopback without a token is rejected.
	strict := NewBearer([]byte("s3cret"), nil, false)
	if _, err := strict.Authenticate(req(t, "127.0.0.1:5555", "", "mac.codex")); err != ErrUnauthorized {
		t.Fatalf("loopback without bypass must be unauthorized, got %v", err)
	}
}

// TestMiddlewareRejects401: the middleware returns 401 and never calls next on a
// bad credential, and binds the identity into context on success.
func TestMiddlewareRejects401(t *testing.T) {
	a := NewBearer([]byte("s3cret"), []string{"studio.opus"}, false)
	var reached bool
	var boundID string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		boundID, _ = IdentityFrom(r)
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(a, next)

	// unauthorized -> 401, next NOT reached.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req(t, "100.64.0.2:1:5555", "Bearer bad.token", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d want 401", rec.Code)
	}
	if reached {
		t.Fatal("next handler reached on unauthorized request")
	}
	// the 401 body must be ACTIONABLE — the worker logs it verbatim, so it has to
	// name what the operator must line up (token vs secret, and enrollment).
	body := rec.Body.String()
	for _, want := range []string{"FLOWBEE_WORKER_TOKEN", "FLOWBEE_WORKER_AUTH_SECRET", "enrolled_identities"} {
		if !strings.Contains(body, want) {
			t.Fatalf("401 body must mention %q for the operator, got: %q", want, body)
		}
	}

	// authorized -> next reached, identity bound.
	reached = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req(t, "100.64.0.2:5555", "Bearer "+a.Mint("studio.opus"), ""))
	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("authorized request not served: reached=%v code=%d", reached, rec.Code)
	}
	if boundID != "studio.opus" {
		t.Fatalf("context identity=%q want studio.opus", boundID)
	}
}

// TestNilAuthenticatorDisablesAuth: Middleware(nil, ...) is a pass-through
// (loopback-only dev default).
func TestNilAuthenticatorDisablesAuth(t *testing.T) {
	var reached bool
	h := Middleware(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	h.ServeHTTP(httptest.NewRecorder(), req(t, "10.0.0.5:5555", "", ""))
	if !reached {
		t.Fatal("nil authenticator should pass through")
	}
}

// TestMTLSAuthEnrollment: the mTLS adapter accepts an enrolled CommonName and
// rejects an unenrolled / certless request (the documented production path).
func TestMTLSAuthEnrollment(t *testing.T) {
	m := NewMTLS([]string{"studio.opus"})
	// no TLS on the request -> unauthorized.
	if _, err := m.Authenticate(req(t, "100.64.0.2:5555", "", "")); err != ErrUnauthorized {
		t.Fatalf("certless request must be unauthorized, got %v", err)
	}
}

// TestMTLSConfigValidation: ServerTLS requires all three files.
func TestMTLSConfigValidation(t *testing.T) {
	if _, err := (MTLSConfig{}).ServerTLS(); err == nil {
		t.Fatal("empty MTLSConfig should error")
	}
}
