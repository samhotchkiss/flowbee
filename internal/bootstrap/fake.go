package bootstrap

import (
	"context"
	"errors"
	"sync"
)

// MemoryCheckpointStore is a deterministic fake persistence boundary. It is not
// a production database adapter; tests can reuse it across Orchestrator values to
// model process restart and lost acknowledgements.
type MemoryCheckpointStore struct {
	mu   sync.Mutex
	rows map[string]Checkpoint
}

func NewMemoryCheckpointStore() *MemoryCheckpointStore {
	return &MemoryCheckpointStore{rows: map[string]Checkpoint{}}
}

func (s *MemoryCheckpointStore) Load(_ context.Context, id string) (Checkpoint, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp, ok := s.rows[id]
	return cloneCheckpoint(cp), ok, nil
}

func (s *MemoryCheckpointStore) Create(_ context.Context, cp Checkpoint) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[cp.BootstrapID]; ok {
		return Checkpoint{}, ErrCheckpointConflict
	}
	cp.Version = 1
	cp = cloneCheckpoint(cp)
	s.rows[cp.BootstrapID] = cp
	return cloneCheckpoint(cp), nil
}

func (s *MemoryCheckpointStore) CompareAndSwap(_ context.Context, cp Checkpoint,
	expected int64) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.rows[cp.BootstrapID]
	if !ok || current.Version != expected || current.PlanSHA256 != cp.PlanSHA256 ||
		current.ProjectID != cp.ProjectID || current.Done && !cp.Done ||
		current.CWD != "" && (current.CWD != cp.CWD || current.RepositoryOrigin != cp.RepositoryOrigin) ||
		!mapExtends(cp.Prepared, current.Prepared) || !mapExtends(cp.Issued, current.Issued) ||
		!mapExtends(cp.Completed, current.Completed) {
		return Checkpoint{}, ErrCheckpointConflict
	}
	cp.Version = expected + 1
	cp = cloneCheckpoint(cp)
	s.rows[cp.BootstrapID] = cp
	return cloneCheckpoint(cp), nil
}

func mapExtends(next, prior map[string]string) bool {
	for key, value := range prior {
		if next[key] != value {
			return false
		}
	}
	return true
}

// FakePort implements every environmental boundary with exact identities and an
// idempotency ledger. FailAfterEffectOnce simulates the dangerous crash/lost-ack
// interval: the effect is durable and visible to its fact probe, but the caller
// receives an error before checkpointing the receipt.
type FakePort struct {
	mu sync.Mutex

	Activation         ProjectActivation
	AutoLiveActivation bool
	Init               ProjectInit

	EndpointFacts          map[string]bool
	ActorFacts             map[string]bool
	GroupFacts             map[string]bool
	SeatFacts              map[string]bool
	AttachFacts            map[string]bool
	ProjectExistsFact      bool
	RepositoryAttachedFact bool
	ControlReady           bool
	DelayReady             map[string]bool

	Receipts            map[string]EffectReceipt
	MutationCount       map[string]int
	CallCount           map[string]int
	FailBeforeOnce      map[string]error
	FailAfterEffectOnce map[string]error
	Order               []string
	Trace               []string
	BeforeEffect        func(string, string, EffectRequest) error
}

func NewFakePort(projectID string) *FakePort {
	return &FakePort{Activation: ProjectActivation{ProjectID: projectID,
		BootstrapAllowed: true}, Init: ProjectInit{ProjectID: projectID,
		RepositoryOrigin: "example/" + projectID, CWD: "/workspace/" + projectID},
		EndpointFacts: map[string]bool{}, ActorFacts: map[string]bool{},
		GroupFacts: map[string]bool{}, SeatFacts: map[string]bool{}, AttachFacts: map[string]bool{},
		Receipts: map[string]EffectReceipt{}, MutationCount: map[string]int{},
		CallCount: map[string]int{}, FailBeforeOnce: map[string]error{},
		FailAfterEffectOnce: map[string]error{}, DelayReady: map[string]bool{}}
}

func (f *FakePort) fact(kind, id string, values map[string]bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["probe:"+kind+":"+id]++
	f.Trace = append(f.Trace, "probe:"+kind+":"+id)
	return values[id], nil
}

func (f *FakePort) effect(kind, id string, req EffectRequest, apply func()) (EffectReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := kind + ":" + id
	f.CallCount[key]++
	if err := f.FailBeforeOnce[key]; err != nil {
		delete(f.FailBeforeOnce, key)
		return EffectReceipt{}, err
	}
	if receipt, ok := f.Receipts[req.ActionID]; ok {
		return receipt, nil
	}
	if f.BeforeEffect != nil {
		if err := f.BeforeEffect(kind, id, req); err != nil {
			return EffectReceipt{}, err
		}
	}
	receipt := EffectReceipt{ID: "receipt:" + req.ActionID}
	f.Receipts[req.ActionID] = receipt
	f.MutationCount[key]++
	f.Order = append(f.Order, key)
	f.Trace = append(f.Trace, "effect:"+key)
	if !f.DelayReady[key] {
		apply()
	}
	if err := f.FailAfterEffectOnce[key]; err != nil {
		delete(f.FailAfterEffectOnce, key)
		return EffectReceipt{}, err
	}
	return receipt, nil
}

func (f *FakePort) EndpointReady(_ context.Context, e EndpointRef) (bool, error) {
	return f.fact("endpoint", e.key(), f.EndpointFacts)
}
func (f *FakePort) EnsureEndpoint(_ context.Context, e EndpointRef, req EffectRequest) (EffectReceipt, error) {
	return f.effect("endpoint", e.key(), req, func() { f.EndpointFacts[e.key()] = true })
}
func (f *FakePort) LiveReady(context.Context, ControlPlaneSpec) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["probe:control"]++
	f.Trace = append(f.Trace, "probe:control")
	return f.ControlReady, nil
}
func (f *FakePort) Ensure(_ context.Context, _ ControlPlaneSpec, req EffectRequest) (EffectReceipt, error) {
	return f.effect("control", "flowbee", req, func() { f.ControlReady = true })
}
func (f *FakePort) ExactProjectActivation(_ context.Context, projectID string) (ProjectActivation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["probe:activation"]++
	f.Trace = append(f.Trace, "probe:activation")
	out := f.Activation
	if f.AutoLiveActivation && len(f.ActorFacts) >= 2 && len(f.SeatFacts) > 0 {
		out.LiveReady = true
	}
	return out, nil
}
func (f *FakePort) ResolveProjectInit(_ context.Context, _ string) (ProjectInit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["resolve:project"]++
	f.Trace = append(f.Trace, "resolve:project")
	return f.Init, nil
}
func (f *FakePort) ProjectExists(_ context.Context, _ ProjectInit) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["probe:project:create"]++
	f.Trace = append(f.Trace, "probe:project:create")
	return f.ProjectExistsFact, nil
}
func (f *FakePort) AddProject(_ context.Context, _ ProjectInit, req EffectRequest) (EffectReceipt, error) {
	return f.effect("project", "create", req, func() {
		f.ProjectExistsFact = true
		f.Activation.Exists = true
	})
}
func (f *FakePort) RepositoryAttached(_ context.Context, _ ProjectInit) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CallCount["probe:project_repo:attach"]++
	f.Trace = append(f.Trace, "probe:project_repo:attach")
	return f.RepositoryAttachedFact, nil
}
func (f *FakePort) AttachRepository(_ context.Context, _ ProjectInit, req EffectRequest) (EffectReceipt, error) {
	return f.effect("project_repo", "attach", req, func() { f.RepositoryAttachedFact = true })
}
func (f *FakePort) ActorReady(_ context.Context, a ActorSpec) (bool, error) {
	return f.fact("actor", a.ID, f.ActorFacts)
}
func (f *FakePort) EnsureOrAdoptActor(_ context.Context, a ActorSpec, req EffectRequest) (EffectReceipt, error) {
	return f.effect("actor", a.ID, req, func() { f.ActorFacts[a.ID] = true })
}
func (f *FakePort) GroupReady(_ context.Context, g GroupSpec) (bool, error) {
	return f.fact("group", g.ID, f.GroupFacts)
}
func (f *FakePort) EnsureGroup(_ context.Context, g GroupSpec, req EffectRequest) (EffectReceipt, error) {
	return f.effect("group", g.ID, req, func() { f.GroupFacts[g.ID] = true })
}
func (f *FakePort) SeatBound(_ context.Context, s SeatSpec) (bool, error) {
	return f.fact("seat", s.ID, f.SeatFacts)
}
func (f *FakePort) BindLocalSeat(_ context.Context, s SeatSpec, req EffectRequest) (EffectReceipt, error) {
	return f.effect("seat", s.ID, req, func() { f.SeatFacts[s.ID] = true })
}
func (f *FakePort) IntentAttached(_ context.Context, a AttachIntentSpec) (bool, error) {
	return f.fact("attach", a.ID, f.AttachFacts)
}
func (f *FakePort) AttachIntent(_ context.Context, a AttachIntentSpec, req EffectRequest) (EffectReceipt, error) {
	return f.effect("attach", a.ID, req, func() { f.AttachFacts[a.ID] = true })
}

func (f *FakePort) SetLostAck(kind, id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.FailAfterEffectOnce[kind+":"+id] = errors.New("simulated lost acknowledgement")
}
