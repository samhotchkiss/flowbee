package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestRejectSyntheticDriverControlBinding(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	if err := rejectSyntheticDriverControlBinding(ctx, st.DB); err != nil {
		t.Fatalf("empty registry should be safe: %v", err)
	}

	_, err = st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: store.DriverControlIdentity,
		Role: store.DriverControlRole, HostID: "host", StoreID: "store",
		TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server", LifecycleKey: "flowbee-control",
		LifecycleOwnership: "driver_managed",
		TargetEpoch:        1, ProfileID: "flowbee-control", WorkspaceRootID: "root",
		WorkspaceRelativePath: ".", SessionID: "session", PaneInstanceID: "pane",
		AgentRunID: "run", Provider: "flowbee",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = rejectSyntheticDriverControlBinding(ctx, st.DB)
	if err == nil || !strings.Contains(err.Error(), "GAP-FD-003") {
		t.Fatalf("expected explicit GAP-FD-003 refusal, got %v", err)
	}
}

func TestDriverControlReadinessRequiresExactAuthenticatedCapability(t *testing.T) {
	ctx := context.Background()
	if got := driverControlReadiness(ctx, false, nil); got.Required || got.Available || got.Status != "disabled" {
		t.Fatalf("disabled readiness=%+v", got)
	}
	got := driverControlReadiness(ctx, true, nil)
	if !got.Required || got.Available || got.Status != "route_unavailable" || got.Gap != "GAP-FD-003" {
		t.Fatalf("uninitialized v2 readiness=%+v", got)
	}

	fake := driver.NewFake()
	got = driverControlReadiness(ctx, true, fake)
	if !got.Required || got.Available || got.Status != "route_unavailable" || got.Gap != "GAP-FD-003" {
		t.Fatalf("missing metadata feature readiness=%+v", got)
	}
	fake.Meta.ControlPrincipalOrigin = true
	got = driverControlReadiness(ctx, true, fake)
	if !got.Required || !got.Available || got.Status != "ready" || got.Gap != "" {
		t.Fatalf("authorized control origin readiness=%+v", got)
	}
	fake.Capability.MissingScopes = []string{"messages:send"}
	fake.Capability.Authorized = false
	got = driverControlReadiness(ctx, true, fake)
	if got.Available || got.Status != "route_unavailable" || got.Gap != "GAP-FD-003" {
		t.Fatalf("unauthorized control origin readiness=%+v", got)
	}
}

func TestDriverControlStateReadyRevokedRestored(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	fake := driver.NewFake()
	fake.Meta.ControlPrincipalOrigin = true
	state := newDriverControlState(api.DriverControlReadiness{Required: true, Status: "route_unavailable"})
	if got := probeDriverControlState(ctx, state, st.DB, fake); !got.Available || !state.Available() {
		t.Fatalf("ready probe=%+v snapshot=%+v", got, state.Snapshot())
	}
	fake.Capability.Authorized = false
	fake.Capability.MissingScopes = []string{"messages:send"}
	if got := probeDriverControlState(ctx, state, st.DB, fake); got.Available || state.Available() || got.Gap != "GAP-FD-003" {
		t.Fatalf("revoked probe=%+v snapshot=%+v", got, state.Snapshot())
	}
	fake.Capability.Authorized = true
	fake.Capability.MissingScopes = nil
	if got := probeDriverControlState(ctx, state, st.DB, fake); !got.Available || !state.Available() {
		t.Fatalf("restored probe=%+v snapshot=%+v", got, state.Snapshot())
	}
	// The same periodic probe also closes a post-start forged binding. Database
	// inventory can appear after startup, but can never elevate it to authority.
	binding, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{
		ProjectID: "default", WorkerIdentity: store.DriverControlIdentity, Role: store.DriverControlRole,
		HostID: "host", StoreID: "store", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server",
		LifecycleKey: "synthetic-control", TargetEpoch: 1, ProfileID: "flowbee-control",
		LifecycleOwnership: "driver_managed",
		WorkspaceRootID:    "root", WorkspaceRelativePath: ".", SessionID: "session",
		PaneInstanceID: "pane", AgentRunID: "run", Provider: "flowbee",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got := probeDriverControlState(ctx, state, st.DB, fake); got.Available || state.Available() {
		t.Fatalf("post-start synthetic binding did not close gate: %+v", got)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET state='fenced' WHERE binding_id=?`, binding.BindingID); err != nil {
		t.Fatal(err)
	}
	if got := probeDriverControlState(ctx, state, st.DB, fake); !got.Available || !state.Available() {
		t.Fatalf("clean binding registry did not recover: %+v", got)
	}
}

func TestDriverEndpointControlStateNeverCollapsesExactDomains(t *testing.T) {
	external := serveDriverEndpoint{InstanceRef: "external-default", Key: driver.EndpointKey{
		HostID: "host", StoreID: "external-store", TmuxServerDomainID: "default",
	}}
	managed := serveDriverEndpoint{InstanceRef: "flowbee-managed", Key: driver.EndpointKey{
		HostID: "host", StoreID: "managed-store", TmuxServerDomainID: "flowbee",
	}}
	state := newDriverEndpointControlState([]serveDriverEndpoint{external, managed})
	if state.Available() {
		t.Fatal("unproven endpoint inventory must be held")
	}
	state.Update(external.InstanceRef, api.DriverControlReadiness{Required: true, Available: true, Status: "ready"})
	if state.Available() {
		t.Fatal("one healthy domain must not authorize the other domain")
	}
	if !state.AvailableFor(external.Key) || state.AvailableFor(managed.Key) {
		t.Fatal("endpoint-scoped readiness did not remain isolated")
	}
	if state.AvailableFor(driver.EndpointKey{HostID: "host", StoreID: "external-store", TmuxServerDomainID: "flowbee"}) {
		t.Fatal("mixed host/store/domain tuple received routing authority")
	}
	state.Update(managed.InstanceRef, api.DriverControlReadiness{Required: true, Available: true, Status: "ready"})
	if !state.Available() {
		t.Fatalf("all exact endpoint proofs should make the capability inventory ready: %+v", state.Snapshot())
	}
	state.Update(external.InstanceRef, api.DriverControlReadiness{Required: true, Status: "route_unavailable", Reason: "revoked"})
	got := state.Snapshot()
	if got.Available || !strings.Contains(got.Reason, "external-default: revoked") {
		t.Fatalf("revoked external domain did not close aggregate inventory proof: %+v", got)
	}
	// Unknown references are ignored; a caller cannot add or overwrite routing
	// authority after the immutable inventory has been constructed.
	state.Update("single-default-fallback", api.DriverControlReadiness{Required: true, Available: true, Status: "ready"})
	if state.EndpointSnapshot("single-default-fallback").Available || state.Snapshot().Available {
		t.Fatal("unknown endpoint reference became routing authority")
	}
}
