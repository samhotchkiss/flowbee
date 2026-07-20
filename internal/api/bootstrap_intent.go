package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const BootstrapActionFormat = "flowbee.bootstrap-action/v1"

type BootstrapAction struct {
	FormatVersion string          `json:"format_version"`
	BootstrapID   string          `json:"bootstrap_id"`
	ProjectID     string          `json:"project_id"`
	ActionID      string          `json:"action_id"`
	Kind          string          `json:"kind"`
	PayloadSHA256 string          `json:"payload_sha256"`
	Payload       json.RawMessage `json:"payload"`
}

type BootstrapActionReceipt struct {
	FormatVersion string `json:"format_version"`
	ActionID      string `json:"action_id"`
	ReceiptID     string `json:"receipt_id"`
	State         string `json:"state"`
}

// BootstrapActionStatus is the payload-redacted mechanical state exposed to
// bootstrap clients. The immutable payload and its hash deliberately remain
// server-side: a status read proves progress without becoming a secret-bearing
// bootstrap configuration endpoint.
type BootstrapActionStatus struct {
	FormatVersion string    `json:"format_version"`
	BootstrapID   string    `json:"bootstrap_id"`
	ProjectID     string    `json:"project_id"`
	ActionID      string    `json:"action_id"`
	Kind          string    `json:"kind"`
	State         string    `json:"state"`
	ActionEpoch   int64     `json:"action_epoch"`
	Attempts      int       `json:"attempts"`
	RecoveryCount int       `json:"recovery_count"`
	ReceiptID     string    `json:"receipt_id,omitempty"`
	ReceiptState  string    `json:"receipt_state,omitempty"`
	LastError     string    `json:"last_error,omitempty"`
	AlertPending  bool      `json:"alert_pending"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type BootstrapActionIntake interface {
	CommitBootstrapAction(context.Context, BootstrapAction, string) (BootstrapActionReceipt, error)
}

func (s *Server) SetBootstrapActionIntake(intake BootstrapActionIntake) { s.bootstrapIntake = intake }

func (s *Server) bootstrapActionCommit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHumanPortfolio(w, r, auth.HumanProjectManage); !ok {
		return
	}
	if s.bootstrapIntake == nil {
		http.Error(w, "bootstrap action intake is disabled", http.StatusServiceUnavailable)
		return
	}
	key, ok := requireProjectIdempotencyKey(w, r)
	if !ok {
		return
	}
	var action BootstrapAction
	if err := decodeBoundedJSON(r, &action); err != nil || !validBootstrapAction(action) || key != action.ActionID {
		http.Error(w, "invalid bootstrap action", http.StatusBadRequest)
		return
	}
	receipt, err := s.bootstrapIntake.CommitBootstrapAction(r.Context(), action, key)
	if err != nil {
		http.Error(w, "bootstrap action commit failed", http.StatusConflict)
		return
	}
	if receipt.ActionID != action.ActionID || receipt.ReceiptID == "" {
		http.Error(w, "bootstrap action receipt is invalid", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusAccepted, receipt)
}

func (s *Server) bootstrapActionStatus(w http.ResponseWriter, r *http.Request) {
	record, err := s.store.GetBootstrapAction(r.Context(), r.PathValue("action_id"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "bootstrap action not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "bootstrap action status failed", http.StatusInternalServerError)
		return
	}
	if !s.authorizeBootstrapStatus(w, r, record.ProjectID) {
		return
	}
	writeJSON(w, http.StatusOK, bootstrapActionStatusFromRecord(record))
}

func (s *Server) authorizeBootstrapStatus(w http.ResponseWriter, r *http.Request, projectID string) bool {
	principal, ok := auth.HumanPrincipalFrom(r)
	if !ok {
		http.Error(w, "human authentication required", http.StatusUnauthorized)
		return false
	}
	if err := s.human.Authorize(principal, projectID, auth.HumanProjectRead); err == nil {
		return true
	}
	if err := s.human.AuthorizePortfolio(principal, auth.HumanProjectRead); err == nil {
		return true
	}
	http.Error(w, "bootstrap action status is not authorized", http.StatusForbidden)
	return false
}

func bootstrapActionStatusFromRecord(record store.BootstrapActionRecord) BootstrapActionStatus {
	return BootstrapActionStatus{
		FormatVersion: "flowbee.bootstrap-action-status/v1",
		BootstrapID:   record.BootstrapID,
		ProjectID:     record.ProjectID,
		ActionID:      record.ID,
		Kind:          record.Kind,
		State:         record.State,
		ActionEpoch:   record.ActionEpoch,
		Attempts:      record.Attempts,
		RecoveryCount: record.RecoveryCount,
		ReceiptID:     record.ReceiptID,
		ReceiptState:  record.ReceiptState,
		LastError:     record.LastError,
		AlertPending:  record.AlertPending,
		UpdatedAt:     record.UpdatedAt,
	}
}

func validBootstrapAction(action BootstrapAction) bool {
	if action.FormatVersion != BootstrapActionFormat || action.BootstrapID == "" || action.ProjectID == "" ||
		action.ActionID == "" || len(action.Payload) == 0 {
		return false
	}
	sum := sha256.Sum256(action.Payload)
	if action.PayloadSHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
		return false
	}
	switch action.Kind {
	case "project_upsert", "repository_attach", "actor_route", "actor_lifecycle", "seat_bind", "managed_topology":
	default:
		return false
	}
	var value any
	return json.Unmarshal(action.Payload, &value) == nil && value != nil
}

var ErrBootstrapActionConflict = errors.New("bootstrap action idempotency conflict")
