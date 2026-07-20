package bootstrap

import (
	"context"
	"strings"
	"testing"
)

type driverServiceFake struct {
	ready   bool
	request DriverServiceEnsureRequest
	receipt DriverServiceEnsureReceipt
}

func (f *driverServiceFake) ExactEndpointReady(context.Context, EndpointRef) (bool, error) {
	return f.ready, nil
}
func (f *driverServiceFake) EnsureDriverService(_ context.Context, req DriverServiceEnsureRequest) (DriverServiceEnsureReceipt, error) {
	f.request = req
	return f.receipt, nil
}

func TestDriverServicePortRequiresStrictPinnedReceipt(t *testing.T) {
	endpoint := bootstrapPlan().Endpoints[0]
	fake := &driverServiceFake{receipt: DriverServiceEnsureReceipt{FormatVersion: DriverServiceEnsureReceiptFormat,
		ServiceReceiptID: "receipt", ActionID: "action", RequestFingerprint: "sha256:" + strings.Repeat("d", 64),
		Status: "ready", Readiness: "ready", Change: "started", ReleaseID: endpoint.ReleaseID,
		ExecutablePath: endpoint.ExecutablePath, ExecutableSHA256: endpoint.ExecutableSHA256,
		ConfigPath: endpoint.ConfigPath, ConfigSHA256: endpoint.ConfigSHA256, Label: "local.tmux-driver.managed",
		Destination: "/Users/test/Library/LaunchAgents/local.tmux-driver.managed.plist", UDSPath: endpoint.UDSPath,
		PID: 123, StoreID: endpoint.StoreID, ServerDomainID: endpoint.TmuxServerDomainID,
		Contracts: endpoint.RequiredContracts, AcceptedAt: "2026-07-19T00:00:00Z", CompletedAt: "2026-07-19T00:00:01Z"}}
	port := DriverServicePort{Probe: fake, Ensurer: fake}
	if _, err := port.EnsureEndpoint(context.Background(), endpoint, EffectRequest{ActionID: "action"}); err != nil {
		t.Fatal(err)
	}
	if fake.request.ActionID != "action" || fake.request.ExecutableSHA256 != endpoint.ExecutableSHA256 {
		t.Fatalf("request = %+v", fake.request)
	}
	fake.receipt.StoreID = "different"
	if _, err := port.EnsureEndpoint(context.Background(), endpoint, EffectRequest{ActionID: "action"}); err == nil {
		t.Fatal("mismatched service receipt was accepted")
	}
}

func TestDriverServicePortPersistsNonReadyReceiptsWithoutClaimingReadiness(t *testing.T) {
	endpoint := bootstrapPlan().Endpoints[0]
	base := DriverServiceEnsureReceipt{FormatVersion: DriverServiceEnsureReceiptFormat,
		ServiceReceiptID: "receipt", ActionID: "action", RequestFingerprint: "sha256:" + strings.Repeat("d", 64),
		Change:    "none",
		ReleaseID: endpoint.ReleaseID, ExecutablePath: endpoint.ExecutablePath,
		ExecutableSHA256: endpoint.ExecutableSHA256, ConfigPath: endpoint.ConfigPath,
		ConfigSHA256: endpoint.ConfigSHA256, Label: "local.tmux-driver.managed",
		Destination: "/Users/test/Library/LaunchAgents/local.tmux-driver.managed.plist",
		UDSPath:     endpoint.UDSPath, Contracts: endpoint.RequiredContracts, AcceptedAt: "2026-07-19T00:00:00Z"}
	for _, tc := range []struct {
		name, status, readiness, completed, diagnostic string
	}{
		{name: "accepted", status: "accepted", readiness: "pending"},
		{name: "uncertain", status: "uncertain", readiness: "unproven", completed: "2026-07-19T00:00:01Z", diagnostic: "service_effect_unproven"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			receipt := base
			receipt.Status, receipt.Readiness, receipt.CompletedAt, receipt.DiagnosticCode = tc.status, tc.readiness, tc.completed, tc.diagnostic
			fake := &driverServiceFake{receipt: receipt}
			got, err := (DriverServicePort{Probe: fake, Ensurer: fake}).EnsureEndpoint(context.Background(), endpoint, EffectRequest{ActionID: "action"})
			if err != nil || got.ID != "receipt" || got.State != tc.status {
				t.Fatalf("EnsureEndpoint() = %+v, %v", got, err)
			}
		})
	}
}

func TestDriverServicePortFailsClosedWithoutSupportedEnsureAdapter(t *testing.T) {
	endpoint := bootstrapPlan().Endpoints[0]
	port := DriverServicePort{Probe: &driverServiceFake{}}
	if _, err := port.EnsureEndpoint(context.Background(), endpoint, EffectRequest{ActionID: "action"}); err == nil {
		t.Fatal("missing pinned service adapter was accepted")
	}
}
