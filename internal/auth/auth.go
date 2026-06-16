// Package auth is Flowbee's worker-transport trust boundary (DESIGN §7.6, §9.4.1):
// "are you an enrolled worker?" — a standing credential, orthogonal to fencing
// ("are you the current holder of THIS lease?", which lease_epoch answers).
//
// The private worker API is mutually authenticated against an allowlist of
// enrolled identities. Two credential classes are supported, behind one
// Authenticator interface (the §3.2 "one auth.Authenticator interface contains
// the swap"):
//
//   - signed per-worker bearer tokens (HMAC-SHA256 over the identity, keyed by a
//     server secret) — curl-debuggable over Tailscale, simple to enroll. This is
//     the in-environment path the M12 non-loopback acceptance test exercises.
//   - mTLS client certs — documented and wired as a TLSConfig builder; it needs a
//     CA + per-worker certs that are real infra, so it is not exercised in-env
//     (per the M12 "documented, not required in-env" carve-out). See MTLSConfig.
//
// This package deliberately imports no clock, no rand-for-logic, no GitHub, and
// no core package — it sits at the api/cmd edge, outside the deterministic core.
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strings"
)

// ErrUnauthorized is returned when a credential is missing, malformed, or not on
// the enrolled-identity allowlist. The handler maps it to 401.
var ErrUnauthorized = errors.New("unauthorized: caller is not an enrolled worker")

// Authenticator answers "are you an enrolled worker, and who are you?" for one
// inbound request. It returns the authenticated, unforgeable identity bound to
// the credential (DESIGN §7.6) — never a value the caller self-asserted in a
// query param or body.
type Authenticator interface {
	// Authenticate verifies the request's credential and returns the bound
	// identity, or ErrUnauthorized. The returned identity is authoritative.
	Authenticate(r *http.Request) (identity string, err error)
}

// BearerAuth implements signed per-worker bearer tokens (HMAC-SHA256). A token is
// "<identity>.<base64url(HMAC(secret, identity))>". The identity is unforgeable:
// without the server secret a caller cannot produce a valid MAC for any identity,
// and only enrolled identities (Enrolled) are accepted.
//
// LoopbackBypass permits unauthenticated requests that originate from loopback
// (the §12.4 "bearer fallback on loopback" — a same-box worker on 127.0.0.1 needs
// no token). For loopback callers the bound identity comes from the request's
// declared identity (query/body) since there is no token to bind; the network
// boundary is the trust boundary. Non-loopback callers ALWAYS need a valid token.
type BearerAuth struct {
	secret         []byte
	enrolled       map[string]struct{}
	loopbackBypass bool
}

// NewBearer builds a bearer authenticator with the given server secret and the
// set of enrolled identities. loopbackBypass enables the same-box no-token path.
func NewBearer(secret []byte, enrolled []string, loopbackBypass bool) *BearerAuth {
	set := make(map[string]struct{}, len(enrolled))
	for _, id := range enrolled {
		set[id] = struct{}{}
	}
	return &BearerAuth{secret: secret, enrolled: set, loopbackBypass: loopbackBypass}
}

// Mint produces the signed token a worker presents in Authorization: Bearer.
// (Enrollment hands this to the worker out-of-band; the worker never derives it.)
func (b *BearerAuth) Mint(identity string) string {
	return identity + "." + sign(b.secret, identity)
}

// Authenticate verifies the bearer token (or accepts a loopback caller when
// LoopbackBypass is on). The returned identity is the one bound by the token's
// MAC — for non-loopback callers it cannot be forged.
func (b *BearerAuth) Authenticate(r *http.Request) (string, error) {
	tok := bearerToken(r)
	if tok == "" {
		// no token: only loopback may proceed, and only if bypass is enabled.
		if b.loopbackBypass && isLoopback(r) {
			return declaredIdentity(r), nil
		}
		return "", ErrUnauthorized
	}
	// the identity itself contains dots (e.g. "studio.opus"), so split on the LAST
	// dot: everything before it is the identity, the suffix is the MAC.
	dot := strings.LastIndex(tok, ".")
	if dot <= 0 || dot == len(tok)-1 {
		return "", ErrUnauthorized
	}
	id, mac := tok[:dot], tok[dot+1:]
	// constant-time MAC check: a forged identity cannot produce a valid MAC.
	if !hmac.Equal([]byte(mac), []byte(sign(b.secret, id))) {
		return "", ErrUnauthorized
	}
	if _, ok := b.enrolled[id]; !ok {
		return "", ErrUnauthorized
	}
	return id, nil
}

func sign(secret []byte, identity string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(identity))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// declaredIdentity reads the caller-asserted identity (query param) for the
// loopback bypass path only. Off-loopback this value is never trusted.
func declaredIdentity(r *http.Request) string {
	if v := r.URL.Query().Get("identity"); v != "" {
		return v
	}
	return "loopback"
}

func isLoopback(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// identityCtxKey carries the authenticated identity down to handlers.
type identityCtxKey struct{}

func withIdentity(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// Middleware wraps an http.Handler, authenticating every request before it
// reaches the worker API. On failure it returns 401 and the handler is never
// reached. The authenticated identity is stashed in the request context so the
// lease handler binds the lease to the credential-bound identity, not a
// self-asserted query param (DESIGN §7.6: identity is unforgeable). A nil
// Authenticator disables auth (loopback-only dev default).
func Middleware(a Authenticator, next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := a.Authenticate(r)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := withIdentity(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdentityFrom returns the authenticated identity stashed by Middleware, if any.
func IdentityFrom(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(identityCtxKey{}).(string)
	return id, ok && id != ""
}
