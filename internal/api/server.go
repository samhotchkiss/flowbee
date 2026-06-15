// Package api hosts Flowbee's two HTTP servers (DESIGN §12.1): a health listener
// and the private worker API. M0 ships /healthz; the worker API endpoints are
// stubbed (501) until M1.
package api

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

	"github.com/samhotchkiss/flowbee/internal/store"
)

type Server struct {
	store        *store.Store
	riverStarted *atomic.Bool
	version      string
}

func New(st *store.Store, riverStarted *atomic.Bool, version string) *Server {
	return &Server{store: st, riverStarted: riverStarted, version: version}
}

// HealthHandler serves the health listener.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// PrivateHandler serves the worker API listener (loopback / Tailscale only).
func (s *Server) PrivateHandler() http.Handler {
	mux := http.NewServeMux()
	notImpl := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented until M1", http.StatusNotImplemented)
	}
	mux.HandleFunc("POST /v1/workers/register", notImpl)
	mux.HandleFunc("GET /v1/lease", notImpl)
	mux.HandleFunc("POST /v1/jobs/{job}/heartbeat", notImpl)
	mux.HandleFunc("POST /v1/jobs/{job}/result", notImpl)
	mux.HandleFunc("POST /v1/jobs/{job}/release", notImpl)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	dbOK := s.store.Ping(r.Context()) == nil
	riverOK := s.riverStarted.Load()

	status, code := "ok", http.StatusOK
	if !dbOK || !riverOK {
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":  status,
		"db":      dbOK,
		"river":   riverOK,
		"version": s.version,
	})
}
