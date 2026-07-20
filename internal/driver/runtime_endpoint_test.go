package driver

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"
)

func endpointRuntimeFake(host, storeID, domain, ownership string) *FakePort {
	fake := NewFake()
	fake.Meta.HostID = host
	fake.Meta.StoreID = storeID
	fake.Meta.ControlPrincipalOrigin = true
	fake.Meta.TmuxServer.DomainID = domain
	fake.Meta.TmuxServer.Ownership = ownership
	if ownership == "external" {
		fake.Meta.TmuxServer.ConnectionVisibility = "default_or_external"
		fake.ProfileInventory.TmuxServerDomainID = domain
		fake.ProfileInventory.Profiles = append(fake.ProfileInventory.Profiles, LifecycleProfile{
			ProfileID: "claude_interactor_managed", Provider: "claude",
			InitialPromptAdapter: "argv_element", TargetCredentialAdapter: "file_environment",
			EnsureSupported: true, BootstrapArtifactSupported: true,
			FlowbeeCredentialInstallSupported: true, HumanVisibleSessionSupported: true})
		sort.Slice(fake.ProfileInventory.Profiles, func(i, j int) bool {
			return fake.ProfileInventory.Profiles[i].ProfileID < fake.ProfileInventory.Profiles[j].ProfileID
		})
	}
	return fake
}

func runtimeResolver(t *testing.T, entries ...EndpointEntry) *EndpointResolver {
	t.Helper()
	resolver, err := NewEndpointResolver(context.Background(), entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if _, err := resolver.ControlReadiness(context.Background(), entry.Expected); err != nil {
			t.Fatalf("authorize endpoint %s: %v", entry.InstanceRef, err)
		}
	}
	return resolver
}

func TestRuntimeResolvesExternalAndManagedActionsOnlyToExactEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, storeID, domain, ownership string
	}{
		{name: "external adopted interactor", storeID: "external-store", domain: "default", ownership: "external"},
		{name: "managed dedicated worker", storeID: "managed-store", domain: "flowbee", ownership: "managed_dedicated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := seedSQLStoreEpic(t)
			external := endpointRuntimeFake("mac", "external-store", "default", "external")
			managed := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
			resolver := runtimeResolver(t,
				EndpointEntry{InstanceRef: "external", Port: external,
					Expected:                EndpointKey{HostID: "mac", StoreID: "external-store", TmuxServerDomainID: "default"},
					ExpectedServerOwnership: "external"},
				EndpointEntry{InstanceRef: "managed", Port: managed,
					Expected:                EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"},
					ExpectedServerOwnership: "managed_dedicated"},
			)
			a = routedAction(a)
			a.Epoch = 0
			a.TargetHostID, a.TargetStoreID, a.TargetServerDomainID = "mac", tc.storeID, tc.domain
			a.TargetServerID = managed.Meta.TmuxServer.InstanceID
			if err := s.CommitAction(context.Background(), a); err != nil {
				t.Fatal(err)
			}
			target := external
			other := managed
			if tc.ownership == "managed_dedicated" {
				target, other = managed, external
			}
			target.Sessions[a.RecipientSessionID] = a.SessionTarget().Identity
			now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
			rep, err := (Runtime{Resolver: resolver, Store: s, Owner: "resolver-runtime",
				Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return false, nil })}).Tick(
				context.Background(), now)
			if err != nil || rep.Delivered != 1 || target.SendCalls != 1 || other.SendCalls != 0 {
				t.Fatalf("report=%+v target_sends=%d other_sends=%d err=%v", rep, target.SendCalls, other.SendCalls, err)
			}
		})
	}
}

func TestRuntimeMissingEndpointNeverFallsBackToLegacyPort(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	a = routedAction(a)
	a.Epoch = 0
	a.TargetHostID, a.TargetStoreID, a.TargetServerDomainID = "mac", "external-store", "default"
	legacy := endpointRuntimeFake("mac", "external-store", "default", "external")
	managed := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	a.TargetServerID = legacy.Meta.TmuxServer.InstanceID
	if err := s.CommitAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	resolver := runtimeResolver(t, EndpointEntry{InstanceRef: "managed", Port: managed,
		Expected:                EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"},
		ExpectedServerOwnership: "managed_dedicated"})
	rep, err := (Runtime{Resolver: resolver, Port: legacy, Store: s, Owner: "no-fallback"}).Tick(
		context.Background(), time.Date(2026, 7, 19, 23, 10, 0, 0, time.UTC))
	if err != nil || rep.Retried != 1 || legacy.SendCalls != 0 || managed.SendCalls != 0 {
		t.Fatalf("report=%+v legacy=%d managed=%d err=%v", rep, legacy.SendCalls, managed.SendCalls, err)
	}
	var detail string
	if err := s.DB.QueryRow(`SELECT last_error FROM epic_actions WHERE id=?`, a.ActionID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != ErrEndpointNotFound.Error() {
		t.Fatalf("durable hold detail=%q", detail)
	}
}

func TestRuntimeOldStoreKeyIsFencedAfterEndpointStoreReset(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	a = routedAction(a)
	a.Epoch = 0
	a.TargetHostID, a.TargetStoreID, a.TargetServerDomainID = "mac", "old-store", "flowbee"
	newStore := endpointRuntimeFake("mac", "new-store", "flowbee", "managed_dedicated")
	a.TargetServerID = newStore.Meta.TmuxServer.InstanceID
	if err := s.CommitAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	resolver := runtimeResolver(t, EndpointEntry{InstanceRef: "managed-reset", Port: newStore,
		Expected:                EndpointKey{HostID: "mac", StoreID: "new-store", TmuxServerDomainID: "flowbee"},
		ExpectedServerOwnership: "managed_dedicated"})
	rep, err := (Runtime{Resolver: resolver, Store: s, Owner: "store-reset"}).Tick(
		context.Background(), time.Date(2026, 7, 19, 23, 20, 0, 0, time.UTC))
	if err != nil || rep.Retried != 1 || newStore.SendCalls != 0 || len(newStore.Grants) != 0 {
		t.Fatalf("report=%+v sends=%d grants=%d err=%v", rep, newStore.SendCalls, len(newStore.Grants), err)
	}
}

func TestSessionOriginMustShareExactEndpointBeforeGrant(t *testing.T) {
	a := routedAction(NewAction("session-origin", "route", 1))
	a.SenderPrincipalID = ""
	a.SenderSessionID, a.SenderAgentRunID = "sender", "sender-run"
	a.SenderHostID, a.SenderStoreID = a.TargetHostID, "other-store"
	a.SenderServerDomainID, a.SenderServerID = a.TargetServerDomainID, a.TargetServerID
	if err := validateSessionOriginEndpoint(a); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("cross-domain session origin error=%v", err)
	}
	a.SenderStoreID = a.TargetStoreID
	if err := validateSessionOriginEndpoint(a); err != nil {
		t.Fatalf("same-domain session origin rejected: %v", err)
	}
}
