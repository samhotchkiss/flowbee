package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHumanSessionIsExpiringProjectScopedAndCSRFBound(t *testing.T) {
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	a := NewHumanAccess([]byte("01234567890123456789012345678901"), nil,
		map[string][]HumanGrant{"sam": {{ProjectID: "russ", Role: HumanApprover}}}, false)
	a.now = func() time.Time { return now }
	token, err := a.MintSession("sam", "browser-1", "csrf-secret", now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://flowbee/v1/decisions/d/approve", nil)
	r.RemoteAddr = "100.64.0.2:1234"
	r.AddCookie(&http.Cookie{Name: HumanSessionCookie, Value: token})
	r.Header.Set("X-Flowbee-CSRF", "csrf-secret")
	p, err := a.Authenticate(r)
	if err != nil || p.Identity != "sam" || p.SessionID != "browser-1" {
		t.Fatalf("principal=%+v err=%v", p, err)
	}
	if err := a.ValidateCSRF(r, p); err != nil {
		t.Fatalf("valid CSRF: %v", err)
	}
	if err := a.Authorize(p, "russ", HumanDecisionRespond); err != nil {
		t.Fatalf("project approval: %v", err)
	}
	if err := a.Authorize(p, "other", HumanDecisionRespond); !errors.Is(err, ErrHumanForbidden) {
		t.Fatalf("cross-project grant widened: %v", err)
	}
	if err := a.AuthorizePortfolio(p, HumanDecisionRead); !errors.Is(err, ErrHumanForbidden) {
		t.Fatalf("project grant widened to portfolio: %v", err)
	}
	r.Header.Set("X-Flowbee-CSRF", "wrong")
	if err := a.ValidateCSRF(r, p); !errors.Is(err, ErrHumanCSRF) {
		t.Fatalf("wrong CSRF accepted: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := a.Authenticate(r); !errors.Is(err, ErrHumanUnauthorized) {
		t.Fatalf("expired session accepted: %v", err)
	}
}

func TestWorkerEnrollmentDoesNotImplyHumanAuthority(t *testing.T) {
	worker := NewBearer([]byte("worker-secret"), []string{"worker-a", "operator-a"}, false)
	a := NewHumanAccess([]byte("01234567890123456789012345678901"), worker,
		map[string][]HumanGrant{"operator-a": {{ProjectID: "russ", Role: HumanOperator}}}, false)
	request := func(identity string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "https://flowbee/v1/work-intents/i/pause", nil)
		r.RemoteAddr = "100.64.0.2:1234"
		r.Header.Set("Authorization", "Bearer "+worker.Mint(identity))
		return r
	}
	p, err := a.Authenticate(request("worker-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(p, "russ", HumanWorkIntentPause); !errors.Is(err, ErrHumanForbidden) {
		t.Fatalf("worker enrollment granted dashboard authority: %v", err)
	}
	p, err = a.Authenticate(request("operator-a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Authorize(p, "russ", HumanWorkIntentPause); err != nil {
		t.Fatalf("explicit automation grant rejected: %v", err)
	}
}

func TestHumanConversationActionsAreProjectScopedByRole(t *testing.T) {
	a := NewHumanAccess(nil, nil, map[string][]HumanGrant{
		"viewer":  {{ProjectID: "russ", Role: HumanViewer}},
		"planner": {{ProjectID: "russ", Role: HumanPlanner}},
	}, true)
	viewer := HumanPrincipal{Identity: "viewer"}
	planner := HumanPrincipal{Identity: "planner"}
	// Use a non-loopback credential kind so this exercises the role matrix instead
	// of the development bypass.
	viewer.credentialKind = "session"
	planner.credentialKind = "session"
	if err := a.Authorize(viewer, "russ", HumanConversationRead); err != nil {
		t.Fatalf("viewer read: %v", err)
	}
	if err := a.Authorize(viewer, "russ", HumanConversationSend); !errors.Is(err, ErrHumanForbidden) {
		t.Fatalf("viewer send=%v", err)
	}
	if err := a.Authorize(planner, "russ", HumanConversationSend); err != nil {
		t.Fatalf("planner send: %v", err)
	}
	if err := a.Authorize(planner, "russ", HumanConversationManage); err != nil {
		t.Fatalf("planner manage: %v", err)
	}
	if err := a.Authorize(planner, "other", HumanConversationRead); !errors.Is(err, ErrHumanForbidden) {
		t.Fatalf("cross-project conversation read=%v", err)
	}
}

func TestHumanSessionRejectsTamperingAndNonLoopbackAnonymous(t *testing.T) {
	now := time.Now().UTC()
	a := NewHumanAccess([]byte("01234567890123456789012345678901"), nil, nil, false)
	a.now = func() time.Time { return now }
	token, err := a.MintSession("sam", "s1", "csrf", now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", token + "x"} {
		r := httptest.NewRequest(http.MethodGet, "https://flowbee/v1/decisions", nil)
		r.RemoteAddr = "100.64.0.2:1234"
		if value != "" {
			r.AddCookie(&http.Cookie{Name: HumanSessionCookie, Value: value})
		}
		if _, err := a.Authenticate(r); !errors.Is(err, ErrHumanUnauthorized) {
			t.Fatalf("credential %q accepted: %v", value, err)
		}
	}
}

func TestParseHumanGrantsNeverInfersPortfolioAuthority(t *testing.T) {
	grants, err := ParseHumanGrants([]string{"sam@russ=approver", "sam@*=viewer", "interactor@russ=planner"})
	if err != nil {
		t.Fatal(err)
	}
	if len(grants["sam"]) != 2 || grants["sam"][0].ProjectID != "russ" || grants["sam"][1].ProjectID != "*" {
		t.Fatalf("parsed grants=%+v", grants)
	}
	for _, invalid := range []string{"sam=admin", "sam@=admin", "@russ=admin", "sam@russ=owner", "sam@russ"} {
		if _, err := ParseHumanGrants([]string{invalid}); err == nil {
			t.Fatalf("invalid grant %q accepted", invalid)
		}
	}
}
