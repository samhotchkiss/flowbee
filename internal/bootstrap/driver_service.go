package bootstrap

import (
	"context"
	"errors"
	"path/filepath"
)

const DriverServiceEnsureReceiptFormat = "tmux-driver.service-ensure-receipt/v1"

type DriverServiceEnsureRequest struct {
	ActionID, ReleaseID                            string
	ExecutablePath, ExecutableSHA256               string
	ConfigPath, ConfigSHA256                       string
	InstanceRef, ExpectedStoreID, ExpectedDomainID string
	RequiredContracts                              map[string]string
	UpdateAuthorized                               bool
}

type DriverServiceEnsureReceipt struct {
	FormatVersion      string            `json:"format_version"`
	ServiceReceiptID   string            `json:"service_receipt_id"`
	ActionID           string            `json:"action_id"`
	RequestFingerprint string            `json:"request_fingerprint"`
	Status             string            `json:"status"`
	Readiness          string            `json:"readiness"`
	Change             string            `json:"change"`
	Replayed           bool              `json:"replayed"`
	ReleaseID          string            `json:"release_id"`
	ExecutablePath     string            `json:"executable_path"`
	ExecutableSHA256   string            `json:"executable_sha256"`
	ConfigPath         string            `json:"config_path"`
	ConfigSHA256       string            `json:"config_sha256"`
	Label              string            `json:"label"`
	Destination        string            `json:"destination"`
	UDSPath            string            `json:"uds_path"`
	PID                int               `json:"pid"`
	StoreID            string            `json:"store_id"`
	ServerDomainID     string            `json:"server_domain_id"`
	Contracts          map[string]string `json:"contracts"`
	AcceptedAt         string            `json:"accepted_at"`
	CompletedAt        string            `json:"completed_at"`
	DiagnosticCode     string            `json:"diagnostic_code"`
}

type DriverEndpointProbe interface {
	ExactEndpointReady(context.Context, EndpointRef) (bool, error)
}

type DriverServiceEnsurer interface {
	// Same ActionID + exact request is an idempotent reconcile/by-action call.
	// Implementations must return the existing accepted/ready/uncertain receipt,
	// never start a second service effect.
	EnsureDriverService(context.Context, DriverServiceEnsureRequest) (DriverServiceEnsureReceipt, error)
}

type DriverServicePort struct {
	Probe   DriverEndpointProbe
	Ensurer DriverServiceEnsurer
}

func (p DriverServicePort) EndpointReady(ctx context.Context, endpoint EndpointRef) (bool, error) {
	if p.Probe == nil {
		return false, errors.New("exact Driver endpoint probe is required")
	}
	return p.Probe.ExactEndpointReady(ctx, endpoint)
}

func (p DriverServicePort) EnsureEndpoint(ctx context.Context, endpoint EndpointRef,
	req EffectRequest) (EffectReceipt, error) {
	if p.Ensurer == nil {
		return EffectReceipt{}, errors.New("pinned tmux-driver-service Ensure adapter is unavailable")
	}
	request := DriverServiceEnsureRequest{ActionID: req.ActionID, ReleaseID: endpoint.ReleaseID,
		ExecutablePath: endpoint.ExecutablePath, ExecutableSHA256: endpoint.ExecutableSHA256,
		ConfigPath: endpoint.ConfigPath, ConfigSHA256: endpoint.ConfigSHA256,
		InstanceRef: endpoint.InstanceRef, ExpectedStoreID: endpoint.StoreID,
		ExpectedDomainID: endpoint.TmuxServerDomainID, RequiredContracts: cloneStrings(endpoint.RequiredContracts),
		UpdateAuthorized: endpoint.ServiceUpdateAuthorized}
	receipt, err := p.Ensurer.EnsureDriverService(ctx, request)
	if err != nil {
		return EffectReceipt{}, err
	}
	if !validDriverServiceReceipt(receipt, request, endpoint.UDSPath) {
		return EffectReceipt{}, errors.New("tmux-driver-service Ensure receipt does not match exact requested authority")
	}
	// accepted/uncertain are durable transport states, not errors and not
	// readiness. The orchestrator records this receipt and waits for the separate
	// exact EndpointReady fact instead of minting a new action.
	return EffectReceipt{ID: receipt.ServiceReceiptID, State: receipt.Status}, nil
}

func validDriverServiceReceipt(r DriverServiceEnsureReceipt, req DriverServiceEnsureRequest, udsPath string) bool {
	if r.FormatVersion != DriverServiceEnsureReceiptFormat || r.ServiceReceiptID == "" || r.ActionID != req.ActionID ||
		!validSHA256(r.RequestFingerprint) || r.ReleaseID != req.ReleaseID ||
		r.ExecutablePath != req.ExecutablePath || r.ExecutableSHA256 != req.ExecutableSHA256 ||
		r.ConfigPath != req.ConfigPath || r.ConfigSHA256 != req.ConfigSHA256 || r.UDSPath != udsPath ||
		r.Label == "" || !filepath.IsAbs(r.Destination) || !sameStringMap(r.Contracts, req.RequiredContracts) {
		return false
	}
	changes := map[string]bool{"none": true, "installed": true, "updated": true, "loaded": true, "started": true}
	if !changes[r.Change] || r.AcceptedAt == "" {
		return false
	}
	switch r.Status {
	case "accepted":
		return r.Readiness == "pending" && r.CompletedAt == "" && r.Change == "none" && r.PID == 0
	case "ready":
		return r.Readiness == "ready" && r.CompletedAt != "" && r.PID > 0 &&
			r.StoreID == req.ExpectedStoreID && r.ServerDomainID == req.ExpectedDomainID && r.DiagnosticCode == ""
	case "uncertain":
		return r.Readiness == "unproven" && r.CompletedAt != "" && r.DiagnosticCode == "service_effect_unproven"
	default:
		return false
	}
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}
