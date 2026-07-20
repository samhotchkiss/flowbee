package bootstrap

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type conflictOnceCheckpointStore struct {
	*MemoryCheckpointStore
	conflicted bool
}

func (s *conflictOnceCheckpointStore) CompareAndSwap(ctx context.Context, cp Checkpoint,
	expected int64) (Checkpoint, error) {
	if !s.conflicted {
		s.conflicted = true
		current, ok, err := s.Load(ctx, cp.BootstrapID)
		if err != nil || !ok {
			return Checkpoint{}, err
		}
		// Model the competing invocation committing this exact monotonic step.
		if _, err := s.MemoryCheckpointStore.CompareAndSwap(ctx, cp, current.Version); err != nil {
			return Checkpoint{}, err
		}
		return Checkpoint{}, ErrCheckpointConflict
	}
	return s.MemoryCheckpointStore.CompareAndSwap(ctx, cp, expected)
}

func bootstrapPlan() Plan {
	external := EndpointRef{Purpose: EndpointExternalInteractor, HostID: "local",
		StoreID: "external-store", TmuxServerDomainID: ExternalTmuxServerDomain,
		InstanceRef: "external", EnsureMechanism: DriverEnsureManagedLaunchdTD,
		ServiceManagerPath: "/opt/tmux-driver-service", ServiceManagerSHA256: "sha256:" + strings.Repeat("d", 64),
		ReleaseID: "driver-1", ExecutablePath: "/opt/driver", ExecutableSHA256: "sha256:" + strings.Repeat("a", 64),
		ConfigPath: "/etc/driver-external.toml", ConfigSHA256: "sha256:" + strings.Repeat("b", 64), UDSPath: "/tmp/external.sock",
		RequiredContracts: map[string]string{"lifecycle_ensure": "tmux-driver.lifecycle-ensure/v3"}}
	managed := EndpointRef{Purpose: EndpointManagedFleet, HostID: "local",
		StoreID: "managed-store", TmuxServerDomainID: ManagedTmuxServerDomain,
		InstanceRef: "managed", EnsureMechanism: DriverEnsureManagedLaunchdTD,
		ServiceManagerPath: "/opt/tmux-driver-service", ServiceManagerSHA256: "sha256:" + strings.Repeat("d", 64),
		ReleaseID: "driver-1", ExecutablePath: "/opt/driver", ExecutableSHA256: "sha256:" + strings.Repeat("a", 64),
		ConfigPath: "/etc/driver-managed.toml", ConfigSHA256: "sha256:" + strings.Repeat("c", 64), UDSPath: "/tmp/managed.sock",
		RequiredContracts: map[string]string{"lifecycle_ensure": "tmux-driver.lifecycle-ensure/v3"}}
	return Plan{ID: "bootstrap-russ", ProjectID: "russ", Endpoints: []EndpointRef{managed, external},
		ControlPlane: ControlPlaneSpec{ID: "flowbee"},
		Actors: []ActorSpec{
			{ID: "russ-orchestrator", Role: "orchestrator", Operation: ActorEnsure, Endpoint: managed,
				PresentationName: "russ-orchestrator"},
			{ID: "russ-claude", Role: "interactor", Operation: ActorAdopt, Endpoint: external,
				ExistingSessionID: "session-russ-claude", PresentationName: "russ-interactor"},
		},
		Groups: []GroupSpec{{ID: "flowbee-russ", TmuxServerDomainID: ManagedTmuxServerDomain,
			ActorIDs: []string{"russ-orchestrator"}, MemberClasses: []string{
				"control_plane", "project_orchestrator", "dynamic_worker", "driver_console"}}},
		LocalSeats: []SeatSpec{{ID: "codex1", Endpoint: managed}},
		AttachIntent: AttachIntentSpec{ID: "intent-russ", InteractorActorID: "russ-claude",
			TmuxServerDomainID: ExternalTmuxServerDomain, PresentationName: "russ-interactor"}}
}

type deadlineDriver struct{}

func (deadlineDriver) EndpointReady(context.Context, EndpointRef) (bool, error) { return false, nil }
func (deadlineDriver) EnsureEndpoint(ctx context.Context, _ EndpointRef, _ EffectRequest) (EffectReceipt, error) {
	<-ctx.Done()
	return EffectReceipt{}, ctx.Err()
}

func TestBootstrapDeadlineLeavesPreparedActionResumable(t *testing.T) {
	store := NewMemoryCheckpointStore()
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	o := newBootstrap(fake, store)
	o.Ports.Driver = deadlineDriver{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := o.Run(ctx, bootstrapPlan()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline Run error=%v", err)
	}
	cp, ok, err := store.Load(context.Background(), "bootstrap-russ")
	if err != nil || !ok || len(cp.Prepared) != 1 || len(cp.Issued) != 0 {
		t.Fatalf("durable interrupted checkpoint=%+v ok=%v err=%v", cp, ok, err)
	}
	prepared := ""
	for _, actionID := range cp.Prepared {
		prepared = actionID
	}
	fake.BeforeEffect = func(kind, _ string, req EffectRequest) error {
		if kind == "endpoint" && req.ActionID == prepared {
			return nil
		}
		return nil
	}
	o.Ports.Driver = fake
	result, err := o.Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("resumed Run=%+v err=%v", result, err)
	}
	if _, exists := fake.Receipts[prepared]; !exists {
		t.Fatalf("prepared action %q was not reused: receipts=%v", prepared, fake.Receipts)
	}
}

func TestConcurrentBootstrapCASConflictReloadsInsteadOfFailing(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	store := &conflictOnceCheckpointStore{MemoryCheckpointStore: NewMemoryCheckpointStore()}
	o := newBootstrap(fake, store)
	result, err := o.Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("Run() after concurrent CAS = %+v, %v", result, err)
	}
	if !store.conflicted {
		t.Fatal("test did not inject a concurrent checkpoint advance")
	}
	for key, count := range fake.MutationCount {
		if count != 1 {
			t.Fatalf("mutation %q executed %d times after CAS recovery", key, count)
		}
	}
}

func newBootstrap(fake *FakePort, store CheckpointStore) Orchestrator {
	return Orchestrator{Store: store, Ports: Ports{Driver: fake, Control: fake, Activation: fake,
		Init: fake, Projects: fake, Actors: fake, Groups: fake, Seats: fake, Interactor: fake}}
}

func TestCleanBootstrapOrdersProjectProvisionBeforeActorMutations(t *testing.T) {
	ctx := context.Background()
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	o := newBootstrap(fake, NewMemoryCheckpointStore())
	result, err := o.Run(ctx, bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
	want := []string{
		"endpoint:external_interactor:local:external-store:default",
		"endpoint:managed_fleet:local:managed-store:flowbee",
		"control:flowbee",
		"project:create",
		"project_repo:attach",
		"actor:russ-orchestrator",
		"actor:russ-claude",
		"group:flowbee-russ",
		"seat:codex1",
		"attach:intent-russ",
	}
	if !reflect.DeepEqual(fake.Order, want) {
		t.Fatalf("mutation order\n got: %#v\nwant: %#v", fake.Order, want)
	}
	firstEffect := -1
	probedExternal, probedManaged := false, false
	for i, entry := range fake.Trace {
		if strings.HasPrefix(entry, "probe:endpoint:external_interactor:") {
			probedExternal = true
		}
		if strings.HasPrefix(entry, "probe:endpoint:managed_fleet:") {
			probedManaged = true
		}
		if strings.HasPrefix(entry, "effect:") {
			firstEffect = i
			break
		}
	}
	if firstEffect < 0 || !probedExternal || !probedManaged {
		t.Fatalf("both exact endpoint facts were not probed before first mutation: %#v", fake.Trace)
	}
	beforeActors := strings.Join(fake.Order[:5], "|")
	if !strings.Contains(beforeActors, "project:create") || !strings.Contains(beforeActors, "project_repo:attach") {
		t.Fatal("clean-state project add and repo attach did not precede actors")
	}

	counts := cloneIntMap(fake.MutationCount)
	result, err = o.Run(ctx, bootstrapPlan())
	if err != nil || !result.Complete || !reflect.DeepEqual(fake.MutationCount, counts) {
		t.Fatalf("idempotent resume = %+v, %v, mutations=%v", result, err, fake.MutationCount)
	}
}

func TestLostAcknowledgementDoesNotRecreateActor(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	fake.SetLostAck("actor", "russ-claude")
	store := NewMemoryCheckpointStore()
	if _, err := newBootstrap(fake, store).Run(context.Background(), bootstrapPlan()); err == nil {
		t.Fatal("expected simulated lost acknowledgement")
	}
	result, err := newBootstrap(fake, store).Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("restart Run() = %+v, %v", result, err)
	}
	if got := fake.MutationCount["actor:russ-claude"]; got != 1 {
		t.Fatalf("adopt mutations = %d, want 1", got)
	}
}

func TestLostAcknowledgementDoesNotDuplicateProjectCreation(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	fake.SetLostAck("project", "create")
	store := NewMemoryCheckpointStore()
	if _, err := newBootstrap(fake, store).Run(context.Background(), bootstrapPlan()); err == nil {
		t.Fatal("expected simulated lost acknowledgement")
	}
	result, err := newBootstrap(fake, store).Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("restart Run() = %+v, %v", result, err)
	}
	if got := fake.MutationCount["project:create"]; got != 1 {
		t.Fatalf("project create mutations = %d, want 1", got)
	}
}

func TestBootstrapPersistsImmutableIntentBeforeEveryEffect(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryCheckpointStore()
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	fake.BeforeEffect = func(kind, id string, req EffectRequest) error {
		cp, ok, err := store.Load(ctx, "bootstrap-russ")
		if err != nil || !ok {
			return errors.New("checkpoint missing before effect")
		}
		key := kind + ":" + id
		switch kind {
		case "endpoint":
			key = "driver_endpoint:" + id
		case "control":
			key = "control_plane:" + id
		}
		if cp.Prepared[key] != req.ActionID {
			return errors.New("effect ran before immutable action intent was durable")
		}
		return nil
	}
	result, err := newBootstrap(fake, store).Run(ctx, bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("Run() = %+v, %v", result, err)
	}
}

func TestIssuedEffectWaitsForMechanicalReadinessWithoutResend(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	fake.DelayReady["control:flowbee"] = true
	store := NewMemoryCheckpointStore()
	o := newBootstrap(fake, store)
	result, err := o.Run(context.Background(), bootstrapPlan())
	if err != nil || result.Hold != "control_plane:flowbee:awaiting_live_fact" {
		t.Fatalf("delayed readiness Run() = %+v, %v", result, err)
	}
	result, err = o.Run(context.Background(), bootstrapPlan())
	if err != nil || fake.MutationCount["control:flowbee"] != 1 {
		t.Fatalf("waiting resume = %+v, %v mutations=%v", result, err, fake.MutationCount)
	}
	fake.ControlReady = true
	result, err = o.Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("ready resume = %+v, %v", result, err)
	}
}

type serviceRecoveryDriver struct {
	ready          map[string]bool
	calls          map[string]int
	receipts       map[string]string
	uncertainReady bool
}

func (d *serviceRecoveryDriver) EndpointReady(_ context.Context, endpoint EndpointRef) (bool, error) {
	return d.ready[endpoint.key()], nil
}

func (d *serviceRecoveryDriver) EnsureEndpoint(_ context.Context, endpoint EndpointRef, req EffectRequest) (EffectReceipt, error) {
	d.calls[endpoint.key()]++
	receipt := d.receipts[req.ActionID]
	if receipt == "" {
		receipt = "receipt:" + req.ActionID
		d.receipts[req.ActionID] = receipt
		return EffectReceipt{ID: receipt, State: "accepted"}, nil
	}
	if endpoint.Purpose == EndpointExternalInteractor || d.uncertainReady {
		d.ready[endpoint.key()] = true
		return EffectReceipt{ID: receipt, State: "ready"}, nil
	}
	return EffectReceipt{ID: receipt, State: "uncertain"}, nil
}

func TestAcceptedServiceCrashReconcilesSameActionOnRestart(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	driver := &serviceRecoveryDriver{ready: map[string]bool{}, calls: map[string]int{}, receipts: map[string]string{}}
	store := NewMemoryCheckpointStore()
	o := newBootstrap(fake, store)
	o.Ports.Driver = driver
	result, err := o.Run(context.Background(), bootstrapPlan())
	if err != nil || result.Hold != "driver_endpoints:awaiting_both_live_facts" {
		t.Fatalf("accepted run = %+v, %v", result, err)
	}
	for _, endpoint := range bootstrapPlan().Endpoints {
		if driver.calls[endpoint.key()] != 1 {
			t.Fatalf("first-pass calls[%s]=%d", endpoint.key(), driver.calls[endpoint.key()])
		}
	}
	result, err = o.Run(context.Background(), bootstrapPlan())
	if err != nil || result.Hold != "driver_endpoints:awaiting_both_live_facts" {
		t.Fatalf("restart reconcile = %+v, %v", result, err)
	}
	if driver.calls[bootstrapPlan().Endpoints[0].key()] != 2 || driver.calls[bootstrapPlan().Endpoints[1].key()] != 2 {
		t.Fatalf("same-action reconciliation calls = %v", driver.calls)
	}
	driver.uncertainReady = true
	result, err = o.Run(context.Background(), bootstrapPlan())
	if err != nil || !result.Complete {
		t.Fatalf("mechanically ready resume = %+v, %v", result, err)
	}
	if len(driver.receipts) != 2 {
		t.Fatalf("new actions minted during service reconciliation: %v", driver.receipts)
	}
}

func TestCompletedBootstrapRevalidatesExactFactsOnEveryRun(t *testing.T) {
	fake := NewFakePort("russ")
	fake.AutoLiveActivation = true
	o := newBootstrap(fake, NewMemoryCheckpointStore())
	if result, err := o.Run(context.Background(), bootstrapPlan()); err != nil || !result.Complete {
		t.Fatalf("initial Run() = %+v, %v", result, err)
	}
	fake.ActorFacts["russ-claude"] = false
	result, err := o.Run(context.Background(), bootstrapPlan())
	if err != nil || result.Complete || result.Hold != "completed_bootstrap_actor_not_ready:russ-claude" {
		t.Fatalf("revalidation Run() = %+v, %v", result, err)
	}
	if fake.MutationCount["actor:russ-claude"] != 1 {
		t.Fatal("revalidation blindly repeated a lifecycle mutation")
	}
}

func TestBootstrapFailsClosedBeforeDownstreamMutation(t *testing.T) {
	t.Run("driver endpoint", func(t *testing.T) {
		fake := NewFakePort("russ")
		plan := bootstrapPlan()
		key := "endpoint:" + sortedPlan(plan).Endpoints[0].key()
		fake.FailBeforeOnce[key] = errors.New("driver unavailable")
		_, err := newBootstrap(fake, NewMemoryCheckpointStore()).Run(context.Background(), plan)
		if err == nil || len(fake.Order) != 0 {
			t.Fatalf("Run error=%v mutations=%v", err, fake.Order)
		}
	})
	t.Run("exact activation", func(t *testing.T) {
		fake := NewFakePort("russ")
		fake.Activation.ProjectID = "different"
		result, err := newBootstrap(fake, NewMemoryCheckpointStore()).Run(context.Background(), bootstrapPlan())
		if err != nil || result.Hold != "exact_project_mismatch" {
			t.Fatalf("Run() = %+v, %v", result, err)
		}
		for _, mutation := range fake.Order {
			if strings.HasPrefix(mutation, "actor:") || strings.HasPrefix(mutation, "seat:") ||
				strings.HasPrefix(mutation, "attach:") {
				t.Fatalf("downstream mutation escaped activation fence: %v", fake.Order)
			}
		}
	})
	t.Run("Interactor create held", func(t *testing.T) {
		fake := NewFakePort("russ")
		plan := bootstrapPlan()
		for i := range plan.Actors {
			if plan.Actors[i].Role == "interactor" {
				plan.Actors[i].Operation = ActorEnsure
			}
		}
		if _, err := newBootstrap(fake, NewMemoryCheckpointStore()).Run(context.Background(), plan); err == nil {
			t.Fatal("Interactor creation was not rejected")
		}
		if len(fake.Order) != 0 {
			t.Fatalf("validation failure mutated state: %v", fake.Order)
		}
	})
	t.Run("not live after bindings", func(t *testing.T) {
		fake := NewFakePort("russ")
		result, err := newBootstrap(fake, NewMemoryCheckpointStore()).Run(context.Background(), bootstrapPlan())
		if err != nil || !strings.HasPrefix(result.Hold, "project_not_live_ready:") {
			t.Fatalf("Run() = %+v, %v", result, err)
		}
		if fake.MutationCount["attach:intent-russ"] != 0 {
			t.Fatal("attach escaped final exact LiveReady gate")
		}
	})
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
