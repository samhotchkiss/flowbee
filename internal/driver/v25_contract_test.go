package driver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestV25MetadataHandshakeFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing lifecycle feature", mutate: func(meta map[string]any) {
			delete(meta["features"].(map[string]any), "lifecycle_control")
		}},
		{name: "malformed lifecycle feature", mutate: func(meta map[string]any) {
			meta["features"].(map[string]any)["lifecycle_control"] = "true"
		}},
		{name: "wrong contract id", mutate: func(meta map[string]any) {
			meta["contracts"].(map[string]any)["lifecycle_ensure"].(map[string]any)["contract_id"] = "tmux-driver.lifecycle-ensure/v1"
		}},
		{name: "unsupported contract", mutate: func(meta map[string]any) {
			meta["contracts"].(map[string]any)["managed_tmux_server_isolation"].(map[string]any)["supported"] = false
		}},
		{name: "illegal domain ownership", mutate: func(meta map[string]any) {
			meta["tmux_server"].(map[string]any)["domain_id"] = "default"
		}},
		{name: "selector leakage", mutate: func(meta map[string]any) {
			meta["tmux_server"].(map[string]any)["socket_path"] = "/tmp/tmux.sock"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			meta := controlOriginMetaFixture(true, true)
			test.mutate(meta)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(meta)
			}))
			defer srv.Close()
			if _, err := (&HTTPPort{BaseURL: srv.URL, Token: "secret"}).Metadata(context.Background()); err == nil {
				t.Fatal("unsafe v2.5 metadata was accepted")
			}
		})
	}
}

func TestV25ControlAndSessionOriginsStaySeparate(t *testing.T) {
	control := NewAction("control", "payload", 3)
	control.SenderPrincipalID = "flowbee-control"
	control.GrantID, control.GrantEpoch = "grant-control", 3
	control.RecipientSessionID, control.RecipientPaneInstanceID = "recipient", "pane"
	control.RecipientAgentRunID = "recipient-run"
	controlGrant := control.RouteGrant()
	if controlGrant.ExpectedRecipientAgentRunID != "recipient-run" ||
		control.ExpectedReceipt().ExpectedRecipientAgentRunID != "recipient-run" {
		t.Fatal("control origin omitted recipient-run fence")
	}
	if err := ValidateSend(SendRequest{Action: control, GrantID: controlGrant.GrantID,
		RecipientSessionID: "recipient", RecipientPaneInstanceID: "pane",
		ExpectedRecipientAgentRunID: "recipient-run", GrantEpoch: 3}, controlGrant); err != nil {
		t.Fatalf("control v2 rejected: %v", err)
	}

	session := NewAction("session", "payload", 4)
	session.SenderSessionID, session.SenderAgentRunID = "sender", "sender-run"
	session.GrantID, session.GrantEpoch = "grant-session", 4
	session.RecipientSessionID, session.RecipientPaneInstanceID = "recipient", "pane"
	session.RecipientAgentRunID = "recipient-run"
	sessionGrant := session.RouteGrant()
	if sessionGrant.ExpectedRecipientAgentRunID != "" ||
		session.ExpectedReceipt().ExpectedRecipientAgentRunID != "" {
		t.Fatal("session v1 gained a control-only recipient-run field")
	}
	if err := ValidateSend(SendRequest{Action: session, GrantID: sessionGrant.GrantID,
		RecipientSessionID: "recipient", RecipientPaneInstanceID: "pane",
		OnBehalfOfSessionID: "sender", GrantEpoch: 4}, sessionGrant); err != nil {
		t.Fatalf("session v1 rejected: %v", err)
	}
}

func TestV25ObservedRouteFencesDomainServerAndRunBeforeGrantOrSend(t *testing.T) {
	base := Identity{HostID: "host", StoreID: "store", TmuxServerDomainID: "default",
		TmuxServerInstanceID: "server", SessionID: "session", PaneInstanceID: "pane",
		AgentRunID: "run", Provider: "claude"}
	for _, test := range []struct {
		name   string
		mutate func(*Identity)
	}{
		{name: "domain", mutate: func(id *Identity) { id.TmuxServerDomainID = "other" }},
		{name: "server", mutate: func(id *Identity) { id.TmuxServerInstanceID = "replacement-server" }},
		{name: "agent run", mutate: func(id *Identity) { id.AgentRunID = "replacement-run" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := NewFake()
			observed := base
			test.mutate(&observed)
			fake.Snapshot = SessionSnapshot{HostID: base.HostID, StoreID: base.StoreID,
				Sessions: []SessionProjection{{Identity: observed, Lifecycle: "active"}}}
			a := NewAction("route-"+test.name, "payload", 2)
			a.ExecutorKind = "driver"
			a.TargetHostID, a.TargetStoreID = base.HostID, base.StoreID
			a.TargetServerDomainID, a.TargetServerID = base.TmuxServerDomainID, base.TmuxServerInstanceID
			a.SenderPrincipalID = "flowbee-control"
			a.RecipientSessionID, a.RecipientPaneInstanceID, a.RecipientAgentRunID = base.SessionID, base.PaneInstanceID, base.AgentRunID
			a.GrantID, a.GrantEpoch = "grant-"+test.name, 2
			_, err := (Executor{Port: fake, Store: &memoryCommit{}}).Execute(context.Background(), a.SessionTarget(), a.RouteGrant(), a)
			if !errors.Is(err, ErrIdentityMismatch) {
				t.Fatalf("stale %s err=%v", test.name, err)
			}
			if len(fake.Grants) != 0 || fake.SendCalls != 0 {
				t.Fatalf("stale %s mutated Driver: grants=%d sends=%d", test.name, len(fake.Grants), fake.SendCalls)
			}
		})
	}
}

func TestV25UnadoptedExternalSessionCannotClaimLifecycleOwnership(t *testing.T) {
	fake := NewFake()
	id := Identity{HostID: "host", StoreID: "store", TmuxServerDomainID: "default",
		TmuxServerInstanceID: "server", SessionID: "session", PaneInstanceID: "pane",
		AgentRunID: "run", Ownership: "external_observed"}
	fake.Snapshot = SessionSnapshot{HostID: id.HostID, StoreID: id.StoreID,
		Sessions: []SessionProjection{{Identity: Identity{HostID: id.HostID, StoreID: id.StoreID,
			TmuxServerDomainID: id.TmuxServerDomainID, TmuxServerInstanceID: id.TmuxServerInstanceID,
			SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID}, Lifecycle: "active"}}}
	if _, err := exactObservedIdentity(context.Background(), fake, id); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("unadopted ownership err=%v", err)
	}
	if len(fake.Grants) != 0 || fake.SendCalls != 0 {
		t.Fatal("unadopted ownership claim mutated Driver")
	}
}
