package auth

// This file is the dashboard-human trust boundary. Worker enrollment answers
// "may this process lease work?"; HumanAccess separately answers "which human
// may perform which action for which project?". Keeping those questions
// separate prevents an enrolled worker token from becoming an implicit global
// dashboard administrator.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const HumanSessionCookie = "flowbee_human_session"

var (
	ErrHumanUnauthorized = errors.New("human authentication required")
	ErrHumanForbidden    = errors.New("human action is not authorized")
	ErrHumanCSRF         = errors.New("human browser mutation failed CSRF validation")
)

type HumanAction string

const (
	HumanDecisionRead       HumanAction = "decision.read"
	HumanDecisionCreate     HumanAction = "decision.create"
	HumanDecisionView       HumanAction = "decision.view"
	HumanDecisionRespond    HumanAction = "decision.respond"
	HumanWorkIntentRead     HumanAction = "work_intent.read"
	HumanWorkIntentCreate   HumanAction = "work_intent.create"
	HumanWorkIntentDefine   HumanAction = "work_intent.define"
	HumanWorkIntentRegister HumanAction = "work_intent.register"
	HumanWorkIntentPause    HumanAction = "work_intent.pause"
	HumanWorkIntentResume   HumanAction = "work_intent.resume"
	HumanWorkIntentCancel   HumanAction = "work_intent.cancel"
	HumanConversationRead   HumanAction = "conversation.read"
	HumanConversationSend   HumanAction = "conversation.send"
	HumanConversationManage HumanAction = "conversation.manage"
	HumanProjectRead        HumanAction = "project.read"
	HumanProjectManage      HumanAction = "project.manage"
)

type HumanRole string

const (
	HumanViewer   HumanRole = "viewer"
	HumanApprover HumanRole = "approver"
	HumanOperator HumanRole = "operator"
	HumanPlanner  HumanRole = "planner"
	HumanAdmin    HumanRole = "admin"
)

// HumanGrant is deliberately project-scoped. Project "*" is the distinct
// portfolio grant; it is never inferred from having access to one project.
type HumanGrant struct {
	ProjectID string
	Role      HumanRole
}

// ParseHumanGrants parses the compact configuration form
// "identity@project=role". An explicit project "*" is required for portfolio
// authority; a missing project is never interpreted as global.
func ParseHumanGrants(entries []string) (map[string][]HumanGrant, error) {
	out := make(map[string][]HumanGrant)
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		parts := strings.Split(entry, "=")
		if len(parts) != 2 {
			return nil, errors.New("human grant must be identity@project=role")
		}
		left, roleText := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		at := strings.LastIndexByte(left, '@')
		if at <= 0 || at == len(left)-1 {
			return nil, errors.New("human grant must name an exact identity and project")
		}
		identity, projectID := left[:at], left[at+1:]
		role := HumanRole(roleText)
		if !validHumanRole(role) {
			return nil, errors.New("human grant has an unknown role")
		}
		out[identity] = append(out[identity], HumanGrant{ProjectID: projectID, Role: role})
	}
	return out, nil
}

type HumanPrincipal struct {
	Identity       string
	SessionID      string
	credentialKind string
	csrfHash       string
}

type humanPrincipalCtxKey struct{}

type humanClaims struct {
	Version   int    `json:"v"`
	Subject   string `json:"sub"`
	SessionID string `json:"sid"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	CSRFHash  string `json:"csrf_sha256"`
}

// HumanAccess authenticates signed, expiring browser sessions and performs the
// server-owned role lookup. Automation may optionally authenticate with the
// existing worker Authenticator, but still needs an explicit HumanGrant: worker
// enrollment alone never grants human authority.
type HumanAccess struct {
	secret           []byte
	automation       Authenticator
	grants           map[string][]HumanGrant
	allowLoopbackDev bool
	now              func() time.Time
}

func NewHumanAccess(secret []byte, automation Authenticator, grants map[string][]HumanGrant, allowLoopbackDev bool) *HumanAccess {
	copyGrants := make(map[string][]HumanGrant, len(grants))
	for identity, rows := range grants {
		copyGrants[identity] = append([]HumanGrant(nil), rows...)
	}
	return &HumanAccess{secret: append([]byte(nil), secret...), automation: automation,
		grants: copyGrants, allowLoopbackDev: allowLoopbackDev, now: time.Now}
}

// MintSession produces an opaque-to-the-browser, signed session cookie value
// and binds it to a separately generated CSRF token. The raw CSRF token is not
// present in the cookie, so a cross-site request cannot recover it.
func (a *HumanAccess) MintSession(identity, sessionID, csrfToken string, issuedAt, expiresAt time.Time) (string, error) {
	if len(a.secret) < 32 || strings.TrimSpace(identity) == "" || strings.TrimSpace(sessionID) == "" ||
		strings.TrimSpace(csrfToken) == "" || !expiresAt.After(issuedAt) {
		return "", ErrHumanUnauthorized
	}
	claims := humanClaims{Version: 1, Subject: identity, SessionID: sessionID,
		IssuedAt: issuedAt.UTC().Unix(), ExpiresAt: expiresAt.UTC().Unix(), CSRFHash: hashCSRF(csrfToken)}
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + humanSign(a.secret, payload), nil
}

func (a *HumanAccess) Authenticate(r *http.Request) (HumanPrincipal, error) {
	if cookie, err := r.Cookie(HumanSessionCookie); err == nil && strings.TrimSpace(cookie.Value) != "" {
		claims, err := a.verifySession(cookie.Value)
		if err != nil {
			return HumanPrincipal{}, err
		}
		return HumanPrincipal{Identity: claims.Subject, SessionID: claims.SessionID,
			credentialKind: "session", csrfHash: claims.CSRFHash}, nil
	}
	if a.automation != nil {
		if identity, err := a.automation.Authenticate(r); err == nil && identity != "" {
			return HumanPrincipal{Identity: identity, SessionID: "automation:" + identity,
				credentialKind: "automation"}, nil
		}
	}
	if a.allowLoopbackDev && isLoopback(r) {
		return HumanPrincipal{Identity: "loopback-human", SessionID: "loopback-dev",
			credentialKind: "loopback"}, nil
	}
	return HumanPrincipal{}, ErrHumanUnauthorized
}

func (a *HumanAccess) verifySession(token string) (humanClaims, error) {
	if len(a.secret) < 32 {
		return humanClaims{}, ErrHumanUnauthorized
	}
	dot := strings.LastIndexByte(token, '.')
	if dot <= 0 || dot == len(token)-1 {
		return humanClaims{}, ErrHumanUnauthorized
	}
	payload, mac := token[:dot], token[dot+1:]
	if !hmac.Equal([]byte(mac), []byte(humanSign(a.secret, payload))) {
		return humanClaims{}, ErrHumanUnauthorized
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return humanClaims{}, ErrHumanUnauthorized
	}
	var claims humanClaims
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Version != 1 || claims.Subject == "" ||
		claims.SessionID == "" || claims.CSRFHash == "" || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return humanClaims{}, ErrHumanUnauthorized
	}
	now := a.now().UTC().Unix()
	if now < claims.IssuedAt-30 || now >= claims.ExpiresAt {
		return humanClaims{}, ErrHumanUnauthorized
	}
	return claims, nil
}

func (a *HumanAccess) Authorize(p HumanPrincipal, projectID string, action HumanAction) error {
	if p.Identity == "" || projectID == "" {
		return ErrHumanForbidden
	}
	if p.credentialKind == "loopback" && a.allowLoopbackDev {
		return nil
	}
	for _, grant := range a.grants[p.Identity] {
		if (grant.ProjectID == projectID || grant.ProjectID == "*") && roleAllows(grant.Role, action) {
			return nil
		}
	}
	return ErrHumanForbidden
}

// AuthorizePortfolio is intentionally distinct from Authorize: a project grant
// cannot be widened into the cross-project Needs You feed.
func (a *HumanAccess) AuthorizePortfolio(p HumanPrincipal, action HumanAction) error {
	if p.credentialKind == "loopback" && a.allowLoopbackDev {
		return nil
	}
	for _, grant := range a.grants[p.Identity] {
		if grant.ProjectID == "*" && roleAllows(grant.Role, action) {
			return nil
		}
	}
	return ErrHumanForbidden
}

func (a *HumanAccess) ValidateCSRF(r *http.Request, p HumanPrincipal) error {
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions ||
		p.credentialKind != "session" {
		return nil
	}
	got := hashCSRF(strings.TrimSpace(r.Header.Get("X-Flowbee-CSRF")))
	if !hmac.Equal([]byte(got), []byte(p.csrfHash)) {
		return ErrHumanCSRF
	}
	return nil
}

// HumanMiddleware authenticates a dashboard/API human before a Phase-1 route
// runs and enforces CSRF for cookie-backed browser mutations. Authorization is
// intentionally left to the handler because the project is part of the typed
// request, not a trustworthy ambient URL for every endpoint.
func HumanMiddleware(a *HumanAccess, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a == nil {
			http.Error(w, "human authentication is not configured", http.StatusUnauthorized)
			return
		}
		principal, err := a.Authenticate(r)
		if err != nil {
			http.Error(w, "human authentication required", http.StatusUnauthorized)
			return
		}
		if err := a.ValidateCSRF(r, principal); err != nil {
			http.Error(w, "CSRF validation failed", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), humanPrincipalCtxKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func HumanPrincipalFrom(r *http.Request) (HumanPrincipal, bool) {
	p, ok := r.Context().Value(humanPrincipalCtxKey{}).(HumanPrincipal)
	return p, ok && p.Identity != ""
}

func roleAllows(role HumanRole, action HumanAction) bool {
	if role == HumanAdmin {
		return true
	}
	switch action {
	case HumanDecisionRead, HumanWorkIntentRead, HumanConversationRead, HumanProjectRead:
		return role == HumanViewer || role == HumanApprover || role == HumanOperator || role == HumanPlanner
	case HumanDecisionView, HumanDecisionRespond, HumanConversationSend:
		return role == HumanApprover || role == HumanOperator || role == HumanPlanner
	case HumanWorkIntentPause, HumanWorkIntentResume, HumanWorkIntentCancel:
		return role == HumanOperator
	case HumanConversationManage:
		return role == HumanOperator || role == HumanPlanner
	case HumanDecisionCreate, HumanWorkIntentCreate, HumanWorkIntentDefine, HumanWorkIntentRegister:
		return role == HumanPlanner
	default:
		return false
	}
}

func validHumanRole(role HumanRole) bool {
	return role == HumanViewer || role == HumanApprover || role == HumanOperator ||
		role == HumanPlanner || role == HumanAdmin
}

func humanSign(secret []byte, payload string) string {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("flowbee-human-session/v1\x00"))
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func hashCSRF(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
