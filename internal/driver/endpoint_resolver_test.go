package driver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

type endpointCountingPort struct {
	DriverPort
	metadataCalls   int
	capabilityCalls int
	capabilityErr   error
	onCapability    func()
}

type blockingMetadataPort struct {
	DriverPort
	blockOn int32
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *blockingMetadataPort) Metadata(ctx context.Context) (DriverMetadata, error) {
	call := p.calls.Add(1)
	if call == p.blockOn {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return DriverMetadata{}, ctx.Err()
		}
	}
	return p.DriverPort.Metadata(ctx)
}

func (p *endpointCountingPort) Metadata(ctx context.Context) (DriverMetadata, error) {
	p.metadataCalls++
	return p.DriverPort.Metadata(ctx)
}

func (p *endpointCountingPort) ControlOriginCapability(ctx context.Context) (ControlOriginCapability, error) {
	p.capabilityCalls++
	if p.onCapability != nil {
		p.onCapability()
	}
	if p.capabilityErr != nil {
		return ControlOriginCapability{}, p.capabilityErr
	}
	return p.DriverPort.ControlOriginCapability(ctx)
}

func endpointFake(hostID, storeID, domainID, ownership string) *endpointCountingPort {
	fake := NewFake()
	fake.Meta.HostID = hostID
	fake.Meta.StoreID = storeID
	fake.Meta.ControlPrincipalOrigin = true
	fake.Meta.TmuxServer.DomainID = domainID
	fake.Meta.TmuxServer.Ownership = ownership
	if ownership == "external" {
		fake.Meta.TmuxServer.ConnectionVisibility = "default_or_external"
	} else {
		fake.Meta.TmuxServer.ConnectionVisibility = "isolated_socket"
	}
	return &endpointCountingPort{DriverPort: fake}
}

func TestEndpointResolverKeepsExternalAndManagedDomainsSeparate(t *testing.T) {
	ctx := context.Background()
	externalKey := EndpointKey{HostID: "mac", StoreID: "external-store", TmuxServerDomainID: "default"}
	managedKey := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	external := endpointFake("mac", "external-store", "default", "external")
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")

	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{
		{InstanceRef: "external-default", Port: external, Expected: externalKey, ExpectedServerOwnership: "external"},
		{InstanceRef: "flowbee-managed", Port: managed, Expected: managedKey, ExpectedServerOwnership: "managed_dedicated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if external.metadataCalls != 1 || managed.metadataCalls != 1 {
		t.Fatalf("construction probes external=%d managed=%d, want 1 each", external.metadataCalls, managed.metadataCalls)
	}

	gotExternal, err := resolver.Resolve(externalKey)
	if err != nil {
		t.Fatal(err)
	}
	if gotExternal.Port != external || gotExternal.InstanceRef != "external-default" {
		t.Fatalf("external key resolved across domain: %#v", gotExternal)
	}
	readiness, err := resolver.ControlReadiness(ctx, managedKey)
	if err != nil {
		t.Fatal(err)
	}
	gotManaged, err := resolver.ResolveControlAction(Action{
		TargetHostID: "mac", TargetStoreID: "managed-store", TargetServerDomainID: "flowbee",
		TargetServerID: managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotManaged.Port != managed || gotManaged.InstanceRef != "flowbee-managed" {
		t.Fatalf("managed key resolved across domain: %#v", gotManaged)
	}

	if readiness.Endpoint.Port != managed || managed.metadataCalls != 3 || managed.capabilityCalls != 1 ||
		external.metadataCalls != 1 || external.capabilityCalls != 0 {
		t.Fatalf("readiness crossed domains: external=%d managed=%d", external.capabilityCalls, managed.capabilityCalls)
	}
}

func TestEndpointResolverMissingKeyHasNoFallbackOrPortCalls(t *testing.T) {
	ctx := context.Background()
	external := endpointFake("mac", "external-store", "default", "external")
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{
		InstanceRef: "external-default", Port: external,
		Expected:                EndpointKey{HostID: "mac", StoreID: "external-store", TmuxServerDomainID: "default"},
		ExpectedServerOwnership: "external",
	}})
	if err != nil {
		t.Fatal(err)
	}
	metadataCalls := external.metadataCalls
	capabilityCalls := external.capabilityCalls

	_, err = resolver.ControlReadiness(ctx, EndpointKey{
		HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee",
	})
	if !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("missing exact key error = %v, want ErrEndpointNotFound", err)
	}
	if external.metadataCalls != metadataCalls || external.capabilityCalls != capabilityCalls {
		t.Fatalf("missing key called fallback: metadata %d->%d capability %d->%d",
			metadataCalls, external.metadataCalls, capabilityCalls, external.capabilityCalls)
	}
	if _, err := resolver.Resolve(EndpointKey{HostID: "mac", StoreID: "external-store"}); !errors.Is(err, ErrEndpointNotFound) {
		t.Fatalf("incomplete key error = %v, want ErrEndpointNotFound", err)
	}
}

func TestEndpointResolverReadinessRefreshesServerIncarnationAndFencesOldActions(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{
		InstanceRef: "flowbee-managed", Port: managed, Expected: key,
		ExpectedServerOwnership: "managed_dedicated",
	}})
	if err != nil {
		t.Fatal(err)
	}
	oldServerID := managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID
	oldAction := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID, TargetServerID: oldServerID}
	if _, err := resolver.ResolveControlAction(oldAction); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("construction authorized action without capability proof: %v", err)
	}
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatalf("authorize initial incarnation: %v", err)
	}
	if _, err := resolver.ResolveControlAction(oldAction); err != nil {
		t.Fatalf("resolve initial action: %v", err)
	}

	newServerID := "00000000-0000-4000-8000-000000000099"
	managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = newServerID
	readiness, err := resolver.ControlReadiness(ctx, key)
	if err != nil {
		t.Fatalf("refresh replacement server incarnation: %v", err)
	}
	if readiness.Endpoint.Metadata.TmuxServer.InstanceID != newServerID || managed.capabilityCalls != 2 {
		t.Fatalf("replacement readiness=%+v capability_calls=%d", readiness, managed.capabilityCalls)
	}
	if _, err := resolver.ResolveControlAction(oldAction); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("old action after replacement error = %v, want ErrIdentityMismatch", err)
	}
	newAction := oldAction
	newAction.TargetServerID = newServerID
	if _, err := resolver.ResolveControlAction(newAction); err != nil {
		t.Fatalf("new action did not adopt authenticated replacement incarnation: %v", err)
	}
}

func TestEndpointResolverFailedCapabilityStillFencesReplacedIncarnation(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{InstanceRef: "managed", Port: managed,
		Expected: key, ExpectedServerOwnership: "managed_dedicated"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	oldID := managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID
	newID := "00000000-0000-4000-8000-000000000099"
	managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = newID
	managed.capabilityErr = errors.New("capability temporarily unavailable")
	if _, err := resolver.ControlReadiness(ctx, key); err == nil {
		t.Fatal("replacement capability failure returned ready")
	}
	oldAction := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID, TargetServerID: oldID}
	if _, err := resolver.ResolveControlAction(oldAction); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("old incarnation after failed proof error=%v, want ErrIdentityMismatch", err)
	}
	newAction := oldAction
	newAction.TargetServerID = newID
	if _, err := resolver.ResolveControlAction(newAction); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("unverified replacement action error=%v, want ErrEndpointUnverified", err)
	}
}

func TestEndpointResolverRejectsIncarnationChangingDuringCapabilityProof(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{InstanceRef: "managed", Port: managed,
		Expected: key, ExpectedServerOwnership: "managed_dedicated"}})
	if err != nil {
		t.Fatal(err)
	}
	firstReplacement := "00000000-0000-4000-8000-000000000098"
	secondReplacement := "00000000-0000-4000-8000-000000000099"
	managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = firstReplacement
	managed.onCapability = func() { managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = secondReplacement }
	if _, err := resolver.ControlReadiness(ctx, key); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("changing incarnation proof error=%v, want ErrIdentityMismatch", err)
	}
	newAction := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID, TargetServerID: secondReplacement}
	if _, err := resolver.ResolveControlAction(newAction); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("unstable proof authorized latest incarnation: %v", err)
	}
}

func TestEndpointResolverRestartPreservesDurableIncarnationFence(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")
	entry := EndpointEntry{InstanceRef: "managed", Port: managed, Expected: key,
		ExpectedServerOwnership: "managed_dedicated"}
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	oldAction := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID,
		TargetServerID:       managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID}
	managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = "00000000-0000-4000-8000-000000000099"
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	newAction := oldAction
	newAction.TargetServerID = managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID

	// Reconstructing the resolver simulates a Flowbee process crash/restart. The
	// durable action IDs remain unchanged; authority is re-proven from Driver.
	restarted, err := NewEndpointResolver(ctx, []EndpointEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ResolveControlAction(newAction); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("restart authorized construction metadata: %v", err)
	}
	if _, err := restarted.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.ResolveControlAction(oldAction); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("restart revived old durable action: %v", err)
	}
	if _, err := restarted.ResolveControlAction(newAction); err != nil {
		t.Fatalf("restart did not authorize current durable action: %v", err)
	}
}

func TestEndpointResolverConcurrentProbeWithdrawsControlAuthority(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	port := &blockingMetadataPort{DriverPort: fake, blockOn: 4, entered: make(chan struct{}), release: make(chan struct{})}
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{InstanceRef: "managed", Port: port,
		Expected: key, ExpectedServerOwnership: "managed_dedicated"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	action := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID, TargetServerID: fake.Meta.TmuxServer.InstanceID}
	result := make(chan error, 1)
	go func() {
		_, err := resolver.ControlReadiness(ctx, key)
		result <- err
	}()
	<-port.entered
	if _, err := resolver.ResolveControlAction(action); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("control action remained authorized during in-flight proof: %v", err)
	}
	close(port.release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveControlAction(action); err != nil {
		t.Fatalf("stable proof did not restore control authority: %v", err)
	}
}

func TestEndpointResolverIncarnationRefreshNeverWeakensExactBoundary(t *testing.T) {
	ctx := context.Background()
	key := EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"}
	managed := endpointFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver, err := NewEndpointResolver(ctx, []EndpointEntry{{
		InstanceRef: "flowbee-managed", Port: managed, Expected: key,
		ExpectedServerOwnership: "managed_dedicated",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ControlReadiness(ctx, key); err != nil {
		t.Fatal(err)
	}
	capabilityCalls := managed.capabilityCalls
	originalServerID := managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID
	managed.DriverPort.(*FakePort).Meta.TmuxServer.InstanceID = "00000000-0000-4000-8000-000000000099"
	managed.DriverPort.(*FakePort).Meta.HostID = "different-host"
	if _, err := resolver.ControlReadiness(ctx, key); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("host mutation error = %v, want ErrIdentityMismatch", err)
	}
	if managed.capabilityCalls != capabilityCalls {
		t.Fatalf("identity mismatch called capability endpoint: %d -> %d", capabilityCalls, managed.capabilityCalls)
	}
	oldAction := Action{TargetHostID: key.HostID, TargetStoreID: key.StoreID,
		TargetServerDomainID: key.TmuxServerDomainID, TargetServerID: originalServerID}
	if _, err := resolver.ResolveAction(oldAction); !errors.Is(err, ErrEndpointUnverified) {
		t.Fatalf("failed identity refresh did not close cached authority: %v", err)
	}
}

func TestEndpointResolverRejectsDuplicateExactTupleBeforeProbe(t *testing.T) {
	key := EndpointKey{HostID: "mac", StoreID: "same-store", TmuxServerDomainID: "default"}
	first := endpointFake("mac", "same-store", "default", "external")
	second := endpointFake("mac", "same-store", "default", "external")
	_, err := NewEndpointResolver(context.Background(), []EndpointEntry{
		{InstanceRef: "first", Port: first, Expected: key, ExpectedServerOwnership: "external"},
		{InstanceRef: "second", Port: second, Expected: key, ExpectedServerOwnership: "external"},
	})
	if !errors.Is(err, ErrEndpointAmbiguous) {
		t.Fatalf("duplicate exact key error = %v, want ErrEndpointAmbiguous", err)
	}
	if first.metadataCalls != 0 || second.metadataCalls != 0 {
		t.Fatalf("ambiguous inventory was probed: first=%d second=%d", first.metadataCalls, second.metadataCalls)
	}
}

func TestEndpointResolverRejectsDuplicateInstanceRefAndMetadataMismatch(t *testing.T) {
	ctx := context.Background()
	firstKey := EndpointKey{HostID: "mac", StoreID: "store-a", TmuxServerDomainID: "default"}
	secondKey := EndpointKey{HostID: "mac", StoreID: "store-b", TmuxServerDomainID: "flowbee"}
	first := endpointFake("mac", "store-a", "default", "external")
	second := endpointFake("mac", "store-b", "flowbee", "managed_dedicated")
	_, err := NewEndpointResolver(ctx, []EndpointEntry{
		{InstanceRef: "same", Port: first, Expected: firstKey, ExpectedServerOwnership: "external"},
		{InstanceRef: "same", Port: second, Expected: secondKey, ExpectedServerOwnership: "managed_dedicated"},
	})
	if !errors.Is(err, ErrEndpointAmbiguous) {
		t.Fatalf("duplicate instance_ref error = %v, want ErrEndpointAmbiguous", err)
	}
	if first.metadataCalls != 0 || second.metadataCalls != 0 {
		t.Fatal("duplicate instance_ref must fail before probing")
	}

	mismatch := endpointFake("mac", "actual-store", "default", "external")
	_, err = NewEndpointResolver(ctx, []EndpointEntry{{
		InstanceRef: "mismatch", Port: mismatch,
		Expected:                EndpointKey{HostID: "mac", StoreID: "expected-store", TmuxServerDomainID: "default"},
		ExpectedServerOwnership: "external",
	}})
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("metadata mismatch error = %v, want ErrIdentityMismatch", err)
	}
}
