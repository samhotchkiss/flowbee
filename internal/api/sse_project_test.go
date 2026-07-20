package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestPhase2ProjectSSECarriesExplicitScope(t *testing.T) {
	b := NewBroker()
	id, ch := b.subscribe()
	defer b.unsubscribe(id)
	b.Publish(LifeEvent{ProjectID: "mail", State: "projects", Event: "project_state_changed"})
	blob := <-ch
	if topic := topicOf(blob); topic != "projects" {
		t.Fatalf("topic=%q", topic)
	}
	var got LifeEvent
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != "mail" || got.Event != "project_state_changed" {
		t.Fatalf("event lost project scope: %+v", got)
	}
}

func TestGlobalLifecycleNudgesAreContentFreeAllowlistedWakes(t *testing.T) {
	tests := []struct {
		name  string
		event LifeEvent
		want  bool
	}{
		{name: "generic capacity wake", event: LifeEvent{State: "capacity", Event: "account_at_ceiling", Global: true}, want: true},
		{name: "generic epic wake", event: LifeEvent{State: "epics", Event: "capacity_fold", DigestSeq: 42, Global: true}, want: true},
		{name: "project identifier", event: LifeEvent{ProjectID: "alpha", State: "capacity", Event: "account_at_ceiling", Global: true}},
		{name: "job identifier", event: LifeEvent{JobID: "job-alpha", State: "capacity", Event: "account_at_ceiling", Global: true}},
		{name: "lease or session epoch", event: LifeEvent{State: "capacity", Event: "account_at_ceiling", Epoch: 7, Global: true}},
		{name: "account identifier in event", event: LifeEvent{State: "capacity", Event: "account_at_ceiling:codex1", Global: true}},
		{name: "session identifier in event", event: LifeEvent{State: "capacity", Event: "session:managed-123", Global: true}},
		{name: "unmarked projectless payload", event: LifeEvent{State: "capacity", Event: "account_at_ceiling"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := globalLifecycleNudgeSafe(tc.event); got != tc.want {
				t.Fatalf("safe=%t want=%t event=%+v", got, tc.want, tc.event)
			}
		})
	}
}

const sseHumanSecret = "01234567890123456789012345678901"

func mintSSESession(t *testing.T, access *auth.HumanAccess, identity string) string {
	t.Helper()
	now := time.Now().UTC()
	token, err := access.MintSession(identity, "sse-"+identity, "csrf-"+identity, now.Add(-time.Minute), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func openLifecycleSSE(t *testing.T, client *http.Client, url, token string) (*http.Response, <-chan LifeEvent) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.HumanSessionCookie, Value: token})
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan LifeEvent, 32)
	if resp.StatusCode == http.StatusOK {
		go func() {
			defer close(events)
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event LifeEvent
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil {
					events <- event
				}
			}
		}()
	}
	return resp, events
}

func TestLifecycleSSEHumanAuthorizationMatrix(t *testing.T) {
	access := auth.NewHumanAccess([]byte(sseHumanSecret), nil, map[string][]auth.HumanGrant{
		"alpha-viewer":     {{ProjectID: "alpha", Role: auth.HumanViewer}},
		"portfolio-viewer": {{ProjectID: "*", Role: auth.HumanViewer}},
	}, false)
	st := testutil.NewStore(t)
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{HumanAccess: access}, "sse-auth-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	alpha := mintSSESession(t, access, "alpha-viewer")
	portfolio := mintSSESession(t, access, "portfolio-viewer")
	offLoopback := httptest.NewRequest(http.MethodGet, "/v1/events?project_id=alpha", nil)
	offLoopback.RemoteAddr = "100.64.12.34:4444"
	offResponse := httptest.NewRecorder()
	srv.PrivateHandler().ServeHTTP(offResponse, offLoopback)
	if offResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated off-loopback status=%d want=%d", offResponse.Code, http.StatusUnauthorized)
	}

	tests := []struct {
		name, path, token string
		want              int
	}{
		{name: "unauthenticated denied", path: "/v1/events?project_id=alpha", want: http.StatusUnauthorized},
		{name: "exact project", path: "/v1/events?project_id=alpha", token: alpha, want: http.StatusOK},
		{name: "different project denied", path: "/v1/events?project_id=beta", token: alpha, want: http.StatusForbidden},
		{name: "project grant never globalizes", path: "/v1/events", token: alpha, want: http.StatusForbidden},
		{name: "portfolio grant global access", path: "/v1/events", token: portfolio, want: http.StatusOK},
		{name: "wildcard is not a project parameter", path: "/v1/events?project_id=*", token: portfolio, want: http.StatusBadRequest},
		{name: "blank scope rejected", path: "/v1/events?project_id=", token: portfolio, want: http.StatusBadRequest},
		{name: "ambiguous scope rejected", path: "/v1/events?project_id=alpha&project_id=beta", token: portfolio, want: http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := openLifecycleSSE(t, ts.Client(), ts.URL+tc.path, tc.token)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status=%d want=%d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestLifecycleSSELoopbackDevAndConcurrentProjectIsolation(t *testing.T) {
	// The explicit development posture remains cookie-free on loopback.
	devStore := testutil.NewStore(t)
	devServer := New(devStore, clock.Real{}, ulid.NewMinter(nil), Config{}, "sse-loopback-test")
	devHTTP := httptest.NewServer(devServer.PrivateHandler())
	resp, _ := openLifecycleSSE(t, devHTTP.Client(), devHTTP.URL+"/v1/events?project_id=alpha", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback development status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	devHTTP.Close()

	access := auth.NewHumanAccess([]byte(sseHumanSecret), nil, map[string][]auth.HumanGrant{
		"alpha": {{ProjectID: "alpha", Role: auth.HumanViewer}},
		"beta":  {{ProjectID: "beta", Role: auth.HumanViewer}},
	}, false)
	st := testutil.NewStore(t)
	for _, projectID := range []string{"alpha", "beta"} {
		if _, err := st.CreatePortfolioProject(context.Background(), store.PortfolioProject{
			ID: projectID, Name: projectID,
		}, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.SeedJob(context.Background(), store.SeedParams{
			ID: projectID + "-job", ProjectID: projectID, Kind: job.KindBuild, Flow: "build",
			Stage: "build", Role: job.RoleEngWorker, Now: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{HumanAccess: access}, "sse-isolation-test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	alphaResp, alphaEvents := openLifecycleSSE(t, ts.Client(), ts.URL+"/v1/events?project_id=alpha", mintSSESession(t, access, "alpha"))
	defer alphaResp.Body.Close()
	betaResp, betaEvents := openLifecycleSSE(t, ts.Client(), ts.URL+"/v1/events?project_id=beta", mintSSESession(t, access, "beta"))
	defer betaResp.Body.Close()

	const perProject = 12
	var publishes sync.WaitGroup
	for i := 0; i < perProject; i++ {
		publishes.Add(2)
		go func() {
			defer publishes.Done()
			srv.Broker().Publish(LifeEvent{JobID: "alpha-job", State: "building", Event: "alpha-only"})
		}()
		go func() {
			defer publishes.Done()
			srv.Broker().Publish(LifeEvent{JobID: "beta-job", State: "building", Event: "beta-only"})
		}()
	}
	publishes.Wait()
	// A Global assertion is not enough: free-form identity-bearing payloads are
	// rejected, while the generic content-free wake reaches both projects.
	srv.Broker().Publish(LifeEvent{State: "capacity", Event: "account_at_ceiling:account-secret", Global: true})
	srv.Broker().Publish(LifeEvent{State: "capacity", Event: "account_at_ceiling", Global: true})
	srv.Broker().Publish(LifeEvent{State: "control", Event: "unscoped-owned-data"})
	srv.Broker().Publish(LifeEvent{ProjectID: "alpha", JobID: "beta-job", State: "building", Event: "forged-scope"})

	assertStream := func(name, projectID string, events <-chan LifeEvent) {
		t.Helper()
		deadline := time.After(3 * time.Second)
		projectCount, globalCount := 0, 0
		for projectCount < perProject || globalCount < 1 {
			select {
			case event := <-events:
				if event.ProjectID != "" && event.ProjectID != projectID {
					t.Fatalf("%s observed cross-project payload: %+v", name, event)
				}
				if event.Event == projectID+"-only" {
					projectCount++
				} else if event.Event == "account_at_ceiling" && event.Global {
					globalCount++
				} else {
					t.Fatalf("%s observed unauthorized/unmarked payload: %+v", name, event)
				}
			case <-deadline:
				t.Fatalf("%s stream incomplete: project=%d/%d global=%d/1", name, projectCount, perProject, globalCount)
			}
		}
	}
	assertStream("alpha", "alpha", alphaEvents)
	assertStream("beta", "beta", betaEvents)
}
