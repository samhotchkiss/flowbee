package driver

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// FakePort is deterministic and intentionally strict: forbidden or stale sends
// produce zero mutations, and a receipt never implies workflow completion.
type FakePort struct {
	mu                    sync.Mutex
	Meta                  DriverMetadata
	ProfileInventory      LifecycleProfileInventory
	Capability            ControlOriginCapability
	Snapshot              SessionSnapshot
	Grants                map[string]Grant
	Sessions              map[string]Identity
	Receipts              map[string]Receipt
	LifecycleReceipts     map[string]LifecycleReceipt
	LifecycleFingerprints map[string]string
	EnsureTargets         []SessionTarget
	Watches               map[string]ExternalWatch
	Observations          []Observation
	Batches               []ObservationBatch
	ObserveCalls          []string
	SendRequests          []SendRequest
	SendCalls             int
	EnsureCalls           int
	WatchCalls            int
	AdoptCalls            int
	ReattachCalls         int
	ReleaseCalls          int
	StopCalls             int
	NextStatus            ReceiptStatus
	NextLifecycleStatus   string
	NextError             error
}

func NewFake() *FakePort {
	return &FakePort{Meta: DriverMetadata{
		LifecycleControl:              true,
		LifecycleProfileInventoryPath: "/v2/lifecycle/profiles",
		TmuxServer: TmuxServerMetadata{DomainID: "flowbee", Ownership: "managed_dedicated",
			InstanceID: "00000000-0000-4000-8000-000000000003", ConnectionVisibility: "isolated_socket"},
		Contracts: defaultDriverContractCapabilities(),
	}, ProfileInventory: LifecycleProfileInventory{APIVersion: "v2", ServerTime: "2026-07-19T00:00:00Z",
		FormatVersion: "tmux-driver.lifecycle-profile-inventory/v1", LifecycleEnabled: true,
		TmuxServerDomainID: "flowbee", Profiles: []LifecycleProfile{
			managedProfile("claude_builder", "claude"), managedProfile("claude_reviewer", "claude"),
			managedProfile("codex_builder", "codex"), managedProfile("codex_orchestrator", "codex"),
			managedProfile("codex_reviewer", "codex"), managedProfile("grok_builder", "grok"),
			managedProfile("grok_reviewer", "grok"),
		}}, Capability: ControlOriginCapability{
		FormatVersion: controlOriginCapabilityFormat, Supported: true, Authorized: true,
		PrincipalID: "flowbee-control", PrincipalKind: "control_plane",
		RequiredScopes:   []string{"messages:send", "routes:manage"},
		GrantedScopes:    []string{"messages:send", "routes:manage"},
		RouteGrantFormat: controlRouteGrantFormat, DeliveryReceiptFormat: controlDeliveryReceiptFormat,
		GrantEndpoint: "/v2/routes/grants", MessageEndpoint: "/v2/messages",
		OnBehalfOfSessionIDRule: "forbidden",
	}, Grants: map[string]Grant{}, Sessions: map[string]Identity{}, Receipts: map[string]Receipt{},
		LifecycleReceipts: map[string]LifecycleReceipt{}, LifecycleFingerprints: map[string]string{},
		Watches: map[string]ExternalWatch{}}
}

func managedProfile(id, provider string) LifecycleProfile {
	return LifecycleProfile{ProfileID: id, Provider: provider, InitialPromptAdapter: "argv_element",
		TargetCredentialAdapter: "file_environment", EnsureSupported: true,
		BootstrapArtifactSupported: true, FlowbeeCredentialInstallSupported: true,
		ManagedDisplayNameSupported: true}
}

func (f *FakePort) EnsureExternalWatch(_ context.Context, paneID, provider, profile string) (ExternalWatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(paneID) < 2 || paneID[0] != '%' || provider == "" || (profile != "interactive" && profile != "build") {
		return ExternalWatch{}, ErrIdentityMismatch
	}
	if watch, ok := f.Watches[paneID]; ok {
		if watch.Provider != provider || watch.Profile != profile {
			return ExternalWatch{}, ErrIdentityMismatch
		}
		return watch, nil
	}
	f.WatchCalls++
	watch := ExternalWatch{WatchID: fmt.Sprintf("00000000-0000-4000-8000-%012d", f.WatchCalls),
		PaneID: paneID, Enabled: true, Lifecycle: "active", Provider: provider, Profile: profile}
	f.Watches[paneID] = watch
	return watch, nil
}

func (f *FakePort) AdoptSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	if !fakeHasWatch(f.Watches, t.ExternalWatchID) || t.Identity.Ownership != "" ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || t.ProfileID == "" ||
		t.LeaseID == "" || t.LeaseEpoch < 1 || !identityHasExactDriverTuple(t.Identity) {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.AdoptCalls++
	after := t.Identity
	after.LifecycleKey, after.TargetEpoch, after.Ownership = t.LifecycleKey, t.TargetEpoch, "external_observed"
	f.Sessions[after.SessionID] = after
	r := LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v2",
		LifecycleReceiptID: "lifecycle-" + a.ActionID, Operation: "adopt",
		ActionID: a.ActionID, ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch,
		LifecycleKey: t.LifecycleKey, TmuxServerDomainID: after.TmuxServerDomainID,
		ExternalWatchID: t.ExternalWatchID, TargetEpoch: t.TargetEpoch, Status: "adopted", IdentityAfter: after}
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func (f *FakePort) ReleaseSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	if !fakeHasWatch(f.Watches, t.ExternalWatchID) || t.Identity.Ownership != "external_observed" ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 ||
		!identityHasExactDriverTuple(t.Identity) {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.ReleaseCalls++
	if current, ok := f.Sessions[t.Identity.SessionID]; ok {
		current.Ownership, current.LifecycleKey, current.TargetEpoch = "", "", 0
		f.Sessions[current.SessionID] = current
	}
	r := LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v2",
		LifecycleReceiptID: "lifecycle-" + a.ActionID, Operation: "release",
		ActionID: a.ActionID, ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch,
		LifecycleKey: t.LifecycleKey, TmuxServerDomainID: t.Identity.TmuxServerDomainID,
		ExternalWatchID: t.ExternalWatchID, TargetEpoch: t.TargetEpoch, Status: "released", IdentityBefore: t.Identity}
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func (f *FakePort) ReattachSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	if !identityHasExactDriverTuple(t.Identity) || t.LifecycleKey == "" || t.TargetEpoch < 1 ||
		t.LeaseID == "" || t.LeaseEpoch < 1 ||
		(t.Identity.Ownership != "driver_managed" && t.Identity.Ownership != "external_observed") ||
		(t.Identity.Ownership == "external_observed" && (!fakeHasWatch(f.Watches, t.ExternalWatchID))) {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	current, ok := f.Sessions[t.Identity.SessionID]
	if !ok || !lifecycleIdentityMatches(current, t.Identity) {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.ReattachCalls++
	r := LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v2",
		LifecycleReceiptID: "lifecycle-" + a.ActionID, Operation: "reattach",
		ActionID: a.ActionID, ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch,
		LifecycleKey: t.LifecycleKey, TmuxServerDomainID: t.Identity.TmuxServerDomainID,
		ExternalWatchID: t.ExternalWatchID, TargetEpoch: t.TargetEpoch, Status: "reattached",
		IdentityBefore: t.Identity, IdentityAfter: current}
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func fakeHasWatch(watches map[string]ExternalWatch, watchID string) bool {
	if watchID == "" {
		return false
	}
	for _, watch := range watches {
		if watch.WatchID == watchID && watch.Enabled {
			return true
		}
	}
	return false
}

func identityHasExactDriverTuple(id Identity) bool {
	return id.HostID != "" && id.StoreID != "" && id.TmuxServerDomainID != "" &&
		id.TmuxServerInstanceID != "" && id.SessionID != "" && id.PaneInstanceID != "" && id.AgentRunID != ""
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

func (f *FakePort) LifecycleProfiles(_ context.Context) (LifecycleProfileInventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.ProfileInventory
	out.Profiles = append([]LifecycleProfile(nil), out.Profiles...)
	return out, f.NextError
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
	if current, ok := f.Sessions[t.Identity.SessionID]; ok {
		if !identityMatchesTarget(current, t) {
			return Identity{}, ErrIdentityMismatch
		}
		return current, nil
	}
	return t.Identity, nil
}

func (f *FakePort) EnsureLifecycleSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t.Identity.StoreID == "" || t.Identity.HostID == "" || t.Identity.TmuxServerDomainID == "" ||
		t.Identity.TmuxServerInstanceID == "" ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	v3 := t.Bootstrap != nil || t.CredentialEnvelope != nil || t.PresentationName != ""
	if v3 {
		if err := validateLifecycleV3Contracts(f.Meta.Contracts, t); err != nil {
			return LifecycleReceipt{}, err
		}
		if err := validateLifecycleV3Launch(t); err != nil {
			return LifecycleReceipt{}, err
		}
		if err := f.ProfileInventory.ValidateLaunch(t.ProfileID, t.Identity.TmuxServerDomainID, t); err != nil {
			return LifecycleReceipt{}, err
		}
	}
	fingerprint := fmt.Sprintf("%s|%d|%s|%s|%s|%d|%s|%s|%s|%d|%s", t.LifecycleKey,
		t.TargetEpoch, t.ProfileID, t.WorkspaceRootID, t.WorkspaceRelativePath, a.Epoch,
		valueOrEmpty(t.Bootstrap), t.PresentationName, credentialID(t.CredentialEnvelope),
		credentialEpoch(t.CredentialEnvelope), credentialHash(t.CredentialEnvelope))
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		if f.LifecycleFingerprints[a.ActionID] != fingerprint {
			return LifecycleReceipt{}, ErrIdempotencyBody
		}
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
	id.Ownership = "driver_managed"
	if id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.EnsureCalls++
	observedTarget := t
	if t.Bootstrap != nil {
		copy := *t.Bootstrap
		observedTarget.Bootstrap = &copy
	}
	if t.CredentialEnvelope != nil {
		copy := *t.CredentialEnvelope
		observedTarget.CredentialEnvelope = &copy
	}
	f.EnsureTargets = append(f.EnsureTargets, observedTarget)
	f.Sessions[id.SessionID] = id
	format := "tmux-driver.lifecycle-receipt/v2"
	if v3 {
		format = "tmux-driver.lifecycle-receipt/v3"
	}
	r := LifecycleReceipt{FormatVersion: format,
		LifecycleReceiptID: "lifecycle-" + a.ActionID,
		Operation:          "ensure", ActionID: a.ActionID, ActionEpoch: a.Epoch,
		LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch, LifecycleKey: t.LifecycleKey,
		TmuxServerDomainID: id.TmuxServerDomainID, TargetEpoch: t.TargetEpoch,
		Status: "ensured", IdentityAfter: id}
	if v3 {
		if t.Bootstrap != nil {
			r.BootstrapArtifact = LifecycleBootstrapReceipt{ArtifactID: t.Bootstrap.ArtifactID,
				Format: t.Bootstrap.Format, PayloadSHA256: t.Bootstrap.PayloadSHA256, Status: "injected"}
			r.BootstrapArtifactPresent = true
		}
		if t.CredentialEnvelope != nil {
			r.CredentialInstall = LifecycleCredentialReceipt{EnvelopeID: t.CredentialEnvelope.EnvelopeID,
				CredentialEpoch: t.CredentialEnvelope.CredentialEpoch,
				PayloadSHA256:   t.CredentialEnvelope.PayloadSHA256, Status: "installed"}
			r.CredentialInstallPresent = true
		}
		if t.PresentationName != "" {
			r.PresentationName, r.PresentationNamePresent = t.PresentationName, true
		}
	}
	f.LifecycleFingerprints[a.ActionID] = fingerprint
	f.LifecycleReceipts[a.ActionID] = r
	return r, nil
}

func valueOrEmpty(v *LifecycleBootstrapArtifact) string {
	if v == nil {
		return ""
	}
	return v.ArtifactID + "|" + v.Format + "|" + v.PayloadSHA256
}
func credentialID(v *LifecycleCredentialEnvelope) string {
	if v == nil {
		return ""
	}
	return v.EnvelopeID
}
func credentialEpoch(v *LifecycleCredentialEnvelope) int64 {
	if v == nil {
		return 0
	}
	return v.CredentialEpoch
}
func credentialHash(v *LifecycleCredentialEnvelope) string {
	if v == nil {
		return ""
	}
	return v.PayloadSHA256
}

func (f *FakePort) StopSession(_ context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.LifecycleReceipts[a.ActionID]; ok {
		return r, nil
	}
	id := t.Identity
	if id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" ||
		id.StoreID == "" || id.HostID == "" || id.TmuxServerDomainID == "" || id.TmuxServerInstanceID == "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	f.StopCalls++
	delete(f.Sessions, id.SessionID)
	r := LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v2",
		LifecycleReceiptID: "lifecycle-" + a.ActionID,
		Operation:          "stop", ActionID: a.ActionID, ActionEpoch: a.Epoch,
		LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch, LifecycleKey: t.LifecycleKey,
		TmuxServerDomainID: id.TmuxServerDomainID, TargetEpoch: t.TargetEpoch,
		Status: "stopped", IdentityBefore: id,
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
	if g.SenderPrincipalID != "" {
		if g.SenderSessionID != "" || g.SenderAgentRunID != "" || g.ExpectedRecipientAgentRunID == "" {
			return ErrIdentityMismatch
		}
		if current, ok := f.Sessions[g.RecipientSessionID]; ok &&
			current.AgentRunID != g.ExpectedRecipientAgentRunID {
			return ErrIdentityMismatch
		}
	} else if g.ExpectedRecipientAgentRunID != "" {
		return ErrIdentityMismatch
	}
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
		SenderPrincipalID:           g.SenderPrincipalID,
		Recipient:                   Identity{SessionID: req.RecipientSessionID, PaneInstanceID: req.RecipientPaneInstanceID},
		ExpectedRecipientAgentRunID: req.ExpectedRecipientAgentRunID,
		PayloadSHA256:               req.PayloadSHA256, Status: status, CompatibilityCode: 0}
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
