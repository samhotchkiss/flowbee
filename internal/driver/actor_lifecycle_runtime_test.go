package driver

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	flowstore "github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func actorRuntimeProject(t *testing.T, role, actorID string) (*flowstore.Store, flowstore.ProjectActorRoute, time.Time) {
	t.Helper()
	st := testutil.NewStore(t)
	st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil
	}
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(context.Background(), flowstore.PortfolioProject{
		ID: "russ", Name: "Russ", Priority: 10, SchedulerWeight: 1, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(context.Background(), flowstore.Repo{ID: "russ-repo", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(context.Background(), "russ", "russ-repo", now); err != nil {
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
		ExpectedPaneInstanceID: "pane-russ-claude", ExpectedAgentRunID: "run-russ-claude-1",
		ManagedRecoveryProfileID:             "claude_interactor_managed",
		ManagedRecoveryWorkspaceRootID:       "russ-root",
		ManagedRecoveryWorkspaceRelativePath: "russ"}
}

func managedActorCommand(route flowstore.ProjectActorRoute, serverID string) flowstore.ProjectActorLifecycleCommand {
	return flowstore.ProjectActorLifecycleCommand{ProjectID: route.ProjectID, Role: route.Role, ActorID: route.ActorID,
		ExpectedRouteStateVersion: int64(route.StateVersion), Operation: "ensure", IdempotencyKey: "ensure-russ-codex",
		InstanceRef: "managed", TargetHostID: "mac", TargetStoreID: "managed-store",
		TargetServerDomainID: "flowbee", TargetServerID: serverID, LifecycleOwnership: "driver_managed",
		LifecycleKey: "russ-codex", TargetEpoch: 1, ProfileID: "codex_orchestrator",
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

func adoptedInteractorRuntime(t *testing.T) (*flowstore.Store, *FakePort, ActorLifecycleRuntime, time.Time) {
	t.Helper()
	st, route, now := actorRuntimeProject(t, flowstore.DriverInteractorRole, "russ-claude")
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("adopted-interactor-recovery-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "external-store", "default", "external")
	fake.Watches["%1"] = ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1", Enabled: true,
		Lifecycle: "active", Provider: "claude", Profile: "interactive"}
	resolver := actorRuntimeResolver(t, fake, nil)
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		externalActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "adopted-interactor-runtime",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}
	report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Executed != 1 || fake.AdoptCalls != 1 {
		t.Fatalf("adopt report=%+v adopt_calls=%d err=%v", report, fake.AdoptCalls, err)
	}
	return st, fake, runtime, now
}

func TestActorLifecycleRuntimeAdoptedInteractorExactDeathPromotesToManagedV3(t *testing.T) {
	st, fake, runtime, now := adoptedInteractorRuntime(t)
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || active.State != "active" || active.LifecycleOwnership != "external_observed" {
		t.Fatalf("adopted lifecycle=%+v err=%v", active, err)
	}
	delete(fake.Sessions, "session-russ-claude")
	report, err := runtime.Tick(context.Background(), now.Add(3*time.Minute))
	if err != nil || report.PresenceRecovered != 1 || report.Executed != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("death recovery report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	recovered, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || recovered.State != "active" || recovered.LifecycleOwnership != "driver_managed" ||
		recovered.TargetEpoch != 2 || recovered.CredentialGeneration != 2 || recovered.ActiveBindingID == "" ||
		recovered.ActiveBindingID == active.ActiveBindingID || recovered.PresentationName != "russ-interactor" {
		t.Fatalf("recovered lifecycle=%+v err=%v", recovered, err)
	}
	var staleState string
	if err := st.DB.QueryRow(`SELECT state FROM driver_session_bindings WHERE binding_id=?`,
		active.ActiveBindingID).Scan(&staleState); err != nil || staleState != "superseded" {
		t.Fatalf("stale adopted binding state=%q err=%v", staleState, err)
	}
	var alertState string
	if err := st.DB.QueryRow(`SELECT state FROM control_alerts
		WHERE project_id='russ' AND kind='project_actor_incarnation_recovered'`).Scan(&alertState); err != nil ||
		alertState != "pending" {
		t.Fatalf("session-death recovery alert state=%q err=%v", alertState, err)
	}
	// Replaying reconciliation cannot resurrect or re-send the old external
	// incarnation; the new stable Driver identity is the only active authority.
	report, err = runtime.Tick(context.Background(), now.Add(4*time.Minute))
	if err != nil || fake.EnsureCalls != 1 || report.PresenceRecovered != 0 {
		t.Fatalf("idempotent replay report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
}

func TestActorLifecycleRuntimeAdoptedInteractorMissingRecoveryPrerequisiteMutatesNothing(t *testing.T) {
	st, fake, runtime, now := adoptedInteractorRuntime(t)
	delete(fake.Sessions, "session-russ-claude")
	if _, err := st.DB.Exec(`DELETE FROM project_actor_managed_recovery_policies
		WHERE project_id='russ' AND role='interactor' AND actor_id='russ-claude'`); err != nil {
		t.Fatal(err)
	}
	var before int64
	if err := st.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	report, err := runtime.Tick(context.Background(), now.Add(3*time.Minute))
	if err != nil || report.PresenceErrors != 1 || report.PresenceRecovered != 0 || fake.EnsureCalls != 0 {
		t.Fatalf("missing prerequisite report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	var after int64
	if err := st.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("missing recovery policy mutated product DB: before=%d after=%d", before, after)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || lifecycle.State != "active" || lifecycle.LifecycleOwnership != "external_observed" {
		t.Fatalf("missing prerequisite changed lifecycle=%+v err=%v", lifecycle, err)
	}
}

func TestActorLifecycleRuntimeAdoptedInteractorUncertifiedProfileMutatesNothing(t *testing.T) {
	st, fake, runtime, now := adoptedInteractorRuntime(t)
	delete(fake.Sessions, "session-russ-claude")
	profiles := fake.ProfileInventory.Profiles[:0]
	for _, profile := range fake.ProfileInventory.Profiles {
		if profile.ProfileID != "claude_interactor_managed" {
			profiles = append(profiles, profile)
		}
	}
	fake.ProfileInventory.Profiles = profiles
	var before int64
	if err := st.DB.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	report, err := runtime.Tick(context.Background(), now.Add(3*time.Minute))
	if err != nil || report.PresenceErrors != 1 || report.PresenceRecovered != 0 || fake.EnsureCalls != 0 {
		t.Fatalf("uncertified profile report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	var after int64
	if err := st.DB.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("uncertified profile mutated product DB: before=%d after=%d", before, after)
	}
}

func TestActorLifecycleRuntimeAdoptedInteractorMissingCredentialMaterializerRollsBackFence(t *testing.T) {
	st, fake, runtime, now := adoptedInteractorRuntime(t)
	before, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	delete(fake.Sessions, "session-russ-claude")
	st.ProjectActorCredentialMaterializer = nil
	report, err := runtime.Tick(context.Background(), now.Add(3*time.Minute))
	if err != nil || report.PresenceErrors != 1 || report.PresenceRecovered != 0 || fake.EnsureCalls != 0 {
		t.Fatalf("missing materializer report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	after, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || after.State != "active" || after.ActiveBindingID != before.ActiveBindingID ||
		after.LifecycleOwnership != "external_observed" || after.StateVersion != before.StateVersion {
		t.Fatalf("failed atomic promotion changed lifecycle before=%+v after=%+v err=%v", before, after, err)
	}
	var bindingState string
	if err := st.DB.QueryRow(`SELECT state FROM driver_session_bindings WHERE binding_id=?`,
		before.ActiveBindingID).Scan(&bindingState); err != nil || bindingState != "active" {
		t.Fatalf("failed atomic promotion fenced binding state=%q err=%v", bindingState, err)
	}
}

type lostRecoveryEnsureResponsePort struct {
	*FakePort
	lost bool
}

type preEffectRejectingEnsurePort struct{ *FakePort }

func (p *preEffectRejectingEnsurePort) EnsureLifecycleSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error) {
	return LifecycleReceipt{}, preEffect(errors.New("profile inventory rejected before lifecycle submission"))
}

func TestActorLifecycleRuntimeKnownPreEffectEnsureFailureIsRetryable(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-orchestrator")
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("known-pre-effect-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver := actorRuntimeResolver(t, nil, &preEffectRejectingEnsurePort{FakePort: fake})
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	runtime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "known-pre-effect",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}
	report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Retried != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("known pre-effect failure was not retried report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	action, err := st.GetProjectActorLifecycleAction(context.Background(), lifecycle.CurrentActionID)
	if err != nil || action.State != "pending" || lifecycle.State != "awaiting_ensure" {
		t.Fatalf("pre-effect rejection became uncertain action=%+v lifecycle=%+v err=%v", action, lifecycle, err)
	}
}

type rejectedEnsurePort struct{ *FakePort }

func (p *rejectedEnsurePort) EnsureLifecycleSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error) {
	return LifecycleReceipt{}, &HTTPError{Status: 400, Code: "invalid_request", Detail: "workspace root is not configured"}
}

func TestActorLifecycleRuntimeLifecycleHTTPValidationRejectionIsRetryable(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-orchestrator")
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("http-validation-rejection-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	resolver := actorRuntimeResolver(t, nil, &rejectedEnsurePort{FakePort: fake})
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	report, err := (ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "http-validation",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}).Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Retried != 1 {
		t.Fatalf("HTTP validation rejection was not retried report=%+v err=%v", report, err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil || lifecycle.State != "awaiting_ensure" || lifecycle.LastError == "" {
		t.Fatalf("HTTP validation rejection became uncertain lifecycle=%+v err=%v", lifecycle, err)
	}
}

func (p *lostRecoveryEnsureResponsePort) EnsureLifecycleSession(ctx context.Context,
	target SessionTarget, action Action) (LifecycleReceipt, error) {
	receipt, err := p.FakePort.EnsureLifecycleSession(ctx, target, action)
	if err != nil {
		return receipt, err
	}
	if !p.lost {
		p.lost = true
		return LifecycleReceipt{}, errors.New("response lost after managed recovery Ensure commit")
	}
	return receipt, nil
}

func TestActorLifecycleRuntimeAdoptedInteractorUncertainPromotionNeverBlindlyResends(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverInteractorRole, "russ-claude")
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("adopted-interactor-uncertain-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "external-store", "default", "external")
	fake.Watches["%1"] = ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1", Enabled: true,
		Lifecycle: "active", Provider: "claude", Profile: "interactive"}
	resolver := actorRuntimeResolver(t, fake, nil)
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		externalActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	baseRuntime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "adopt-first",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}
	if report, err := baseRuntime.Tick(context.Background(), now.Add(2*time.Minute)); err != nil || report.Executed != 1 {
		t.Fatalf("adopt report=%+v err=%v", report, err)
	}
	delete(fake.Sessions, "session-russ-claude")
	lost := &lostRecoveryEnsureResponsePort{FakePort: fake}
	lostResolver := actorRuntimeResolver(t, lost, nil)
	runtime := ActorLifecycleRuntime{Resolver: lostResolver, Store: st, Owner: "recover-uncertain",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}
	report, err := runtime.Tick(context.Background(), now.Add(3*time.Minute))
	if err != nil || report.PresenceRecovered != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("uncertain recovery report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	report, err = runtime.Tick(context.Background(), now.Add(5*time.Minute))
	if err != nil || report.Verified != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("uncertain verification report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverInteractorRole)
	if err != nil || active.State != "active" || active.TargetEpoch != 2 {
		t.Fatalf("uncertain recovery lifecycle=%+v err=%v", active, err)
	}
}

func TestActorLifecycleRuntimeManagedEnsureRestartUsesDurableReceipt(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("actor-q3-runtime-test-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
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
	driverAction, cleanup, err := materials.ResolveLifecycleLaunch(context.Background(), driverAction,
		now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := executeActorLifecycle(context.Background(), fake, driverAction)
	cleanup(err == nil && receipt.Resolved())
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.EnsureTargets) != 1 || fake.EnsureTargets[0].Bootstrap == nil ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"project_name":"Russ"`) ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"repository_ids":["russ-repo"]`) ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"role":"orchestrator"`) ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"model_family":"codex"`) ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"operating_discipline_version":"v1"`) ||
		!strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8, `"initial_handoff_utf8":`) ||
		fake.EnsureTargets[0].Bootstrap.PayloadSHA256 != sha256Text(fake.EnsureTargets[0].Bootstrap.ContentUTF8) {
		t.Fatalf("Driver did not receive exact actor identity/context bundle: %+v", fake.EnsureTargets)
	}
	if fake.EnsureTargets[0].CredentialEnvelope == nil ||
		strings.Contains(fake.EnsureTargets[0].Bootstrap.ContentUTF8,
			fake.EnsureTargets[0].CredentialEnvelope.SecretUTF8) {
		t.Fatal("actor credential leaked into public bootstrap")
	}
	firstPublicBootstrap := fake.EnsureTargets[0].Bootstrap.ContentUTF8
	replayed, err := fake.EnsureLifecycleSession(context.Background(), fake.EnsureTargets[0], driverAction)
	if err != nil || replayed.LifecycleReceiptID != receipt.LifecycleReceiptID || fake.EnsureCalls != 1 ||
		fake.EnsureTargets[0].Bootstrap.ContentUTF8 != firstPublicBootstrap {
		t.Fatalf("exact actor restart replay changed context or duplicated Ensure: receipt=%+v calls=%d err=%v",
			replayed, fake.EnsureCalls, err)
	}
	if _, err := st.PersistProjectActorLifecycleReceipt(context.Background(), actorStoreReceipt(receipt),
		now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// Process dies after receipt persistence and before projection/ack. A fresh
	// runtime folds the durable receipt and never repeats Ensure.
	runtime := ActorLifecycleRuntime{Resolver: resolver, Store: st, Owner: "actor-runtime",
		ClaimTTL: time.Minute, MaxRecovery: 3, Materials: materials, RequireManagedAgentV3: true}
	report, err := runtime.Tick(context.Background(), now.Add(4*time.Minute))
	if err != nil || report.Verified != 1 || fake.EnsureCalls != 1 {
		t.Fatalf("restart report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil || active.State != "active" || active.ActiveBindingID == "" {
		t.Fatalf("active orchestrator lifecycle=%+v err=%v", active, err)
	}
	// The orchestrator dies after a successful, acknowledged Ensure. Continuous
	// exact-presence reconciliation must fence the dead run and raise a durable
	// replacement with higher target+credential epochs; the stale binding may
	// never remain active or be resurrected.
	fake.Sessions = map[string]Identity{}
	report, err = runtime.Tick(context.Background(), now.Add(5*time.Minute))
	if err != nil || report.PresenceRecovered != 1 || report.Executed != 1 || fake.EnsureCalls != 2 {
		t.Fatalf("post-ack actor death report=%+v ensure_calls=%d err=%v", report, fake.EnsureCalls, err)
	}
	replacement, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil || replacement.State != "active" || replacement.ActiveBindingID == "" ||
		replacement.TargetEpoch != 2 || replacement.CredentialGeneration != 2 {
		t.Fatalf("recovered orchestrator lifecycle=%+v err=%v", replacement, err)
	}
	var oldState string
	if err := st.DB.QueryRow(`SELECT state FROM driver_session_bindings WHERE binding_id=?`,
		active.ActiveBindingID).Scan(&oldState); err != nil || oldState != "superseded" {
		t.Fatalf("dead orchestrator binding state=%q err=%v", oldState, err)
	}
}

func TestActorLifecycleRuntimeMissingCommittedQ3EnvelopeMakesZeroDriverEnsureCalls(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
	dir := t.TempDir()
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: dir,
		WorkerAuthSecret: []byte("actor-q3-missing-envelope-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("actor envelope entries=%d err=%v", len(entries), err)
	}
	if err := os.Remove(filepath.Join(dir, entries[0].Name())); err != nil {
		t.Fatal(err)
	}
	runtime := ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, nil, fake), Store: st,
		Owner: "actor-runtime-missing-q3", ClaimTTL: time.Minute, MaxRecovery: 3,
		Materials: materials, RequireManagedAgentV3: true}
	report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || report.Retried != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("missing actor Q3 envelope report=%+v ensure_calls=%d err=%v",
			report, fake.EnsureCalls, err)
	}
}

func TestActorLifecycleRuntimeRejectsInvalidQ3BytesBeforeDriverEnsure(t *testing.T) {
	tests := []struct {
		name             string
		bootstrapPayload string
		credential       string
	}{
		{name: "bootstrap boundary plus one", bootstrapPayload: strings.Repeat("b", (16<<10)+1)},
		{name: "bootstrap invalid utf8", bootstrapPayload: string(append(bytes.Repeat([]byte{'b'}, 64), 0xff))},
		{name: "credential boundary plus one", credential: strings.Repeat("A1", (8<<10)/2+1)},
		{name: "credential invalid utf8", credential: strings.Repeat("A1", 20) + string([]byte{0xff})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
			dir := t.TempDir()
			materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: dir,
				WorkerAuthSecret: []byte("actor-q3-invalid-bytes-secret")}
			st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
			fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
			_, action, err := st.CommitProjectActorLifecycleIntent(context.Background(),
				managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.DB.Exec(`DROP TRIGGER trg_project_actor_q3_action_immutable`); err != nil {
				t.Fatal(err)
			}
			if tc.bootstrapPayload != "" {
				if _, err := st.DB.Exec(`UPDATE project_actor_lifecycle_actions
					SET bootstrap_payload=?,bootstrap_sha256=? WHERE id=?`, tc.bootstrapPayload,
					sha256Text(tc.bootstrapPayload), action.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.credential != "" {
				entries, err := os.ReadDir(dir)
				if err != nil || len(entries) != 1 {
					t.Fatalf("credential entries=%d err=%v", len(entries), err)
				}
				if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte(tc.credential), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := st.DB.Exec(`UPDATE project_actor_lifecycle_actions
					SET credential_payload_sha256=? WHERE id=?`, sha256Text(tc.credential), action.ID); err != nil {
					t.Fatal(err)
				}
			}
			runtime := ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, nil, fake), Store: st,
				Owner: "actor-runtime-invalid-q3", ClaimTTL: time.Minute, MaxRecovery: 3,
				Materials: materials, RequireManagedAgentV3: true}
			report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute))
			if err != nil || report.Retried != 1 || fake.EnsureCalls != 0 {
				t.Fatalf("invalid Q3 reached Driver: report=%+v ensure_calls=%d err=%v",
					report, fake.EnsureCalls, err)
			}
		})
	}
}

func TestActorLifecycleRuntimeStopRevokesAndDeletesActorCredentialEnvelope(t *testing.T) {
	st, route, now := actorRuntimeProject(t, flowstore.DriverOrchestratorRole, "russ-codex")
	dir := t.TempDir()
	materials := SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: dir,
		WorkerAuthSecret: []byte("actor-q3-stop-cleanup-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	fake := endpointRuntimeFake("mac", "managed-store", "flowbee", "managed_dedicated")
	runtime := ActorLifecycleRuntime{Resolver: actorRuntimeResolver(t, nil, fake), Store: st,
		Owner: "actor-runtime-stop", ClaimTTL: time.Minute, MaxRecovery: 3,
		Materials: materials, RequireManagedAgentV3: true}
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(),
		managedActorCommand(route, fake.Meta.TmuxServer.InstanceID), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if report, err := runtime.Tick(context.Background(), now.Add(2*time.Minute)); err != nil || report.Executed != 1 {
		t.Fatalf("ensure report=%+v err=%v", report, err)
	}
	active, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if active.CredentialEnvelopeDeletedAt == "" || active.CredentialRevokedAt != "" {
		t.Fatalf("installed credential tombstone is wrong before Stop: %+v", active)
	}
	if _, _, err := st.CommitProjectActorLifecycleIntent(context.Background(), flowstore.ProjectActorLifecycleCommand{
		ProjectID: "russ", Role: flowstore.DriverOrchestratorRole, ActorID: "russ-codex",
		ExpectedRouteStateVersion: int64(route.StateVersion), ExpectedLifecycleStateVersion: active.StateVersion,
		Operation: "stop", IdempotencyKey: "stop-russ-codex", InstanceRef: "managed",
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if report, err := runtime.Tick(context.Background(), now.Add(4*time.Minute)); err != nil ||
		report.Executed != 1 || fake.StopCalls != 1 {
		t.Fatalf("stop report=%+v calls=%d err=%v", report, fake.StopCalls, err)
	}
	stopped, err := st.CurrentProjectActorLifecycle(context.Background(), "russ", flowstore.DriverOrchestratorRole)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != "stopped" || stopped.CredentialEnvelopeDeletedAt == "" ||
		stopped.CredentialRevokedAt == "" || stopped.ActiveBindingID != "" {
		t.Fatalf("Stop did not revoke/tombstone actor credential and binding: %+v", stopped)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("credential envelope directory after Stop has %d entries: %v", len(entries), err)
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
