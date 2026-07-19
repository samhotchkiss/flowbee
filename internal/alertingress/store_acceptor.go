package alertingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

const ControlAlertIngressPath = "/v1/control-alerts/ingress"

const (
	ExternalWatchdogHeartbeatKind          = "external_watchdog_heartbeat"
	ExternalWatchdogHeartbeatFormatVersion = "flowbee.deadman-heartbeat/v1"
)

type ExternalWatchdogHeartbeatPayload struct {
	FormatVersion string    `json:"format_version"`
	ProjectID     string    `json:"project_id"`
	WatchdogID    string    `json:"watchdog_id"`
	Target        string    `json:"target"`
	Sequence      int64     `json:"sequence"`
	ObservedAt    time.Time `json:"observed_at"`
}

// StoreAcceptor is the production Acceptor adapter. It re-validates the exact
// body/hash/envelope boundary rather than trusting a caller-constructed
// Submission, then delegates one atomic ingress+control-alert transaction to
// Store.
type StoreAcceptor struct {
	Store               *store.Store
	AuthorizedProjectID string
	Now                 func() time.Time
}

func (a StoreAcceptor) Accept(ctx context.Context, submission Submission) error {
	if a.Store == nil {
		return errors.New("control-alert ingress store is required")
	}
	if a.AuthorizedProjectID == "" {
		return errors.New("control-alert ingress requires an exact authorized project")
	}
	digest := sha256.Sum256(submission.Body)
	computed := hex.EncodeToString(digest[:])
	if submission.BodySHA256 != computed {
		return errors.New("control-alert ingress submission body hash mismatch")
	}
	envelope, err := decodeEnvelope(bytes.Clone(submission.Body), submission.IdempotencyKey)
	if err != nil {
		return err
	}
	if envelope.ProjectID != a.AuthorizedProjectID {
		return store.ErrControlAlertIngressProjectUnauthorized
	}
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if envelope.Kind == ExternalWatchdogHeartbeatKind {
		var payload ExternalWatchdogHeartbeatPayload
		decoder := json.NewDecoder(bytes.NewReader(envelope.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&payload); err != nil {
			return fmt.Errorf("decode external watchdog heartbeat: %w", err)
		}
		if decoder.Decode(&struct{}{}) != io.EOF ||
			payload.FormatVersion != ExternalWatchdogHeartbeatFormatVersion ||
			payload.ProjectID != envelope.ProjectID || payload.WatchdogID == "" ||
			payload.Target == "" || payload.Sequence < 1 || payload.ObservedAt.IsZero() {
			return errors.New("external watchdog heartbeat payload is incomplete or project-mismatched")
		}
		err = a.Store.AcceptExternalWatchdogHeartbeat(ctx, store.ExternalWatchdogHeartbeatInput{
			IdempotencyKey: submission.IdempotencyKey, BodySHA256: submission.BodySHA256,
			Body: bytes.Clone(submission.Body), EnvelopeID: envelope.ID, ProjectID: envelope.ProjectID,
			WatchdogID: payload.WatchdogID, Target: payload.Target, Sequence: payload.Sequence,
			ObservedAt: payload.ObservedAt,
		}, now)
	} else {
		err = a.Store.AcceptControlAlertIngress(ctx, store.ControlAlertIngressInput{
			IdempotencyKey: submission.IdempotencyKey,
			BodySHA256:     submission.BodySHA256,
			Body:           bytes.Clone(submission.Body),
			AlertID:        envelope.ID,
			ProjectID:      envelope.ProjectID,
			EpicID:         envelope.EpicID,
			Kind:           envelope.Kind,
			PayloadJSON:    string(envelope.Payload),
		}, now)
	}
	if errors.Is(err, store.ErrControlAlertIngressConflict) {
		return errors.Join(ErrIdempotencyConflict, err)
	}
	return err
}
