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

const controlRecipientRunID = "77777777-7777-4777-8777-777777777777"

func TestHTTPPortCheckRequiresBothMetaAndInstance(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer control-token" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/v2/meta" {
			_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(true, true))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "control-token"}
	if err := p.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{"/v2/meta", "/v2/instance"}) {
		t.Fatalf("paths=%v", paths)
	}
}

func TestHTTPPortLifecycleEnsureUsesExactV23WireContract(t *testing.T) {
	const host = "11111111-1111-1111-1111-111111111111"
	const store = "22222222-2222-2222-2222-222222222222"
	const server = "33333333-3333-3333-3333-333333333333"
	const session = "44444444-4444-4444-4444-444444444444"
	const pane = "55555555-5555-5555-5555-555555555555"
	const run = "66666666-6666-6666-6666-666666666666"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/lifecycle/ensure" || r.Header.Get("Idempotency-Key") != "action-1" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request path=%s idem=%s auth=%s", r.URL.Path, r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_epoch", "action_id", "format_version", "launch", "lease_epoch", "lease_id", "target"}) {
			t.Errorf("top-level fields=%v", got)
		}
		target := body["target"].(map[string]any)
		if got := sortedKeys(target); !reflect.DeepEqual(got, []string{"expected_host_id", "expected_store_id", "expected_tmux_server_domain_id", "expected_tmux_server_instance_id", "lifecycle_key", "target_epoch"}) {
			t.Errorf("target fields=%v", got)
		}
		launch := body["launch"].(map[string]any)
		if launch["profile_id"] != "codex_builder" {
			t.Errorf("profile=%v", launch["profile_id"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "receipt": map[string]any{
			"format_version":       "tmux-driver.lifecycle-receipt/v2",
			"lifecycle_receipt_id": "77777777-7777-4777-8777-777777777777",
			"operation":            "ensure", "action_id": "action-1", "action_epoch": 4,
			"lease_id": "lease-1", "lease_epoch": 9, "lifecycle_key": "builder-seat-7",
			"tmux_server_domain_id": "flowbee", "target_epoch": 3, "status": "ensured", "identity_after": map[string]any{
				"host_id": host, "store_id": store, "tmux_server_domain_id": "flowbee", "tmux_server_instance_id": server,
				"ownership":     "driver_managed",
				"lifecycle_key": "builder-seat-7", "target_epoch": 3, "session_id": session,
				"pane_instance_id": pane, "agent_run_id": run, "provider": "codex",
				"conversation_id": nil, "state_cursor": nil,
			},
		}})
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	target := SessionTarget{Identity: Identity{HostID: host, StoreID: store, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: server}, LifecycleKey: "builder-seat-7", TargetEpoch: 3, ProfileID: "codex_builder", WorkspaceRootID: "flowbee", WorkspaceRelativePath: "project/epic", LeaseID: "lease-1", LeaseEpoch: 9}
	got, err := p.EnsureSession(context.Background(), target, NewAction("action-1", "assignment", 4))
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != session || got.PaneInstanceID != pane || got.AgentRunID != run || got.LifecycleKey != target.LifecycleKey || got.TargetEpoch != target.TargetEpoch {
		t.Fatalf("identity=%+v", got)
	}
}

func TestHTTPPortLifecycleStopAndVerifyUseExactV23WireContract(t *testing.T) {
	const host = "11111111-1111-1111-1111-111111111111"
	const storeID = "22222222-2222-2222-2222-222222222222"
	const server = "33333333-3333-3333-3333-333333333333"
	const session = "44444444-4444-4444-8444-444444444444"
	const pane = "55555555-5555-4555-8555-555555555555"
	const run = "66666666-6666-4666-8666-666666666666"
	const receiptID = "77777777-7777-4777-8777-777777777777"
	var stopSeen, verifySeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Idempotency-Key") != "park-action" {
			t.Errorf("auth/idempotency=%q/%q", r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"))
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/lifecycle/stop":
			stopSeen = true
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_epoch", "action_id", "format_version", "lease_epoch", "lease_id", "target"}) {
				t.Errorf("stop fields=%v", got)
			}
			target := body["target"].(map[string]any)
			if got := sortedKeys(target); !reflect.DeepEqual(got, []string{"expected_agent_run_id", "expected_host_id", "expected_pane_instance_id", "expected_session_id", "expected_store_id", "expected_tmux_server_domain_id", "expected_tmux_server_instance_id", "lifecycle_key", "target_epoch"}) {
				t.Errorf("stop target fields=%v", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "receipt": map[string]any{
				"format_version":       "tmux-driver.lifecycle-receipt/v2",
				"lifecycle_receipt_id": receiptID, "operation": "stop", "action_id": "park-action",
				"action_epoch": 4, "lease_id": "lease-1", "lease_epoch": 9,
				"lifecycle_key": "builder-seat-7", "tmux_server_domain_id": "flowbee", "target_epoch": 3, "status": "uncertain",
				"identity_before": map[string]any{"host_id": host, "store_id": storeID,
					"tmux_server_domain_id": "flowbee", "tmux_server_instance_id": server,
					"ownership": "driver_managed", "lifecycle_key": "builder-seat-7", "target_epoch": 3,
					"session_id": session, "pane_instance_id": pane, "agent_run_id": run},
			}})
		case "/v2/lifecycle/receipts/" + receiptID + "/verify":
			verifySeen = true
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_epoch", "action_id", "format_version", "lease_epoch", "lease_id"}) {
				t.Errorf("verify fields=%v", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "replayed": true, "receipt": map[string]any{
				"format_version":       "tmux-driver.lifecycle-receipt/v2",
				"lifecycle_receipt_id": receiptID, "operation": "stop", "action_id": "park-action",
				"action_epoch": 5, "lease_id": "lease-1", "lease_epoch": 9,
				"lifecycle_key": "builder-seat-7", "tmux_server_domain_id": "flowbee", "target_epoch": 3, "status": "stopped",
				"absence_observed_at": "2026-07-19T12:00:00Z",
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	target := SessionTarget{Identity: Identity{HostID: host, StoreID: storeID,
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: server, SessionID: session, PaneInstanceID: pane, AgentRunID: run},
		LifecycleKey: "builder-seat-7", TargetEpoch: 3, LeaseID: "lease-1", LeaseEpoch: 9}
	action := NewAction("park-action", "park", 4)
	uncertain, err := p.StopSession(context.Background(), target, action)
	if !errors.Is(err, ErrUncertain) || uncertain.Status != "uncertain" {
		t.Fatalf("stop receipt=%+v err=%v", uncertain, err)
	}
	action.Epoch = 5
	resolved, err := p.VerifyLifecycleEffect(context.Background(), receiptID, target, action)
	if err != nil || resolved.Status != "stopped" || resolved.AbsenceObservedAt == "" {
		t.Fatalf("verify receipt=%+v err=%v", resolved, err)
	}
	if !stopSeen || !verifySeen {
		t.Fatalf("stop=%v verify=%v", stopSeen, verifySeen)
	}
}

func TestHTTPPortLifecyclePresenceUsesFencedTargetLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/lifecycle/targets/builder-seat-7" || r.URL.Query().Get("target_epoch") != "3" {
			t.Errorf("presence request=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"api_version":"v2","server_time":"2026-07-19T12:00:00Z","presence":"absent","identity":null,"observed_at":"2026-07-19T12:00:00Z","freshness_ms":0,"diagnostic_code":null}`))
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	presence, err := p.LifecycleTargetPresence(context.Background(), "builder-seat-7", 3)
	if err != nil || !presence.ExactAbsent() {
		t.Fatalf("presence=%+v err=%v", presence, err)
	}
}

func TestHTTPPortRoutedMessageUsesGrantAndReceiptContract(t *testing.T) {
	const (
		grantID     = "11111111-1111-4111-8111-111111111111"
		senderID    = "22222222-2222-4222-8222-222222222222"
		senderRun   = "33333333-3333-4333-8333-333333333333"
		recipientID = "44444444-4444-4444-8444-444444444444"
		paneID      = "55555555-5555-4555-8555-555555555555"
	)
	var sawGrant, sawMessage, sawLookup bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/routes/grants":
			sawGrant = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"epoch", "grant_id", "maximum_payload_bytes", "recipient_pane_instance_id", "recipient_session_id", "sender_agent_run_id", "sender_session_id"}) {
				t.Errorf("grant fields=%v", got)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "grant": map[string]any{
				"format_version": "tmux-driver.route-grant/v1", "grant_id": grantID,
				"issuer_principal_id": "flowbee-control", "sender_session_id": senderID,
				"sender_agent_run_id": senderRun, "recipient_session_id": recipientID,
				"recipient_pane_instance_id": paneID, "operation": "message", "epoch": 7,
				"maximum_payload_bytes": 1024, "allow_draft_stash": false,
				"issued_at": "2026-07-19T12:00:00.000Z", "expires_at": nil, "revoked_at": nil,
			}})
		case "/v2/messages":
			sawMessage = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_id", "grant_epoch", "grant_id", "on_behalf_of_session_id", "payload", "payload_sha256", "recipient_session_id"}) {
				t.Errorf("message fields=%v", got)
			}
			if r.Header.Get("Idempotency-Key") != "action-msg" {
				t.Errorf("idempotency key=%q", r.Header.Get("Idempotency-Key"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "receipt": receiptFixture()})
		case "/v2/messages/receipts/by-action/action-msg":
			sawLookup = true
			if r.URL.Query().Get("grant_epoch") != "7" || r.URL.Query().Get("sender_session_id") != senderID {
				t.Errorf("lookup query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v2", "receipt": receiptFixture()})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	g := Grant{GrantID: grantID, SenderSessionID: senderID, SenderAgentRunID: senderRun, RecipientSessionID: recipientID, RecipientPaneInstanceID: paneID, Epoch: 7, MaximumPayloadBytes: 1024}
	if err := p.Grant(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	a := NewAction("action-msg", "review this", 7)
	a.GrantID, a.GrantEpoch = g.GrantID, g.Epoch
	a.SenderSessionID, a.SenderAgentRunID = g.SenderSessionID, g.SenderAgentRunID
	a.RecipientSessionID, a.RecipientPaneInstanceID = g.RecipientSessionID, g.RecipientPaneInstanceID
	r, err := p.Send(context.Background(), SendRequest{Action: a, GrantID: g.GrantID, GrantEpoch: g.Epoch, RecipientSessionID: g.RecipientSessionID, RecipientPaneInstanceID: g.RecipientPaneInstanceID, OnBehalfOfSessionID: g.SenderSessionID})
	if err != nil || r.Status != ReceiptSubmitted || r.Recipient.PaneInstanceID != paneID {
		t.Fatalf("receipt=%+v err=%v", r, err)
	}
	if _, ok, err := p.ReceiptByAction(context.Background(), a.ExpectedReceipt()); err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
	if !sawGrant || !sawMessage || !sawLookup {
		t.Fatalf("grant=%v message=%v lookup=%v", sawGrant, sawMessage, sawLookup)
	}
}

func TestHTTPPortControlOriginCapabilityRequiresExactAuthorizedContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/meta" {
			_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(true, true))
			return
		}
		if r.URL.Path != "/v2/control/capabilities" || r.URL.RawQuery != "" {
			t.Fatalf("request=%s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capability": map[string]any{
			"format_version": "tmux-driver.control-principal-origin-capability/v1",
			"supported":      true, "authorized": true, "principal_id": "flowbee-control",
			"principal_kind":          "control_plane",
			"required_scopes":         []string{"messages:send", "routes:manage"},
			"granted_scopes":          []string{"messages:send", "routes:manage", "messages:read:any"},
			"missing_scopes":          []string{},
			"route_grant_format":      "tmux-driver.control-route-grant/v2",
			"delivery_receipt_format": "tmux-driver.control-delivery-receipt/v2",
			"grant_endpoint":          "/v2/routes/grants", "message_endpoint": "/v2/messages",
			"on_behalf_of_session_id": "forbidden",
		}})
	}))
	defer srv.Close()
	capability, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ControlOriginCapability(context.Background())
	if err != nil || capability.PrincipalID != "flowbee-control" || !capability.Authorized {
		t.Fatalf("capability=%+v err=%v", capability, err)
	}
}

func TestHTTPPortControlOriginCapabilityFailsClosed(t *testing.T) {
	fixture := map[string]any{
		"format_version": "tmux-driver.control-principal-origin-capability/v1",
		"supported":      true, "authorized": false, "principal_id": "flowbee-control",
		"principal_kind": "control_plane", "required_scopes": []string{"messages:send", "routes:manage"},
		"granted_scopes": []string{"routes:manage"}, "missing_scopes": []string{"messages:send"},
		"route_grant_format":      "tmux-driver.control-route-grant/v2",
		"delivery_receipt_format": "tmux-driver.control-delivery-receipt/v2",
		"grant_endpoint":          "/v2/routes/grants", "message_endpoint": "/v2/messages",
		"on_behalf_of_session_id": "forbidden",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/meta" {
			_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(true, true))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capability": fixture})
	}))
	defer srv.Close()
	if _, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ControlOriginCapability(context.Background()); err == nil {
		t.Fatal("unauthorized control origin was advertised as available")
	}
	fixture["authorized"] = true
	fixture["missing_scopes"] = []string{}
	fixture["unexpected_authority"] = "raw-pane"
	if _, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ControlOriginCapability(context.Background()); err == nil {
		t.Fatal("unknown capability field was silently accepted")
	}
}

func TestHTTPPortControlOriginCapabilityRequiresMetaFeature(t *testing.T) {
	for _, test := range []struct {
		name, feature string
		include       bool
	}{
		{name: "missing", include: false},
		{name: "false", feature: "false", include: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilityCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/meta" {
					_ = json.NewEncoder(w).Encode(controlOriginMetaFixture(test.feature == "true", test.include))
					return
				}
				capabilityCalls++
				_ = json.NewEncoder(w).Encode(map[string]any{"capability": map[string]any{}})
			}))
			defer srv.Close()
			if _, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).ControlOriginCapability(context.Background()); err == nil {
				t.Fatal("capability enabled without meta feature")
			}
			if capabilityCalls != 0 {
				t.Fatalf("unauthorized capability endpoint calls=%d", capabilityCalls)
			}
		})
	}
}

func TestHTTPPortControlOriginGrantSendAndOwnReceiptLookup(t *testing.T) {
	const (
		grantID     = "11111111-1111-4111-8111-111111111111"
		recipientID = "44444444-4444-4444-8444-444444444444"
		paneID      = "55555555-5555-4555-8555-555555555555"
	)
	grantCreated := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/routes/grants/"+grantID && !grantCreated:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/routes/grants":
			grantCreated = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			want := []string{"epoch", "expected_recipient_agent_run_id", "grant_id", "maximum_payload_bytes", "recipient_pane_instance_id", "recipient_session_id", "sender_principal_id"}
			if got := sortedKeys(body); !reflect.DeepEqual(got, want) {
				t.Errorf("control grant fields=%v", got)
			}
			if body["sender_principal_id"] != "flowbee-control" {
				t.Errorf("control origin=%v", body["sender_principal_id"])
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"grant": controlGrantFixture(grantID, recipientID, paneID)})
		case r.Method == http.MethodPost && r.URL.Path == "/v2/messages":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if got := sortedKeys(body); !reflect.DeepEqual(got, []string{"action_id", "expected_recipient_agent_run_id", "grant_epoch", "grant_id", "payload", "payload_sha256", "recipient_session_id"}) {
				t.Errorf("control message fields=%v", got)
			}
			if _, present := body["on_behalf_of_session_id"]; present {
				t.Error("control-origin message included on_behalf_of_session_id")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": controlReceiptFixture(grantID, recipientID, paneID)})
		case r.Method == http.MethodGet && r.URL.Path == "/v2/messages/receipts/by-action/action-msg":
			if r.URL.Query().Get("grant_epoch") != "7" || r.URL.Query().Has("sender_session_id") {
				t.Errorf("principal-owned lookup query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"receipt": controlReceiptFixture(grantID, recipientID, paneID)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	g := Grant{GrantID: grantID, SenderPrincipalID: "flowbee-control", RecipientSessionID: recipientID,
		RecipientPaneInstanceID: paneID, ExpectedRecipientAgentRunID: controlRecipientRunID,
		Epoch: 7, MaximumPayloadBytes: 1024}
	if err := p.Grant(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	a := NewAction("action-msg", "review this", 7)
	a.SenderPrincipalID = "flowbee-control"
	a.GrantID, a.GrantEpoch = g.GrantID, g.Epoch
	a.RecipientSessionID, a.RecipientPaneInstanceID = g.RecipientSessionID, g.RecipientPaneInstanceID
	a.RecipientAgentRunID = controlRecipientRunID
	receipt, err := p.Send(context.Background(), SendRequest{Action: a, GrantID: grantID,
		GrantEpoch: 7, RecipientSessionID: recipientID, RecipientPaneInstanceID: paneID,
		ExpectedRecipientAgentRunID: controlRecipientRunID})
	if err != nil || receipt.SenderPrincipalID != "flowbee-control" || receipt.Sender.SessionID != "" {
		t.Fatalf("receipt=%+v err=%v", receipt, err)
	}
	if _, ok, err := p.ReceiptByAction(context.Background(), a.ExpectedReceipt()); err != nil || !ok {
		t.Fatalf("lookup ok=%v err=%v", ok, err)
	}
}

func TestHTTPPortRejectsMixedControlOriginWireObjects(t *testing.T) {
	grant := controlGrantFixture("11111111-1111-4111-8111-111111111111",
		"44444444-4444-4444-8444-444444444444", "55555555-5555-4555-8555-555555555555")
	grant["sender_session_id"] = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"grant": grant})
	}))
	defer srv.Close()
	err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Grant(context.Background(), Grant{
		GrantID: "11111111-1111-4111-8111-111111111111", SenderPrincipalID: "flowbee-control",
		RecipientSessionID:      "44444444-4444-4444-8444-444444444444",
		RecipientPaneInstanceID: "55555555-5555-4555-8555-555555555555", Epoch: 7, MaximumPayloadBytes: 1024,
		ExpectedRecipientAgentRunID: controlRecipientRunID,
	})
	if err == nil {
		t.Fatal("mixed control grant was accepted")
	}
}

func TestHTTPPortRejectsDriverGrantProjectionThatRedirectsRecipient(t *testing.T) {
	grant := Grant{
		GrantID:                 "11111111-1111-4111-8111-111111111111",
		SenderSessionID:         "22222222-2222-4222-8222-222222222222",
		SenderAgentRunID:        "33333333-3333-4333-8333-333333333333",
		RecipientSessionID:      "44444444-4444-4444-8444-444444444444",
		RecipientPaneInstanceID: "55555555-5555-4555-8555-555555555555",
		Epoch:                   7,
		MaximumPayloadBytes:     1024,
		ExpiresAt:               "2026-07-19T12:10:00.123456Z",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"grant": map[string]any{
			"format_version": "tmux-driver.route-grant/v1", "grant_id": grant.GrantID,
			"issuer_principal_id": "flowbee-control", "sender_session_id": grant.SenderSessionID,
			"sender_agent_run_id":        grant.SenderAgentRunID,
			"recipient_session_id":       "77777777-7777-4777-8777-777777777777",
			"recipient_pane_instance_id": grant.RecipientPaneInstanceID,
			"operation":                  "message", "epoch": grant.Epoch,
			"maximum_payload_bytes": grant.MaximumPayloadBytes, "allow_draft_stash": false,
			"issued_at": "2026-07-19T12:00:00.000Z", "expires_at": "2026-07-19T12:10:00.123Z",
			"revoked_at": nil,
		}})
	}))
	defer srv.Close()
	err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Grant(context.Background(), grant)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("redirected route error=%v", err)
	}
}

func TestHTTPPortReconcilesExistingExactGrantWithoutSecondMutation(t *testing.T) {
	grant := Grant{
		GrantID:                 "11111111-1111-4111-8111-111111111111",
		SenderSessionID:         "22222222-2222-4222-8222-222222222222",
		SenderAgentRunID:        "33333333-3333-4333-8333-333333333333",
		RecipientSessionID:      "44444444-4444-4444-8444-444444444444",
		RecipientPaneInstanceID: "55555555-5555-4555-8555-555555555555",
		Epoch:                   7,
		MaximumPayloadBytes:     1024,
	}
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"grant": map[string]any{
			"format_version": "tmux-driver.route-grant/v1", "grant_id": grant.GrantID,
			"issuer_principal_id": "flowbee-control", "sender_session_id": grant.SenderSessionID,
			"sender_agent_run_id":        grant.SenderAgentRunID,
			"recipient_session_id":       grant.RecipientSessionID,
			"recipient_pane_instance_id": grant.RecipientPaneInstanceID,
			"operation":                  "message", "epoch": grant.Epoch,
			"maximum_payload_bytes": grant.MaximumPayloadBytes, "allow_draft_stash": false,
			"issued_at": "2026-07-19T12:00:00.000Z", "expires_at": nil, "revoked_at": nil,
		}})
	}))
	defer srv.Close()
	if err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Grant(context.Background(), grant); err != nil {
		t.Fatal(err)
	}
	if posts != 0 {
		t.Fatalf("exact durable grant replay made %d mutations", posts)
	}
}

func TestHTTPPortRejectsNonSDKGrantIdentityBeforeDriverCall(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Grant(context.Background(), Grant{
		GrantID: "review-grant-legacy", SenderSessionID: "sender", SenderAgentRunID: "run",
		RecipientSessionID: "recipient", RecipientPaneInstanceID: "pane", Epoch: 1,
	})
	if err == nil || calls != 0 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestHTTPPortRejectsReceiptThatDoesNotBindImmutablePayload(t *testing.T) {
	fixture := receiptFixture()
	fixture["payload_sha256"] = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"receipt": fixture})
	}))
	defer srv.Close()
	action := NewAction("action-msg", "review this", 7)
	action.SenderAgentRunID = "33333333-3333-4333-8333-333333333333"
	_, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Send(context.Background(), SendRequest{
		Action: action, GrantID: "11111111-1111-4111-8111-111111111111", GrantEpoch: 7,
		RecipientSessionID:      "44444444-4444-4444-8444-444444444444",
		RecipientPaneInstanceID: "55555555-5555-4555-8555-555555555555",
		OnBehalfOfSessionID:     "22222222-2222-4222-8222-222222222222",
	})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("mutated receipt error=%v", err)
	}
}

func TestHTTPPortObservationUsesFullV2EnvelopeAndStableSnapshotIdentity(t *testing.T) {
	const (
		host      = "11111111-1111-1111-1111-111111111111"
		store     = "22222222-2222-2222-2222-222222222222"
		boot      = "33333333-3333-3333-3333-333333333333"
		sessionID = "44444444-4444-4444-4444-444444444444"
		pane      = "55555555-5555-5555-5555-555555555555"
		run       = "66666666-6666-6666-6666-666666666666"
		server    = "77777777-7777-7777-7777-777777777777"
	)
	event := map[string]any{
		"spec_version": "tmux-driver.events/v2", "event_id": "018f0000-0000-7000-8000-000000000001",
		"store_id": store, "cursor": "tdc2.event-1", "store_seq": 9, "session_seq": 3,
		"transition_id": "018f0000-0000-7000-8000-000000000002", "transition_index": 0,
		"transition_count": 1, "host_id": host, "session_id": sessionID,
		"pane_instance_id": pane, "producer_boot_id": boot, "kind": "phase.changed",
		"observed_at": "2026-07-19T12:00:00.000Z", "source_at": nil, "historical": false,
		"source": map[string]any{"kind": "provider_log"}, "correlation": map[string]any{},
		"caused_by": []string{}, "payload": map[string]any{"phase": "working"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/meta":
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "2.3", "host_id": host,
				"store_id": store, "instance": "local", "producer_boot_id": boot,
				"replay_floor_cursor": "tdc2.floor", "durable_high_water_cursor": "tdc2.high",
				"features": map[string]any{"lifecycle_control": true,
					"lifecycle_profile_inventory": "/v2/lifecycle/profiles"}, "tmux_server": map[string]any{
					"domain_id": "flowbee", "ownership": "managed_dedicated", "instance_id": server,
					"connection_visibility": "isolated_socket"}, "contracts": driverContractsFixture()})
		case "/v2/sessions":
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "2.3", "as_of_cursor": "tdc2.high",
				"sessions": []map[string]any{{"session_id": sessionID}}, "next_cursor": nil})
		case "/v2/sessions/" + sessionID:
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "2.3", "as_of_cursor": "tdc2.high",
				"state_revision": 4, "session": map[string]any{"format": "tmux-driver.session/v2",
					"session_id": sessionID, "host_id": host, "store_id": store, "provider": "codex",
					"conversation_id": "conversation-1", "pane_instance_id": pane},
				"state": map[string]any{"agent_run_id": run, "tmux_server_instance_id": server,
					"lifecycle": "observing", "phase": "working", "binding_status": "bound", "binding_epoch": 2}})
		case "/v2/events":
			if r.URL.Query().Get("after") != "tdc2.old" || r.URL.Query().Get("view") != "effective" {
				t.Errorf("event query=%s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "2.3", "events": []any{event},
				"next_cursor": "tdc2.event-1", "durable_high_water_cursor": "tdc2.event-1",
				"history_complete": true, "view": "effective"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := &HTTPPort{BaseURL: srv.URL, Token: "secret"}
	meta, err := p.Metadata(context.Background())
	if err != nil || meta.StoreID != store || meta.HostID != host {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
	snapshot, err := p.SnapshotSessions(context.Background())
	if err != nil || len(snapshot.Sessions) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
	identity := snapshot.Sessions[0].Identity
	if identity.SessionID != sessionID || identity.PaneInstanceID != pane || identity.AgentRunID != run ||
		identity.TmuxServerDomainID != "flowbee" || identity.Ownership != "" || identity.TmuxServerInstanceID != server {
		t.Fatalf("identity=%+v", identity)
	}
	batch, err := p.Observe(context.Background(), "tdc2.old")
	if err != nil || len(batch.Events) != 1 || batch.StoreID != store || !batch.HistoryComplete {
		t.Fatalf("batch=%+v err=%v", batch, err)
	}
	got := batch.Events[0]
	if got.EventID != event["event_id"] || got.TransitionID != event["transition_id"] ||
		got.Identity.SessionID != sessionID || got.Identity.PaneInstanceID != pane || got.Kind != "phase.changed" {
		t.Fatalf("event=%+v", got)
	}
}

func TestHTTPPortObservationRejectsUnknownEnvelopeFields(t *testing.T) {
	raw := json.RawMessage(`{"spec_version":"tmux-driver.events/v2","event_id":"e","store_id":"s","cursor":"tdc2.x","store_seq":1,"session_seq":1,"transition_id":"t","transition_index":0,"transition_count":1,"host_id":"h","session_id":"session","pane_instance_id":"pane","producer_boot_id":"boot","kind":"future.kind","observed_at":"2026-07-19T12:00:00.000Z","source_at":null,"historical":false,"source":{},"correlation":{},"caused_by":[],"payload":{},"unexpected_authority":"raw-pane"}`)
	if _, err := decodeObservation(raw); err == nil {
		t.Fatal("unknown envelope authority was silently accepted")
	}
}

func receiptFixture() map[string]any {
	return map[string]any{"format_version": "tmux-driver.delivery-receipt/v1",
		"delivery_id": "66666666-6666-4666-8666-666666666666", "action_id": "action-msg",
		"grant_id": "11111111-1111-4111-8111-111111111111", "grant_epoch": 7,
		"sender_session_id":          "22222222-2222-4222-8222-222222222222",
		"sender_agent_run_id":        "33333333-3333-4333-8333-333333333333",
		"recipient_session_id":       "44444444-4444-4444-8444-444444444444",
		"recipient_pane_instance_id": "55555555-5555-4555-8555-555555555555",
		"payload_sha256":             NewAction("action-msg", "review this", 7).PayloadSHA256,
		"payload_bytes":              11, "payload_media_type": "text/plain; charset=utf-8",
		"request_fingerprint": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"status":              "submitted", "compatibility_code": 0, "verification": "strong",
		"pane_hash_before": nil, "pane_hash_after": nil, "enter_attempts": 1,
		"accepted_at": "2026-07-19T12:00:00.000Z", "completed_at": "2026-07-19T12:00:00.100Z",
		"diagnostic_code": nil}
}

func controlGrantFixture(grantID, recipientID, paneID string) map[string]any {
	return map[string]any{
		"format_version": "tmux-driver.control-route-grant/v2", "grant_id": grantID,
		"issuer_principal_id": "flowbee-control", "sender_principal_id": "flowbee-control",
		"recipient_session_id": recipientID, "recipient_pane_instance_id": paneID,
		"expected_recipient_agent_run_id": controlRecipientRunID,
		"operation":                       "message", "epoch": 7, "maximum_payload_bytes": 1024,
		"allow_draft_stash": false, "issued_at": "2026-07-19T12:00:00.000Z",
		"expires_at": nil, "revoked_at": nil,
	}
}

func controlReceiptFixture(grantID, recipientID, paneID string) map[string]any {
	return map[string]any{
		"format_version": "tmux-driver.control-delivery-receipt/v2",
		"delivery_id":    "66666666-6666-4666-8666-666666666666", "action_id": "action-msg",
		"grant_id": grantID, "grant_epoch": 7, "sender_principal_id": "flowbee-control",
		"recipient_session_id": recipientID, "recipient_pane_instance_id": paneID,
		"expected_recipient_agent_run_id": controlRecipientRunID,
		"payload_sha256":                  NewAction("action-msg", "review this", 7).PayloadSHA256,
		"payload_bytes":                   11, "payload_media_type": "text/plain; charset=utf-8",
		"request_fingerprint": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"status":              "submitted", "compatibility_code": 0, "verification": "strong",
		"pane_hash_before": nil, "pane_hash_after": nil, "enter_attempts": 1,
		"accepted_at": "2026-07-19T12:00:00.000Z", "completed_at": "2026-07-19T12:00:00.100Z",
		"diagnostic_code": nil,
	}
}

func controlOriginMetaFixture(enabled, include bool) map[string]any {
	features := map[string]any{}
	features["lifecycle_control"] = true
	features["lifecycle_profile_inventory"] = "/v2/lifecycle/profiles"
	if include {
		features["control_principal_origin"] = enabled
	}
	return map[string]any{
		"api_version": "2.4", "host_id": "11111111-1111-4111-8111-111111111111",
		"store_id": "22222222-2222-4222-8222-222222222222", "instance": "local",
		"producer_boot_id":    "33333333-3333-4333-8333-333333333333",
		"replay_floor_cursor": "tdc2.floor", "durable_high_water_cursor": "tdc2.high",
		"features": features,
		"tmux_server": map[string]any{"domain_id": "flowbee", "ownership": "managed_dedicated",
			"instance_id": "44444444-4444-4444-8444-444444444444", "connection_visibility": "isolated_socket"},
		"contracts": driverContractsFixture(),
	}
}

func driverContractsFixture() map[string]any {
	contracts := defaultDriverContractCapabilities()
	raw, _ := json.Marshal(contracts)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
