package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

const (
	bareE2EHostID          = "00000000-0000-4000-8000-000000000101"
	bareE2EExternalStoreID = "00000000-0000-4000-8000-000000000102"
	bareE2EManagedStoreID  = "00000000-0000-4000-8000-000000000103"
)

// bareE2ESystem is deliberately made out of DriverPort operations. It models
// the no-argument CLI's machine boundary without a daemon, a real tmux server,
// or a second terminal mutation path.
type bareE2ESystem struct {
	cfg       config.Config
	inventory config.DriverEndpointInventory
	repoRoot  string
	external  *driver.FakePort
	managed   *driver.FakePort

	driversReady, bootstrapReady, liveReady bool
	driverProbes, controlEnsures            int
}

func (s *bareE2ESystem) Config() (config.Config, error) { return s.cfg, nil }

func (s *bareE2ESystem) Git(_ context.Context, args ...string) (string, error) {
	switch {
	case len(args) == 2 && args[0] == "rev-parse" && args[1] == "--show-toplevel":
		return s.repoRoot, nil
	case len(args) == 5 && args[0] == "-C" && args[1] == s.repoRoot &&
		args[2] == "remote" && args[3] == "get-url" && args[4] == "origin":
		return "git@github.com:Sam/Russ.git", nil
	case len(args) == 5 && args[0] == "-C" && args[1] == s.repoRoot &&
		args[2] == "rev-parse" && args[3] == "--git-path" && args[4] == "info":
		return filepath.Join(s.repoRoot, ".git", "info"), nil
	default:
		return "", fmt.Errorf("unexpected git invocation: %v", args)
	}
}

func (s *bareE2ESystem) DriverInventory() (config.DriverEndpointInventory, bool, error) {
	return s.inventory, true, nil
}

func (s *bareE2ESystem) DriverServiceEnsurer(config.DriverEndpoint) (bootstrap.DriverServiceEnsurer, error) {
	return nil, errors.New("test endpoint has no service Ensure authority")
}

func (s *bareE2ESystem) ProbeDrivers(ctx context.Context, inventory config.DriverEndpointInventory) error {
	s.driverProbes++
	if !s.driversReady {
		return errors.New("Driver endpoints are down")
	}
	if len(inventory.Endpoints) != 2 {
		return errors.New("exact external and managed Driver endpoints are required")
	}
	ports := map[string]*driver.FakePort{"external": s.external, "managed": s.managed}
	seenExternal, seenManaged := false, false
	for _, endpoint := range inventory.Endpoints {
		port := ports[endpoint.InstanceRef]
		if port == nil {
			return errors.New("unknown Driver instance_ref")
		}
		meta, err := port.Metadata(ctx)
		if err != nil || meta.HostID != endpoint.ExpectedHostID || meta.StoreID != endpoint.ExpectedStoreID ||
			meta.TmuxServer.DomainID != endpoint.ExpectedTmuxServerDomainID ||
			meta.TmuxServer.Ownership != endpoint.ExpectedTmuxServerOwnership {
			return errors.New("Driver endpoint identity mismatch")
		}
		if _, err := port.ControlOriginCapability(ctx); err != nil {
			return err
		}
		seenExternal = seenExternal || meta.TmuxServer.DomainID == "default" && meta.TmuxServer.Ownership == "external"
		seenManaged = seenManaged || meta.TmuxServer.DomainID == "flowbee" && meta.TmuxServer.Ownership == "managed_dedicated"
	}
	if !seenExternal || !seenManaged {
		return errors.New("Driver topology collapsed")
	}
	return nil
}

func (s *bareE2ESystem) ControlPlaneReady(context.Context, config.Config) (bool, error) {
	return s.liveReady, nil
}

func (s *bareE2ESystem) ControlPlaneBootstrapReady(context.Context, config.Config, string) (bool, error) {
	return s.bootstrapReady || s.liveReady, nil
}

func (s *bareE2ESystem) EnsureControlPlane(ctx context.Context, _ config.DriverEndpointInventory,
	spec bareControlPlaneSpec, actionID string) (driver.LifecycleReceipt, error) {
	s.controlEnsures++
	action := driver.NewAction(actionID, "flowbee-control-plane:"+spec.LifecycleKey, spec.TargetEpoch)
	target := driver.SessionTarget{Identity: driver.Identity{HostID: spec.HostID, StoreID: spec.StoreID,
		TmuxServerDomainID: spec.TmuxServerDomainID, TmuxServerInstanceID: spec.TmuxServerInstanceID},
		LifecycleKey: spec.LifecycleKey, TargetEpoch: spec.TargetEpoch, ProfileID: spec.ProfileID,
		WorkspaceRootID: spec.WorkspaceRootID, WorkspaceRelativePath: spec.WorkspaceRelativePath,
		LeaseID: "flowbee-control-plane-" + spec.LifecycleKey, LeaseEpoch: spec.TargetEpoch,
		PresentationName: "flowbee"}
	receipt, err := s.managed.EnsureLifecycleSession(ctx, target, action)
	if err == nil && receipt.Status == "ensured" {
		s.bootstrapReady = true
	}
	return receipt, err
}

type bareE2EHarness struct {
	t      *testing.T
	ctx    context.Context
	now    time.Time
	clock  *clock.Fake
	store  *store.Store
	system *bareE2ESystem
	plan   bareServerActionPlan
	db     interface{ Close() error }
	ledger bootstrap.CheckpointStore

	resolver  *driver.EndpointResolver
	materials driver.SQLLifecycleLaunchMaterials
	capacity  bool
	attached  int
}

func newBareE2EHarness(t *testing.T, driversReady bool) *bareE2EHarness {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 30, 0, 0, time.UTC)
	repoRoot := filepath.Join(t.TempDir(), "russ")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git", "info"), 0o700); err != nil {
		t.Fatal(err)
	}
	repoRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	external := bareE2EFakePort(bareE2EExternalStoreID, "default", "external")
	managed := bareE2EFakePort(bareE2EManagedStoreID, "flowbee", "managed_dedicated")
	managed.ProfileInventory.Profiles = append(managed.ProfileInventory.Profiles, driver.LifecycleProfile{
		ProfileID: "flowbee_control", EnsureSupported: true, ManagedDisplayNameSupported: true,
	})
	external.Watches["%1"] = driver.ExternalWatch{WatchID: "watch-russ-claude", PaneID: "%1",
		Enabled: true, Lifecycle: "active", Provider: "claude", Profile: "interactive"}

	buildSeat := store.Seat{AgentFamily: "codex", CodexHome: "/test/codex", MaxConcurrent: 1}
	reviewSeat := store.Seat{AgentFamily: "grok", ConfigDir: "/test/grok", MaxConcurrent: 1}
	project := config.BootstrapProjectConfig{ProjectID: "russ", Name: "Russ",
		RepositoryIDs: []string{"russ-repo"},
		ControlPlane: config.BootstrapControlPlaneConfig{InstanceRef: "managed", LifecycleKey: "flowbee-control",
			TargetEpoch: 1, ProfileID: "flowbee_control", WorkspaceRootID: "dev",
			WorkspaceRelativePath: "flowbee", TmuxServerInstanceID: managed.Meta.TmuxServer.InstanceID},
		Interactor: config.BootstrapInteractorConfig{ActorID: "russ-claude", PresentationName: "russ-interactor",
			Operation: "adopt", InstanceRef: "external", LifecycleKey: "russ-claude", TargetEpoch: 1,
			ProfileID: "claude_interactor", ExternalWatchID: "watch-russ-claude",
			ExistingSessionID: "session-russ-claude", ExpectedPaneInstanceID: "pane-russ-claude",
			ExpectedAgentRunID: "run-russ-claude", TmuxServerInstanceID: external.Meta.TmuxServer.InstanceID,
			RecoveryProfileID: "claude_interactor_managed", RecoveryWorkspaceRootID: "dev",
			RecoveryWorkspaceRelativePath: "russ/interactor"},
		Orchestrator: config.BootstrapOrchestratorConfig{ActorID: "russ-orchestrator",
			PresentationName: "russ-orchestrator", InstanceRef: "managed", LifecycleKey: "russ-orchestrator",
			TargetEpoch: 1, ProfileID: "codex_orchestrator", WorkspaceRootID: "dev",
			WorkspaceRelativePath: "russ", TmuxServerInstanceID: managed.Meta.TmuxServer.InstanceID},
		LocalSeats: []config.BootstrapSeatConfig{
			{SeatID: buildSeat.ComposeID(), HostID: bareE2EHostID, AgentFamily: "codex", CodexHome: "/test/codex",
				MaxConcurrent: 1, AccountKey: "codex-account", CredentialLineage: "codex-lineage", ReservePct: 10,
				AccountMaximum: 1, InstanceRef: "managed", TmuxServerDomainID: "flowbee",
				TmuxServerInstanceID: managed.Meta.TmuxServer.InstanceID, ProfileID: "codex_builder",
				WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ/workers"},
			{SeatID: reviewSeat.ComposeID(), HostID: bareE2EHostID, AgentFamily: "grok", ConfigDir: "/test/grok",
				MaxConcurrent: 1, AccountKey: "grok-account", CredentialLineage: "grok-lineage", ReservePct: 10,
				AccountMaximum: 1, InstanceRef: "managed", TmuxServerDomainID: "flowbee",
				TmuxServerInstanceID: managed.Meta.TmuxServer.InstanceID, ProfileID: "grok_reviewer",
				WorkspaceRootID: "dev", WorkspaceRelativeBase: "russ/reviewers"},
		},
	}
	cfg := config.Config{Repos: []config.RepoConfig{{ID: "russ-repo", Owner: "sam", Repo: "russ"}},
		BootstrapProjects: []config.BootstrapProjectConfig{project}}
	inventory := config.DriverEndpointInventory{Endpoints: []config.DriverEndpoint{
		{InstanceRef: "external", ExpectedHostID: bareE2EHostID, ExpectedStoreID: bareE2EExternalStoreID,
			ExpectedTmuxServerDomainID: "default", ExpectedTmuxServerOwnership: "external"},
		{InstanceRef: "managed", ExpectedHostID: bareE2EHostID, ExpectedStoreID: bareE2EManagedStoreID,
			ExpectedTmuxServerDomainID: "flowbee", ExpectedTmuxServerOwnership: "managed_dedicated"},
	}}
	system := &bareE2ESystem{cfg: cfg, inventory: inventory, repoRoot: repoRoot,
		external: external, managed: managed, driversReady: driversReady}

	st := testutil.NewStore(t)
	st.EnableCapacityV2 = true
	st.EnableEpicDedicatedWorkersV2 = true
	if err := st.RegisterRepo(ctx, store.Repo{ID: "russ-repo", Owner: "sam", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	materials := driver.SQLLifecycleLaunchMaterials{DB: st.DB, EnvelopeDirectory: t.TempDir(),
		WorkerAuthSecret: []byte("bare-bootstrap-e2e-actor-secret")}
	st.ProjectActorCredentialMaterializer = materials.PrepareEnvelope
	st.DriverControlOriginEndpointGate = func(hostID, storeID, domain string) bool {
		return hostID == bareE2EHostID && (storeID == bareE2EExternalStoreID && domain == "default" ||
			storeID == bareE2EManagedStoreID && domain == "flowbee")
	}

	resolver, err := driver.NewEndpointResolver(ctx, []driver.EndpointEntry{
		{InstanceRef: "external", Port: external,
			Expected:                driver.EndpointKey{HostID: bareE2EHostID, StoreID: bareE2EExternalStoreID, TmuxServerDomainID: "default"},
			ExpectedServerOwnership: "external"},
		{InstanceRef: "managed", Port: managed,
			Expected:                driver.EndpointKey{HostID: bareE2EHostID, StoreID: bareE2EManagedStoreID, TmuxServerDomainID: "flowbee"},
			ExpectedServerOwnership: "managed_dedicated"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []driver.EndpointKey{
		{HostID: bareE2EHostID, StoreID: bareE2EExternalStoreID, TmuxServerDomainID: "default"},
		{HostID: bareE2EHostID, StoreID: bareE2EManagedStoreID, TmuxServerDomainID: "flowbee"},
	} {
		if _, err := resolver.ControlReadiness(ctx, key); err != nil {
			t.Fatal(err)
		}
	}

	preflight, err := resolveBareBootstrap(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	init, err := (bootstrap.FileProjectInitResolver{RepoRoot: preflight.RepoRoot, GitInfoDir: preflight.GitInfoDir,
		RequestedProjectID: preflight.ProjectID, Origins: fixedBareOrigin(preflight.Origin)}).
		ResolveProjectInit(ctx, preflight.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := buildBareServerActionPlan(cfg, inventory, preflight, init)
	if err != nil {
		t.Fatal(err)
	}
	db, ledger, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	h := &bareE2EHarness{t: t, ctx: ctx, now: now, clock: clock.NewFake(now), store: st, system: system,
		plan: plan, db: db, ledger: ledger, resolver: resolver, materials: materials}
	h.syncDriverFacts()
	return h
}

func bareE2EFakePort(storeID, domain, ownership string) *driver.FakePort {
	fake := driver.NewFake()
	fake.Meta.HostID, fake.Meta.StoreID, fake.Meta.ProducerBootID = bareE2EHostID, storeID, "boot-"+domain
	fake.Meta.ControlPrincipalOrigin = true
	fake.Meta.ReplayFloorCursor, fake.Meta.DurableHighWaterCursor = "tdc2.0", "tdc2.9"
	fake.Meta.TmuxServer.DomainID, fake.Meta.TmuxServer.Ownership = domain, ownership
	fake.ProfileInventory.TmuxServerDomainID = domain
	if ownership == "external" {
		fake.Meta.TmuxServer.ConnectionVisibility = "default_or_external"
		fake.ProfileInventory.Profiles = append(fake.ProfileInventory.Profiles, driver.LifecycleProfile{
			ProfileID: "claude_interactor_managed", Provider: "claude", InitialPromptAdapter: "argv_element",
			TargetCredentialAdapter: "file_environment", EnsureSupported: true,
			BootstrapArtifactSupported: true, FlowbeeCredentialInstallSupported: true,
			HumanVisibleSessionSupported: true,
		})
	}
	return fake
}

func (h *bareE2EHarness) syncDriverFacts() {
	h.t.Helper()
	obs := driver.ObservationSQLStore{DB: h.store.DB, Now: h.clock.Now}
	for _, item := range []struct {
		ref  string
		port *driver.FakePort
	}{{"external", h.system.external}, {"managed", h.system.managed}} {
		if _, err := obs.EnsureInstance(h.ctx, item.ref, item.port.Meta); err != nil {
			h.t.Fatal(err)
		}
		ids := make([]driver.Identity, 0, len(item.port.Sessions))
		for _, id := range item.port.Sessions {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i].SessionID < ids[j].SessionID })
		projections := make([]driver.SessionProjection, 0, len(ids))
		for _, id := range ids {
			projections = append(projections, driver.SessionProjection{Identity: id, Lifecycle: "active",
				Phase: "idle", AsOfCursor: "tdc2.9", RawState: []byte(`{}`)})
		}
		if err := obs.ReplaceSnapshot(h.ctx, item.ref, driver.SessionSnapshot{HostID: item.port.Meta.HostID,
			StoreID: item.port.Meta.StoreID, AsOfCursor: "tdc2.9", Sessions: projections}); err != nil {
			h.t.Fatal(err)
		}
	}
}

func (h *bareE2EHarness) seedCapacity() error {
	if h.capacity {
		return nil
	}
	var seats []struct{ id, family, account, lineage string }
	rows, err := h.store.DB.QueryContext(h.ctx, `SELECT id,agent_family,expected_account_key,expected_credential_lineage
		FROM seats WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var item struct{ id, family, account, lineage string }
		if err := rows.Scan(&item.id, &item.family, &item.account, &item.lineage); err != nil {
			rows.Close()
			return err
		}
		seats = append(seats, item)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(seats) != 2 {
		return fmt.Errorf("want two bootstrapped seats, got %d", len(seats))
	}
	generation := store.CapacityGeneration{ID: "bare-e2e-capacity", StartedAt: h.clock.Now()}
	for _, seat := range seats {
		generation.ExpectedSeatIDs = append(generation.ExpectedSeatIDs, seat.id)
		observation := store.CapacitySeatObservation{ObservationID: "obs-" + seat.family,
			SeatID: seat.id, HostID: bareE2EHostID, Provider: seat.family, AccountKey: seat.account,
			CredentialLineage: seat.lineage, CollectorID: "bare-e2e", TrustState: "verified",
			IntegrityState: "verified", FetchedAt: h.clock.Now(), RawSHA256: "sha256:" + seat.family,
			AdapterVersion: "fixture/v1"}
		if seat.family == "grok" {
			observation.Source, observation.BillingPeriodActive = "live_billing", true
			observation.Windows = []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 10}}
		} else {
			observation.Source = "live_app_server"
			observation.Windows = []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}}
		}
		generation.Observations = append(generation.Observations, observation)
	}
	if err := h.store.CommitCapacityGeneration(h.ctx, generation, h.clock.Now()); err != nil {
		return err
	}
	h.capacity = true
	return nil
}

type bareE2EAPIClient struct {
	harness *bareE2EHarness
	client  bootstrapAPIClient
	owner   string
}

func (c *bareE2EAPIClient) Commit(ctx context.Context, action api.BootstrapAction) (api.BootstrapActionReceipt, error) {
	return c.client.Commit(ctx, action)
}

func (c *bareE2EAPIClient) Status(ctx context.Context, actionID string) (api.BootstrapActionStatus, error) {
	if err := c.pump(ctx); err != nil {
		return api.BootstrapActionStatus{}, err
	}
	return c.client.Status(ctx, actionID)
}

func (c *bareE2EAPIClient) Activation(ctx context.Context, projectID string) (store.ProjectActivationStatus, error) {
	c.harness.syncDriverFacts()
	if err := c.harness.seedCapacity(); err != nil {
		return store.ProjectActivationStatus{}, err
	}
	activation, err := c.client.Activation(ctx, projectID)
	if err == nil && activation.LiveReady {
		c.harness.system.liveReady = true
	}
	return activation, err
}

func (c *bareE2EAPIClient) pump(ctx context.Context) error {
	h := c.harness
	h.clock.Advance(time.Second)
	now := h.clock.Now()
	if err := (bootstrapActionRuntime{Store: h.store, Owner: "bootstrap-" + c.owner}).Tick(ctx, now); err != nil {
		return err
	}
	actorRuntime := driver.ActorLifecycleRuntime{Resolver: h.resolver, Store: h.store,
		Materials: h.materials, RequireManagedAgentV3: true, Owner: "actors-" + c.owner,
		ClaimTTL: time.Minute, MaxRecovery: 3}
	if _, err := actorRuntime.Tick(ctx, now); err != nil {
		return err
	}
	h.syncDriverFacts()
	return (bootstrapActionRuntime{Store: h.store, Owner: "bootstrap-" + c.owner}).Tick(ctx, now.Add(time.Millisecond))
}

func (h *bareE2EHarness) newServerClient(owner string) (*httptest.Server, *bareE2EAPIClient) {
	h.t.Helper()
	authn := auth.NewBearer([]byte("bare-bootstrap-e2e-human-secret"), []string{"bootstrap-cli"}, false)
	human := auth.NewHumanAccess([]byte(strings.Repeat("h", 32)), authn, map[string][]auth.HumanGrant{
		"bootstrap-cli": {{ProjectID: "*", Role: auth.HumanAdmin}},
	}, false)
	srv := api.New(h.store, h.clock, ulid.NewMinter(nil), api.Config{HumanAccess: human}, "bare-e2e")
	srv.SetBootstrapActionIntake(&serverBootstrapIntake{Store: h.store, Now: h.clock.Now})
	ts := httptest.NewServer(srv.PrivateHandler())
	client := &bareE2EAPIClient{harness: h, owner: owner,
		client: bootstrapAPIClient{BaseURL: ts.URL, Bearer: authn.Mint("bootstrap-cli"), Client: ts.Client()}}
	return ts, client
}

type interruptAfterCommitClient struct {
	delegate  bareBootstrapActionClient
	server    *httptest.Server
	remaining int
}

func (c *interruptAfterCommitClient) Commit(ctx context.Context, action api.BootstrapAction) (api.BootstrapActionReceipt, error) {
	receipt, err := c.delegate.Commit(ctx, action)
	if err != nil {
		return receipt, err
	}
	c.remaining--
	if c.remaining == 0 {
		c.server.Close()
		return api.BootstrapActionReceipt{}, errors.New("serve died after durably committing bootstrap action")
	}
	return receipt, nil
}
func (c *interruptAfterCommitClient) Status(ctx context.Context, id string) (api.BootstrapActionStatus, error) {
	return c.delegate.Status(ctx, id)
}
func (c *interruptAfterCommitClient) Activation(ctx context.Context, project string) (store.ProjectActivationStatus, error) {
	return c.delegate.Activation(ctx, project)
}

func TestBareBootstrapEndToEndAuthenticatedRestartAndIdempotentAttach(t *testing.T) {
	h := newBareE2EHarness(t, true)
	defer h.db.Close()
	if countRowsWhere(t, h.store, "projects", "id='russ'") != 0 || countRows(t, h.store, "project_actor_routes") != 0 ||
		countRows(t, h.store, "bootstrap_actions") != 0 {
		t.Fatal("harness did not begin with no serve-owned project or actors")
	}
	marker, ok, err := bootstrap.ExistingProjectMarker(h.system.repoRoot)
	if err != nil || !ok || marker.ProjectID != "russ" || marker.RepositoryOrigin != "github.com/sam/russ" {
		t.Fatalf("validated private project marker=%+v found=%v err=%v", marker, ok, err)
	}
	if err := ensureBareDriverServices(h.ctx, h.system, h.system.inventory, h.plan, h.ledger); err != nil {
		t.Fatal(err)
	}
	if err := ensureBareControlPlane(h.ctx, h.system, h.system.cfg, h.system.inventory, h.plan, h.ledger); err != nil {
		t.Fatal(err)
	}
	if !h.system.bootstrapReady || h.system.managed.EnsureCalls != 1 || h.system.controlEnsures != 1 {
		t.Fatalf("control bootstrap ready=%v Driver ensures=%d calls=%d", h.system.bootstrapReady,
			h.system.managed.EnsureCalls, h.system.controlEnsures)
	}
	h.syncDriverFacts()

	firstServer, firstClient := h.newServerClient("before-death")
	unauthorized, err := http.Get(firstServer.URL + "/v1/projects/russ/activation")
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bootstrap API did not require authentication: %d", unauthorized.StatusCode)
	}
	interrupted := &interruptAfterCommitClient{delegate: firstClient, server: firstServer, remaining: 3}
	runner := bareServerActionRunner{Store: h.ledger, Client: interrupted, PollInterval: time.Millisecond,
		FinalReady: func(context.Context) (bool, error) { return h.system.liveReady, nil },
		Attach:     func(intent bootstrap.AttachIntentSpec) error { h.attached++; return nil }}
	if err := runner.Run(h.ctx, h.plan); err == nil || !strings.Contains(err.Error(), "serve died") {
		t.Fatalf("server death was not surfaced: %v", err)
	}
	if h.attached != 0 || countRows(t, h.store, "bootstrap_actions") != 3 {
		t.Fatalf("partial bootstrap attached=%d durable actions=%d", h.attached,
			countRows(t, h.store, "bootstrap_actions"))
	}

	// A fresh private server and fresh runtime owners continue from the same
	// immutable checkpoint. The third action's lost acknowledgement is replayed
	// by exact id, not re-authored as a second action.
	secondServer, secondClient := h.newServerClient("after-death")
	defer secondServer.Close()
	runner.Client = secondClient
	if err := runner.Run(h.ctx, h.plan); err != nil {
		for _, action := range h.plan.Actions {
			record, _ := h.store.GetBootstrapAction(h.ctx, action.ActionID)
			t.Logf("bootstrap action kind=%s id=%s state=%s error=%q", action.Kind, action.ActionID,
				record.State, record.LastError)
		}
		t.Fatal(err)
	}
	if h.attached != 1 {
		t.Fatalf("successful bootstrap attach count=%d", h.attached)
	}
	activation, err := secondClient.client.Activation(h.ctx, "russ")
	if err != nil || !activation.LiveReady || len(activation.Holds) != 0 {
		t.Fatalf("final activation=%+v err=%v", activation, err)
	}
	if countRows(t, h.store, "bootstrap_actions") != len(h.plan.Actions) ||
		countRows(t, h.store, "project_actor_routes") != 2 || countRows(t, h.store, "builder_driver_targets") != 2 {
		t.Fatalf("actions=%d/%d actors=%d targets=%d", countRows(t, h.store, "bootstrap_actions"), len(h.plan.Actions),
			countRows(t, h.store, "project_actor_routes"), countRows(t, h.store, "builder_driver_targets"))
	}
	if h.system.external.AdoptCalls != 1 || h.system.managed.EnsureCalls != 2 ||
		h.system.external.SendCalls != 0 || h.system.managed.SendCalls != 0 {
		t.Fatalf("Driver effects adopt=%d ensure=%d sends=%d/%d", h.system.external.AdoptCalls,
			h.system.managed.EnsureCalls, h.system.external.SendCalls, h.system.managed.SendCalls)
	}
	for _, action := range h.plan.Actions {
		record, err := h.store.GetBootstrapAction(h.ctx, action.ActionID)
		if err != nil || record.State != "succeeded" {
			t.Fatalf("action %s state=%q err=%v", action.ActionID, record.State, err)
		}
	}

	// Bare re-run revalidates the real activation proof and final health, then
	// attaches. It commits no action and creates no Driver lifecycle effect.
	beforeActions, beforeAdopts, beforeEnsures := countRows(t, h.store, "bootstrap_actions"),
		h.system.external.AdoptCalls, h.system.managed.EnsureCalls
	if err := runner.Run(h.ctx, h.plan); err != nil {
		t.Fatal(err)
	}
	if h.attached != 2 || countRows(t, h.store, "bootstrap_actions") != beforeActions ||
		h.system.external.AdoptCalls != beforeAdopts || h.system.managed.EnsureCalls != beforeEnsures {
		t.Fatalf("rerun attached=%d actions=%d/%d adopts=%d/%d ensures=%d/%d", h.attached,
			countRows(t, h.store, "bootstrap_actions"), beforeActions, h.system.external.AdoptCalls, beforeAdopts,
			h.system.managed.EnsureCalls, beforeEnsures)
	}
	cp, found, err := h.ledger.Load(h.ctx, h.plan.BootstrapID)
	if err != nil || !found || !cp.Done {
		t.Fatalf("durable completed checkpoint=%+v found=%v err=%v", cp, found, err)
	}
}

func TestBareBootstrapDriverDownLeavesProductStateUntouched(t *testing.T) {
	h := newBareE2EHarness(t, false)
	defer h.db.Close()
	if err := ensureBareDriverServices(h.ctx, h.system, h.system.inventory, h.plan, h.ledger); err == nil {
		t.Fatal("down Driver without pinned service Ensure was accepted")
	}
	if got := countRowsWhere(t, h.store, "projects", "id='russ'"); got != 0 {
		t.Fatalf("Driver-down precondition created russ project rows=%d", got)
	}
	for _, table := range []string{"project_actor_routes", "project_actor_lifecycles",
		"project_actor_lifecycle_actions", "bootstrap_actions", "builder_driver_targets"} {
		if got := countRows(t, h.store, table); got != 0 {
			t.Fatalf("Driver-down precondition mutated %s rows=%d", table, got)
		}
	}
	if h.system.external.AdoptCalls != 0 || h.system.managed.EnsureCalls != 0 || h.system.controlEnsures != 0 {
		t.Fatalf("Driver-down path mutated fake ports adopt=%d ensure=%d control=%d", h.system.external.AdoptCalls,
			h.system.managed.EnsureCalls, h.system.controlEnsures)
	}
}

func countRows(t *testing.T, st *store.Store, table string) int {
	t.Helper()
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countRowsWhere(t *testing.T, st *store.Store, table, predicate string) int {
	t.Helper()
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM ` + table + ` WHERE ` + predicate).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
