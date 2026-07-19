package driver

import (
	"context"
	"errors"
	"sync"
)

// FakePort is deterministic and intentionally strict: forbidden or stale sends
// produce zero mutations, and a receipt never implies workflow completion.
type FakePort struct {
	mu                  sync.Mutex
	Meta                DriverMetadata
	Capability          ControlOriginCapability
	Snapshot            SessionSnapshot
	Grants              map[string]Grant
	Sessions            map[string]Identity
	Receipts            map[string]Receipt
	LifecycleReceipts   map[string]LifecycleReceipt
	Observations        []Observation
	Batches             []ObservationBatch
	ObserveCalls        []string
	SendRequests        []SendRequest
	SendCalls           int
	EnsureCalls         int
	StopCalls           int
	NextStatus          ReceiptStatus
	NextLifecycleStatus string
	NextError           error
}

func NewFake() *FakePort {
	return &FakePort{Capability: ControlOriginCapability{
		FormatVersion: controlOriginCapabilityFormat, Supported: true, Authorized: true,
		PrincipalID: "flowbee-control", PrincipalKind: "control_plane",
		RequiredScopes:   []string{"messages:send", "routes:manage"},
		GrantedScopes:    []string{"messages:send", "routes:manage"},
		RouteGrantFormat: controlRouteGrantFormat, DeliveryReceiptFormat: controlDeliveryReceiptFormat,
		GrantEndpoint: "/v2/routes/grants", MessageEndpoint: "/v2/messages",
		OnBehalfOfSessionIDRule: "forbidden",
	}, Grants: map[string]Grant{}, Sessions: map[string]Identity{}, Receipts: map[string]Receipt{}, LifecycleReceipts: map[string]LifecycleReceipt{}}
}

func (f *FakePort) ControlOriginCapability(_ context.Context) (ControlOriginCapability, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.Meta.ControlPrincipalOrigin {
		return ControlOriginCapability{}, errors.New("driver control origin capability: meta feature is not enabled")
	}
	if err := validateControlOriginCapability(f.Capability); err != nil {
		return ControlOriginCapability{}, err
	}
	return f.Capability, f.NextError
}

func (f *FakePort) Metadata(_ context.Context) (DriverMetadata, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Meta, f.NextError
}

func (f *FakePort) SnapshotSessions(_ context.Context) (SessionSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snapshot := f.Snapshot
	snapshot.Sessions = append([]SessionProjection(nil), snapshot.Sessions...)
	return snapshot, f.NextError
}

func (f *FakePort) EnsureSession(_ context.Context, t SessionTarget, _ Action) (Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The routed-message executor uses EnsureSession only as an exact identity
	// check. Lifecycle mutation is deliberately isolated behind
	// EnsureLifecycleSession so a message can never create/reincarnate a pane.
	if t.Identity.StoreID == "" || t.Identity.SessionID == "" || t.Identity.PaneInstanceID == "" {
		return Identity{}, ErrIdentityMismatch
	}
	if current, ok := f.Sessions[t.Identity.SessionID]; ok && current != t.Identity {
		return Identity{}, ErrIdentityMismatch
	}
	return t.Identity, nil
}

func (f *FakePort) EnsureLifecycleSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Identity.StoreID == "" || t.Identity.HostID == "" || t.Identity.TmuxServerInstanceID == "" ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	id := t.Identity
	if id.SessionID == "" {
		id.SessionID = "session-" + a.ActionID
	}
	if id.PaneInstanceID == "" {
		id.PaneInstanceID = "pane-" + a.ActionID
	}
	if id.AgentRunID == "" {
		id.AgentRunID = "run-" + a.ActionID
	}
	id.LifecycleKey, id.TargetEpoch = t.LifecycleKey, t.TargetEpoch
	if id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.EnsureCalls++
	f.Sessions[id.SessionID] = id
	r := LifecycleReceipt{LifecycleReceiptID: "lifecycle-" + a.ActionID,
		Operation: "ensure", ActionID: a.ActionID, ActionEpoch: a.Epoch,
		LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch, LifecycleKey: t.LifecycleKey,
		TargetEpoch: t.TargetEpoch, Status: "ensured", IdentityAfter: id}
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func (f *FakePort) StopSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	id := t.Identity
	if id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" ||
		id.StoreID == "" || id.HostID == "" || id.TmuxServerInstanceID == "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.StopCalls++
	delete(f.Sessions, id.SessionID)
	r := LifecycleReceipt{LifecycleReceiptID: "lifecycle-" + a.ActionID,
		Operation: "stop", ActionID: a.ActionID, ActionEpoch: a.Epoch,
		LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch, LifecycleKey: t.LifecycleKey,
		TargetEpoch: t.TargetEpoch, Status: "stopped", IdentityBefore: id,
		AbsenceObservedAt: "2026-07-19T00:00:00Z"}
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func (f *FakePort) LifecycleReceiptByAction(_ context.Context, actionID, lifecycleKey string, targetEpoch int64) (LifecycleReceipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.LifecycleReceipts[actionID]
	if ok && (r.LifecycleKey != lifecycleKey || r.TargetEpoch != targetEpoch) {
		return LifecycleReceipt{}, false, nil
	}
	return r, ok, nil
}

func (f *FakePort) VerifyLifecycleEffect(_ context.Context, receiptID string, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.LifecycleReceipts[a.ActionID]
	if !ok || r.LifecycleReceiptID != receiptID || r.LifecycleKey != t.LifecycleKey ||
		r.TargetEpoch != t.TargetEpoch || a.Epoch <= r.ActionEpoch || a.LeaseEpoch < r.LeaseEpoch {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	r.ActionEpoch = a.Epoch
	r.LeaseEpoch = a.LeaseEpoch
	if f.NextLifecycleStatus != "" {
		r.Status = f.NextLifecycleStatus
		f.NextLifecycleStatus = ""
	}
	f.LifecycleReceipts[a.ActionID] = r
	if r.Uncertain() {
		return r, ErrUncertain
	}
	return r, nil
}

func (f *FakePort) LifecycleTargetPresence(_ context.Context, lifecycleKey string, targetEpoch int64) (LifecyclePresence, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, identity := range f.Sessions {
		if identity.LifecycleKey != lifecycleKey {
			continue
		}
		if identity.TargetEpoch != targetEpoch {
			return LifecyclePresence{Presence: "mismatch", Identity: identity,
				ObservedAt: "2026-07-19T00:00:00Z"}, nil
		}
		return LifecyclePresence{Presence: "present", Identity: identity,
			ObservedAt: "2026-07-19T00:00:00Z"}, nil
	}
	return LifecyclePresence{Presence: "absent", ObservedAt: "2026-07-19T00:00:00Z"}, nil
}
func (f *FakePort) Grant(_ context.Context, g Grant) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if existing, ok := f.Grants[g.GrantID]; ok && existing != g {
		return ErrIdempotencyBody
	}
	f.Grants[g.GrantID] = g
	return nil
}
func (f *FakePort) RevokeGrant(_ context.Context, id string, epoch int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Grants, id)
	return nil
}
func (f *FakePort) Send(_ context.Context, req SendRequest) (Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	g, ok := f.Grants[req.GrantID]
	if !ok {
		return Receipt{}, ErrGrantDenied
	}
	if err := ValidateSend(req, g); err != nil {
		return Receipt{}, err
	}
	if r, ok := f.Receipts[req.ActionID]; ok {
		if r.PayloadSHA256 != req.PayloadSHA256 {
			return Receipt{}, ErrIdempotencyBody
		}
		return r, nil
	}
	f.SendCalls++
	f.SendRequests = append(f.SendRequests, req)
	if f.NextError != nil {
		return Receipt{}, f.NextError
	}
	status := f.NextStatus
	if status == "" {
		status = ReceiptSubmitted
	}
	r := Receipt{DeliveryID: "delivery-" + req.ActionID, ActionID: req.ActionID, GrantID: req.GrantID,
		GrantEpoch: req.GrantEpoch, Sender: Identity{SessionID: g.SenderSessionID, AgentRunID: g.SenderAgentRunID},
		SenderPrincipalID: g.SenderPrincipalID,
		Recipient:         Identity{SessionID: req.RecipientSessionID, PaneInstanceID: req.RecipientPaneInstanceID},
		PayloadSHA256:     req.PayloadSHA256, Status: status, CompatibilityCode: 0}
	f.Receipts[req.ActionID] = r
	return r, nil
}
func (f *FakePort) ReceiptByAction(_ context.Context, expected ReceiptExpectation) (Receipt, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.Receipts[expected.ActionID]
	if ok {
		if err := expected.Validate(r); err != nil {
			return Receipt{}, false, err
		}
	}
	return r, ok, nil
}
func (f *FakePort) Observe(_ context.Context, cursor string) (ObservationBatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ObserveCalls = append(f.ObserveCalls, cursor)
	if len(f.Batches) > 0 {
		batch := f.Batches[0]
		f.Batches = f.Batches[1:]
		batch.Events = append([]Observation(nil), batch.Events...)
		return batch, f.NextError
	}
	return ObservationBatch{NextCursor: cursor, Events: append([]Observation(nil), f.Observations...)}, nil
}
