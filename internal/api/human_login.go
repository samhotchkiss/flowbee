package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const humanCSRFCookie = "flowbee_csrf"

// humanLoginPage deliberately receives the one-time bearer in the URL fragment:
// fragments are not sent in HTTP requests, access logs, or Referer headers. The
// tiny bootstrap exchanges it once, erases the fragment, and redirects home.
func (s *Server) humanLoginPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="no-referrer"><title>Flowbee sign in</title><style>body{background:#0b1015;color:#d7dee7;font:16px system-ui;display:grid;place-items:center;min-height:100vh}main{max-width:34rem;padding:2rem}p{color:#8e9aa8}</style></head><body><main><h1>Opening Flowbee…</h1><p id="status">Exchanging your one-time sign-in link.</p></main><script>(async()=>{const status=document.getElementById('status');const token=new URLSearchParams(location.hash.slice(1)).get('token');history.replaceState(null,'',location.pathname);if(!token){status.textContent='This sign-in link is missing its one-time token.';return}try{const r=await fetch('/v1/human/session',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({token})});if(!r.ok)throw new Error('Sign-in failed or this link was already used.');location.replace('/dashboard')}catch(e){status.textContent=e.message}})()</script></body></html>`))
}

func (s *Server) humanSessionCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		http.Error(w, "invalid or expired sign-in link", http.StatusUnauthorized)
		return
	}
	now := s.clock.Now().UTC()
	login, err := s.store.ConsumeHumanLoginToken(r.Context(), body.Token, now)
	if err != nil {
		// Deliberately collapse invalid/expired/used so this endpoint is not a
		// token-state oracle.
		if errors.Is(err, store.ErrHumanLoginInvalid) || errors.Is(err, store.ErrHumanLoginExpired) || errors.Is(err, store.ErrHumanLoginUsed) {
			http.Error(w, "invalid or expired sign-in link", http.StatusUnauthorized)
			return
		}
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	csrf, err := randomBrowserSecret(32)
	if err != nil {
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	session, err := s.human.MintSession(login.Identity, login.SessionID, csrf, now, now.Add(12*time.Hour))
	if err != nil {
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	secure := r.TLS != nil || strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
	http.SetCookie(w, &http.Cookie{Name: auth.HumanSessionCookie, Value: session, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
	// This is a double-submit value, not an authentication credential. It must
	// remain readable so dashboard JS can restore CSRF protection after reload.
	http.SetCookie(w, &http.Cookie{Name: humanCSRFCookie, Value: csrf, Path: "/",
		HttpOnly: false, Secure: secure, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"authenticated":true}`))
}

// humanLoginLinkCreate is the authenticated bootstrap edge. It creates a link
// only for the already-authenticated principal and requires an explicit project
// grant; no caller can mint a browser identity for somebody else.
func (s *Server) humanLoginLinkCreate(w http.ResponseWriter, r *http.Request) {
	principal, ok := auth.HumanPrincipalFrom(r)
	if !ok {
		http.Error(w, "human authentication required", http.StatusUnauthorized)
		return
	}
	var body struct {
		ProjectID string `json:"project_id"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil || strings.TrimSpace(body.ProjectID) == "" {
		http.Error(w, "project_id is required", http.StatusBadRequest)
		return
	}
	if _, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanDecisionRead); !ok {
		return
	}
	rawToken, err := randomBrowserSecret(32)
	if err != nil {
		http.Error(w, "login link unavailable", http.StatusInternalServerError)
		return
	}
	sessionID, err := randomBrowserSecret(24)
	if err != nil {
		http.Error(w, "login link unavailable", http.StatusInternalServerError)
		return
	}
	now := s.clock.Now().UTC()
	if err := s.store.CreateHumanLoginToken(r.Context(), rawToken, principal.Identity, sessionID, now.Add(10*time.Minute), now); err != nil {
		http.Error(w, "login link unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, map[string]any{
		"login_fragment_path": "/login#token=" + rawToken,
		"expires_at":          now.Add(10 * time.Minute).Format(time.RFC3339Nano),
	})
}

func randomBrowserSecret(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
