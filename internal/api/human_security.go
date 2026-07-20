package api

import (
	"net/http"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/auth"
)

func (s *Server) requireHumanProject(w http.ResponseWriter, r *http.Request, projectID string, action auth.HumanAction) (auth.HumanPrincipal, bool) {
	principal, ok := auth.HumanPrincipalFrom(r)
	if !ok {
		http.Error(w, "human authentication required", http.StatusUnauthorized)
		return auth.HumanPrincipal{}, false
	}
	if strings.TrimSpace(projectID) == "" {
		http.Error(w, "project_id is required", http.StatusBadRequest)
		return auth.HumanPrincipal{}, false
	}
	if err := s.human.Authorize(principal, projectID, action); err != nil {
		http.Error(w, "human action is not authorized for this project", http.StatusForbidden)
		return auth.HumanPrincipal{}, false
	}
	return principal, true
}

func (s *Server) requireHumanPortfolio(w http.ResponseWriter, r *http.Request, action auth.HumanAction) (auth.HumanPrincipal, bool) {
	principal, ok := auth.HumanPrincipalFrom(r)
	if !ok {
		http.Error(w, "human authentication required", http.StatusUnauthorized)
		return auth.HumanPrincipal{}, false
	}
	if err := s.human.AuthorizePortfolio(principal, action); err != nil {
		http.Error(w, "portfolio access is not authorized", http.StatusForbidden)
		return auth.HumanPrincipal{}, false
	}
	return principal, true
}

// A decision authorizes exactly the project-scoped artifact shown in the
// request. The client may echo that scope, but it cannot widen it to a repo,
// portfolio, actor, or future artifact wildcard.
func exactHumanAuthorizationScope(projectID, supplied string) (string, bool) {
	want := "project:" + projectID
	if strings.TrimSpace(supplied) == "" || supplied == want {
		return want, true
	}
	return "", false
}
