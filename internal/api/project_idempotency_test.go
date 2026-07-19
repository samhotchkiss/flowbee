package api_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestPhase2ProjectMutationAPIReplaysAndRejectsKeyRebinding(t *testing.T) {
	access := auth.NewHumanAccess([]byte(humanTestSecret), nil, map[string][]auth.HumanGrant{
		"admin": {{ProjectID: "mail", Role: auth.HumanAdmin}},
	}, false)
	st, ts := phase2ProjectAPIServer(t, access)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []store.Repo{
		{ID: "mail-repo", Owner: "acme", Repo: "mail", Active: true},
		{ID: "other-repo", Owner: "acme", Repo: "other", Active: true},
	} {
		if err := st.RegisterRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	token := signedHumanSession(t, access, "admin", "csrf-admin")
	post := func(path string, body any, key string) int {
		t.Helper()
		resp, _ := humanRequest(t, ts.Client(), http.MethodPost, ts.URL+path, body,
			token, "csrf-admin", key)
		return resp.StatusCode
	}

	state := map[string]any{"state": "paused", "reason": "maintenance", "expected_state_version": 1}
	if got := post("/v1/projects/mail/state", state, "pause-mail"); got != http.StatusOK {
		t.Fatalf("state first status=%d", got)
	}
	if got := post("/v1/projects/mail/state", state, "pause-mail"); got != http.StatusOK {
		t.Fatalf("state replay status=%d", got)
	}
	if got := post("/v1/projects/mail/state", map[string]any{
		"state": "paused", "reason": "changed", "expected_state_version": 1,
	}, "pause-mail"); got != http.StatusConflict {
		t.Fatalf("state key rebinding status=%d want 409", got)
	}

	if got := post("/v1/projects/mail/repos", map[string]any{"repo_id": "mail-repo"}, "attach-repo"); got != http.StatusNoContent {
		t.Fatalf("repo first status=%d", got)
	}
	if got := post("/v1/projects/mail/repos", map[string]any{"repo_id": "mail-repo"}, "attach-repo"); got != http.StatusNoContent {
		t.Fatalf("repo replay status=%d", got)
	}
	if got := post("/v1/projects/mail/repos", map[string]any{"repo_id": "other-repo"}, "attach-repo"); got != http.StatusConflict {
		t.Fatalf("repo key rebinding status=%d want 409", got)
	}

	actor := map[string]any{"role": store.DriverInteractorRole, "actor_id": "interactor-v1"}
	if got := post("/v1/projects/mail/actors", actor, "bind-interactor"); got != http.StatusOK {
		t.Fatalf("actor first status=%d", got)
	}
	if got := post("/v1/projects/mail/actors", actor, "bind-interactor"); got != http.StatusOK {
		t.Fatalf("actor replay status=%d", got)
	}
	if got := post("/v1/projects/mail/actors", map[string]any{
		"role": store.DriverInteractorRole, "actor_id": "interactor-v2",
	}, "bind-interactor"); got != http.StatusConflict {
		t.Fatalf("actor key rebinding status=%d want 409", got)
	}
}
