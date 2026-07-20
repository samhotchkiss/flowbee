package driver

// HTTPPort is the production v2.4 adapter. It speaks only the documented
// lifecycle, route-grant, routed-message, receipt, and observation endpoints.
// It exposes no raw tmux operation.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

type HTTPPort struct {
	BaseURL, Token string
	Client         *http.Client
}

var _ DriverPort = (*HTTPPort)(nil)

// NewUDSPort connects to the local Driver daemon over its private Unix socket.
// The URL is synthetic; the transport always dials socket, preserving the same
// HTTP wire contract as remote deployments without a second protocol.
func NewUDSPort(socket, token string) *HTTPPort {
	t := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &HTTPPort{BaseURL: "http://driver.local", Token: token, Client: &http.Client{Transport: t}}
}

// Check proves the configured daemon endpoint and authenticated control-plane
// principal are reachable before Flowbee advertises v2 readiness.
func (p *HTTPPort) Check(ctx context.Context) error {
	if _, err := p.Metadata(ctx); err != nil {
		return fmt.Errorf("driver meta: %w", err)
	}
	if err := p.call(ctx, http.MethodGet, "/v2/instance", "", nil, nil); err != nil {
		return fmt.Errorf("driver instance: %w", err)
	}
	return nil
}

func (p *HTTPPort) ControlOriginCapability(ctx context.Context) (ControlOriginCapability, error) {
	metadata, err := p.Metadata(ctx)
	if err != nil {
		return ControlOriginCapability{}, err
	}
	if !metadata.ControlPrincipalOrigin {
		return ControlOriginCapability{}, errors.New("driver control origin capability: meta feature is not enabled")
	}
	var out struct {
		Capability ControlOriginCapability `json:"capability"`
	}
	if err := p.call(ctx, http.MethodGet, "/v2/control/capabilities", "", nil, &out); err != nil {
		return ControlOriginCapability{}, err
	}
	if err := validateControlOriginCapability(out.Capability); err != nil {
		return ControlOriginCapability{}, err
	}
	return out.Capability, nil
}

type HTTPError struct {
	Status int
	Code   string
	Detail string
}

// PreEffectError certifies that a lifecycle failure occurred before Flowbee
// submitted any mutation to Driver. Runtimes may safely retry this class of
// failure; every other error after an action is claimed remains uncertain and
// must be reconciled by receipt/presence rather than blindly resent.
//
// This marker must not be used for HTTP failures: a response error can arrive
// after Driver has accepted the request.
type PreEffectError struct{ Err error }

func (e *PreEffectError) Error() string { return e.Err.Error() }
func (e *PreEffectError) Unwrap() error { return e.Err }

func preEffect(err error) error {
	if err == nil {
		return nil
	}
	return &PreEffectError{Err: err}
}

func (e *HTTPError) Error() string {
	if e.Code == "" {
		if e.Detail != "" {
			return fmt.Sprintf("driver http %d: %s", e.Status, e.Detail)
		}
		return fmt.Sprintf("driver http %d", e.Status)
	}
	if e.Detail != "" {
		return fmt.Sprintf("driver http %d: %s: %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("driver http %d: %s", e.Status, e.Code)
}

func (p *HTTPPort) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return http.DefaultClient
}

func (p *HTTPPort) call(ctx context.Context, method, path, idem string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(p.BaseURL, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	r, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		var problem struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
			Error  struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&problem)
		if problem.Code == "" {
			problem.Code = problem.Error.Code
		}
		if problem.Detail == "" {
			problem.Detail = problem.Error.Message
		}
		return &HTTPError{Status: r.StatusCode, Code: problem.Code, Detail: problem.Detail}
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(out)
	}
	return nil
}

func (p *HTTPPort) Metadata(ctx context.Context) (DriverMetadata, error) {
	var out struct {
		APIVersion             string                     `json:"api_version"`
		HostID                 string                     `json:"host_id"`
		StoreID                string                     `json:"store_id"`
		Instance               string                     `json:"instance"`
		ProducerBootID         string                     `json:"producer_boot_id"`
		ReplayFloorCursor      string                     `json:"replay_floor_cursor"`
		DurableHighWaterCursor string                     `json:"durable_high_water_cursor"`
		Features               map[string]json.RawMessage `json:"features"`
		TmuxServer             TmuxServerMetadata         `json:"tmux_server"`
		Contracts              DriverContractCapabilities `json:"contracts"`
	}
	if err := p.call(ctx, http.MethodGet, "/v2/meta", "", nil, &out); err != nil {
		return DriverMetadata{}, err
	}
	if out.HostID == "" || out.StoreID == "" || out.ProducerBootID == "" ||
		out.ReplayFloorCursor == "" || out.DurableHighWaterCursor == "" {
		return DriverMetadata{}, errors.New("driver metadata: incomplete cursor-domain identity")
	}
	metadata := DriverMetadata{APIVersion: out.APIVersion, HostID: out.HostID, StoreID: out.StoreID,
		Instance: out.Instance, ProducerBootID: out.ProducerBootID,
		ReplayFloorCursor: out.ReplayFloorCursor, DurableHighWaterCursor: out.DurableHighWaterCursor,
		TmuxServer: out.TmuxServer, Contracts: out.Contracts}
	if raw, present := out.Features["control_principal_origin"]; present {
		if err := json.Unmarshal(raw, &metadata.ControlPrincipalOrigin); err != nil {
			return DriverMetadata{}, errors.New("driver metadata: control_principal_origin must be boolean")
		}
	}
	if raw, present := out.Features["lifecycle_control"]; present {
		if err := json.Unmarshal(raw, &metadata.LifecycleControl); err != nil {
			return DriverMetadata{}, errors.New("driver metadata: lifecycle_control must be boolean")
		}
	}
	if raw, present := out.Features["lifecycle_profile_inventory"]; present {
		if err := json.Unmarshal(raw, &metadata.LifecycleProfileInventoryPath); err != nil {
			return DriverMetadata{}, errors.New("driver metadata: lifecycle_profile_inventory must be a path string")
		}
	}
	if err := validateDriverMetadata(metadata); err != nil {
		return DriverMetadata{}, err
	}
	return metadata, nil
}

func (p *HTTPPort) LifecycleProfiles(ctx context.Context) (LifecycleProfileInventory, error) {
	meta, err := p.Metadata(ctx)
	if err != nil {
		return LifecycleProfileInventory{}, err
	}
	if meta.LifecycleProfileInventoryPath != "/v2/lifecycle/profiles" {
		return LifecycleProfileInventory{}, errors.New("driver lifecycle profile inventory path is unavailable")
	}
	var out LifecycleProfileInventory
	if err := p.call(ctx, http.MethodGet, meta.LifecycleProfileInventoryPath, "", nil, &out); err != nil {
		return LifecycleProfileInventory{}, err
	}
	if err := validateLifecycleProfileInventory(out); err != nil {
		return LifecycleProfileInventory{}, err
	}
	if out.TmuxServerDomainID != meta.TmuxServer.DomainID {
		return LifecycleProfileInventory{}, ErrIdentityMismatch
	}
	return out, nil
}

func (p *HTTPPort) SnapshotSessions(ctx context.Context) (SessionSnapshot, error) {
	meta, err := p.Metadata(ctx)
	if err != nil {
		return SessionSnapshot{}, err
	}
	if meta.TmuxServer.InstanceID == "" {
		return SessionSnapshot{}, errors.New("driver session snapshot: tmux server incarnation is unknown")
	}
	type summary struct {
		SessionID string `json:"session_id"`
	}
	var snapshot SessionSnapshot
	pageCursor := ""
	for {
		q := url.Values{"limit": []string{"2000"}}
		if pageCursor != "" {
			q.Set("cursor", pageCursor)
		}
		var page struct {
			AsOfCursor string    `json:"as_of_cursor"`
			Sessions   []summary `json:"sessions"`
			NextCursor *string   `json:"next_cursor"`
		}
		if err := p.call(ctx, http.MethodGet, "/v2/sessions?"+q.Encode(), "", nil, &page); err != nil {
			return SessionSnapshot{}, err
		}
		if page.AsOfCursor == "" {
			return SessionSnapshot{}, errors.New("driver session snapshot: missing as_of_cursor")
		}
		if snapshot.AsOfCursor == "" {
			snapshot.AsOfCursor = page.AsOfCursor
		} else if snapshot.AsOfCursor != page.AsOfCursor {
			return SessionSnapshot{}, errors.New("driver session snapshot: pagination cursor changed snapshot")
		}
		for _, item := range page.Sessions {
			if item.SessionID == "" {
				return SessionSnapshot{}, errors.New("driver session snapshot: empty session_id")
			}
			projection, err := p.sessionProjection(ctx, item.SessionID)
			if err != nil {
				return SessionSnapshot{}, err
			}
			projection.Identity.TmuxServerDomainID = meta.TmuxServer.DomainID
			if projection.Identity.HostID != meta.HostID || projection.Identity.StoreID != meta.StoreID ||
				(projection.Lifecycle != "ended" && projection.Identity.TmuxServerInstanceID != meta.TmuxServer.InstanceID) {
				return SessionSnapshot{}, errors.New("driver session snapshot: metadata identity changed during read")
			}
			if snapshot.StoreID == "" {
				snapshot.HostID, snapshot.StoreID = projection.Identity.HostID, projection.Identity.StoreID
			} else if projection.Identity.HostID != snapshot.HostID || projection.Identity.StoreID != snapshot.StoreID {
				return SessionSnapshot{}, errors.New("driver session snapshot: mixed cursor domains")
			}
			snapshot.Sessions = append(snapshot.Sessions, projection)
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		pageCursor = *page.NextCursor
	}
	if snapshot.StoreID == "" {
		snapshot.HostID, snapshot.StoreID = meta.HostID, meta.StoreID
	}
	return snapshot, nil
}

func (p *HTTPPort) sessionProjection(ctx context.Context, sessionID string) (SessionProjection, error) {
	var out struct {
		AsOfCursor    string `json:"as_of_cursor"`
		StateRevision uint64 `json:"state_revision"`
		Session       struct {
			Format         string  `json:"format"`
			SessionID      string  `json:"session_id"`
			HostID         string  `json:"host_id"`
			StoreID        string  `json:"store_id"`
			Provider       *string `json:"provider"`
			ConversationID *string `json:"conversation_id"`
			StartedAt      *string `json:"started_at"`
			EndedAt        *string `json:"ended_at"`
			EndReason      *string `json:"end_reason"`
			PaneInstanceID *string `json:"pane_instance_id"`
			WatchID        *string `json:"watch_id"`
		} `json:"session"`
		State json.RawMessage `json:"state"`
	}
	if err := p.call(ctx, http.MethodGet, "/v2/sessions/"+url.PathEscape(sessionID), "", nil, &out); err != nil {
		return SessionProjection{}, err
	}
	if out.Session.Format != "tmux-driver.session/v2" || out.Session.SessionID != sessionID ||
		out.Session.HostID == "" || out.Session.StoreID == "" || out.AsOfCursor == "" {
		return SessionProjection{}, errors.New("driver session snapshot: incomplete stable identity")
	}
	var state struct {
		AgentRunID           string `json:"agent_run_id"`
		TmuxServerInstanceID string `json:"tmux_server_instance_id"`
		Lifecycle            string `json:"lifecycle"`
		Phase                string `json:"phase"`
		BindingStatus        string `json:"binding_status"`
		BindingEpoch         int64  `json:"binding_epoch"`
	}
	if len(out.State) == 0 || string(out.State) == "null" || json.Unmarshal(out.State, &state) != nil {
		return SessionProjection{}, errors.New("driver session snapshot: invalid state object")
	}
	value := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	identity := Identity{HostID: out.Session.HostID, StoreID: out.Session.StoreID,
		TmuxServerInstanceID: state.TmuxServerInstanceID, SessionID: sessionID,
		PaneInstanceID: value(out.Session.PaneInstanceID), AgentRunID: state.AgentRunID,
		Provider: value(out.Session.Provider), ConversationID: value(out.Session.ConversationID),
		StateCursor: out.AsOfCursor}
	if state.Lifecycle != "ended" && (identity.PaneInstanceID == "" || identity.AgentRunID == "" || identity.TmuxServerInstanceID == "") {
		return SessionProjection{}, errors.New("driver session snapshot: active session missing incarnation identity")
	}
	return SessionProjection{Identity: identity, WatchID: value(out.Session.WatchID),
		Lifecycle: state.Lifecycle, Phase: state.Phase,
		BindingStatus: state.BindingStatus, BindingEpoch: state.BindingEpoch,
		StateRevision: out.StateRevision, AsOfCursor: out.AsOfCursor,
		StartedAt: value(out.Session.StartedAt), EndedAt: value(out.Session.EndedAt),
		EndReason: value(out.Session.EndReason), RawState: append(json.RawMessage(nil), out.State...)}, nil
}

type lifecycleIdentityWire struct {
	HostID               string  `json:"host_id"`
	StoreID              string  `json:"store_id"`
	TmuxServerDomainID   string  `json:"tmux_server_domain_id"`
	TmuxServerInstanceID string  `json:"tmux_server_instance_id"`
	Ownership            string  `json:"ownership"`
	LifecycleKey         string  `json:"lifecycle_key"`
	TargetEpoch          int64   `json:"target_epoch"`
	SessionID            *string `json:"session_id"`
	PaneInstanceID       *string `json:"pane_instance_id"`
	AgentRunID           *string `json:"agent_run_id"`
	Provider             *string `json:"provider"`
	ConversationID       *string `json:"conversation_id"`
	StateCursor          *string `json:"state_cursor"`
}

func wireIdentity(w lifecycleIdentityWire) Identity {
	value := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	return Identity{
		HostID: w.HostID, StoreID: w.StoreID, TmuxServerDomainID: w.TmuxServerDomainID,
		TmuxServerInstanceID: w.TmuxServerInstanceID, Ownership: w.Ownership,
		LifecycleKey: w.LifecycleKey, TargetEpoch: w.TargetEpoch, SessionID: value(w.SessionID),
		PaneInstanceID: value(w.PaneInstanceID), AgentRunID: value(w.AgentRunID),
		Provider: value(w.Provider), ConversationID: value(w.ConversationID), StateCursor: value(w.StateCursor),
	}
}

type lifecycleReceiptWire struct {
	FormatVersion      string                 `json:"format_version"`
	LifecycleReceiptID string                 `json:"lifecycle_receipt_id"`
	Operation          string                 `json:"operation"`
	ActionID           string                 `json:"action_id"`
	ActionEpoch        int64                  `json:"action_epoch"`
	LeaseID            string                 `json:"lease_id"`
	LeaseEpoch         int64                  `json:"lease_epoch"`
	LifecycleKey       string                 `json:"lifecycle_key"`
	TmuxServerDomainID string                 `json:"tmux_server_domain_id"`
	ExternalWatchID    *string                `json:"external_watch_id"`
	TargetEpoch        int64                  `json:"target_epoch"`
	Status             string                 `json:"status"`
	IdentityBefore     *lifecycleIdentityWire `json:"identity_before"`
	IdentityAfter      *lifecycleIdentityWire `json:"identity_after"`
	AbsenceObservedAt  *string                `json:"absence_observed_at"`
	DiagnosticCode     *string                `json:"diagnostic_code"`
	BootstrapArtifact  *struct {
		ArtifactID    string `json:"artifact_id"`
		Format        string `json:"format"`
		PayloadSHA256 string `json:"payload_sha256"`
		Status        string `json:"status"`
	} `json:"bootstrap_artifact"`
	CredentialInstall *struct {
		EnvelopeID      string `json:"envelope_id"`
		CredentialEpoch int64  `json:"credential_epoch"`
		PayloadSHA256   string `json:"payload_sha256"`
		Status          string `json:"status"`
	} `json:"credential_install"`
	PresentationName *string `json:"presentation_name"`
}

func wireLifecycleReceipt(w lifecycleReceiptWire) LifecycleReceipt {
	r := LifecycleReceipt{FormatVersion: w.FormatVersion, LifecycleReceiptID: w.LifecycleReceiptID, Operation: w.Operation,
		ActionID: w.ActionID, ActionEpoch: w.ActionEpoch, LeaseID: w.LeaseID,
		LeaseEpoch: w.LeaseEpoch, LifecycleKey: w.LifecycleKey,
		TmuxServerDomainID: w.TmuxServerDomainID, TargetEpoch: w.TargetEpoch,
		Status: w.Status}
	if w.IdentityBefore != nil {
		r.IdentityBefore = wireIdentity(*w.IdentityBefore)
	}
	if w.IdentityAfter != nil {
		r.IdentityAfter = wireIdentity(*w.IdentityAfter)
	}
	if w.AbsenceObservedAt != nil {
		r.AbsenceObservedAt = *w.AbsenceObservedAt
	}
	if w.DiagnosticCode != nil {
		r.DiagnosticCode = *w.DiagnosticCode
	}
	if w.ExternalWatchID != nil {
		r.ExternalWatchID = *w.ExternalWatchID
	}
	if w.BootstrapArtifact != nil {
		r.BootstrapArtifactPresent = true
		r.BootstrapArtifact = LifecycleBootstrapReceipt{ArtifactID: w.BootstrapArtifact.ArtifactID,
			Format: w.BootstrapArtifact.Format, PayloadSHA256: w.BootstrapArtifact.PayloadSHA256,
			Status: w.BootstrapArtifact.Status}
	}
	if w.CredentialInstall != nil {
		r.CredentialInstallPresent = true
		r.CredentialInstall = LifecycleCredentialReceipt{EnvelopeID: w.CredentialInstall.EnvelopeID,
			CredentialEpoch: w.CredentialInstall.CredentialEpoch,
			PayloadSHA256:   w.CredentialInstall.PayloadSHA256, Status: w.CredentialInstall.Status}
	}
	if w.PresentationName != nil {
		r.PresentationNamePresent = true
		r.PresentationName = *w.PresentationName
	}
	return r
}

func (p *HTTPPort) EnsureSession(ctx context.Context, t SessionTarget, a Action) (Identity, error) {
	r, err := p.EnsureLifecycleSession(ctx, t, a)
	if err != nil {
		return Identity{}, err
	}
	return r.IdentityAfter, nil
}

func (p *HTTPPort) EnsureLifecycleSession(ctx context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	profile := t.ProfileID
	if profile == "" {
		profile = t.Role
	}
	if a.ActionID == "" || a.Epoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || profile == "" ||
		t.WorkspaceRootID == "" || t.WorkspaceRelativePath == "" || t.Identity.TmuxServerDomainID == "" ||
		t.Identity.HostID == "" || t.Identity.StoreID == "" || t.Identity.TmuxServerInstanceID == "" {
		return LifecycleReceipt{}, preEffect(errors.New("driver lifecycle ensure: incomplete fenced target"))
	}
	type ensureRequest struct {
		FormatVersion string `json:"format_version"`
		ActionID      string `json:"action_id"`
		ActionEpoch   int64  `json:"action_epoch"`
		LeaseID       string `json:"lease_id"`
		LeaseEpoch    int64  `json:"lease_epoch"`
		Target        struct {
			LifecycleKey                 string `json:"lifecycle_key"`
			TargetEpoch                  int64  `json:"target_epoch"`
			ExpectedHostID               string `json:"expected_host_id"`
			ExpectedStoreID              string `json:"expected_store_id"`
			ExpectedTmuxServerDomainID   string `json:"expected_tmux_server_domain_id"`
			ExpectedTmuxServerInstanceID string `json:"expected_tmux_server_instance_id"`
		} `json:"target"`
		Launch struct {
			ProfileID string `json:"profile_id"`
			Workspace struct {
				RootID       string `json:"root_id"`
				RelativePath string `json:"relative_path"`
			} `json:"workspace"`
			Bootstrap          *LifecycleBootstrapArtifact  `json:"bootstrap,omitempty"`
			CredentialEnvelope *LifecycleCredentialEnvelope `json:"credential_envelope,omitempty"`
			PresentationName   string                       `json:"presentation_name,omitempty"`
		} `json:"launch"`
	}
	v3 := t.Bootstrap != nil || t.CredentialEnvelope != nil || t.PresentationName != ""
	format := "tmux-driver.lifecycle-ensure/v2"
	if v3 {
		if err := validateLifecycleV3Launch(t); err != nil {
			return LifecycleReceipt{}, preEffect(err)
		}
		meta, err := p.Metadata(ctx)
		if err != nil {
			return LifecycleReceipt{}, preEffect(fmt.Errorf("driver lifecycle ensure v3 capability: %w", err))
		}
		if err := validateLifecycleV3Contracts(meta.Contracts, t); err != nil {
			return LifecycleReceipt{}, preEffect(err)
		}
		inventory, err := p.LifecycleProfiles(ctx)
		if err != nil {
			return LifecycleReceipt{}, preEffect(fmt.Errorf("driver lifecycle profile inventory: %w", err))
		}
		if err := inventory.ValidateLaunch(profile, t.Identity.TmuxServerDomainID, t); err != nil {
			return LifecycleReceipt{}, preEffect(err)
		}
		format = "tmux-driver.lifecycle-ensure/v3"
	}
	in := ensureRequest{FormatVersion: format, ActionID: a.ActionID, ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch}
	in.Target.LifecycleKey, in.Target.TargetEpoch = t.LifecycleKey, t.TargetEpoch
	in.Target.ExpectedHostID, in.Target.ExpectedStoreID = t.Identity.HostID, t.Identity.StoreID
	in.Target.ExpectedTmuxServerDomainID = t.Identity.TmuxServerDomainID
	in.Target.ExpectedTmuxServerInstanceID = t.Identity.TmuxServerInstanceID
	in.Launch.ProfileID = profile
	in.Launch.Workspace.RootID, in.Launch.Workspace.RelativePath = t.WorkspaceRootID, t.WorkspaceRelativePath
	in.Launch.Bootstrap, in.Launch.CredentialEnvelope = t.Bootstrap, t.CredentialEnvelope
	in.Launch.PresentationName = t.PresentationName
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/lifecycle/ensure", a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	expectedReceipt := "tmux-driver.lifecycle-receipt/v2"
	if v3 {
		expectedReceipt = "tmux-driver.lifecycle-receipt/v3"
	}
	if r.FormatVersion != expectedReceipt || r.ActionID != a.ActionID ||
		r.ActionEpoch != a.Epoch || r.LifecycleKey != t.LifecycleKey ||
		r.TmuxServerDomainID != t.Identity.TmuxServerDomainID || r.TargetEpoch != t.TargetEpoch {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	if r.Status != "ensured" || r.IdentityAfter.HostID != t.Identity.HostID ||
		r.IdentityAfter.StoreID != t.Identity.StoreID ||
		r.IdentityAfter.TmuxServerDomainID != t.Identity.TmuxServerDomainID ||
		r.IdentityAfter.TmuxServerInstanceID != t.Identity.TmuxServerInstanceID ||
		r.IdentityAfter.Ownership != "driver_managed" || r.IdentityAfter.SessionID == "" ||
		r.IdentityAfter.PaneInstanceID == "" || r.IdentityAfter.AgentRunID == "" {
		return r, fmt.Errorf("driver lifecycle ensure: status %q", r.Status)
	}
	if v3 {
		if t.Bootstrap != nil && (!r.BootstrapArtifactPresent ||
			r.BootstrapArtifact.ArtifactID != t.Bootstrap.ArtifactID ||
			r.BootstrapArtifact.PayloadSHA256 != t.Bootstrap.PayloadSHA256 ||
			r.BootstrapArtifact.Status != "injected") {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
		if t.Bootstrap == nil && r.BootstrapArtifactPresent {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
		if t.CredentialEnvelope != nil && (!r.CredentialInstallPresent ||
			r.CredentialInstall.EnvelopeID != t.CredentialEnvelope.EnvelopeID ||
			r.CredentialInstall.CredentialEpoch != t.CredentialEnvelope.CredentialEpoch ||
			r.CredentialInstall.PayloadSHA256 != t.CredentialEnvelope.PayloadSHA256 ||
			(r.CredentialInstall.Status != "installed" && r.CredentialInstall.Status != "rebound")) {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
		if t.CredentialEnvelope == nil && r.CredentialInstallPresent {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
		if t.PresentationName != "" && (!r.PresentationNamePresent || r.PresentationName != t.PresentationName) {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
		if t.PresentationName == "" && r.PresentationNamePresent {
			return LifecycleReceipt{}, ErrIdentityMismatch
		}
	}
	return r, nil
}

func requireSupportedContract(got DriverContractCapability, want string) error {
	if !got.Supported || got.ContractID != want {
		return fmt.Errorf("driver contract unavailable: %s", want)
	}
	return nil
}

func validateLifecycleV3Contracts(c DriverContractCapabilities, t SessionTarget) error {
	required := []struct {
		got  DriverContractCapability
		want string
	}{
		{c.LifecycleEnsure, "tmux-driver.lifecycle-ensure/v3"},
		{c.LifecycleEnsureBootstrapArtifact, "tmux-driver.lifecycle-ensure-bootstrap-artifact/v1"},
		{c.LifecycleFlowbeeCredentialInstall, "tmux-driver.lifecycle-flowbee-credential-install/v1"},
	}
	if t.PresentationName != "" {
		if t.Identity.TmuxServerDomainID == "default" {
			required = append(required, struct {
				got  DriverContractCapability
				want string
			}{c.LifecycleHumanVisibleSession, "tmux-driver.lifecycle-human-visible-session/v1"})
		} else {
			required = append(required, struct {
				got  DriverContractCapability
				want string
			}{c.LifecycleManagedDisplayName, "tmux-driver.lifecycle-managed-display-name/v1"})
		}
	}
	for _, item := range required {
		if err := requireSupportedContract(item.got, item.want); err != nil {
			return fmt.Errorf("driver lifecycle ensure v3 %w", err)
		}
	}
	return nil
}

func validateLifecycleV3Launch(t SessionTarget) error {
	if t.Bootstrap != nil {
		b := t.Bootstrap
		if b.ArtifactID == "" || b.Format != "initial_prompt_utf8/v1" || b.ContentUTF8 == "" ||
			len([]byte(b.ContentUTF8)) > 16<<10 || !utf8.ValidString(b.ContentUTF8) ||
			strings.ContainsRune(b.ContentUTF8, '\x00') ||
			sha256Text(b.ContentUTF8) != b.PayloadSHA256 {
			return errors.New("driver lifecycle ensure v3 invalid bootstrap artifact")
		}
	}
	if t.CredentialEnvelope != nil {
		c := t.CredentialEnvelope
		classes := 0
		var lower, upper, digit, symbol bool
		for _, ch := range []byte(c.SecretUTF8) {
			if ch < 33 || ch > 126 {
				return errors.New("driver lifecycle ensure v3 invalid credential secret")
			}
			switch {
			case ch >= 'a' && ch <= 'z':
				lower = true
			case ch >= 'A' && ch <= 'Z':
				upper = true
			case ch >= '0' && ch <= '9':
				digit = true
			default:
				symbol = true
			}
		}
		for _, present := range []bool{lower, upper, digit, symbol} {
			if present {
				classes++
			}
		}
		if c.EnvelopeID == "" || c.Format != "flowbee_target_bearer_utf8/v1" ||
			c.CredentialEpoch < 1 || len(c.SecretUTF8) < 32 || len(c.SecretUTF8) > 8<<10 ||
			classes < 2 || sha256Text(c.SecretUTF8) != c.PayloadSHA256 {
			return errors.New("driver lifecycle ensure v3 invalid credential envelope")
		}
	}
	if t.PresentationName != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`).MatchString(t.PresentationName) {
		return errors.New("driver lifecycle ensure v3 invalid presentation name")
	}
	return nil
}

func sha256Text(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}

func (p *HTTPPort) EnsureExternalWatch(ctx context.Context, paneID, provider, profile string) (ExternalWatch, error) {
	if len(paneID) < 2 || paneID[0] != '%' {
		return ExternalWatch{}, errors.New("driver watch bootstrap: pane_id must use %N syntax")
	}
	if _, err := strconv.Atoi(paneID[1:]); err != nil || provider == "" || profile == "" {
		return ExternalWatch{}, errors.New("driver watch bootstrap: incomplete watch policy")
	}
	in := struct {
		Target struct {
			Selector     string `json:"selector"`
			PaneID       string `json:"pane_id"`
			FollowPolicy string `json:"follow_policy"`
		} `json:"target"`
		ProviderHint string         `json:"provider_hint"`
		Profile      string         `json:"profile"`
		Requirements map[string]any `json:"requirements"`
	}{ProviderHint: provider, Profile: profile, Requirements: map[string]any{}}
	in.Target.Selector, in.Target.PaneID, in.Target.FollowPolicy = "pane_id", paneID, "exact_incarnation"
	var out struct {
		Watch struct {
			WatchID   string `json:"watch_id"`
			Enabled   bool   `json:"enabled"`
			Lifecycle string `json:"lifecycle"`
			Provider  string `json:"provider_hint"`
			Profile   string `json:"profile"`
			Target    struct {
				Selector     string `json:"selector"`
				PaneID       string `json:"pane_id"`
				FollowPolicy string `json:"follow_policy"`
			} `json:"target"`
		} `json:"watch"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/watches", "", in, &out); err != nil {
		return ExternalWatch{}, err
	}
	if err := validateCanonicalUUID(out.Watch.WatchID, "watch_id"); err != nil {
		return ExternalWatch{}, err
	}
	if !out.Watch.Enabled || out.Watch.Target.Selector != "pane_id" ||
		out.Watch.Target.PaneID != paneID || out.Watch.Target.FollowPolicy != "exact_incarnation" ||
		out.Watch.Provider != provider || out.Watch.Profile != profile {
		return ExternalWatch{}, ErrIdentityMismatch
	}
	return ExternalWatch{WatchID: out.Watch.WatchID, PaneID: paneID, Enabled: true,
		Lifecycle: out.Watch.Lifecycle, Provider: provider, Profile: profile}, nil
}

func (p *HTTPPort) AdoptSession(ctx context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	id := t.Identity
	if a.ActionID == "" || a.Epoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || t.ProfileID == "" || t.ExternalWatchID == "" ||
		id.HostID == "" || id.StoreID == "" || !tmuxServerDomainPattern.MatchString(id.TmuxServerDomainID) ||
		id.TmuxServerInstanceID == "" || id.SessionID == "" || id.PaneInstanceID == "" ||
		id.AgentRunID == "" || id.Ownership != "" {
		return LifecycleReceipt{}, errors.New("driver lifecycle adopt: incomplete external target")
	}
	target := lifecycleExternalTarget(t)
	in := struct {
		FormatVersion string                      `json:"format_version"`
		ActionID      string                      `json:"action_id"`
		ActionEpoch   int64                       `json:"action_epoch"`
		LeaseID       string                      `json:"lease_id"`
		LeaseEpoch    int64                       `json:"lease_epoch"`
		Target        lifecycleExternalTargetWire `json:"target"`
	}{"tmux-driver.lifecycle-adopt/v1", a.ActionID, a.Epoch, t.LeaseID, t.LeaseEpoch, target}
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/lifecycle/adopt", a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	if err := validateExternalLifecycleReceipt(r, t, a, "adopt"); err != nil {
		return LifecycleReceipt{}, err
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	expected := id
	expected.LifecycleKey, expected.TargetEpoch, expected.Ownership = t.LifecycleKey, t.TargetEpoch, "external_observed"
	// Adopt is an observation-preserving ownership transition: Driver returns
	// the exact pre-existing external incarnation both before and after it
	// records the lifecycle key. Requiring an empty IdentityBefore would reject
	// a valid, fenced Adopt-v1 receipt and strand the actor in verification.
	if r.Status != "adopted" || !lifecycleIdentityMatches(r.IdentityBefore, expected) ||
		!lifecycleIdentityMatches(r.IdentityAfter, expected) || r.AbsenceObservedAt != "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	return r, nil
}

func (p *HTTPPort) ReattachSession(ctx context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	id := t.Identity
	if a.ActionID == "" || a.Epoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || !identityHasExactDriverTuple(id) ||
		(id.Ownership != "driver_managed" && id.Ownership != "external_observed") ||
		(id.Ownership == "external_observed" && t.ExternalWatchID == "") ||
		id.LifecycleKey != t.LifecycleKey || id.TargetEpoch != t.TargetEpoch {
		return LifecycleReceipt{}, errors.New("driver lifecycle reattach: incomplete exact target")
	}
	type reattachRequest struct {
		FormatVersion string `json:"format_version"`
		ActionID      string `json:"action_id"`
		ActionEpoch   int64  `json:"action_epoch"`
		LeaseID       string `json:"lease_id"`
		LeaseEpoch    int64  `json:"lease_epoch"`
		Target        struct {
			LifecycleKey                 string `json:"lifecycle_key"`
			TargetEpoch                  int64  `json:"target_epoch"`
			ExpectedHostID               string `json:"expected_host_id"`
			ExpectedStoreID              string `json:"expected_store_id"`
			ExpectedTmuxServerDomainID   string `json:"expected_tmux_server_domain_id"`
			ExpectedTmuxServerInstanceID string `json:"expected_tmux_server_instance_id"`
			ExpectedSessionID            string `json:"expected_session_id"`
			ExpectedPaneInstanceID       string `json:"expected_pane_instance_id"`
			ExpectedAgentRunID           string `json:"expected_agent_run_id"`
		} `json:"target"`
	}
	in := reattachRequest{FormatVersion: "tmux-driver.lifecycle-reattach/v2", ActionID: a.ActionID,
		ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch}
	in.Target.LifecycleKey, in.Target.TargetEpoch = t.LifecycleKey, t.TargetEpoch
	in.Target.ExpectedHostID, in.Target.ExpectedStoreID = id.HostID, id.StoreID
	in.Target.ExpectedTmuxServerDomainID, in.Target.ExpectedTmuxServerInstanceID = id.TmuxServerDomainID, id.TmuxServerInstanceID
	in.Target.ExpectedSessionID, in.Target.ExpectedPaneInstanceID, in.Target.ExpectedAgentRunID = id.SessionID, id.PaneInstanceID, id.AgentRunID
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/lifecycle/reattach", a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	if r.FormatVersion != "tmux-driver.lifecycle-receipt/v2" || r.Operation != "reattach" ||
		r.ActionID != a.ActionID || r.ActionEpoch != a.Epoch || r.LeaseID != t.LeaseID ||
		r.LeaseEpoch != t.LeaseEpoch || r.LifecycleKey != t.LifecycleKey || r.TargetEpoch != t.TargetEpoch ||
		r.TmuxServerDomainID != id.TmuxServerDomainID || r.ExternalWatchID != t.ExternalWatchID {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	if r.Status != "reattached" || !lifecycleIdentityMatches(r.IdentityBefore, id) ||
		!lifecycleIdentityMatches(r.IdentityAfter, id) || r.AbsenceObservedAt != "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	return r, nil
}

func (p *HTTPPort) ReleaseSession(ctx context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	if t.Identity.Ownership != "external_observed" || t.ExternalWatchID == "" {
		return LifecycleReceipt{}, errors.New("driver lifecycle release: target is not externally adopted")
	}
	target := lifecycleExternalTarget(t)
	target.ProfileID = ""
	in := struct {
		FormatVersion string                      `json:"format_version"`
		ActionID      string                      `json:"action_id"`
		ActionEpoch   int64                       `json:"action_epoch"`
		LeaseID       string                      `json:"lease_id"`
		LeaseEpoch    int64                       `json:"lease_epoch"`
		Target        lifecycleExternalTargetWire `json:"target"`
	}{"tmux-driver.lifecycle-release/v1", a.ActionID, a.Epoch, t.LeaseID, t.LeaseEpoch, target}
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/lifecycle/release", a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	if err := validateExternalLifecycleReceipt(r, t, a, "release"); err != nil {
		return LifecycleReceipt{}, err
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	if r.Status != "released" || !lifecycleIdentityMatches(r.IdentityBefore, t.Identity) ||
		r.IdentityAfter != (Identity{}) || r.AbsenceObservedAt != "" {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	return r, nil
}

func lifecycleIdentityMatches(got, expected Identity) bool {
	return got.HostID == expected.HostID && got.StoreID == expected.StoreID &&
		got.TmuxServerDomainID == expected.TmuxServerDomainID &&
		got.TmuxServerInstanceID == expected.TmuxServerInstanceID && got.Ownership == expected.Ownership &&
		got.LifecycleKey == expected.LifecycleKey && got.TargetEpoch == expected.TargetEpoch &&
		got.SessionID == expected.SessionID && got.PaneInstanceID == expected.PaneInstanceID &&
		got.AgentRunID == expected.AgentRunID
}

type lifecycleExternalTargetWire struct {
	LifecycleKey                 string `json:"lifecycle_key"`
	TargetEpoch                  int64  `json:"target_epoch"`
	ExpectedHostID               string `json:"expected_host_id"`
	ExpectedStoreID              string `json:"expected_store_id"`
	ExpectedTmuxServerDomainID   string `json:"expected_tmux_server_domain_id"`
	ExpectedTmuxServerInstanceID string `json:"expected_tmux_server_instance_id"`
	ExpectedSessionID            string `json:"expected_session_id"`
	ExpectedPaneInstanceID       string `json:"expected_pane_instance_id"`
	ExpectedAgentRunID           string `json:"expected_agent_run_id"`
	ProfileID                    string `json:"profile_id,omitempty"`
	ExternalWatchID              string `json:"external_watch_id"`
}

func lifecycleExternalTarget(t SessionTarget) lifecycleExternalTargetWire {
	id := t.Identity
	return lifecycleExternalTargetWire{LifecycleKey: t.LifecycleKey, TargetEpoch: t.TargetEpoch,
		ExpectedHostID: id.HostID, ExpectedStoreID: id.StoreID,
		ExpectedTmuxServerDomainID:   id.TmuxServerDomainID,
		ExpectedTmuxServerInstanceID: id.TmuxServerInstanceID,
		ExpectedSessionID:            id.SessionID, ExpectedPaneInstanceID: id.PaneInstanceID,
		ExpectedAgentRunID: id.AgentRunID, ProfileID: t.ProfileID, ExternalWatchID: t.ExternalWatchID}
}

func validateExternalLifecycleReceipt(r LifecycleReceipt, t SessionTarget, a Action, operation string) error {
	if r.FormatVersion != "tmux-driver.lifecycle-receipt/v2" || r.Operation != operation ||
		r.Status == "" || r.ActionID != a.ActionID || r.ActionEpoch != a.Epoch ||
		r.LeaseID != t.LeaseID || r.LeaseEpoch != t.LeaseEpoch ||
		r.LifecycleKey != t.LifecycleKey || r.TargetEpoch != t.TargetEpoch ||
		r.TmuxServerDomainID != t.Identity.TmuxServerDomainID || r.ExternalWatchID != t.ExternalWatchID {
		return ErrIdentityMismatch
	}
	return nil
}

func (p *HTTPPort) StopSession(ctx context.Context, t SessionTarget, a Action) (LifecycleReceipt, error) {
	id := t.Identity
	if a.ActionID == "" || a.Epoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 ||
		t.LifecycleKey == "" || t.TargetEpoch < 1 || id.HostID == "" || id.StoreID == "" ||
		id.TmuxServerDomainID == "" || id.TmuxServerInstanceID == "" || id.SessionID == "" || id.PaneInstanceID == "" || id.AgentRunID == "" {
		return LifecycleReceipt{}, errors.New("driver lifecycle stop: incomplete fenced target")
	}
	type stopRequest struct {
		FormatVersion string `json:"format_version"`
		ActionID      string `json:"action_id"`
		ActionEpoch   int64  `json:"action_epoch"`
		LeaseID       string `json:"lease_id"`
		LeaseEpoch    int64  `json:"lease_epoch"`
		Target        struct {
			LifecycleKey                 string `json:"lifecycle_key"`
			TargetEpoch                  int64  `json:"target_epoch"`
			ExpectedHostID               string `json:"expected_host_id"`
			ExpectedStoreID              string `json:"expected_store_id"`
			ExpectedTmuxServerDomainID   string `json:"expected_tmux_server_domain_id"`
			ExpectedTmuxServerInstanceID string `json:"expected_tmux_server_instance_id"`
			ExpectedSessionID            string `json:"expected_session_id"`
			ExpectedPaneInstanceID       string `json:"expected_pane_instance_id"`
			ExpectedAgentRunID           string `json:"expected_agent_run_id"`
		} `json:"target"`
	}
	in := stopRequest{FormatVersion: "tmux-driver.lifecycle-stop/v2", ActionID: a.ActionID,
		ActionEpoch: a.Epoch, LeaseID: t.LeaseID, LeaseEpoch: t.LeaseEpoch}
	in.Target.LifecycleKey, in.Target.TargetEpoch = t.LifecycleKey, t.TargetEpoch
	in.Target.ExpectedHostID, in.Target.ExpectedStoreID = id.HostID, id.StoreID
	in.Target.ExpectedTmuxServerDomainID = id.TmuxServerDomainID
	in.Target.ExpectedTmuxServerInstanceID = id.TmuxServerInstanceID
	in.Target.ExpectedSessionID, in.Target.ExpectedPaneInstanceID = id.SessionID, id.PaneInstanceID
	in.Target.ExpectedAgentRunID = id.AgentRunID
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/lifecycle/stop", a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	// A target created with Ensure-v3 retains that receipt family when Driver
	// stops it. Stop itself is v2-shaped, but v3 is a valid terminal receipt
	// for a v3-managed incarnation.
	if (r.FormatVersion != "tmux-driver.lifecycle-receipt/v2" && r.FormatVersion != "tmux-driver.lifecycle-receipt/v3") || r.ActionID != a.ActionID ||
		r.ActionEpoch != a.Epoch || r.LifecycleKey != t.LifecycleKey ||
		r.TmuxServerDomainID != id.TmuxServerDomainID || r.TargetEpoch != t.TargetEpoch {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	return r, nil
}

func (p *HTTPPort) VerifyLifecycleEffect(ctx context.Context, receiptID string, t SessionTarget, a Action) (LifecycleReceipt, error) {
	if receiptID == "" || a.ActionID == "" || a.Epoch < 1 || t.LeaseID == "" || t.LeaseEpoch < 1 {
		return LifecycleReceipt{}, errors.New("driver lifecycle verify: incomplete claimant fence")
	}
	in := struct {
		FormatVersion string `json:"format_version"`
		ActionID      string `json:"action_id"`
		ActionEpoch   int64  `json:"action_epoch"`
		LeaseID       string `json:"lease_id"`
		LeaseEpoch    int64  `json:"lease_epoch"`
	}{"tmux-driver.lifecycle-verify/v1", a.ActionID, a.Epoch, t.LeaseID, t.LeaseEpoch}
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	path := "/v2/lifecycle/receipts/" + url.PathEscape(receiptID) + "/verify"
	if err := p.call(ctx, http.MethodPost, path, a.ActionID, in, &out); err != nil {
		return LifecycleReceipt{}, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	if r.FormatVersion != "tmux-driver.lifecycle-receipt/v2" ||
		r.TmuxServerDomainID != t.Identity.TmuxServerDomainID ||
		r.LifecycleReceiptID != receiptID || r.ActionID != a.ActionID ||
		r.ActionEpoch != a.Epoch || r.LeaseID != t.LeaseID || r.LeaseEpoch != t.LeaseEpoch ||
		r.LifecycleKey != t.LifecycleKey || r.TargetEpoch != t.TargetEpoch {
		return LifecycleReceipt{}, ErrIdentityMismatch
	}
	if r.Uncertain() {
		return r, ErrUncertain
	}
	return r, nil
}

func (p *HTTPPort) LifecycleTargetPresence(ctx context.Context, lifecycleKey string, targetEpoch int64) (LifecyclePresence, error) {
	if lifecycleKey == "" || targetEpoch < 1 {
		return LifecyclePresence{}, errors.New("driver lifecycle presence: incomplete target fence")
	}
	var out struct {
		Presence       string                 `json:"presence"`
		Identity       *lifecycleIdentityWire `json:"identity"`
		ObservedAt     *string                `json:"observed_at"`
		DiagnosticCode *string                `json:"diagnostic_code"`
	}
	q := url.Values{"target_epoch": []string{strconv.FormatInt(targetEpoch, 10)}}
	path := "/v2/lifecycle/targets/" + url.PathEscape(lifecycleKey) + "?" + q.Encode()
	if err := p.call(ctx, http.MethodGet, path, "", nil, &out); err != nil {
		return LifecyclePresence{}, err
	}
	presence := LifecyclePresence{Presence: out.Presence}
	if out.Identity != nil {
		presence.Identity = wireIdentity(*out.Identity)
		if presence.Identity.LifecycleKey != lifecycleKey ||
			out.Presence == "present" && presence.Identity.TargetEpoch != targetEpoch {
			return LifecyclePresence{}, ErrIdentityMismatch
		}
	}
	if out.ObservedAt != nil {
		presence.ObservedAt = *out.ObservedAt
	}
	if out.DiagnosticCode != nil {
		presence.DiagnosticCode = *out.DiagnosticCode
	}
	if presence.Presence != "present" && presence.Presence != "absent" &&
		presence.Presence != "mismatch" && presence.Presence != "unknown" {
		return LifecyclePresence{}, errors.New("driver lifecycle presence: invalid state")
	}
	return presence, nil
}

func (p *HTTPPort) LifecycleReceiptByAction(ctx context.Context, actionID, lifecycleKey string, targetEpoch int64) (LifecycleReceipt, bool, error) {
	q := url.Values{"lifecycle_key": []string{lifecycleKey}, "target_epoch": []string{strconv.FormatInt(targetEpoch, 10)}}
	var out struct {
		Receipt lifecycleReceiptWire `json:"receipt"`
	}
	err := p.call(ctx, http.MethodGet, "/v2/lifecycle/receipts/by-action/"+url.PathEscape(actionID)+"?"+q.Encode(), "", nil, &out)
	if err != nil {
		var h *HTTPError
		if errors.As(err, &h) && h.Status == http.StatusNotFound {
			return LifecycleReceipt{}, false, nil
		}
		return LifecycleReceipt{}, false, err
	}
	r := wireLifecycleReceipt(out.Receipt)
	if (r.FormatVersion != "tmux-driver.lifecycle-receipt/v2" && r.FormatVersion != "tmux-driver.lifecycle-receipt/v3") || r.TmuxServerDomainID == "" ||
		r.ActionID != actionID || r.LifecycleKey != lifecycleKey || r.TargetEpoch != targetEpoch {
		return LifecycleReceipt{}, false, ErrIdentityMismatch
	}
	return r, true, nil
}

func (p *HTTPPort) Grant(ctx context.Context, g Grant) error {
	if err := validateGrantRequest(g); err != nil {
		return err
	}
	// Grant creation has no Driver idempotency key.  A Flowbee crash after the
	// grant was committed must therefore reconcile the exact durable grant by
	// ID before attempting creation; it must not turn recovery into a second
	// route mutation.
	if existing, ok, err := p.grantByID(ctx, g.GrantID); err != nil {
		return err
	} else if ok {
		return validateProjectedGrant(existing, g, false)
	}
	common := struct {
		GrantID                     string `json:"grant_id,omitempty"`
		SenderPrincipalID           string `json:"sender_principal_id,omitempty"`
		SenderSessionID             string `json:"sender_session_id,omitempty"`
		SenderAgentRunID            string `json:"sender_agent_run_id,omitempty"`
		RecipientSessionID          string `json:"recipient_session_id"`
		RecipientPaneInstanceID     string `json:"recipient_pane_instance_id"`
		ExpectedRecipientAgentRunID string `json:"expected_recipient_agent_run_id,omitempty"`
		Epoch                       int64  `json:"epoch"`
		MaximumPayloadBytes         int    `json:"maximum_payload_bytes,omitempty"`
		AllowDraftStash             bool   `json:"allow_draft_stash,omitempty"`
		ExpiresAt                   string `json:"expires_at,omitempty"`
	}{GrantID: g.GrantID, SenderPrincipalID: g.SenderPrincipalID,
		SenderSessionID: g.SenderSessionID, SenderAgentRunID: g.SenderAgentRunID,
		RecipientSessionID: g.RecipientSessionID, RecipientPaneInstanceID: g.RecipientPaneInstanceID,
		ExpectedRecipientAgentRunID: g.ExpectedRecipientAgentRunID, Epoch: g.Epoch,
		MaximumPayloadBytes: g.MaximumPayloadBytes, AllowDraftStash: g.AllowDraftStash,
		ExpiresAt: g.ExpiresAt}
	var out struct {
		Grant routeGrantWire `json:"grant"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/routes/grants", "", common, &out); err != nil {
		// Close the GET/POST race against another recovery owner. Driver may
		// report duplicate grant creation as a control-ledger failure; only an
		// exact subsequently readable grant converts that failure to success.
		if existing, ok, lookupErr := p.grantByID(ctx, g.GrantID); lookupErr == nil && ok {
			return validateProjectedGrant(existing, g, false)
		}
		return err
	}
	return validateProjectedGrant(out.Grant, g, false)
}

func (p *HTTPPort) grantByID(ctx context.Context, id string) (routeGrantWire, bool, error) {
	var out struct {
		Grant routeGrantWire `json:"grant"`
	}
	err := p.call(ctx, http.MethodGet, "/v2/routes/grants/"+url.PathEscape(id), "", nil, &out)
	if err != nil {
		var h *HTTPError
		if errors.As(err, &h) && h.Status == http.StatusNotFound {
			return routeGrantWire{}, false, nil
		}
		return routeGrantWire{}, false, err
	}
	if err := validateRouteGrantWire(out.Grant); err != nil {
		return routeGrantWire{}, false, err
	}
	if out.Grant.GrantID != id {
		return routeGrantWire{}, false, ErrIdentityMismatch
	}
	return out.Grant, true, nil
}

func (p *HTTPPort) RevokeGrant(ctx context.Context, id string, epoch int64) error {
	if err := validateCanonicalUUID(id, "grant_id"); err != nil || epoch < 1 {
		return errors.New("driver revoke grant: incomplete grant fence")
	}
	var out struct {
		Grant routeGrantWire `json:"grant"`
	}
	if err := p.call(ctx, http.MethodDelete, "/v2/routes/grants/"+url.PathEscape(id), "", nil, &out); err != nil {
		return err
	}
	if out.Grant.GrantID != id || out.Grant.Epoch != epoch || out.Grant.RevokedAt == nil || *out.Grant.RevokedAt == "" {
		return ErrIdentityMismatch
	}
	return validateRouteGrantWire(out.Grant)
}

type deliveryReceiptWire struct {
	FormatVersion               string        `json:"format_version"`
	DeliveryID                  string        `json:"delivery_id"`
	ActionID                    string        `json:"action_id"`
	GrantID                     string        `json:"grant_id"`
	GrantEpoch                  int64         `json:"grant_epoch"`
	SenderPrincipalID           string        `json:"sender_principal_id,omitempty"`
	SenderSessionID             string        `json:"sender_session_id"`
	SenderAgentRunID            string        `json:"sender_agent_run_id"`
	RecipientSessionID          string        `json:"recipient_session_id"`
	RecipientPaneInstanceID     string        `json:"recipient_pane_instance_id"`
	ExpectedRecipientAgentRunID string        `json:"expected_recipient_agent_run_id,omitempty"`
	PayloadSHA256               string        `json:"payload_sha256"`
	PayloadBytes                int           `json:"payload_bytes"`
	PayloadMediaType            string        `json:"payload_media_type"`
	RequestFingerprint          string        `json:"request_fingerprint"`
	Status                      ReceiptStatus `json:"status"`
	CompatibilityCode           *int          `json:"compatibility_code"`
	Verification                *string       `json:"verification"`
	PaneHashBefore              *string       `json:"pane_hash_before"`
	PaneHashAfter               *string       `json:"pane_hash_after"`
	EnterAttempts               int           `json:"enter_attempts"`
	AcceptedAt                  string        `json:"accepted_at"`
	CompletedAt                 *string       `json:"completed_at"`
	DiagnosticCode              *string       `json:"diagnostic_code"`
}

func (w *deliveryReceiptWire) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	// Use explicit wire structs so a session object carrying a principal field,
	// or a control object carrying null session placeholders, is rejected.
	var decoded deliveryReceiptWire
	switch discriminator.FormatVersion {
	case sessionDeliveryReceiptFormat:
		if err := requireExactWireKeys(data, deliveryReceiptRequiredKeys("sender_session_id", "sender_agent_run_id")); err != nil {
			return err
		}
		var value struct {
			FormatVersion           string        `json:"format_version"`
			DeliveryID              string        `json:"delivery_id"`
			ActionID                string        `json:"action_id"`
			GrantID                 string        `json:"grant_id"`
			GrantEpoch              int64         `json:"grant_epoch"`
			SenderSessionID         string        `json:"sender_session_id"`
			SenderAgentRunID        string        `json:"sender_agent_run_id"`
			RecipientSessionID      string        `json:"recipient_session_id"`
			RecipientPaneInstanceID string        `json:"recipient_pane_instance_id"`
			PayloadSHA256           string        `json:"payload_sha256"`
			PayloadBytes            int           `json:"payload_bytes"`
			PayloadMediaType        string        `json:"payload_media_type"`
			RequestFingerprint      string        `json:"request_fingerprint"`
			Status                  ReceiptStatus `json:"status"`
			CompatibilityCode       *int          `json:"compatibility_code"`
			Verification            *string       `json:"verification"`
			PaneHashBefore          *string       `json:"pane_hash_before"`
			PaneHashAfter           *string       `json:"pane_hash_after"`
			EnterAttempts           int           `json:"enter_attempts"`
			AcceptedAt              string        `json:"accepted_at"`
			CompletedAt             *string       `json:"completed_at"`
			DiagnosticCode          *string       `json:"diagnostic_code"`
		}
		if err := decodeStrictWire(data, &value); err != nil {
			return err
		}
		decoded = deliveryReceiptWire{FormatVersion: value.FormatVersion, DeliveryID: value.DeliveryID,
			ActionID: value.ActionID, GrantID: value.GrantID, GrantEpoch: value.GrantEpoch,
			SenderSessionID: value.SenderSessionID, SenderAgentRunID: value.SenderAgentRunID,
			RecipientSessionID: value.RecipientSessionID, RecipientPaneInstanceID: value.RecipientPaneInstanceID,
			PayloadSHA256: value.PayloadSHA256, PayloadBytes: value.PayloadBytes,
			PayloadMediaType: value.PayloadMediaType, RequestFingerprint: value.RequestFingerprint,
			Status: value.Status, CompatibilityCode: value.CompatibilityCode, Verification: value.Verification,
			PaneHashBefore: value.PaneHashBefore, PaneHashAfter: value.PaneHashAfter,
			EnterAttempts: value.EnterAttempts, AcceptedAt: value.AcceptedAt,
			CompletedAt: value.CompletedAt, DiagnosticCode: value.DiagnosticCode}
	case controlDeliveryReceiptFormat:
		if err := requireExactWireKeys(data, deliveryReceiptRequiredKeys("sender_principal_id", "expected_recipient_agent_run_id")); err != nil {
			return err
		}
		var value struct {
			FormatVersion               string        `json:"format_version"`
			DeliveryID                  string        `json:"delivery_id"`
			ActionID                    string        `json:"action_id"`
			GrantID                     string        `json:"grant_id"`
			GrantEpoch                  int64         `json:"grant_epoch"`
			SenderPrincipalID           string        `json:"sender_principal_id"`
			RecipientSessionID          string        `json:"recipient_session_id"`
			RecipientPaneInstanceID     string        `json:"recipient_pane_instance_id"`
			ExpectedRecipientAgentRunID string        `json:"expected_recipient_agent_run_id"`
			PayloadSHA256               string        `json:"payload_sha256"`
			PayloadBytes                int           `json:"payload_bytes"`
			PayloadMediaType            string        `json:"payload_media_type"`
			RequestFingerprint          string        `json:"request_fingerprint"`
			Status                      ReceiptStatus `json:"status"`
			CompatibilityCode           *int          `json:"compatibility_code"`
			Verification                *string       `json:"verification"`
			PaneHashBefore              *string       `json:"pane_hash_before"`
			PaneHashAfter               *string       `json:"pane_hash_after"`
			EnterAttempts               int           `json:"enter_attempts"`
			AcceptedAt                  string        `json:"accepted_at"`
			CompletedAt                 *string       `json:"completed_at"`
			DiagnosticCode              *string       `json:"diagnostic_code"`
		}
		if err := decodeStrictWire(data, &value); err != nil {
			return err
		}
		decoded = deliveryReceiptWire{FormatVersion: value.FormatVersion, DeliveryID: value.DeliveryID,
			ActionID: value.ActionID, GrantID: value.GrantID, GrantEpoch: value.GrantEpoch,
			SenderPrincipalID: value.SenderPrincipalID, RecipientSessionID: value.RecipientSessionID,
			RecipientPaneInstanceID:     value.RecipientPaneInstanceID,
			ExpectedRecipientAgentRunID: value.ExpectedRecipientAgentRunID,
			PayloadSHA256:               value.PayloadSHA256,
			PayloadBytes:                value.PayloadBytes, PayloadMediaType: value.PayloadMediaType,
			RequestFingerprint: value.RequestFingerprint, Status: value.Status,
			CompatibilityCode: value.CompatibilityCode, Verification: value.Verification,
			PaneHashBefore: value.PaneHashBefore, PaneHashAfter: value.PaneHashAfter,
			EnterAttempts: value.EnterAttempts, AcceptedAt: value.AcceptedAt,
			CompletedAt: value.CompletedAt, DiagnosticCode: value.DiagnosticCode}
	default:
		return fmt.Errorf("driver delivery receipt: unsupported format %q", discriminator.FormatVersion)
	}
	*w = decoded
	return nil
}

func deliveryReceiptRequiredKeys(origin ...string) []string {
	keys := []string{"format_version", "delivery_id", "action_id", "grant_id", "grant_epoch"}
	keys = append(keys, origin...)
	return append(keys, "recipient_session_id", "recipient_pane_instance_id", "payload_sha256",
		"payload_bytes", "payload_media_type", "request_fingerprint", "status", "compatibility_code",
		"verification", "pane_hash_before", "pane_hash_after", "enter_attempts", "accepted_at",
		"completed_at", "diagnostic_code")
}

func wireReceipt(w deliveryReceiptWire) Receipt {
	compat := 0
	if w.CompatibilityCode != nil {
		compat = *w.CompatibilityCode
	}
	diagnostic := ""
	if w.DiagnosticCode != nil {
		diagnostic = *w.DiagnosticCode
	}
	return Receipt{DeliveryID: w.DeliveryID, ActionID: w.ActionID, GrantID: w.GrantID, GrantEpoch: w.GrantEpoch,
		Sender:                      Identity{SessionID: w.SenderSessionID, AgentRunID: w.SenderAgentRunID},
		SenderPrincipalID:           w.SenderPrincipalID,
		Recipient:                   Identity{SessionID: w.RecipientSessionID, PaneInstanceID: w.RecipientPaneInstanceID},
		ExpectedRecipientAgentRunID: w.ExpectedRecipientAgentRunID,
		PayloadSHA256:               w.PayloadSHA256, Status: w.Status, CompatibilityCode: compat, DiagnosticCode: diagnostic}
}

func (p *HTTPPort) Send(ctx context.Context, r SendRequest) (Receipt, error) {
	if r.SenderPrincipalID != "" && (r.SenderSessionID != "" || r.SenderAgentRunID != "" || r.OnBehalfOfSessionID != "") {
		return Receipt{}, errors.New("driver message: mixed control origin")
	}
	if r.SenderPrincipalID != "" {
		if r.ExpectedRecipientAgentRunID == "" {
			return Receipt{}, errors.New("driver message: missing recipient agent-run fence")
		}
		if err := validatePrincipalID(r.SenderPrincipalID, "sender_principal_id"); err != nil {
			return Receipt{}, err
		}
		if err := validateCanonicalUUID(r.ExpectedRecipientAgentRunID, "expected_recipient_agent_run_id"); err != nil {
			return Receipt{}, err
		}
	} else if r.ExpectedRecipientAgentRunID != "" {
		return Receipt{}, errors.New("driver message: session origin carries control run fence")
	}
	in := struct {
		GrantID            string `json:"grant_id"`
		RecipientSessionID string `json:"recipient_session_id"`
		GrantEpoch         int64  `json:"grant_epoch"`
		ActionID           string `json:"action_id"`
		Payload            struct {
			MediaType string `json:"media_type"`
			Text      string `json:"text"`
		} `json:"payload"`
		PayloadSHA256               string `json:"payload_sha256"`
		OnBehalfOfSessionID         string `json:"on_behalf_of_session_id,omitempty"`
		ExpectedRecipientAgentRunID string `json:"expected_recipient_agent_run_id,omitempty"`
	}{GrantID: r.GrantID, RecipientSessionID: r.RecipientSessionID, GrantEpoch: r.GrantEpoch,
		ActionID: r.ActionID, PayloadSHA256: r.PayloadSHA256,
		OnBehalfOfSessionID:         r.OnBehalfOfSessionID,
		ExpectedRecipientAgentRunID: r.ExpectedRecipientAgentRunID}
	in.Payload.MediaType, in.Payload.Text = "text/plain; charset=utf-8", r.Payload
	var out struct {
		Receipt deliveryReceiptWire `json:"receipt"`
	}
	if err := p.call(ctx, http.MethodPost, "/v2/messages", r.ActionID, in, &out); err != nil {
		return Receipt{}, err
	}
	receipt := wireReceipt(out.Receipt)
	if err := validateDeliveryReceiptWire(out.Receipt); err != nil {
		return Receipt{}, err
	}
	if receipt.ActionID != r.ActionID || receipt.GrantID != r.GrantID ||
		receipt.GrantEpoch != r.GrantEpoch || receipt.PayloadSHA256 != r.PayloadSHA256 ||
		out.Receipt.PayloadBytes != len([]byte(r.Payload)) ||
		receipt.Recipient.SessionID != r.RecipientSessionID ||
		receipt.Recipient.PaneInstanceID != r.RecipientPaneInstanceID ||
		receipt.ExpectedRecipientAgentRunID != r.ExpectedRecipientAgentRunID ||
		r.SenderPrincipalID != receipt.SenderPrincipalID ||
		r.SenderPrincipalID == "" && r.SenderSessionID != "" && receipt.Sender.SessionID != r.SenderSessionID ||
		r.OnBehalfOfSessionID != "" && receipt.Sender.SessionID != r.OnBehalfOfSessionID ||
		r.SenderPrincipalID == "" && r.SenderAgentRunID != "" && receipt.Sender.AgentRunID != r.SenderAgentRunID {
		return Receipt{}, ErrIdentityMismatch
	}
	return receipt, nil
}

func (p *HTTPPort) ReceiptByAction(ctx context.Context, expected ReceiptExpectation) (Receipt, bool, error) {
	if err := expected.Validate(Receipt{DeliveryID: "expectation-probe", ActionID: expected.ActionID,
		GrantID: expected.GrantID, GrantEpoch: expected.GrantEpoch,
		Sender:                      Identity{SessionID: expected.SenderSessionID, AgentRunID: expected.SenderAgentRunID},
		SenderPrincipalID:           expected.SenderPrincipalID,
		Recipient:                   Identity{SessionID: expected.RecipientSessionID, PaneInstanceID: expected.RecipientPaneInstanceID},
		ExpectedRecipientAgentRunID: expected.ExpectedRecipientAgentRunID,
		PayloadSHA256:               expected.PayloadSHA256}); err != nil {
		return Receipt{}, false, err
	}
	q := url.Values{"grant_epoch": []string{strconv.FormatInt(expected.GrantEpoch, 10)}}
	if expected.SenderSessionID != "" {
		q.Set("sender_session_id", expected.SenderSessionID)
	}
	var out struct {
		Receipt deliveryReceiptWire `json:"receipt"`
	}
	err := p.call(ctx, http.MethodGet, "/v2/messages/receipts/by-action/"+url.PathEscape(expected.ActionID)+"?"+q.Encode(), "", nil, &out)
	if err != nil {
		var h *HTTPError
		if errors.As(err, &h) && h.Status == http.StatusNotFound {
			return Receipt{}, false, nil
		}
		return Receipt{}, false, err
	}
	if err := validateDeliveryReceiptWire(out.Receipt); err != nil {
		return Receipt{}, false, err
	}
	receipt := wireReceipt(out.Receipt)
	if err := expected.Validate(receipt); err != nil {
		return Receipt{}, false, err
	}
	return receipt, true, nil
}

func (p *HTTPPort) Observe(ctx context.Context, after string) (ObservationBatch, error) {
	q := url.Values{"limit": []string{"500"}, "view": []string{"effective"}}
	if after != "" {
		q.Set("after", after)
	}
	var out struct {
		Events                 []json.RawMessage `json:"events"`
		NextCursor             string            `json:"next_cursor"`
		DurableHighWaterCursor string            `json:"durable_high_water_cursor"`
		HistoryComplete        bool              `json:"history_complete"`
		View                   string            `json:"view"`
	}
	err := p.call(ctx, http.MethodGet, "/v2/events?"+q.Encode(), "", nil, &out)
	if err != nil {
		var h *HTTPError
		if errors.As(err, &h) && h.Status == http.StatusGone && h.Code == "cursor_expired" {
			return ObservationBatch{CursorGap: true}, nil
		}
		return ObservationBatch{}, err
	}
	if out.NextCursor == "" || out.DurableHighWaterCursor == "" || out.View != "effective" {
		return ObservationBatch{}, errors.New("driver observations: incomplete event page")
	}
	b := ObservationBatch{NextCursor: out.NextCursor, DurableHighWaterCursor: out.DurableHighWaterCursor,
		HistoryComplete: out.HistoryComplete}
	for _, raw := range out.Events {
		event, err := decodeObservation(raw)
		if err != nil {
			return ObservationBatch{}, err
		}
		if b.StoreID == "" {
			b.StoreID = event.Identity.StoreID
		} else if b.StoreID != event.Identity.StoreID {
			return ObservationBatch{}, errors.New("driver observations: mixed store_id in event page")
		}
		b.Events = append(b.Events, event)
		if event.StoreSeq > b.StoreSeq {
			b.StoreSeq = event.StoreSeq
		}
	}
	return b, nil
}

func decodeObservation(raw json.RawMessage) (Observation, error) {
	type wire struct {
		SpecVersion     string          `json:"spec_version"`
		EventID         string          `json:"event_id"`
		StoreID         string          `json:"store_id"`
		Cursor          string          `json:"cursor"`
		StoreSeq        uint64          `json:"store_seq"`
		SessionSeq      uint64          `json:"session_seq"`
		TransitionID    string          `json:"transition_id"`
		TransitionIndex int             `json:"transition_index"`
		TransitionCount int             `json:"transition_count"`
		HostID          string          `json:"host_id"`
		SessionID       string          `json:"session_id"`
		PaneInstanceID  string          `json:"pane_instance_id"`
		ProducerBootID  string          `json:"producer_boot_id"`
		Kind            string          `json:"kind"`
		ObservedAt      string          `json:"observed_at"`
		SourceAt        *string         `json:"source_at"`
		Historical      bool            `json:"historical"`
		Source          json.RawMessage `json:"source"`
		Correlation     json.RawMessage `json:"correlation"`
		CausedBy        []string        `json:"caused_by"`
		Payload         json.RawMessage `json:"payload"`
	}
	var in wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return Observation{}, fmt.Errorf("driver observation envelope: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Observation{}, errors.New("driver observation envelope: trailing JSON value")
	}
	sourceAt := ""
	if in.SourceAt != nil {
		sourceAt = *in.SourceAt
	}
	event := Observation{SpecVersion: in.SpecVersion, EventID: in.EventID, Cursor: in.Cursor,
		StoreSeq: in.StoreSeq, SessionSeq: in.SessionSeq, TransitionID: in.TransitionID,
		TransitionIndex: in.TransitionIndex, TransitionCount: in.TransitionCount,
		ProducerBootID: in.ProducerBootID, Kind: in.Kind, ObservedAt: in.ObservedAt,
		SourceAt: sourceAt, Historical: in.Historical,
		Identity: Identity{HostID: in.HostID, StoreID: in.StoreID, SessionID: in.SessionID,
			PaneInstanceID: in.PaneInstanceID, StateCursor: in.Cursor},
		Source: append(json.RawMessage(nil), in.Source...), Correlation: append(json.RawMessage(nil), in.Correlation...),
		CausedBy: append([]string(nil), in.CausedBy...), Payload: append(json.RawMessage(nil), in.Payload...),
		Envelope: append(json.RawMessage(nil), raw...)}
	var identityFields struct {
		AgentRunID           string `json:"agent_run_id"`
		TmuxServerInstanceID string `json:"tmux_server_instance_id"`
		Provider             string `json:"provider"`
		ConversationID       string `json:"conversation_id"`
	}
	_ = json.Unmarshal(in.Payload, &identityFields)
	event.Identity.AgentRunID = identityFields.AgentRunID
	event.Identity.TmuxServerInstanceID = identityFields.TmuxServerInstanceID
	event.Identity.Provider = identityFields.Provider
	event.Identity.ConversationID = identityFields.ConversationID
	if err := ValidateObservation(event); err != nil {
		return Observation{}, err
	}
	return event, nil
}

func ValidateObservation(event Observation) error {
	if event.SpecVersion != "tmux-driver.events/v2" || event.EventID == "" || event.Identity.StoreID == "" ||
		event.Cursor == "" || !strings.HasPrefix(event.Cursor, "tdc2.") || event.StoreSeq < 1 ||
		event.SessionSeq < 1 || event.TransitionID == "" || event.TransitionCount < 1 ||
		event.TransitionIndex < 0 || event.TransitionIndex >= event.TransitionCount ||
		event.Identity.HostID == "" || event.Identity.SessionID == "" || event.Identity.PaneInstanceID == "" ||
		event.ProducerBootID == "" || event.Kind == "" || event.ObservedAt == "" {
		return errors.New("driver observation envelope: missing or invalid required field")
	}
	for name, raw := range map[string]json.RawMessage{"source": event.Source, "correlation": event.Correlation, "payload": event.Payload} {
		var object map[string]json.RawMessage
		if len(raw) == 0 || json.Unmarshal(raw, &object) != nil || object == nil {
			return fmt.Errorf("driver observation envelope: %s must be an object", name)
		}
	}
	seen := make(map[string]struct{}, len(event.CausedBy))
	for _, id := range event.CausedBy {
		if id == "" || id == event.EventID {
			return errors.New("driver observation envelope: invalid caused_by")
		}
		if _, exists := seen[id]; exists {
			return errors.New("driver observation envelope: duplicate caused_by")
		}
		seen[id] = struct{}{}
	}
	return nil
}
