// Package driver is Flowbee's replaceable boundary to tmux-driver v2.4.
// It contains no tmux implementation: production uses an adapter, while tests use
// FakePort and contract fixtures.
package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrGrantDenied      = errors.New("driver route grant denied")
	ErrIdentityMismatch = errors.New("driver identity mismatch")
	ErrIdempotencyBody  = errors.New("driver idempotency body mismatch")
	ErrUncertain        = errors.New("driver delivery uncertain")
	ErrStaleActionEpoch = errors.New("stale driver action epoch")
)

// Identity is the only durable join Flowbee may persist for a managed session.
type Identity struct {
	HostID               string `json:"host_id"`
	StoreID              string `json:"store_id"`
	TmuxServerDomainID   string `json:"tmux_server_domain_id"`
	TmuxServerInstanceID string `json:"tmux_server_instance_id"`
	Ownership            string `json:"ownership,omitempty"`
	LifecycleKey         string `json:"lifecycle_key"`
	TargetEpoch          int64  `json:"target_epoch"`
	SessionID            string `json:"session_id"`
	PaneInstanceID       string `json:"pane_instance_id"`
	AgentRunID           string `json:"agent_run_id"`
	Provider             string `json:"provider,omitempty"`
	ConversationID       string `json:"conversation_id,omitempty"`
	StateCursor          string `json:"state_cursor,omitempty"`
}

type SessionTarget struct {
	Identity              Identity
	LifecycleKey          string
	TargetEpoch           int64
	ProfileID             string
	WorkspaceRootID       string
	WorkspaceRelativePath string
	LeaseID               string
	LeaseEpoch            int64
	ExternalWatchID       string
	Role                  string // legacy alias for ProfileID; production requires ProfileID.
}

type Grant struct {
	GrantID string `json:"grant_id"`
	// Exactly one origin variant is permitted. Control-plane product actions use
	// SenderPrincipalID; the session pair remains only for legacy/session A->B
	// compatibility at this low-level boundary.
	SenderPrincipalID           string `json:"sender_principal_id,omitempty"`
	SenderSessionID             string `json:"sender_session_id"`
	SenderAgentRunID            string `json:"sender_agent_run_id"`
	RecipientSessionID          string `json:"recipient_session_id"`
	RecipientPaneInstanceID     string `json:"recipient_pane_instance_id"`
	ExpectedRecipientAgentRunID string `json:"expected_recipient_agent_run_id,omitempty"`
	Epoch                       int64  `json:"epoch"`
	MaximumPayloadBytes         int    `json:"maximum_payload_bytes,omitempty"`
	AllowDraftStash             bool   `json:"allow_draft_stash,omitempty"`
	ExpiresAt                   string `json:"expires_at,omitempty"`
}

type Action struct {
	ActionID      string `json:"action_id"`
	Payload       string `json:"payload"`
	PayloadSHA256 string `json:"payload_sha256"`
	Epoch         int64  `json:"action_epoch"`
	// EvidenceBaselineStoreSeq and EvidenceBaselineUncertaintyEpoch are captured
	// atomically with action creation.  They are Flowbee-owned evidence fences,
	// never fields on Driver's message request.
	EvidenceBaselineStoreSeq         uint64 `json:"-"`
	EvidenceBaselineUncertaintyEpoch uint64 `json:"-"`
	// The remaining fields are Flowbee-owned durable routing metadata. They are
	// deliberately absent from Driver's wire body, but let the SQL committer bind
	// the immutable transport effect to its project, epic, and artifact.
	ProjectID, EpicID, Kind, DedupKey string
	HeadSHA, BaseSHA                  string
	ExecutorKind                      string
	TargetRole                        string
	InstanceRef                       string
	TargetHostID                      string
	TargetStoreID                     string
	TargetServerDomainID              string
	TargetServerID                    string
	TargetLifecycleOwnership          string
	LifecycleKey                      string
	TargetEpoch                       int64
	ProfileID                         string
	WorkspaceRootID                   string
	WorkspaceRelativePath             string
	LeaseID                           string
	LeaseEpoch                        int64
	ExternalWatchID                   string
	SenderHostID                      string
	SenderStoreID                     string
	SenderServerDomainID              string
	SenderServerID                    string
	SenderSessionID                   string
	SenderAgentRunID                  string
	SenderPrincipalID                 string
	RecipientSessionID                string
	RecipientPaneInstanceID           string
	RecipientAgentRunID               string
	GrantID                           string
	GrantEpoch                        int64
	GrantExpiresAt                    string
}

func NewAction(id, payload string, epoch int64) Action {
	h := sha256.Sum256([]byte(payload))
	return Action{ActionID: id, Payload: payload, PayloadSHA256: "sha256:" + hex.EncodeToString(h[:]), Epoch: epoch}
}

func (a Action) SessionTarget() SessionTarget {
	ownership := ""
	if a.TargetLifecycleOwnership == "external_observed" {
		ownership = a.TargetLifecycleOwnership
	}
	return SessionTarget{
		Identity: Identity{HostID: a.TargetHostID, StoreID: a.TargetStoreID,
			TmuxServerDomainID:   a.TargetServerDomainID,
			TmuxServerInstanceID: a.TargetServerID, Ownership: ownership,
			LifecycleKey: a.LifecycleKey,
			TargetEpoch:  a.TargetEpoch, SessionID: a.RecipientSessionID,
			PaneInstanceID: a.RecipientPaneInstanceID, AgentRunID: a.RecipientAgentRunID},
		LifecycleKey: a.LifecycleKey, TargetEpoch: a.TargetEpoch, ProfileID: a.ProfileID,
		WorkspaceRootID: a.WorkspaceRootID, WorkspaceRelativePath: a.WorkspaceRelativePath,
		LeaseID: a.LeaseID, LeaseEpoch: a.LeaseEpoch,
		ExternalWatchID: a.ExternalWatchID,
	}
}

func (a Action) RouteGrant() Grant {
	g := Grant{GrantID: a.GrantID, SenderPrincipalID: a.SenderPrincipalID,
		SenderSessionID:  a.SenderSessionID,
		SenderAgentRunID: a.SenderAgentRunID, RecipientSessionID: a.RecipientSessionID,
		RecipientPaneInstanceID: a.RecipientPaneInstanceID, Epoch: a.Epoch,
		MaximumPayloadBytes: 65536, ExpiresAt: a.GrantExpiresAt}
	if a.SenderPrincipalID != "" {
		g.ExpectedRecipientAgentRunID = a.RecipientAgentRunID
	}
	return g
}

type SendRequest struct {
	Action
	GrantID                     string `json:"grant_id"`
	RecipientSessionID          string `json:"recipient_session_id"`
	RecipientPaneInstanceID     string `json:"recipient_pane_instance_id"`
	ExpectedRecipientAgentRunID string `json:"expected_recipient_agent_run_id,omitempty"`
	GrantEpoch                  int64  `json:"grant_epoch"`
	OnBehalfOfSessionID         string `json:"on_behalf_of_session_id,omitempty"`
}

type ReceiptStatus string

const (
	ReceiptAccepted       ReceiptStatus = "accepted"
	ReceiptDelivering     ReceiptStatus = "delivering"
	ReceiptSubmitted      ReceiptStatus = "submitted"
	ReceiptTyped          ReceiptStatus = "typed"
	ReceiptUnverified     ReceiptStatus = "unverified"
	ReceiptRefused        ReceiptStatus = "refused"
	ReceiptTargetMismatch ReceiptStatus = "target_mismatch"
	ReceiptFailed         ReceiptStatus = "failed"
	ReceiptUncertain      ReceiptStatus = "uncertain"
)

// Submitted is the sole positive terminal-insertion result. Typed and
// unverified prove possible mutation but not a completed terminal insertion;
// they require evidence reconciliation and must never be blindly retried.
func (r Receipt) Submitted() bool { return r.Status == ReceiptSubmitted }

func (r Receipt) MutationUncertain() bool {
	switch r.Status {
	case ReceiptAccepted, ReceiptDelivering, ReceiptTyped, ReceiptUnverified, ReceiptUncertain:
		return true
	default:
		return false
	}
}

type Receipt struct {
	DeliveryID                  string        `json:"delivery_id"`
	ActionID                    string        `json:"action_id"`
	GrantID                     string        `json:"grant_id"`
	GrantEpoch                  int64         `json:"grant_epoch"`
	Sender                      Identity      `json:"-"`
	SenderPrincipalID           string        `json:"sender_principal_id,omitempty"`
	Recipient                   Identity      `json:"-"`
	ExpectedRecipientAgentRunID string        `json:"expected_recipient_agent_run_id,omitempty"`
	PayloadSHA256               string        `json:"payload_sha256"`
	Status                      ReceiptStatus `json:"status"`
	CompatibilityCode           int           `json:"compatibility_code"`
	DiagnosticCode              string        `json:"diagnostic_code"`
}

// ReceiptExpectation is Flowbee's immutable authorization for reading and
// accepting one Driver receipt.  In particular, an empty sender session is not
// shorthand for "any sender": exactly one of SenderPrincipalID or the complete
// SenderSessionID/SenderAgentRunID pair must be present.
type ReceiptExpectation struct {
	ActionID                    string
	ActionEpoch                 int64
	GrantID                     string
	GrantEpoch                  int64
	PayloadSHA256               string
	SenderPrincipalID           string
	SenderSessionID             string
	SenderAgentRunID            string
	RecipientSessionID          string
	RecipientPaneInstanceID     string
	ExpectedRecipientAgentRunID string
}

func (a Action) ExpectedReceipt() ReceiptExpectation {
	grantEpoch := a.GrantEpoch
	if grantEpoch == 0 {
		grantEpoch = a.Epoch
	}
	e := ReceiptExpectation{ActionID: a.ActionID, ActionEpoch: a.Epoch,
		GrantID: a.GrantID, GrantEpoch: grantEpoch, PayloadSHA256: a.PayloadSHA256,
		SenderPrincipalID: a.SenderPrincipalID, SenderSessionID: a.SenderSessionID,
		SenderAgentRunID: a.SenderAgentRunID, RecipientSessionID: a.RecipientSessionID,
		RecipientPaneInstanceID: a.RecipientPaneInstanceID}
	if a.SenderPrincipalID != "" {
		e.ExpectedRecipientAgentRunID = a.RecipientAgentRunID
	}
	return e
}

func (e ReceiptExpectation) Validate(r Receipt) error {
	controlOrigin := e.SenderPrincipalID != "" && e.SenderSessionID == "" && e.SenderAgentRunID == ""
	sessionOrigin := e.SenderPrincipalID == "" && e.SenderSessionID != "" && e.SenderAgentRunID != ""
	if !controlOrigin && !sessionOrigin {
		return fmt.Errorf("receipt expectation has incomplete or mixed sender origin: %w", ErrIdentityMismatch)
	}
	if controlOrigin && e.ExpectedRecipientAgentRunID == "" {
		return fmt.Errorf("control receipt expectation lacks recipient agent run: %w", ErrIdentityMismatch)
	}
	if sessionOrigin && e.ExpectedRecipientAgentRunID != "" {
		return fmt.Errorf("session receipt expectation carries control run fence: %w", ErrIdentityMismatch)
	}
	if e.ActionID == "" || e.ActionEpoch < 1 || e.GrantID == "" || e.GrantEpoch < 1 ||
		e.ActionEpoch != e.GrantEpoch ||
		e.PayloadSHA256 == "" || e.RecipientSessionID == "" || e.RecipientPaneInstanceID == "" {
		return fmt.Errorf("receipt expectation is incomplete: %w", ErrIdentityMismatch)
	}
	if r.DeliveryID == "" || r.ActionID != e.ActionID || r.GrantID != e.GrantID ||
		r.GrantEpoch != e.GrantEpoch || r.PayloadSHA256 != e.PayloadSHA256 ||
		r.Recipient.SessionID != e.RecipientSessionID ||
		r.Recipient.PaneInstanceID != e.RecipientPaneInstanceID ||
		r.ExpectedRecipientAgentRunID != e.ExpectedRecipientAgentRunID ||
		r.SenderPrincipalID != e.SenderPrincipalID ||
		r.Sender.SessionID != e.SenderSessionID || r.Sender.AgentRunID != e.SenderAgentRunID {
		return ErrIdentityMismatch
	}
	return nil
}

// LifecycleReceipt is Driver's durable proof for one exact Ensure/Stop effect.
// It is intentionally distinct from a routed-message receipt: stopped proves
// positive target absence, while ensured returns a newly fenced incarnation.
type LifecycleReceipt struct {
	FormatVersion      string   `json:"format_version"`
	LifecycleReceiptID string   `json:"lifecycle_receipt_id"`
	Operation          string   `json:"operation"`
	ActionID           string   `json:"action_id"`
	ActionEpoch        int64    `json:"action_epoch"`
	LeaseID            string   `json:"lease_id"`
	LeaseEpoch         int64    `json:"lease_epoch"`
	LifecycleKey       string   `json:"lifecycle_key"`
	TmuxServerDomainID string   `json:"tmux_server_domain_id"`
	ExternalWatchID    string   `json:"external_watch_id"`
	TargetEpoch        int64    `json:"target_epoch"`
	Status             string   `json:"status"`
	IdentityBefore     Identity `json:"identity_before"`
	IdentityAfter      Identity `json:"identity_after"`
	AbsenceObservedAt  string   `json:"absence_observed_at"`
	DiagnosticCode     string   `json:"diagnostic_code"`
}

type LifecyclePresence struct {
	Presence, ObservedAt, DiagnosticCode string
	Identity                             Identity
}

func (p LifecyclePresence) ExactAbsent() bool { return p.Presence == "absent" && p.ObservedAt != "" }

type LifecycleGateResult struct {
	Allowed bool
	Detail  string
}

type LifecycleGate interface {
	PrepareLifecycleAction(context.Context, Action, time.Time) (LifecycleGateResult, error)
}

func (r LifecycleReceipt) Resolved() bool {
	return r.Status == "ensured" || r.Status == "stopped" || r.Status == "target_absent" ||
		r.Status == "adopted" || r.Status == "reattached" || r.Status == "released"
}

func (r LifecycleReceipt) Uncertain() bool {
	return r.Status == "accepted" || r.Status == "executing" || r.Status == "verifying" || r.Status == "uncertain"
}

type DriverPort interface {
	Metadata(context.Context) (DriverMetadata, error)
	ControlOriginCapability(context.Context) (ControlOriginCapability, error)
	SnapshotSessions(context.Context) (SessionSnapshot, error)
	EnsureSession(context.Context, SessionTarget, Action) (Identity, error)
	EnsureLifecycleSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error)
	EnsureExternalWatch(context.Context, string, string, string) (ExternalWatch, error)
	AdoptSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error)
	ReattachSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error)
	ReleaseSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error)
	StopSession(context.Context, SessionTarget, Action) (LifecycleReceipt, error)
	VerifyLifecycleEffect(context.Context, string, SessionTarget, Action) (LifecycleReceipt, error)
	LifecycleTargetPresence(context.Context, string, int64) (LifecyclePresence, error)
	LifecycleReceiptByAction(context.Context, string, string, int64) (LifecycleReceipt, bool, error)
	Grant(context.Context, Grant) error
	RevokeGrant(context.Context, string, int64) error
	Send(context.Context, SendRequest) (Receipt, error)
	ReceiptByAction(context.Context, ReceiptExpectation) (Receipt, bool, error)
	Observe(context.Context, string) (ObservationBatch, error)
}

// ExternalWatch is bootstrap-only authority: PaneID may be used once to ask
// Driver for a durable watch UUID, but it is never persisted as lifecycle or
// routing identity. Adopt/Release use WatchID plus the stable Driver tuple.
type ExternalWatch struct {
	WatchID   string
	PaneID    string
	Enabled   bool
	Lifecycle string
	Provider  string
	Profile   string
}

// ControlOriginCapability is Driver's authenticated proof that this exact
// control-plane token can author first-class routed messages without
// impersonating a managed session.
type ControlOriginCapability struct {
	FormatVersion           string   `json:"format_version"`
	Supported               bool     `json:"supported"`
	Authorized              bool     `json:"authorized"`
	PrincipalID             string   `json:"principal_id"`
	PrincipalKind           string   `json:"principal_kind"`
	RequiredScopes          []string `json:"required_scopes"`
	GrantedScopes           []string `json:"granted_scopes"`
	MissingScopes           []string `json:"missing_scopes"`
	RouteGrantFormat        string   `json:"route_grant_format"`
	DeliveryReceiptFormat   string   `json:"delivery_receipt_format"`
	GrantEndpoint           string   `json:"grant_endpoint"`
	MessageEndpoint         string   `json:"message_endpoint"`
	OnBehalfOfSessionIDRule string   `json:"on_behalf_of_session_id"`
}

type ObservationBatch struct {
	StoreID                string
	StoreSeq               uint64
	NextCursor             string
	DurableHighWaterCursor string
	Events                 []Observation
	CursorGap              bool
	StoreReset             bool
	HistoryComplete        bool
}

type Observation struct {
	SpecVersion     string
	EventID         string
	Cursor          string
	StoreSeq        uint64
	SessionSeq      uint64
	TransitionID    string
	TransitionIndex int
	TransitionCount int
	ProducerBootID  string
	Kind            string
	ObservedAt      string
	SourceAt        string
	Historical      bool
	Identity        Identity
	Source          json.RawMessage
	Correlation     json.RawMessage
	CausedBy        []string
	Payload         json.RawMessage
	Envelope        json.RawMessage
}

// DriverMetadata identifies one Driver cursor domain. InstanceRef is deliberately
// not supplied by Driver: it is a Flowbee-owned inventory key passed to the
// observation ingestor. A new StoreID under that key is a reset, never a
// continuation.
type DriverMetadata struct {
	APIVersion             string
	HostID                 string
	StoreID                string
	Instance               string
	ProducerBootID         string
	ReplayFloorCursor      string
	DurableHighWaterCursor string
	TmuxServer             TmuxServerMetadata
	Contracts              DriverContractCapabilities
	// ControlPrincipalOrigin is true only when Driver's protocol metadata
	// explicitly advertises features.control_principal_origin=true. Missing,
	// false, or malformed metadata never enables control-origin delivery.
	ControlPrincipalOrigin bool
	LifecycleControl       bool
}

type TmuxServerMetadata struct {
	DomainID             string `json:"domain_id"`
	Ownership            string `json:"ownership"`
	InstanceID           string `json:"instance_id"`
	ConnectionVisibility string `json:"connection_visibility"`
}

type DriverContractCapability struct {
	Supported  bool   `json:"supported"`
	ContractID string `json:"contract_id"`
}

type DriverContractCapabilities struct {
	FormatVersion                       string                   `json:"format_version"`
	ManagedTmuxServerDomain             DriverContractCapability `json:"managed_tmux_server_domain"`
	ManagedTmuxServerIsolation          DriverContractCapability `json:"managed_tmux_server_isolation"`
	LifecycleEnsure                     DriverContractCapability `json:"lifecycle_ensure"`
	LifecycleExternalAdopt              DriverContractCapability `json:"lifecycle_external_adopt"`
	LifecycleExternalRelease            DriverContractCapability `json:"lifecycle_external_release"`
	ControlOriginRecipientAgentRunFence DriverContractCapability `json:"control_origin_recipient_agent_run_fence"`
}

// SessionProjection is Driver-derived snapshot state. It contains stable IDs and
// mechanical state only; Flowbee never uses workspace/CWD, tmux names, PIDs, or
// prose as identity or routing authority.
type SessionProjection struct {
	Identity      Identity
	WatchID       string
	Lifecycle     string
	Phase         string
	BindingStatus string
	BindingEpoch  int64
	StateRevision uint64
	AsOfCursor    string
	StartedAt     string
	EndedAt       string
	EndReason     string
	RawState      json.RawMessage
}

type SessionSnapshot struct {
	HostID     string
	StoreID    string
	AsOfCursor string
	Sessions   []SessionProjection
}

func (r Receipt) StageComplete() bool { return false }

func ValidateSend(req SendRequest, grant Grant) error {
	if req.GrantID != grant.GrantID || req.GrantEpoch != grant.Epoch || req.Epoch != grant.Epoch {
		return ErrGrantDenied
	}
	if req.RecipientSessionID != grant.RecipientSessionID {
		return ErrIdentityMismatch
	}
	if req.RecipientPaneInstanceID != grant.RecipientPaneInstanceID {
		return ErrIdentityMismatch
	}
	switch {
	case grant.SenderPrincipalID != "":
		if grant.SenderSessionID != "" || grant.SenderAgentRunID != "" ||
			grant.ExpectedRecipientAgentRunID == "" ||
			req.ExpectedRecipientAgentRunID != grant.ExpectedRecipientAgentRunID ||
			req.SenderPrincipalID != grant.SenderPrincipalID ||
			req.SenderSessionID != "" || req.SenderAgentRunID != "" ||
			req.OnBehalfOfSessionID != "" {
			return ErrIdentityMismatch
		}
	case grant.SenderSessionID != "" && grant.SenderAgentRunID != "":
		if grant.ExpectedRecipientAgentRunID != "" || req.ExpectedRecipientAgentRunID != "" ||
			req.SenderPrincipalID != "" ||
			(req.SenderSessionID != "" && req.SenderSessionID != grant.SenderSessionID) ||
			(req.SenderAgentRunID != "" && req.SenderAgentRunID != grant.SenderAgentRunID) ||
			(req.OnBehalfOfSessionID != "" && req.OnBehalfOfSessionID != grant.SenderSessionID) {
			return ErrIdentityMismatch
		}
	default:
		return ErrGrantDenied
	}
	if req.PayloadSHA256 == "" || req.PayloadSHA256 != NewAction(req.ActionID, req.Payload, req.Epoch).PayloadSHA256 {
		return fmt.Errorf("payload hash: %w", ErrIdempotencyBody)
	}
	if grant.MaximumPayloadBytes > 0 && len([]byte(req.Payload)) > grant.MaximumPayloadBytes {
		return fmt.Errorf("payload exceeds grant limit")
	}
	return nil
}
