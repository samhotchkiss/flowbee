package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	flowstore "github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func actorRuntimeProject(t *testing.T, role, actorID string) (*flowstore.Store, flowstore.ProjectActorRoute, time.Time) {
	t.Helper()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(context.Background(), flowstore.PortfolioProject{
		ID: "russ", Name: "Russ", Priority: 10, SchedulerWeight: 1, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	route, err := st.RegisterProjectActor(context.Background(), flowstore.ProjectActorRoute{
		ProjectID: "russ", Role: role, ActorID: actorID,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return st, route, now
}

func actorRuntimeResolver(t *testing.T, external DriverPort, managed DriverPort) *EndpointResolver {
	t.Helper()
	entries := []EndpointEntry{}
	if external != nil {
		entries = append(entries, EndpointEntry{InstanceRef: "external", Port: external,
			Expected:                EndpointKey{HostID: "mac", StoreID: "external-store", TmuxServerDomainID: "default"},
			ExpectedServerOwnership: "external"})
	}
	if managed != nil {
		entries = append(entries, EndpointEntry{InstanceRef: "managed", Port: managed,
			Expected:                EndpointKey{HostID: "mac", StoreID: "managed-store", TmuxServerDomainID: "flowbee"},
			ExpectedServerOwnership: "managed_dedicated"})
	}
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

func externalActorCommand(route flowstore.ProjectActorRoute, serverID string) flowstore.ProjectActorLifecycleCommand {
	return flowstore.ProjectActorLifecycleCommand{ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "adopt", IdempotencyKey: "adopt-russ-claude",
		InstanceRef: "external", TargetHostID: "mac", TargetStoreID: "external-store",
		TargetServerDomainID: "default", TargetServerID: serverID, LifecycleOwnership: "external_observed",
		LifecycleKey: "russ-claude", TargetEpoch: 1, ProfileID: "claude-interactor",
		ExternalWatchID: "watch-russ-claude", ExpectedSessionID: "session-russ-claude",
		ExpectedPaneInstanceID: "pane-russ-claude", ExpectedAgentRunID: "run-russ-claude-1"}
}

func managedActorCommand(route flowstore.ProjectActorRoute, serverID string) flowstore.ProjectActorLifecycleCommand {
	return flowstore.ProjectActorLifecycleCommand{ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "ensure", IdempotencyKey: "ensure-russ-codex",
		InstanceRef: "managed", TargetHostID: "mac", TargetStoreID: "managed-store",
		TargetServerDomainID: "flowbee", TargetServerID: serverID, LifecycleOwnership: "driver_managed",
		LifecycleKey: "russ-codex", TargetEpoch: 1, ProfileID: "codex-orchestrator",
		WorkspaceRootID: "russ-root", WorkspaceRelativePath: "russ"}
}

type lostAdoptResponsePort struct{ *FakePort }

func (p *lostAdoptResponsePort) AdoptSession(ctx context.Context, target SessionTarget, action Action) (LifecycleReceipt, error) {
	if _, err := p.FakePort.AdoptSession(ctx, target, action); err != nil {
		return LifecycleReceipt{}, err
	}
	return LifecycleReceipt{}, errors.New("response lost after adopt commit")
}

func TestActorLifecycleRuntimeRussClaudeAdoptLostResponseRecoversWithoutResend(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverInteractorRole, "russ-claude")
	fake := endpointRuntimeFake("mac", "external-store", "default", "external")
	fake.Watches["%1"] = ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1", Enabled: true,
		Lifecycle: "active", Provider: "claude", Profile: "interactive"}
	port := &lostAdoptResponsePort{FakePort: fake}
	resolver := actorRuntimeResolver(t, port, nil)
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		externalActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "actor-runtime",
		ClaimTTL: time.Minute, MaxRecovery: 3}
	report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Executed != 0 || fake.AdoptCalls != 1 {
		t.Fatalf("lost adopt response report=%+v calls=%d err=%v", report, fake.AdoptCalls, err)
	}
	report, err = runtime.Tick(context.Background(), now.Add(4*time.Minute))
	if err != nil || report.Verified != 1 || fake.AdoptCalls != 1 {
		t.Fatalf("adopt recovery report=%+v calls=%d err=%v", report, fake.AdoptCalls, err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "active" || lifecycle.ActiveBindingID == "" {
		t.Fatalf("adopt recovery did not activate binding: %+v", lifecycle)
	}
}

func TestActorLifecycleRuntimeManagedEnsureRestartUsesDurableReceipt(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver := actorRuntimeResolver(t, nil, fake)
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	claimed, err := st.ClaimNextProjectActorLifecycleAction(context.Background(), "actor-runtime",
		now.Add(2*time.Minute), now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	driverAction := actorDriverAction(claimed)
	receipt, err := executeActorLifecycle(context.Background(), fake, driverAction)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(context.Background(), actorStoreReceipt(receipt),
		now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Process dies after receipt persistence and before projection/ack. A fresh
	// runtime folds the durable receipt and never repeats Ensure.
	report, err := (ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "actor-runtime",
		ClaimTTL: time.Minute, MaxRecovery: 3}).Tick(context.Background(), now.Add(4*time.Minute))
	if err != nil || report.Verified != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("restart report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
}

func TestActorLifecycleRuntimeExternalReattachRejectsStaleRun(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverInteractorRole, "russ-claude")
	fake := endpointRuntimeFake("mac", "external-store", "default", "external")
	fake.Watches["%1"] = ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1", Enabled: true,
		Lifecycle: "active", Provider: "claude", Profile: "interactive"}
	resolver := actorRuntimeResolver(t, fake, nil)
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		externalActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "actor-runtime", ClaimTTL: time.Minute}
	if report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute)); err != nil || report.Executed != 1 {
		t.Fatalf("adopt report=%+v err=%v", report, err)
	}
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	_, reattachAction, err := st.CommitProjectActorLifecycleIntent(context.Background(), flowstore.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: flowstore.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "reattach", IdempotencyKey: "reattach-russ-claude", InstanceRef: "external",
	}, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	current := fake.Sessions["session-russ-claude"]
	current.AgentRunID = "run-russ-claude-2"
	fake.Sessions[current.SessionID] = current
	report, err := runtime.Tick(context.Background(), now.Add(4*time.Minute))
	if err != nil || report.Executed != 0 || fake.ReattachCalls != 0 {
		t.Fatalf("stale-run report=%+v reattach_calls=%d err=%v", report, fake.ReattachCalls, err)
	}
	action, err := st.GetProjectActorLifecycleAction(context.Background(), reattachAction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action.State != "verifying" {
		t.Fatalf("stale-run action state=%q, want verifying", action.State)
	}
}

func TestActorLifecycleRuntimeReleaseAndNoCrossEndpointFallback(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverInteractorRole, "russ-claude")
	external := endpointRuntimeFake("mac", "external-store", "default", "external")
	external.Watches["%1"] = ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1", Enabled: true,
		Lifecycle: "active", Provider: "claude", Profile: "interactive"}
	managed := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		externalActorCommand(route, external.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// A resolver without the action's external endpoint must not fall back to
	// the managed endpoint or invoke either lifecycle API.
	report, err := (ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, nil, managed), Store: st,
		Owner: "actor-runtime", ClaimTTL: time.Minute, MaxRecovery: 3}).Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Retried != 1 || external.AdoptCalls != 0 || managed.AdoptCalls != 0 {
		t.Fatalf("cross-endpoint report=%+v external=%d managed=%d err=%v",
			report, external.AdoptCalls, managed.AdoptCalls, err)
	}
	// Retry is due after one minute and exact inventory now permits only the
	// external endpoint.
	runtime := ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, external, managed), Store: st,
		Owner: "actor-runtime", ClaimTTL: time.Minute, MaxRecovery: 3}
	if report, err = runtime.Tick(context.Background(), now.Add(4*time.Minute)); err != nil || report.Executed != 1 {
		t.Fatalf("external adopt report=%+v err=%v", report, err)
	}
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(), flowstore.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: flowstore.DriverInteractorRole, ActorID: "russ-claude",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "release", IdempotencyKey: "release-russ-claude", InstanceRef: "external",
	}, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if report, err = runtime.Tick(context.Background(), now.Add(6*time.Minute)); err != nil || report.Executed != 1 || external.ReleaseCalls != 1 {
		t.Fatalf("release report=%+v calls=%d err=%v", report, external.ReleaseCalls, err)
	}
	retired, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || retired.State != "released" || retired.ActiveBindingID != "" {
		t.Fatalf("release lifecycle=%+v err=%v", retired, err)
	}
}

func TestActorLifecycleRuntimeRejectsInstanceRefAliasOnExactTuple(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
	managed := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	command := managedActorCommand(route, managed.Meta.TmuxServer.InstanceID)
	command.InstanceRef = "forged-alias"
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(), command, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	report, err := (ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, nil, managed), Store: st,
		Owner: "actor-runtime", ClaimTTL: time.Minute, MaxRecovery: 3}).Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Retried != 1 || managed.EnsureCalls != 0 {
		t.Fatalf("instance-ref alias report=%+v ensure_calls=%d err=%v", report, managed.EnsureCalls, err)
	}
}
