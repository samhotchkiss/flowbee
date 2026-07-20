package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestExternalObservedDriverBindingRequiresAndRoundTripsAdoptAuthority(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	in := store.DriverSessionBinding{ProjectID: "default", WorkerIdentity: "russ-claude",
		Role: store.DriverInteractorRole, HostID: "mac", StoreID: "external-store",
		TmuxServerDomainID: "default", TmuxServerInstanceID: "external-server",
		LifecycleOwnership: "external_observed", ExternalWatchID: "88888888-8888-4888-8888-888888888888",
		LifecycleKey: "project:default:interactor", TargetEpoch: 4, ProfileID: "external_actor_policy",
		SessionID: "session-russ", PaneInstanceID: "pane-russ", AgentRunID: "run-russ",
		Provider: "claude", ObservedAt: now}
	created, err := st.UpsertDriverSessionBinding(ctx, in, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.ActiveDriverSessionBinding(ctx, "default", "russ-claude", store.DriverInteractorRole)
	if err != nil {
		t.Fatal(err)
	}
	if got.BindingID != created.BindingID || got.ExternalWatchID != in.ExternalWatchID ||
		got.LifecycleOwnership != "external_observed" || got.TmuxServerDomainID != "default" ||
		got.LifecycleKey != in.LifecycleKey || got.TargetEpoch != in.TargetEpoch {
		t.Fatalf("round trip=%+v", got)
	}

	unowned := in
	unowned.WorkerIdentity = "raw-observation"
	unowned.LifecycleOwnership, unowned.ExternalWatchID = "", ""
	if _, err := st.UpsertDriverSessionBinding(ctx, unowned, now); err == nil {
		t.Fatal("unowned observation became active routing authority")
	}
	withWorkspace := in
	withWorkspace.WorkerIdentity = "external-with-workspace"
	withWorkspace.WorkspaceRootID, withWorkspace.WorkspaceRelativePath = "projects", "mail"
	if _, err := st.UpsertDriverSessionBinding(ctx, withWorkspace, now); err == nil {
		t.Fatal("external adoption accepted managed workspace authority")
	}
}
