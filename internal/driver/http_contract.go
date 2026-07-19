package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	controlOriginCapabilityFormat = "tmux-driver.control-principal-origin-capability/v1"
	sessionRouteGrantFormat       = "tmux-driver.route-grant/v1"
	controlRouteGrantFormat       = "tmux-driver.control-route-grant/v1"
	sessionDeliveryReceiptFormat  = "tmux-driver.delivery-receipt/v1"
	controlDeliveryReceiptFormat  = "tmux-driver.control-delivery-receipt/v1"
	deliveryMediaType             = "text/plain; charset=utf-8"
)

func (c *ControlOriginCapability) UnmarshalJSON(data []byte) error {
	if err := requireExactWireKeys(data, []string{
		"format_version", "supported", "authorized", "principal_id", "principal_kind",
		"required_scopes", "granted_scopes", "missing_scopes", "route_grant_format",
		"delivery_receipt_format", "grant_endpoint", "message_endpoint", "on_behalf_of_session_id",
	}); err != nil {
		return err
	}
	type plain ControlOriginCapability
	var value plain
	if err := decodeStrictWire(data, &value); err != nil {
		return err
	}
	*c = ControlOriginCapability(value)
	return nil
}

func validateControlOriginCapability(c ControlOriginCapability) error {
	if c.FormatVersion != controlOriginCapabilityFormat || !c.Supported || !c.Authorized ||
		c.PrincipalID != "flowbee-control" || c.PrincipalKind != "control_plane" ||
		c.RouteGrantFormat != controlRouteGrantFormat ||
		c.DeliveryReceiptFormat != controlDeliveryReceiptFormat ||
		c.GrantEndpoint != "/v2/routes/grants" || c.MessageEndpoint != "/v2/messages" ||
		c.OnBehalfOfSessionIDRule != "forbidden" || len(c.MissingScopes) != 0 {
		return errors.New("driver control origin capability: unsupported or unauthorized contract")
	}
	if err := validatePrincipalID(c.PrincipalID, "principal_id"); err != nil {
		return err
	}
	required := map[string]bool{"routes:manage": false, "messages:send": false}
	if len(c.RequiredScopes) != len(required) {
		return errors.New("driver control origin capability: required scopes changed")
	}
	for _, scope := range c.RequiredScopes {
		if _, ok := required[scope]; !ok || required[scope] {
			return errors.New("driver control origin capability: required scopes changed")
		}
		required[scope] = true
	}
	granted := make(map[string]bool, len(c.GrantedScopes))
	for _, scope := range c.GrantedScopes {
		granted[scope] = true
	}
	for scope := range required {
		if !granted[scope] {
			return fmt.Errorf("driver control origin capability: scope %s not granted", scope)
		}
	}
	return nil
}

// routeGrantWire mirrors the current v2 SDK's RouteGrant model. Driver fills the
// issuer and timestamps; Flowbee verifies that it did not narrow, widen, or
// redirect the immutable A-to-B route it projected.
type routeGrantWire struct {
	FormatVersion           string  `json:"format_version"`
	GrantID                 string  `json:"grant_id"`
	IssuerPrincipalID       string  `json:"issuer_principal_id"`
	SenderPrincipalID       string  `json:"sender_principal_id,omitempty"`
	SenderSessionID         string  `json:"sender_session_id"`
	SenderAgentRunID        string  `json:"sender_agent_run_id"`
	RecipientSessionID      string  `json:"recipient_session_id"`
	RecipientPaneInstanceID string  `json:"recipient_pane_instance_id"`
	Operation               string  `json:"operation"`
	Epoch                   int64   `json:"epoch"`
	MaximumPayloadBytes     int     `json:"maximum_payload_bytes"`
	AllowDraftStash         bool    `json:"allow_draft_stash"`
	IssuedAt                string  `json:"issued_at"`
	ExpiresAt               *string `json:"expires_at"`
	RevokedAt               *string `json:"revoked_at"`
}

func (w *routeGrantWire) UnmarshalJSON(data []byte) error {
	var discriminator struct {
		FormatVersion string `json:"format_version"`
	}
	if err := json.Unmarshal(data, &discriminator); err != nil {
		return err
	}
	var decoded routeGrantWire
	switch discriminator.FormatVersion {
	case sessionRouteGrantFormat:
		if err := requireExactWireKeys(data, []string{
			"format_version", "grant_id", "issuer_principal_id", "sender_session_id",
			"sender_agent_run_id", "recipient_session_id", "recipient_pane_instance_id",
			"operation", "epoch", "maximum_payload_bytes", "allow_draft_stash",
			"issued_at", "expires_at", "revoked_at",
		}); err != nil {
			return err
		}
		var value struct {
			FormatVersion           string  `json:"format_version"`
			GrantID                 string  `json:"grant_id"`
			IssuerPrincipalID       string  `json:"issuer_principal_id"`
			SenderSessionID         string  `json:"sender_session_id"`
			SenderAgentRunID        string  `json:"sender_agent_run_id"`
			RecipientSessionID      string  `json:"recipient_session_id"`
			RecipientPaneInstanceID string  `json:"recipient_pane_instance_id"`
			Operation               string  `json:"operation"`
			Epoch                   int64   `json:"epoch"`
			MaximumPayloadBytes     int     `json:"maximum_payload_bytes"`
			AllowDraftStash         bool    `json:"allow_draft_stash"`
			IssuedAt                string  `json:"issued_at"`
			ExpiresAt               *string `json:"expires_at"`
			RevokedAt               *string `json:"revoked_at"`
		}
		if err := decodeStrictWire(data, &value); err != nil {
			return err
		}
		decoded = routeGrantWire{FormatVersion: value.FormatVersion, GrantID: value.GrantID,
			IssuerPrincipalID: value.IssuerPrincipalID, SenderSessionID: value.SenderSessionID,
			SenderAgentRunID: value.SenderAgentRunID, RecipientSessionID: value.RecipientSessionID,
			RecipientPaneInstanceID: value.RecipientPaneInstanceID, Operation: value.Operation,
			Epoch: value.Epoch, MaximumPayloadBytes: value.MaximumPayloadBytes,
			AllowDraftStash: value.AllowDraftStash, IssuedAt: value.IssuedAt,
			ExpiresAt: value.ExpiresAt, RevokedAt: value.RevokedAt}
	case controlRouteGrantFormat:
		if err := requireExactWireKeys(data, []string{
			"format_version", "grant_id", "issuer_principal_id", "sender_principal_id",
			"recipient_session_id", "recipient_pane_instance_id", "operation", "epoch",
			"maximum_payload_bytes", "allow_draft_stash", "issued_at", "expires_at", "revoked_at",
		}); err != nil {
			return err
		}
		var value struct {
			FormatVersion           string  `json:"format_version"`
			GrantID                 string  `json:"grant_id"`
			IssuerPrincipalID       string  `json:"issuer_principal_id"`
			SenderPrincipalID       string  `json:"sender_principal_id"`
			RecipientSessionID      string  `json:"recipient_session_id"`
			RecipientPaneInstanceID string  `json:"recipient_pane_instance_id"`
			Operation               string  `json:"operation"`
			Epoch                   int64   `json:"epoch"`
			MaximumPayloadBytes     int     `json:"maximum_payload_bytes"`
			AllowDraftStash         bool    `json:"allow_draft_stash"`
			IssuedAt                string  `json:"issued_at"`
			ExpiresAt               *string `json:"expires_at"`
			RevokedAt               *string `json:"revoked_at"`
		}
		if err := decodeStrictWire(data, &value); err != nil {
			return err
		}
		decoded = routeGrantWire{FormatVersion: value.FormatVersion, GrantID: value.GrantID,
			IssuerPrincipalID: value.IssuerPrincipalID, SenderPrincipalID: value.SenderPrincipalID,
			RecipientSessionID: value.RecipientSessionID, RecipientPaneInstanceID: value.RecipientPaneInstanceID,
			Operation: value.Operation, Epoch: value.Epoch, MaximumPayloadBytes: value.MaximumPayloadBytes,
			AllowDraftStash: value.AllowDraftStash, IssuedAt: value.IssuedAt,
			ExpiresAt: value.ExpiresAt, RevokedAt: value.RevokedAt}
	default:
		return fmt.Errorf("driver route grant: unsupported format %q", discriminator.FormatVersion)
	}
	*w = decoded
	return nil
}

func decodeStrictWire(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("driver wire object: trailing JSON value")
	}
	return nil
}

func requireExactWireKeys(data []byte, required []string) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		if err != nil {
			return err
		}
		return errors.New("driver wire object must be an object")
	}
	if len(object) != len(required) {
		return errors.New("driver wire object: missing or unknown field")
	}
	for _, key := range required {
		if _, ok := object[key]; !ok {
			return fmt.Errorf("driver wire object: missing %s", key)
		}
	}
	return nil
}

func validateGrantRequest(g Grant) error {
	if g.Epoch < 1 || g.MaximumPayloadBytes < 0 || g.MaximumPayloadBytes > 1<<20 {
		return errors.New("driver route grant: invalid grant epoch or payload limit")
	}
	for name, value := range map[string]string{
		"grant_id":                   g.GrantID,
		"recipient_session_id":       g.RecipientSessionID,
		"recipient_pane_instance_id": g.RecipientPaneInstanceID,
	} {
		if err := validateCanonicalUUID(value, name); err != nil {
			return err
		}
	}
	switch {
	case g.SenderPrincipalID != "":
		if g.SenderSessionID != "" || g.SenderAgentRunID != "" {
			return errors.New("driver route grant: mixed sender origins")
		}
		if err := validatePrincipalID(g.SenderPrincipalID, "sender_principal_id"); err != nil {
			return err
		}
	case g.SenderSessionID != "" && g.SenderAgentRunID != "":
		if err := validateCanonicalUUID(g.SenderSessionID, "sender_session_id"); err != nil {
			return err
		}
		if err := validateCanonicalUUID(g.SenderAgentRunID, "sender_agent_run_id"); err != nil {
			return err
		}
		if g.SenderSessionID == g.RecipientSessionID {
			return ErrGrantDenied
		}
	default:
		return errors.New("driver route grant: incomplete sender origin")
	}
	if g.ExpiresAt != "" {
		if err := validateDriverTime(g.ExpiresAt, "expires_at"); err != nil {
			return err
		}
	}
	return nil
}

func validateProjectedGrant(w routeGrantWire, want Grant, revoked bool) error {
	if err := validateRouteGrantWire(w); err != nil {
		return err
	}
	maximum := want.MaximumPayloadBytes
	if maximum == 0 {
		maximum = 64 * 1024
	}
	expires := ""
	if w.ExpiresAt != nil {
		expires = *w.ExpiresAt
	}
	if w.GrantID != want.GrantID || w.SenderPrincipalID != want.SenderPrincipalID ||
		w.SenderSessionID != want.SenderSessionID ||
		w.SenderAgentRunID != want.SenderAgentRunID ||
		w.RecipientSessionID != want.RecipientSessionID ||
		w.RecipientPaneInstanceID != want.RecipientPaneInstanceID ||
		w.Epoch != want.Epoch || w.MaximumPayloadBytes != maximum ||
		w.AllowDraftStash != want.AllowDraftStash || !driverTimesEquivalent(expires, want.ExpiresAt) ||
		revoked != (w.RevokedAt != nil) {
		return fmt.Errorf("Driver returned a different route grant: %w", ErrIdentityMismatch)
	}
	return nil
}

func validateRouteGrantWire(w routeGrantWire) error {
	if (w.FormatVersion != sessionRouteGrantFormat && w.FormatVersion != controlRouteGrantFormat) || w.Operation != "message" ||
		w.IssuerPrincipalID == "" || w.Epoch < 1 || w.MaximumPayloadBytes < 1 ||
		w.MaximumPayloadBytes > 1<<20 ||
		w.IssuedAt == "" {
		return errors.New("driver route grant: incomplete v2 response")
	}
	if err := validatePrincipalID(w.IssuerPrincipalID, "issuer_principal_id"); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"grant_id":                   w.GrantID,
		"recipient_session_id":       w.RecipientSessionID,
		"recipient_pane_instance_id": w.RecipientPaneInstanceID,
	} {
		if err := validateCanonicalUUID(value, name); err != nil {
			return err
		}
	}
	if w.FormatVersion == controlRouteGrantFormat {
		if w.SenderSessionID != "" || w.SenderAgentRunID != "" {
			return errors.New("driver route grant: mixed control origin")
		}
		if err := validatePrincipalID(w.SenderPrincipalID, "sender_principal_id"); err != nil {
			return err
		}
	} else {
		if w.SenderPrincipalID != "" {
			return errors.New("driver route grant: mixed session origin")
		}
		if err := validateCanonicalUUID(w.SenderSessionID, "sender_session_id"); err != nil {
			return err
		}
		if err := validateCanonicalUUID(w.SenderAgentRunID, "sender_agent_run_id"); err != nil {
			return err
		}
	}
	if err := validateDriverTime(w.IssuedAt, "issued_at"); err != nil {
		return err
	}
	for name, value := range map[string]*string{"expires_at": w.ExpiresAt, "revoked_at": w.RevokedAt} {
		if value != nil {
			if err := validateDriverTime(*value, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateDeliveryReceiptWire(w deliveryReceiptWire) error {
	if (w.FormatVersion != sessionDeliveryReceiptFormat && w.FormatVersion != controlDeliveryReceiptFormat) || w.ActionID == "" || w.GrantEpoch < 1 ||
		w.PayloadBytes < 1 || w.PayloadMediaType != deliveryMediaType || w.AcceptedAt == "" {
		return errors.New("driver delivery receipt: incomplete v2 response")
	}
	for name, value := range map[string]string{
		"delivery_id": w.DeliveryID, "grant_id": w.GrantID,
		"recipient_session_id":       w.RecipientSessionID,
		"recipient_pane_instance_id": w.RecipientPaneInstanceID,
	} {
		if err := validateCanonicalUUID(value, name); err != nil {
			return err
		}
	}
	if w.FormatVersion == controlDeliveryReceiptFormat {
		if w.SenderSessionID != "" || w.SenderAgentRunID != "" {
			return errors.New("driver delivery receipt: mixed control origin")
		}
		if err := validatePrincipalID(w.SenderPrincipalID, "sender_principal_id"); err != nil {
			return err
		}
	} else {
		if w.SenderPrincipalID != "" {
			return errors.New("driver delivery receipt: mixed session origin")
		}
		if err := validateCanonicalUUID(w.SenderSessionID, "sender_session_id"); err != nil {
			return err
		}
		if err := validateCanonicalUUID(w.SenderAgentRunID, "sender_agent_run_id"); err != nil {
			return err
		}
	}
	if !validSHA256(w.PayloadSHA256) || !validSHA256(w.RequestFingerprint) {
		return errors.New("driver delivery receipt: invalid hash")
	}
	if err := validateDriverTime(w.AcceptedAt, "accepted_at"); err != nil {
		return err
	}
	if w.CompletedAt != nil {
		if err := validateDriverTime(*w.CompletedAt, "completed_at"); err != nil {
			return err
		}
	}
	switch w.Status {
	case "accepted", "delivering":
		if w.CompletedAt != nil {
			return errors.New("driver delivery receipt: in-flight receipt has completed_at")
		}
	case "submitted", "typed", "unverified", "refused", "target_mismatch", "failed", "uncertain":
		if w.CompletedAt == nil {
			return errors.New("driver delivery receipt: terminal receipt missing completed_at")
		}
	default:
		return fmt.Errorf("driver delivery receipt: invalid status %q", w.Status)
	}
	return nil
}

func validatePrincipalID(value, name string) error {
	if len(value) < 1 || len(value) > 160 {
		return fmt.Errorf("driver %s is invalid", name)
	}
	for i, character := range value {
		valid := character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' ||
			character >= '0' && character <= '9' || i > 0 && strings.ContainsRune("._:@/-", character)
		if !valid {
			return fmt.Errorf("driver %s is invalid", name)
		}
	}
	return nil
}

func validateCanonicalUUID(value, name string) error {
	if len(value) != 36 {
		return fmt.Errorf("driver %s must be a canonical UUID", name)
	}
	for i, character := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if character != '-' {
				return fmt.Errorf("driver %s must be a canonical UUID", name)
			}
			continue
		}
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return fmt.Errorf("driver %s must be a canonical UUID", name)
		}
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func validateDriverTime(value, name string) error {
	if !strings.HasSuffix(value, "Z") {
		return fmt.Errorf("driver %s must use UTC", name)
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
		return fmt.Errorf("driver %s: %w", name, err)
	}
	return nil
}

func driverTimesEquivalent(got, want string) bool {
	if got == "" || want == "" {
		return got == want
	}
	gotTime, gotErr := time.Parse(time.RFC3339Nano, got)
	wantTime, wantErr := time.Parse(time.RFC3339Nano, want)
	return gotErr == nil && wantErr == nil &&
		gotTime.UTC().Equal(wantTime.UTC().Truncate(time.Millisecond))
}
