// Package alertingress authenticates Flowbee control-alert envelopes and
// delegates durable, idempotent acceptance to a provider-neutral boundary.
// It does not select a human notification provider or acknowledge workflow
// delivery: a signed receiver acknowledgement proves only durable ingress.
package alertingress

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	FormatVersion    = "flowbee.control-alert/v1"
	AckFormatVersion = "flowbee.alert-receiver-ack/v1"
	maxBodyBytes     = 1 << 20
	maxKeyBytes      = 512
)

// ErrIdempotencyConflict tells Handler that the same Idempotency-Key is already
// durably bound to a different request body. Acceptor implementations should
// wrap this sentinel when returning storage-specific context.
var ErrIdempotencyConflict = errors.New("control-alert idempotency conflict")

type Envelope struct {
	FormatVersion string          `json:"format_version"`
	ID            string          `json:"id"`
	DedupKey      string          `json:"dedup_key"`
	ProjectID     string          `json:"project_id"`
	EpicID        string          `json:"epic_id,omitempty"`
	Kind          string          `json:"kind"`
	Payload       json.RawMessage `json:"payload"`
}

// Submission is the exact authenticated object an Acceptor must retain before
// returning success. BodySHA256 is lowercase hex over Body's exact bytes;
// replays with the same key are successful only when that hash is unchanged.
type Submission struct {
	IdempotencyKey string
	BodySHA256     string
	Body           []byte
	Envelope       Envelope
}

// Acceptor is the durability boundary. Returning nil means the exact
// key/body-hash binding is committed (or is an exact replay). The HTTP handler
// emits no signed acknowledgement before this method returns nil.
type Acceptor interface {
	Accept(context.Context, Submission) error
}

type AcceptorFunc func(context.Context, Submission) error

func (f AcceptorFunc) Accept(ctx context.Context, submission Submission) error {
	return f(ctx, submission)
}

type Config struct {
	Secret   string
	Acceptor Acceptor
}

type Handler struct {
	secret   string
	acceptor Acceptor
}

func New(cfg Config) (*Handler, error) {
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, errors.New("control-alert ingress HMAC secret is required")
	}
	if cfg.Acceptor == nil {
		return nil, errors.New("control-alert ingress acceptor is required")
	}
	return &Handler{secret: cfg.Secret, acceptor: cfg.Acceptor}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if req.Header.Get("Content-Type") != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	key := req.Header.Get("Idempotency-Key")
	if key == "" || key != strings.TrimSpace(key) || len(key) > maxKeyBytes {
		http.Error(w, "valid Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "alert body exceeds maximum size", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "invalid alert body", http.StatusBadRequest)
		return
	}
	if len(body) == 0 {
		http.Error(w, "invalid alert body", http.StatusBadRequest)
		return
	}
	if !verifySignature(body, req.Header.Get("X-Flowbee-Signature"), h.secret) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	envelope, err := decodeEnvelope(body, key)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	hash := sha256.Sum256(body)
	hashText := hex.EncodeToString(hash[:])
	submission := Submission{
		IdempotencyKey: key,
		BodySHA256:     hashText,
		Body:           bytes.Clone(body),
		Envelope:       envelope,
	}
	if err := h.acceptor.Accept(req.Context(), submission); err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			http.Error(w, "Idempotency-Key conflicts with a different alert body", http.StatusConflict)
			return
		}
		http.Error(w, "durable alert acceptance failed", http.StatusServiceUnavailable)
		return
	}
	writeAcknowledgement(w, key, hashText, h.secret)
}

func decodeEnvelope(body []byte, key string) (Envelope, error) {
	var envelope Envelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return Envelope{}, fmt.Errorf("invalid %s envelope: %w", FormatVersion, err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return Envelope{}, errors.New("alert body must contain exactly one JSON value")
	}
	if envelope.FormatVersion != FormatVersion {
		return Envelope{}, fmt.Errorf("format_version must equal %s", FormatVersion)
	}
	if strings.TrimSpace(envelope.ID) == "" || strings.TrimSpace(envelope.DedupKey) == "" ||
		strings.TrimSpace(envelope.ProjectID) == "" || strings.TrimSpace(envelope.Kind) == "" || len(envelope.Payload) == 0 {
		return Envelope{}, errors.New("id, dedup_key, project_id, kind, and payload are required")
	}
	if envelope.DedupKey != key {
		return Envelope{}, errors.New("Idempotency-Key must equal dedup_key")
	}
	if !json.Valid(envelope.Payload) {
		return Envelope{}, errors.New("payload must be valid JSON")
	}
	return envelope, nil
}

func verifySignature(body []byte, header, secret string) bool {
	if !strings.HasPrefix(header, "sha256=") || len(header) != len("sha256=")+sha256.Size*2 {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(provided, mac.Sum(nil))
}

// Acknowledgement is the durable-ingress proof returned only after Acceptor has
// committed the exact idempotency-key/body-hash binding. It proves ingress
// acceptance, not Interactor delivery or workflow-stage completion.
type Acknowledgement struct {
	FormatVersion  string `json:"format_version"`
	Status         string `json:"status"`
	IdempotencyKey string `json:"idempotency_key"`
	BodySHA256     string `json:"body_sha256"`
}

// ValidateAcknowledgement verifies the signed durable-ingress proof before a
// publisher retires its own outbox item. An arbitrary proxy-generated 2xx must
// never become acceptance evidence.
func ValidateAcknowledgement(body []byte, signature, secret, key, bodySHA256 string) error {
	if len(body) == 0 || len(body) > 64<<10 {
		return errors.New("control-alert ingress 2xx omitted a bounded acknowledgement")
	}
	if !verifySignature(body, signature, secret) {
		return errors.New("control-alert ingress acknowledgement signature is invalid")
	}
	var ack Acknowledgement
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ack); err != nil {
		return fmt.Errorf("decode control-alert ingress acknowledgement: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("control-alert ingress acknowledgement must contain exactly one JSON value")
	}
	if ack.FormatVersion != AckFormatVersion || ack.Status != "accepted" ||
		ack.IdempotencyKey != key || !hmac.Equal([]byte(ack.BodySHA256), []byte(bodySHA256)) {
		return errors.New("control-alert ingress acknowledgement does not bind the accepted alert key and body hash")
	}
	return nil
}

func writeAcknowledgement(w http.ResponseWriter, key, bodySHA256, secret string) {
	body, err := json.Marshal(Acknowledgement{
		FormatVersion: AckFormatVersion, Status: "accepted", IdempotencyKey: key, BodySHA256: bodySHA256,
	})
	if err != nil {
		http.Error(w, "encode acknowledgement failed", http.StatusInternalServerError)
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write(body)
}
