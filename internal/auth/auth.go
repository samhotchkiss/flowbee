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
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	secret             []byte
	enrolled           map[string]struct{}
	family             map[string]string // identity -> operator-declared model_family (optional)
	loopbackBypass     bool
	credentialVerifier func(CredentialClaims, time.Time) bool
	now                func() time.Time
}

type CredentialClaims struct {
	Identity, ProjectID, WorkerRole, CredentialID string
	Generation                                    int64
	ExpiresAt                                     time.Time
}

// CredentialTokenPrefix is reserved for durable, project/role-bound worker
// credentials. Legacy enrolled identities may not begin with this prefix: the
// parser always treats it as the version discriminator and fails closed.
const CredentialTokenPrefix = "fbw2."

// WithCredentialVerifier enables generation/expiry/revocation enforcement for
// Flowbee-minted per-session credentials. The verifier is consulted on every
// request; durable Stop/replacement can therefore revoke a token without
// restarting the API or rebuilding a static allowlist.
func (b *BearerAuth) WithCredentialVerifier(verifier func(CredentialClaims, time.Time) bool) *BearerAuth {
	b.credentialVerifier = verifier
	return b
}

// WithNow installs the API server clock. One observed-at value is used for both
// signed expiry and the durable verifier, making recovery deterministic.
func (b *BearerAuth) WithNow(now func() time.Time) *BearerAuth {
	if now != nil {
		b.now = now
	}
	return b
}

func (b *BearerAuth) MintCredential(identity, projectID, workerRole, credentialID string, generation int64,
	expiresAt time.Time) string {
	identity64 := base64.RawURLEncoding.EncodeToString([]byte(identity))
	project64 := base64.RawURLEncoding.EncodeToString([]byte(projectID))
	role64 := base64.RawURLEncoding.EncodeToString([]byte(workerRole))
	credential64 := base64.RawURLEncoding.EncodeToString([]byte(credentialID))
	material := fmt.Sprintf("fbw2.%s.%s.%s.%s.%d.%d", identity64, project64, role64,
		credential64, generation, expiresAt.Unix())
	return material + "." + sign(b.secret, material)
}

// NewBearer builds a bearer authenticator with the given server secret and the set of
// enrolled identities. An enrolled entry may optionally bind the identity's model
// family as "identity:family" (e.g. "reviewer-bob:claude-opus"). When bound, the lease
// handler CLAMPS the worker's self-asserted model_family to this operator-declared
// value — so the §5.5 anti-affinity exclusion (a same-family reviewer can't rubber-stamp)
// cannot be defeated by a worker simply declaring a different family at lease time.
// loopbackBypass enables the same-box no-token path.
func NewBearer(secret []byte, enrolled []string, loopbackBypass bool) *BearerAuth {
	set := make(map[string]struct{}, len(enrolled))
	fam := make(map[string]string)
	for _, e := range enrolled {
		id, f := parseEnrolledEntry(e)
		if strings.HasPrefix(id, CredentialTokenPrefix) {
			continue
		}
		set[id] = struct{}{}
		if f != "" {
			fam[id] = f
		}
	}
	return &BearerAuth{secret: secret, enrolled: set, family: fam,
		loopbackBypass: loopbackBypass, now: time.Now}
}

// parseEnrolledEntry splits an "identity:family" enrolled entry into its parts.
// Identities use dots (e.g. "studio.opus"), never colons, so ':' is an unambiguous
// family delimiter. A bare "identity" yields an empty family (unconstrained — the
// legacy behavior, no anti-affinity binding for that identity).
func parseEnrolledEntry(e string) (id, family string) {
	if i := strings.IndexByte(e, ':'); i >= 0 {
		return e[:i], e[i+1:]
	}
	return e, ""
}

// FamilyFor returns the operator-declared model family bound to an enrolled identity,
// if one was configured. Implements FamilyResolver.
func (b *BearerAuth) FamilyFor(identity string) (string, bool) {
	f, ok := b.family[identity]
	return f, ok && f != ""
}

// Mint produces the signed token a worker presents in Authorization: Bearer.
// (Enrollment hands this to the worker out-of-band; the worker never derives it.)
func (b *BearerAuth) Mint(identity string) string {
	if strings.HasPrefix(identity, CredentialTokenPrefix) {
		return ""
	}
	return identity + "." + sign(b.secret, identity)
}

// Authenticate verifies the bearer token (or accepts a loopback caller when
// LoopbackBypass is on). The returned identity is the one bound by the token's
// MAC — for non-loopback callers it cannot be forged.
func (b *BearerAuth) Authenticate(r *http.Request) (string, error) {
	id, _, err := b.authenticateWithClaims(r)
	return id, err
}

// authenticateWithClaims preserves the project/role/generation authority of a
// durable Flowbee credential for endpoint-level authorization. Legacy enrolled
// tokens and the explicit loopback bypass return nil claims.
func (b *BearerAuth) authenticateWithClaims(r *http.Request) (string, *CredentialClaims, error) {
	tok := bearerToken(r)
	if tok == "" {
		// no token: only loopback may proceed, and only if bypass is enabled.
		if b.loopbackBypass && isLoopback(r) {
			return declaredIdentity(r), nil, nil
		}
		return "", nil, ErrUnauthorized
	}
	if strings.HasPrefix(tok, CredentialTokenPrefix) {
		parts := strings.Split(tok, ".")
		if len(parts) != 8 {
			return "", nil, ErrUnauthorized
		}
		material := strings.Join(parts[:7], ".")
		if !hmac.Equal([]byte(parts[7]), []byte(sign(b.secret, material))) {
			return "", nil, ErrUnauthorized
		}
		identityBytes, identityErr := base64.RawURLEncoding.DecodeString(parts[1])
		projectBytes, projectErr := base64.RawURLEncoding.DecodeString(parts[2])
		roleBytes, roleErr := base64.RawURLEncoding.DecodeString(parts[3])
		credentialBytes, credentialErr := base64.RawURLEncoding.DecodeString(parts[4])
		generation, generationErr := strconv.ParseInt(parts[5], 10, 64)
		expiresUnix, expiresErr := strconv.ParseInt(parts[6], 10, 64)
		claims := CredentialClaims{Identity: string(identityBytes), ProjectID: string(projectBytes),
			WorkerRole: string(roleBytes), CredentialID: string(credentialBytes),
			Generation: generation, ExpiresAt: time.Unix(expiresUnix, 0).UTC()}
		observedAt := b.now().UTC()
		if identityErr != nil || projectErr != nil || roleErr != nil || credentialErr != nil ||
			generationErr != nil || expiresErr != nil || claims.Identity == "" || claims.ProjectID == "" ||
			(claims.WorkerRole != "builder" && claims.WorkerRole != "reviewer" &&
				claims.WorkerRole != "interactor" && claims.WorkerRole != "orchestrator") ||
			claims.CredentialID == "" || claims.Generation < 1 || !observedAt.Before(claims.ExpiresAt) ||
			b.credentialVerifier == nil || !b.credentialVerifier(claims, observedAt) {
			return "", nil, ErrUnauthorized
		}
		return claims.Identity, &claims, nil
	}
	// the identity itself contains dots (e.g. "studio.opus"), so split on the LAST
	// dot: everything before it is the identity, the suffix is the MAC.
	dot := strings.LastIndex(tok, ".")
	if dot <= 0 || dot == len(tok)-1 {
		return "", nil, ErrUnauthorized
	}
	id, mac := tok[:dot], tok[dot+1:]
	// constant-time MAC check: a forged identity cannot produce a valid MAC.
	if !hmac.Equal([]byte(mac), []byte(sign(b.secret, id))) {
		return "", nil, ErrUnauthorized
	}
	if _, ok := b.enrolled[id]; !ok {
		return "", nil, ErrUnauthorized
	}
	return id, nil, nil
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

// FamilyResolver is an OPTIONAL capability an Authenticator may implement: it maps an
// authenticated identity to its operator-declared model family. When present, Middleware
// stashes that family so the lease handler can clamp the self-asserted model_family —
// grounding §5.5 anti-affinity in the credential, not the worker's word. An Authenticator
// that doesn't implement it (e.g. mTLS) simply leaves model_family worker-asserted.
type FamilyResolver interface {
	FamilyFor(identity string) (string, bool)
}

// identityCtxKey carries the authenticated identity down to handlers.
type identityCtxKey struct{}

// identityFamilyCtxKey carries the credential-bound model family down to handlers.
type identityFamilyCtxKey struct{}

type credentialClaimsCtxKey struct{}

func withIdentity(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

func withFamily(ctx context.Context, family string) context.Context {
	return context.WithValue(ctx, identityFamilyCtxKey{}, family)
}

func withCredentialClaims(ctx context.Context, claims CredentialClaims) context.Context {
	return context.WithValue(ctx, credentialClaimsCtxKey{}, claims)
}

// FamilyFrom returns the credential-bound model family stashed by Middleware, if the
// authenticator bound one for this identity. The lease handler clamps the self-asserted
// model_family to it (anti-affinity trust root, I-10).
func FamilyFrom(r *http.Request) (string, bool) {
	f, ok := r.Context().Value(identityFamilyCtxKey{}).(string)
	return f, ok && f != ""
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
		var id string
		var claims *CredentialClaims
		var err error
		if bearer, ok := a.(*BearerAuth); ok {
			id, claims, err = bearer.authenticateWithClaims(r)
		} else {
			id, err = a.Authenticate(r)
		}
		if err != nil {
			// Actionable body (the worker logs this verbatim on every retry, and the
			// private listener is loopback/Tailscale-only so it leaks nothing useful to
			// an attacker): name the two things an operator actually has to line up.
			http.Error(w, "unauthorized: worker token missing/invalid or identity not enrolled — "+
				"check the worker's FLOWBEE_WORKER_TOKEN matches the control plane's "+
				"FLOWBEE_WORKER_AUTH_SECRET, and that this identity is in enrolled_identities",
				http.StatusUnauthorized)
			return
		}
		ctx := withIdentity(r.Context(), id)
		if claims != nil {
			ctx = withCredentialClaims(ctx, *claims)
		}
		// bind the operator-declared model family (if any) so the handler can clamp the
		// self-asserted model_family — the anti-affinity trust root (I-10).
		if fr, ok := a.(FamilyResolver); ok {
			if fam, ok := fr.FamilyFor(id); ok {
				ctx = withFamily(ctx, fam)
			}
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// IdentityFrom returns the authenticated identity stashed by Middleware, if any.
func IdentityFrom(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(identityCtxKey{}).(string)
	return id, ok && id != ""
}

// CredentialClaimsFrom returns signed, durable credential claims after
// Middleware authentication. Values from request bodies or query parameters are
// never promoted into this context.
func CredentialClaimsFrom(r *http.Request) (CredentialClaims, bool) {
	claims, ok := r.Context().Value(credentialClaimsCtxKey{}).(CredentialClaims)
	return claims, ok && claims.Identity != "" && claims.ProjectID != "" && claims.WorkerRole != ""
}
