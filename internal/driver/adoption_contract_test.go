package driver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"testing"
)

func TestHTTPPortExternalLifecycleUncertainReceiptEntersReconciliation(t *testing.T) {
	for _, operation := range []string{"adopt", "reattach", "release"} {
		t.Run(operation, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_ = json.NewEncoder(w).Encode(map[string]any{"receipt": map[string]any{
					"format_version":       "tmux-driver.lifecycle-receipt/v2",
					"lifecycle_receipt_id": "99999999-9999-4999-8999-999999999999",
					"operation":            operation, "action_id": operation + "-action", "action_epoch": 4,
					"lease_id": "lease", "lease_epoch": 2, "lifecycle_key": "actor:mail",
					"tmux_server_domain_id": "flowbee", "external_watch_id": "88888888-8888-4888-8888-888888888888",
					"target_epoch": 3, "status": "uncertain", "identity_before": nil,
					"identity_after": nil, "absence_observed_at": nil, "diagnostic_code": "verification_pending",
				}})
			}))
			defer srv.Close()
			ownership := ""
			if operation == "release" || operation == "reattach" {
				ownership = "external_observed"
			}
			target := SessionTarget{Identity: Identity{HostID: "host", StoreID: "store",
				TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", Ownership: ownership,
				LifecycleKey: "actor:mail", TargetEpoch: 3, SessionID: "session", PaneInstanceID: "pane", AgentRunID: "run"},
				LifecycleKey: "actor:mail", TargetEpoch: 3, ProfileID: "external_actor_policy",
				LeaseID: "lease", LeaseEpoch: 2, ExternalWatchID: "88888888-8888-4888-8888-888888888888"}
			action := NewAction(operation+"-action", operation, 4)
			var receipt LifecycleReceipt
			var err error
			switch operation {
			case "adopt":
				receipt, err = (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).AdoptSession(context.Background(), target, action)
			case "reattach":
				receipt, err = (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ReattachSession(context.Background(), target, action)
			case "release":
				receipt, err = (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ReleaseSession(context.Background(), target, action)
			}
			if !errors.Is(err, ErrUncertain) || receipt.Status != "uncertain" || calls != 1 {
				t.Fatalf("receipt=%+v err=%v calls=%d", receipt, err, calls)
			}
		})
	}
}

func TestFakeExternalLifecycleAdoptReattachReleaseDoesNotMutateTmuxSession(t *testing.T) {
	fake := NewFake()
	watch, err := fake.EnsureExternalWatch(context.Background(), "%42", "claude", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	id := Identity{HostID: "host", StoreID: "store", TmuxServerDomainID: "default",
		TmuxServerInstanceID: "server", SessionID: "session", PaneInstanceID: "pane", AgentRunID: "run"}
	fake.Sessions[id.SessionID] = id
	target := SessionTarget{Identity: id, LifecycleKey: "actor:russ", TargetEpoch: 1,
		ProfileID: "external_actor_policy", LeaseID: "lease", LeaseEpoch: 1, ExternalWatchID: watch.WatchID}
	adopted, err := fake.AdoptSession(context.Background(), target, NewAction("adopt", "adopt", 1))
	if err != nil {
		t.Fatal(err)
	}
	target.Identity = adopted.IdentityAfter
	if _, err := fake.ReattachSession(context.Background(), target, NewAction("reattach", "reattach", 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.ReleaseSession(context.Background(), target, NewAction("release", "release", 3)); err != nil {
		t.Fatal(err)
	}
	if _, ok := fake.Sessions[id.SessionID]; !ok || fake.WatchCalls != 1 || fake.AdoptCalls != 1 ||
		fake.ReattachCalls != 1 || fake.ReleaseCalls != 1 {
		t.Fatalf("session retained=%v counters watch/adopt/reattach/release=%d/%d/%d/%d",
			ok, fake.WatchCalls, fake.AdoptCalls, fake.ReattachCalls, fake.ReleaseCalls)
	}
}

func TestHTTPPortExternalWatchSnapshotAdoptReleaseExactContract(t *testing.T) {
	const (
		host    = "11111111-1111-4111-8111-111111111111"
		store   = "22222222-2222-4222-8222-222222222222"
		server  = "44444444-4444-4444-8444-444444444444"
		session = "55555555-5555-4555-8555-555555555555"
		pane    = "66666666-6666-4666-8666-666666666666"
		run     = "77777777-7777-4777-8777-777777777777"
		watchID = "88888888-8888-4888-8888-888888888888"
	)
	var calls []string
	identityWire := func(ownership string) map[string]any {
		return map[string]any{
			"host_id": host, "store_id": store, "tmux_server_domain_id": "flowbee",
			"tmux_server_instance_id": server, "ownership": ownership,
			"lifecycle_key": "project:mail:interactor", "target_epoch": 3,
			"session_id": session, "pane_instance_id": pane, "agent_run_id": run,
			"provider": "claude", "conversation_id": nil, "state_cursor": "tdc2.high",
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "POST /v2/watches":
			if r.Header.Get("Idempotency-Key") != "" {
				t.Errorf("watch bootstrap unexpectedly used lifecycle idempotency key")
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"profile", "provider_hint", "requirements", "target"}) {
				t.Errorf("watch fields=%v", got)
			}
			target := body["target"].(map[string]any)
			if got := sortedKeys(target); !reflect.DeepEqual(got, []string{"follow_policy", "pane_id", "selector"}) ||
				target["pane_id"] != "%42" || body["profile"] != "interactive" {
				t.Errorf("watch target=%v profile=%v", target, body["profile"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"watch": map[string]any{
				"watch_id": watchID, "enabled": true, "lifecycle": "active", "provider_hint": "claude",
				"profile": "interactive", "target": map[string]any{
					"selector": "pane_id", "pane_id": "%42", "follow_policy": "exact_incarnation"},
			}})
		case "GET /v2/meta":
			_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(false, false))
		case "GET /v2/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"as_of_cursor": "tdc2.high",
				"sessions": []map[string]any{{"session_id": session}}, "next_cursor": nil})
		case "GET /v2/sessions/" + session:
			_ = json.NewEncoder(w).Encode(map[string]any{"as_of_cursor": "tdc2.high", "state_revision": 4,
				"session": map[string]any{"format": "tmux-driver.session/v2", "session_id": session,
					"host_id": host, "store_id": store, "provider": "claude", "pane_instance_id": pane,
					"watch_id": watchID},
				"state": map[string]any{"agent_run_id": run, "tmux_server_instance_id": server,
					"lifecycle": "observing", "phase": "working", "binding_status": "bound", "binding_epoch": 1}})
		case "POST /v2/lifecycle/adopt":
			if r.Header.Get("Idempotency-Key") != "adopt-mail" {
				t.Errorf("adopt idempotency=%q", r.Header.Get("Idempotency-Key"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assertExternalLifecycleRequest(t, body, true, watchID)
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": map[string]any{
				"format_version": "tmux-driver.lifecycle-receipt/v2", "lifecycle_receipt_id": "99999999-9999-4999-8999-999999999999",
				"operation": "adopt", "action_id": "adopt-mail", "action_epoch": 1,
				"lease_id": "project-bind:mail", "lease_epoch": 1, "lifecycle_key": "project:mail:interactor",
				"tmux_server_domain_id": "flowbee", "external_watch_id": watchID,
				"target_epoch": 3, "status": "adopted", "identity_before": identityWire("external_observed"),
				"identity_after": identityWire("external_observed"), "absence_observed_at": nil, "diagnostic_code": nil,
			}})
		case "POST /v2/lifecycle/release":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			assertExternalLifecycleRequest(t, body, false, watchID)
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": map[string]any{
				"format_version": "tmux-driver.lifecycle-receipt/v2", "lifecycle_receipt_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
				"operation": "release", "action_id": "release-mail", "action_epoch": 3,
				"lease_id": "project-bind:mail", "lease_epoch": 1, "lifecycle_key": "project:mail:interactor",
				"tmux_server_domain_id": "flowbee", "external_watch_id": watchID,
				"target_epoch": 3, "status": "released", "identity_before": identityWire("external_observed"),
				"identity_after": nil, "absence_observed_at": nil, "diagnostic_code": nil,
			}})
		case "POST /v2/lifecycle/reattach":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["format_version"] != "tmux-driver.lifecycle-reattach/v2" {
				t.Errorf("reattach format=%v", body["format_version"])
			}
			target := body["target"].(map[string]any)
			want := []string{"expected_agent_run_id", "expected_host_id", "expected_pane_instance_id", "expected_session_id",
				"expected_store_id", "expected_tmux_server_domain_id", "expected_tmux_server_instance_id", "lifecycle_key", "target_epoch"}
			if got := sortedKeys(target); !reflect.DeepEqual(got, sortedStrings(want)) {
				t.Errorf("reattach target fields=%v", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": map[string]any{
				"format_version": "tmux-driver.lifecycle-receipt/v2", "lifecycle_receipt_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
				"operation": "reattach", "action_id": "reattach-mail", "action_epoch": 2,
				"lease_id": "project-bind:mail", "lease_epoch": 1, "lifecycle_key": "project:mail:interactor",
				"tmux_server_domain_id": "flowbee", "external_watch_id": watchID,
				"target_epoch": 3, "status": "reattached", "identity_before": identityWire("external_observed"),
				"identity_after": identityWire("external_observed"), "absence_observed_at": nil, "diagnostic_code": nil,
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	watch, err := p.EnsureExternalWatch(context.Background(), "%42", "claude", "interactive")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.SnapshotSessions(context.Background())
	if err != nil || len(snapshot.Sessions) != 1 || snapshot.Sessions[0].WatchID != watch.WatchID {
		t.Fatalf("snapshot=%+v watch=%+v err=%v", snapshot, watch, err)
	}
	target := SessionTarget{Identity: snapshot.Sessions[0].Identity,
		LifecycleKey: "project:mail:interactor", TargetEpoch: 3, ProfileID: "external_actor_policy",
		LeaseID: "project-bind:mail", LeaseEpoch: 1, ExternalWatchID: watch.WatchID}
	adopted, err := p.AdoptSession(context.Background(), target, NewAction("adopt-mail", "adopt", 1))
	if err != nil || adopted.IdentityAfter.Ownership != "external_observed" {
		t.Fatalf("adopted=%+v err=%v", adopted, err)
	}
	target.Identity = adopted.IdentityAfter
	reattached, err := p.ReattachSession(context.Background(), target, NewAction("reattach-mail", "reattach", 2))
	if err != nil || reattached.Status != "reattached" {
		t.Fatalf("reattached=%+v err=%v", reattached, err)
	}
	released, err := p.ReleaseSession(context.Background(), target, NewAction("release-mail", "release", 3))
	if err != nil || released.Status != "released" {
		t.Fatalf("released=%+v err=%v", released, err)
	}
	wantCalls := []string{"POST /v2/watches", "GET /v2/meta", "GET /v2/sessions",
		"GET /v2/sessions/" + session, "POST /v2/lifecycle/adopt", "POST /v2/lifecycle/reattach", "POST /v2/lifecycle/release"}
	if !reflect.DeepEqual(calls, wantCalls) {
		t.Fatalf("calls=%v", calls)
	}
}

func assertExternalLifecycleRequest(t *testing.T, body map[string]any, adopt bool, watchID string) {
	t.Helper()
	if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_epoch", "action_id", "format_version", "lease_epoch", "lease_id", "target"}) {
		t.Errorf("lifecycle fields=%v", got)
	}
	target := body["target"].(map[string]any)
	want := []string{"expected_agent_run_id", "expected_host_id", "expected_pane_instance_id", "expected_session_id",
		"expected_store_id", "expected_tmux_server_domain_id", "expected_tmux_server_instance_id", "external_watch_id",
		"lifecycle_key", "target_epoch"}
	if adopt {
		want = append(want, "profile_id")
	}
	// sortedKeys sorts both the actual fields and the expected slice.
	if got := sortedKeys(target); !reflect.DeepEqual(got, sortedStrings(want)) {
		t.Errorf("external lifecycle target fields=%v want=%v", got, sortedStrings(want))
	}
	if target["external_watch_id"] != watchID || target["expected_tmux_server_domain_id"] != "flowbee" {
		t.Errorf("external lifecycle target=%v", target)
	}
	if adopt && target["profile_id"] != "external_actor_policy" {
		t.Errorf("adopt lifecycle profile=%v", target["profile_id"])
	}
}

func sortedStrings(values []string) []string {
	out := append([]string(nil), values...)
	slices.Sort(out)
	return out
}
