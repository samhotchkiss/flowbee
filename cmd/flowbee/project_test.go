package main

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestBindObservedProjectSessionRequiresRouteAndExactLiveLifecycleTarget(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "mail",
		Role: store.DriverInteractorRole, ActorID: "mail-interactor"}, now); err != nil {
		t.Fatal(err)
	}
	id := driver.Identity{HostID: "host-1", StoreID: "store-1", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", Ownership: "driver_managed",
		LifecycleKey: "actor:mail:interactor", TargetEpoch: 3, SessionID: "session-1",
		PaneInstanceID: "pane-1", AgentRunID: "run-1", Provider: "claude", ConversationID: "conversation-1"}
	fake := driver.NewFake()
	fake.Meta = driver.DriverMetadata{HostID: id.HostID, StoreID: id.StoreID, ProducerBootID: "boot-1",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.9",
		TmuxServer: driver.TmuxServerMetadata{DomainID: id.TmuxServerDomainID, InstanceID: id.TmuxServerInstanceID,
			Ownership: "managed_dedicated", ConnectionVisibility: "isolated_socket"}}
	fake.Sessions[id.SessionID] = id
	fake.Snapshot = driver.SessionSnapshot{HostID: id.HostID, StoreID: id.StoreID, AsOfCursor: "tdc2.9",
		Sessions: []driver.SessionProjection{{Identity: id, Lifecycle: "active", AsOfCursor: "tdc2.9"}}}
	in := projectSessionBindingInput{ProjectID: "mail", Role: store.DriverInteractorRole,
		WorkerIdentity: "mail-interactor", LifecycleKey: id.LifecycleKey, TargetEpoch: id.TargetEpoch,
		ProfileID: "claude-fable", WorkspaceRootID: "projects", WorkspaceRelativePath: "mail"}
	binding, err := bindObservedProjectSession(ctx, st, fake, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SessionID != id.SessionID || binding.PaneInstanceID != id.PaneInstanceID ||
		binding.AgentRunID != id.AgentRunID || binding.BindingEpoch != 1 ||
		binding.TmuxServerDomainID != id.TmuxServerDomainID || binding.LifecycleOwnership != "driver_managed" {
		t.Fatalf("binding=%+v", binding)
	}

	// Exact replay is idempotent; a moved pane in the snapshot cannot be used
	// with the old lifecycle presence fact.
	replay, err := bindObservedProjectSession(ctx, st, fake, in, now.Add(time.Minute))
	if err != nil || replay.BindingID != binding.BindingID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	fake.Snapshot.Sessions[0].Identity.PaneInstanceID = "pane-reused"
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now.Add(2*time.Minute)); err == nil {
		t.Fatal("pane reuse was accepted")
	}
}

func TestProjectWorkerAuthStatusRequiresTokensEnrollmentAndNoBypass(t *testing.T) {
	t.Setenv("FLOWBEE_INSECURE", "")
	t.Setenv("FLOWBEE_CAPACITY_COLLECTOR_ID", "capacity-local")
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	activation := store.ProjectActivationStatus{ReviewerIdentities: []string{"reviewer-russ"},
		Reviewers: []store.ProjectReviewerActivation{{WorkerIdentity: "reviewer-russ", ModelFamily: "grok",
			FamilyCapacityConfigured: true, FamilyCapacityRoutable: true}}}
	cfg := config.Config{WorkerAuthSecret: "owner-secret", EnrolledIdentities: []string{
		"reviewer-russ:grok", "capacity-local",
	}, WorkerAttestations: map[string][]string{"reviewer-russ": {"role:code_reviewer"}, "capacity-local": {}},
		AuthLoopbackBypass: false}
	runtime := &store.WorkerAuthRuntimePosture{Fingerprint: workerAuthRuntimeFingerprint(cfg), PID: 42, UpdatedAt: now}
	got := projectWorkerAuthStatus(cfg, activation, runtime, now.Add(time.Second))
	if !got.Secure || len(got.Holds) != 0 || got.EnrolledIdentityCount != 2 {
		t.Fatalf("secure auth=%+v", got)
	}
	atBoundary := projectWorkerAuthStatus(cfg, activation, runtime, now.Add(workerAuthRuntimeFreshness))
	if !atBoundary.Secure {
		t.Fatalf("normal heartbeat jitter flickered activation red at freshness boundary: %+v", atBoundary)
	}
	stale := projectWorkerAuthStatus(cfg, activation, runtime,
		now.Add(workerAuthRuntimeFreshness+time.Nanosecond))
	if stale.Secure || stale.RuntimeConfigVerified ||
		!containsString(stale.Holds, "running_service_auth_posture_stale_or_mismatched") {
		t.Fatalf("stale runtime posture false-greened: %+v", stale)
	}
	t.Setenv("FLOWBEE_INSECURE", "1")
	cfg.AuthLoopbackBypass = true
	cfg.EnrolledIdentities = []string{"capacity-local"}
	got = projectWorkerAuthStatus(cfg, activation, runtime, now.Add(time.Second))
	if got.Secure || len(got.MissingIdentities) != 1 || got.MissingIdentities[0] != "reviewer-russ" {
		t.Fatalf("unsafe auth=%+v", got)
	}
}

func TestProjectWorkerAuthStatusRejectsDifferentActiveSecret(t *testing.T) {
	t.Setenv("FLOWBEE_INSECURE", "")
	t.Setenv("FLOWBEE_CAPACITY_COLLECTOR_ID", "capacity-local")
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	activation := store.ProjectActivationStatus{ReviewerIdentities: []string{"reviewer-russ"},
		Reviewers: []store.ProjectReviewerActivation{{WorkerIdentity: "reviewer-russ", ModelFamily: "grok"}}}
	running := config.Config{WorkerAuthSecret: "running-secret", EnrolledIdentities: []string{
		"reviewer-russ:grok", "capacity-local",
	}, WorkerAttestations: map[string][]string{"reviewer-russ": {"role:code_reviewer"}, "capacity-local": {}},
		AuthLoopbackBypass: false}
	posture := &store.WorkerAuthRuntimePosture{Fingerprint: workerAuthRuntimeFingerprint(running), PID: 42, UpdatedAt: now}
	invoking := running
	invoking.WorkerAuthSecret = "different-shell-secret"
	got := projectWorkerAuthStatus(invoking, activation, posture, now.Add(time.Second))
	if got.Secure || got.RuntimeConfigVerified ||
		!containsString(got.Holds, "running_service_auth_posture_stale_or_mismatched") {
		t.Fatalf("different active secret false-greened: %+v", got)
	}
	if workerAuthSecretKeyID("running-secret") == "running-secret" ||
		workerAuthSecretKeyID("running-secret") == workerAuthSecretKeyID("different-shell-secret") {
		t.Fatal("worker auth secret key id exposed or failed to distinguish keys")
	}
}

func TestProjectWorkerAuthStatusRejectsWrongReviewerRoleFamilyAndShellOnlyGreen(t *testing.T) {
	t.Setenv("FLOWBEE_INSECURE", "")
	t.Setenv("FLOWBEE_CAPACITY_COLLECTOR_ID", "capacity-local")
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	activation := store.ProjectActivationStatus{Reviewers: []store.ProjectReviewerActivation{{
		WorkerIdentity: "reviewer-russ", ModelFamily: "grok", FamilyCapacityConfigured: true,
	}}}
	cfg := config.Config{WorkerAuthSecret: "secret", EnrolledIdentities: []string{
		"reviewer-russ:claude", "capacity-local",
	}, WorkerAttestations: map[string][]string{
		"reviewer-russ": {"role:eng_worker"}, "capacity-local": {},
	}}
	got := projectWorkerAuthStatus(cfg, activation, nil, now)
	if got.Secure || len(got.MissingAttestations) != 2 ||
		!containsString(got.Holds, "running_service_auth_posture_missing") {
		t.Fatalf("wrong-role/family or invoking-shell-only posture false-greened: %+v", got)
	}
}

func TestProjectAndSeatOfflineMutationsRespectServeWriterLock(t *testing.T) {
	st := testutil.NewStore(t)
	if err := st.AcquireWriterLock(); err != nil {
		t.Fatal(err)
	}
	var dbPath string
	if err := st.DB.QueryRow(`PRAGMA database_list`).Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLOWBEE_DATABASE_URL", dbPath)
	if err := runProject([]string{"add", "--id", "held", "--name", "Held"}); err == nil {
		t.Fatal("project mutation raced the active writer")
	}
	if err := runSeat([]string{"set-max-concurrent", "--family", "codex", "--codex-home", "/missing", "2"}); err == nil {
		t.Fatal("seat mutation raced the active writer")
	}
	if err := runProject([]string{"list"}); err != nil {
		t.Fatalf("read-only project list should remain available: %v", err)
	}
	if err := runSeat([]string{"list"}); err != nil {
		t.Fatalf("read-only seat list should remain available: %v", err)
	}
}

func TestBindObservedProjectSessionRejectsActorRouteMismatchAndAbsentLifecycle(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "mail",
		Role: store.DriverOrchestratorRole, ActorID: "expected"}, now); err != nil {
		t.Fatal(err)
	}
	fake := driver.NewFake()
	fake.Meta = driver.DriverMetadata{HostID: "host", StoreID: "store", ProducerBootID: "boot",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.1",
		TmuxServer: driver.TmuxServerMetadata{DomainID: "flowbee", InstanceID: "server",
			Ownership: "managed_dedicated", ConnectionVisibility: "isolated_socket"}}
	in := projectSessionBindingInput{ProjectID: "mail", Role: store.DriverOrchestratorRole,
		WorkerIdentity: "wrong", LifecycleKey: "orchestrator:mail", TargetEpoch: 1,
		ProfileID: "grok", WorkspaceRootID: "projects", WorkspaceRelativePath: "mail"}
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now); err == nil {
		t.Fatal("actor route mismatch was accepted")
	}
	in.WorkerIdentity = "expected"
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now); err == nil {
		t.Fatal("absent lifecycle target was accepted")
	}
	var bindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings`).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatalf("bindings=%d err=%v", bindings, err)
	}
}

func TestBindObservedProjectSessionRejectsExternalActorWithoutAdoptReceipt(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "russ",
		Role: store.DriverInteractorRole, ActorID: "russ-claude"}, now); err != nil {
		t.Fatal(err)
	}
	id := driver.Identity{HostID: "host-russ", StoreID: "store-russ", TmuxServerDomainID: "default", TmuxServerInstanceID: "server-russ",
		Ownership: "external_observed",
		SessionID: "session-russ", PaneInstanceID: "pane-russ", AgentRunID: "run-russ", Provider: "claude"}
	fake := driver.NewFake()
	fake.Meta = driver.DriverMetadata{HostID: id.HostID, StoreID: id.StoreID, ProducerBootID: "boot",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.2",
		TmuxServer: driver.TmuxServerMetadata{DomainID: id.TmuxServerDomainID, InstanceID: id.TmuxServerInstanceID,
			Ownership: "external", ConnectionVisibility: "default_or_external"}}
	fake.Snapshot = driver.SessionSnapshot{HostID: id.HostID, StoreID: id.StoreID, AsOfCursor: "tdc2.2",
		Sessions: []driver.SessionProjection{{Identity: id, Lifecycle: "active", AsOfCursor: "tdc2.2"}}}
	in := projectSessionBindingInput{ProjectID: "russ", Role: store.DriverInteractorRole,
		WorkerIdentity: "russ-claude", HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerDomainID: id.TmuxServerDomainID, TmuxServerInstanceID: id.TmuxServerInstanceID, SessionID: id.SessionID,
		PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID}
	// External actors must enter through the durable Watch -> Adopt lifecycle
	// outbox. A raw observed tuple has no external_watch_id or adopted receipt and
	// therefore cannot become routing authority through this legacy direct-bind
	// seam.
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now); err == nil {
		t.Fatal("external actor without an adopted lifecycle receipt was accepted")
	}
	var bindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("external direct-bind failure persisted %d binding(s)", bindings)
	}
}

func TestBindObservedProjectSessionRejectsCrossDomainTupleBeforePersistence(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "domain-fence", Name: "Domain fence"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "domain-fence",
		Role: store.DriverInteractorRole, ActorID: "external-interactor"}, now); err != nil {
		t.Fatal(err)
	}
	id := driver.Identity{HostID: "host-domain", StoreID: "store-domain", TmuxServerDomainID: "default",
		TmuxServerInstanceID: "server-domain", SessionID: "session-domain", PaneInstanceID: "pane-domain",
		AgentRunID: "run-domain", Provider: "claude"}
	fake := driver.NewFake()
	fake.Meta = driver.DriverMetadata{HostID: id.HostID, StoreID: id.StoreID, ProducerBootID: "boot",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.2",
		TmuxServer: driver.TmuxServerMetadata{DomainID: "default", InstanceID: id.TmuxServerInstanceID,
			Ownership: "external", ConnectionVisibility: "default_or_external"}}
	fake.Snapshot = driver.SessionSnapshot{HostID: id.HostID, StoreID: id.StoreID, AsOfCursor: "tdc2.2",
		Sessions: []driver.SessionProjection{{Identity: id, Lifecycle: "active", AsOfCursor: "tdc2.2"}}}
	in := projectSessionBindingInput{ProjectID: "domain-fence", Role: store.DriverInteractorRole,
		WorkerIdentity: "external-interactor", HostID: id.HostID, StoreID: id.StoreID,
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: id.TmuxServerInstanceID,
		SessionID: id.SessionID, PaneInstanceID: id.PaneInstanceID, AgentRunID: id.AgentRunID}
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now); err == nil {
		t.Fatal("external/default session was accepted under managed domain")
	}
	var bindings int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings`).Scan(&bindings); err != nil {
		t.Fatal(err)
	}
	if bindings != 0 {
		t.Fatalf("cross-domain failure persisted %d bindings", bindings)
	}
}

func TestBindObservedProjectReviewerRequiresExactCapacitySeat(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail-review", Name: "Mail Review"}, now); err != nil {
		t.Fatal(err)
	}
	seat := store.Seat{Box: "review-host", AgentFamily: "grok", ConfigDir: "/grok/reviewer"}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: seat.ID,
		HostID: "review-host", AccountKey: "grok-account", CredentialLineage: "grok-lineage"}, now); err != nil {
		t.Fatal(err)
	}
	id := driver.Identity{HostID: "review-host", StoreID: "review-store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "review-server", SessionID: "review-session",
		PaneInstanceID: "review-pane", AgentRunID: "review-run", Provider: "grok",
		Ownership: "driver_managed", LifecycleKey: "actor:mail-review:reviewer", TargetEpoch: 1}
	fake := driver.NewFake()
	fake.Meta = driver.DriverMetadata{HostID: id.HostID, StoreID: id.StoreID, ProducerBootID: "boot",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.2",
		TmuxServer: driver.TmuxServerMetadata{DomainID: id.TmuxServerDomainID, InstanceID: id.TmuxServerInstanceID,
			Ownership: "managed_dedicated", ConnectionVisibility: "isolated_socket"}}
	fake.Snapshot = driver.SessionSnapshot{HostID: id.HostID, StoreID: id.StoreID, AsOfCursor: "tdc2.2",
		Sessions: []driver.SessionProjection{{Identity: id, Lifecycle: "active", AsOfCursor: "tdc2.2"}}}
	fake.Sessions[id.SessionID] = id
	in := projectSessionBindingInput{ProjectID: "mail-review", Role: store.DriverReviewerRole,
		WorkerIdentity: "reviewer-mail", LifecycleKey: id.LifecycleKey, TargetEpoch: id.TargetEpoch,
		ProfileID: "grok-reviewer", WorkspaceRootID: "projects", WorkspaceRelativePath: "mail-review"}
	if _, err := bindObservedProjectSession(ctx, st, fake, in, now); err == nil {
		t.Fatal("reviewer onboarding accepted no capacity seat")
	}
	in.SeatID = seat.ID
	binding, err := bindObservedProjectSession(ctx, st, fake, in, now)
	if err != nil {
		t.Fatal(err)
	}
	if binding.SeatID != seat.ID || binding.Provider != "grok" || binding.HostID != "review-host" {
		t.Fatalf("reviewer binding=%+v", binding)
	}
}

func TestProjectActorLifecycleCLICommitsIntentAndRejectsRawPaneSelector(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "russ", Name: "Russ"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "russ",
		Role: store.DriverInteractorRole, ActorID: "russ-claude"}, now); err != nil {
		t.Fatal(err)
	}
	base := []string{"--project-id", "russ", "--role", "interactor", "--actor-id", "russ-claude",
		"--operation", "adopt", "--idempotency-key", "adopt-russ-claude", "--instance-ref", "external",
		"--host-id", "mac", "--store-id", "external-store", "--tmux-server-domain-id", "default",
		"--tmux-server-instance-id", "server-external", "--lifecycle-key", "russ-claude",
		"--target-epoch", "1", "--profile-id", "claude-interactor", "--external-watch-id", "watch-russ-claude",
		"--session-id", "session-russ-claude", "--agent-run-id", "run-russ-claude"}
	if err := runProjectActorLifecycle(ctx, st, append(base, "--pane-instance-id", "%1")); err == nil {
		t.Fatal("actor lifecycle CLI accepted raw tmux pane selector")
	}
	if err := runProjectActorLifecycle(ctx, st, append(base, "--pane-instance-id", "pane-instance-russ-claude")); err != nil {
		t.Fatal(err)
	}
	lifecycle, err := st.CurrentProjectActorLifecycle(ctx, "russ", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.State != "awaiting_adopt" || lifecycle.CurrentActionID == "" || lifecycle.ExternalWatchID != "watch-russ-claude" {
		t.Fatalf("CLI did not commit durable adopt intent: %+v", lifecycle)
	}
}

func TestProjectBindSessionDisablesActorRoles(t *testing.T) {
	st := testutil.NewStore(t)
	err := runProjectBindSession(context.Background(), st, []string{
		"--project-id", "russ", "--role", "interactor", "--worker-identity", "russ-claude",
	})
	if err == nil {
		t.Fatal("direct actor bind-session remained enabled")
	}
}
