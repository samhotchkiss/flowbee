package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestProjectActivationDistinguishesConfiguredInventoryFromLiveCapacity(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.ProjectActorCredentialMaterializer = func(_, _, _, _ string, _ int64, _ time.Time) (string, error) {
		return "sha256:" + strings.Repeat("a", 64), nil
	}
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "mail-repo", Owner: "acme", Repo: "mail", Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "mail-repo", now); err != nil {
		t.Fatal(err)
	}
	for _, route := range []store.ProjectActorRoute{
		{ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-mail"},
		{ProjectID: "mail", Role: store.DriverOrchestratorRole, ActorID: "orchestrator-mail"},
	} {
		if _, err := st.RegisterProjectActor(ctx, route, now); err != nil {
			t.Fatal(err)
		}
	}

	buildSeat := store.Seat{Box: "", AgentFamily: "codex", CodexHome: "/codex/build", MaxConcurrent: 2}
	reviewSeat := store.Seat{Box: "", AgentFamily: "grok", ConfigDir: "/grok/review"}
	for i, item := range []*store.Seat{&buildSeat, &reviewSeat} {
		if err := st.AddSeat(ctx, *item, now); err != nil {
			t.Fatal(err)
		}
		item.ID = item.ComposeID()
		if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: item.ID,
			HostID: "driver-host", AccountKey: []string{"codex-account", "grok-account"}[i],
			CredentialLineage: []string{"codex-lineage", "grok-lineage"}[i], ReservePct: 10}, now); err != nil {
			t.Fatal(err)
		}
	}
	unrelatedReview := store.Seat{Box: "", AgentFamily: "grok", ConfigDir: "/grok/unrelated"}
	if err := st.AddSeat(ctx, unrelatedReview, now); err != nil {
		t.Fatal(err)
	}
	unrelatedReview.ID = unrelatedReview.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: unrelatedReview.ID,
		HostID: "driver-host", AccountKey: "grok-unrelated-account",
		CredentialLineage: "grok-unrelated-lineage", ReservePct: 10}, now); err != nil {
		t.Fatal(err)
	}
	// This seat belongs to the shared fleet but has no project target and no
	// capacity identity. It must be irrelevant to Mail's activation proof.
	unrelatedIncomplete := store.Seat{Box: "other-host", AgentFamily: "claude", ConfigDir: "/claude/unrelated"}
	if err := st.AddSeat(ctx, unrelatedIncomplete, now); err != nil {
		t.Fatal(err)
	}

	meta := driver.DriverMetadata{HostID: "driver-host", StoreID: "driver-store", ProducerBootID: "boot-1",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.9",
		TmuxServer: driver.TmuxServerMetadata{DomainID: "flowbee", Ownership: "managed_dedicated"}}
	externalMeta := driver.DriverMetadata{HostID: "external-host", StoreID: "external-store", ProducerBootID: "boot-external",
		ReplayFloorCursor: "tdc2.0", DurableHighWaterCursor: "tdc2.9",
		TmuxServer: driver.TmuxServerMetadata{DomainID: "default", Ownership: "external"}}
	obsStore := driver.ObservationSQLStore{DB: st.DB, Now: func() time.Time { return now }}
	if _, err := obsStore.EnsureInstance(ctx, "local-driver", meta); err != nil {
		t.Fatal(err)
	}
	if _, err := obsStore.EnsureInstance(ctx, "external-driver", externalMeta); err != nil {
		t.Fatal(err)
	}
	identities := []driver.Identity{
		{HostID: externalMeta.HostID, StoreID: externalMeta.StoreID, TmuxServerDomainID: "default", TmuxServerInstanceID: "server-external", SessionID: "interactor-session", PaneInstanceID: "pane-i", AgentRunID: "run-i", Provider: "claude"},
		{HostID: meta.HostID, StoreID: meta.StoreID, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", SessionID: "orchestrator-session", PaneInstanceID: "pane-o", AgentRunID: "run-o", Provider: "codex"},
		{HostID: meta.HostID, StoreID: meta.StoreID, TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1", SessionID: "reviewer-session", PaneInstanceID: "pane-r", AgentRunID: "run-r", Provider: "grok"},
	}
	if err := obsStore.ReplaceSnapshot(ctx, "external-driver", driver.SessionSnapshot{HostID: externalMeta.HostID,
		StoreID: externalMeta.StoreID, AsOfCursor: "tdc2.9", Sessions: []driver.SessionProjection{{
			Identity: identities[0], Lifecycle: "active", Phase: "idle", AsOfCursor: "tdc2.9", RawState: []byte(`{}`),
		}}}); err != nil {
		t.Fatal(err)
	}
	var managedProjections []driver.SessionProjection
	for _, id := range identities[1:] {
		managedProjections = append(managedProjections, driver.SessionProjection{Identity: id, Lifecycle: "active",
			Phase: "idle", AsOfCursor: "tdc2.9", RawState: []byte(`{}`)})
	}
	if err := obsStore.ReplaceSnapshot(ctx, "local-driver", driver.SessionSnapshot{HostID: meta.HostID,
		StoreID: meta.StoreID, AsOfCursor: "tdc2.9", Sessions: managedProjections}); err != nil {
		t.Fatal(err)
	}
	externalEndpointReady, managedEndpointReady := true, true
	st.DriverControlOriginEndpointGate = func(hostID, storeID, domainID string) bool {
		return externalEndpointReady && hostID == externalMeta.HostID && storeID == externalMeta.StoreID && domainID == "default" ||
			managedEndpointReady && hostID == meta.HostID && storeID == meta.StoreID && domainID == "flowbee"
	}
	bindings := []struct {
		identity, role, instanceRef, ownership, watchID string
		id                                              driver.Identity
	}{
		{"interactor-mail", store.DriverInteractorRole, "external-driver", "external_observed", "watch-interactor", identities[0]},
		{"orchestrator-mail", store.DriverOrchestratorRole, "local-driver", "driver_managed", "", identities[1]},
		{"reviewer-mail", store.DriverReviewerRole, "local-driver", "driver_managed", "", identities[2]},
	}
	for _, item := range bindings {
		seatID := ""
		if item.role == store.DriverReviewerRole {
			seatID = reviewSeat.ID
		}
		if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{ProjectID: "mail",
			WorkerIdentity: item.identity, Role: item.role, SeatID: seatID,
			HostID: item.id.HostID, StoreID: item.id.StoreID,
			TmuxServerDomainID: item.id.TmuxServerDomainID, TmuxServerInstanceID: item.id.TmuxServerInstanceID,
			LifecycleOwnership: item.ownership, ExternalWatchID: item.watchID, LifecycleKey: item.identity,
			TargetEpoch: 1, ProfileID: item.id.Provider,
			WorkspaceRootID:       map[bool]string{true: "projects"}[item.ownership == "driver_managed"],
			WorkspaceRelativePath: map[bool]string{true: "mail"}[item.ownership == "driver_managed"], SessionID: item.id.SessionID,
			PaneInstanceID: item.id.PaneInstanceID, AgentRunID: item.id.AgentRunID,
			Provider: item.id.Provider, ObservedAt: now}, now); err != nil {
			t.Fatal(err)
		}
	}
	// Actor readiness requires the durable lifecycle state to name the same
	// exact binding; a legacy binding alone must not make activation green.
	for index, item := range bindings[:2] {
		route, err := st.GetProjectActor(ctx, "mail", item.role)
		if err != nil {
			t.Fatal(err)
		}
		operation := "ensure"
		if item.ownership == "external_observed" {
			operation = "adopt"
		}
		command := store.ProjectActorLifecycleCommand{
			ProjectID: "mail", Role: item.role, ActorID: item.identity,
			ExpectedRouteStateVersion: int64(route.StateVersion), Operation: operation,
			IdempotencyKey: operation + "-" + item.identity, InstanceRef: item.instanceRef,
			TargetHostID: item.id.HostID, TargetStoreID: item.id.StoreID,
			TargetServerDomainID: item.id.TmuxServerDomainID, TargetServerID: item.id.TmuxServerInstanceID,
			LifecycleOwnership: item.ownership, ExternalWatchID: item.watchID,
			LifecycleKey: item.identity, TargetEpoch: 1, ProfileID: map[string]string{
				store.DriverOrchestratorRole: "codex_orchestrator", store.DriverInteractorRole: item.id.Provider,
			}[item.role],
		}
		if item.ownership == "driver_managed" {
			command.WorkspaceRootID, command.WorkspaceRelativePath = "projects", "mail"
		} else {
			command.ExpectedSessionID, command.ExpectedPaneInstanceID = item.id.SessionID, item.id.PaneInstanceID
			command.ExpectedAgentRunID = item.id.AgentRunID
			command.ManagedRecoveryProfileID = "claude_interactor_managed"
			command.ManagedRecoveryWorkspaceRootID = "projects"
			command.ManagedRecoveryWorkspaceRelativePath = "mail"
		}
		_, _, err = st.CommitProjectActorLifecycleIntent(ctx, command, now.Add(time.Duration(index+1)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		action, err := st.ClaimNextProjectActorLifecycleAction(ctx, "activation-fixture",
			now.Add(time.Duration(index+2)*time.Second), now.Add(time.Duration(index+20)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		status := "ensured"
		if operation == "adopt" {
			status = "adopted"
		}
		receipt := store.ProjectActorLifecycleReceiptProjection{ActionID: action.ID,
			Operation: operation, LifecycleKey: action.LifecycleKey, ExternalWatchID: item.watchID, Status: status,
			ActionEpoch: action.ActionEpoch, TargetEpoch: action.TargetEpoch,
			LeaseID: action.LeaseID, LeaseEpoch: action.LeaseEpoch,
			TmuxServerDomainID: action.TargetServerDomainID,
			IdentityAfter: store.ProjectActorLifecycleIdentity{HostID: item.id.HostID, StoreID: item.id.StoreID,
				TmuxServerDomainID: item.id.TmuxServerDomainID, TmuxServerInstanceID: item.id.TmuxServerInstanceID,
				LifecycleOwnership: item.ownership, LifecycleKey: item.identity, TargetEpoch: 1,
				SessionID: item.id.SessionID, PaneInstanceID: item.id.PaneInstanceID, AgentRunID: item.id.AgentRunID,
				Provider: item.id.Provider},
		}
		if err := st.ProjectProjectActorLifecycleResult(ctx, receipt, now.Add(time.Duration(index+3)*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := st.AcknowledgeProjectActorLifecycleAction(ctx, action.ID, "activation-fixture",
			action.ActionEpoch, now.Add(time.Duration(index+4)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "mail",
		SeatID: buildSeat.ID, InstanceRef: "local-driver", TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "server-1",
		ProfileID: "codex-builder", WorkspaceRootID: "projects", WorkspaceRelativeBase: "mail", Enabled: true,
	}, now); err != nil {
		t.Fatal(err)
	}

	configured, err := st.ProjectActivation(ctx, "mail", now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !configured.Configured || configured.LiveReady || configured.ActiveCapacityGeneration != "" {
		t.Fatalf("configured status=%+v", configured)
	}

	observations := []store.CapacitySeatObservation{
		{ObservationID: "obs-build", SeatID: buildSeat.ID, HostID: meta.HostID, Provider: "codex",
			AccountKey: "codex-account", CredentialLineage: "codex-lineage", CollectorID: "collector",
			Source: "live_app_server", TrustState: "verified", IntegrityState: "verified",
			Windows:   []capacity.RouteWindow{{Kind: "weekly", Applicable: true, Known: true, Percent: 20}},
			FetchedAt: now, RawSHA256: "sha256:build", AdapterVersion: "fixture/v1"},
		{ObservationID: "obs-review", SeatID: reviewSeat.ID, HostID: meta.HostID, Provider: "grok",
			AccountKey: "grok-account", CredentialLineage: "grok-lineage", CollectorID: "collector",
			Source: "live_billing", TrustState: "verified", IntegrityState: "verified", BillingPeriodActive: true,
			Windows:   []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 10}},
			FetchedAt: now, RawSHA256: "sha256:review", AdapterVersion: "fixture/v1"},
		{ObservationID: "obs-unrelated-review", SeatID: unrelatedReview.ID, HostID: meta.HostID, Provider: "grok",
			AccountKey: "grok-unrelated-account", CredentialLineage: "grok-unrelated-lineage", CollectorID: "collector",
			Source: "live_billing", TrustState: "verified", IntegrityState: "verified", BillingPeriodActive: true,
			Windows:   []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 5}},
			FetchedAt: now, RawSHA256: "sha256:unrelated-review", AdapterVersion: "fixture/v1"},
	}
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-1", StartedAt: now,
		ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID, unrelatedReview.ID}, Observations: observations}, now); err != nil {
		t.Fatal(err)
	}
	live, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !live.Configured || !live.LiveReady || live.RoutableSeats != 1 || live.EnabledSeats != 1 ||
		live.CapacityBoundSeats != 1 || live.ActiveCapacityGeneration != "generation-1" || len(live.Holds) != 0 {
		t.Fatalf("live status=%+v", live)
	}

	t.Run("dedicated worker activation uses routable reviewer pool without persistent session", func(t *testing.T) {
		st.EnableEpicDedicatedWorkersV2 = true
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET profile_id='codex_builder'
			WHERE project_id='mail' AND seat_id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "mail",
			SeatID: reviewSeat.ID, InstanceRef: "local-driver", TmuxServerDomainID: "flowbee",
			TmuxServerInstanceID: "server-1", ProfileID: "grok_reviewer", WorkspaceRootID: "projects",
			WorkspaceRelativeBase: "mail-review", Enabled: true}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET state='superseded'
			WHERE project_id='mail' AND role='code_reviewer' AND state='active'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil || !status.LiveReady || status.CurrentReviewerBindings != 0 ||
			status.EnabledReviewerTargets != 1 || status.CurrentReviewerTargets != 1 ||
			status.BuilderDistinctReviewerReadyTargets != 1 {
			t.Fatalf("dedicated pool activation=%+v err=%v", status, err)
		}

		blocked := append([]store.CapacitySeatObservation(nil), observations...)
		for i := range blocked {
			blocked[i].ObservationID += "-blocked"
			blocked[i].RawSHA256 += "-blocked"
			blocked[i].FetchedAt = now.Add(2 * time.Minute)
			if blocked[i].SeatID == reviewSeat.ID {
				blocked[i].Windows = []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 100}}
			}
		}
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-reviewer-blocked",
			StartedAt: now.Add(2 * time.Minute), ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID,
				unrelatedReview.ID}, Observations: blocked}, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		status, err = st.ProjectActivation(ctx, "mail", now.Add(3*time.Minute), 5*time.Minute)
		if err != nil || status.LiveReady || !activationHasHold(status, "reviewer_family_capacity_not_routable") {
			t.Fatalf("unroutable reviewer pool false-greened=%+v err=%v", status, err)
		}

		sameFamily := append([]store.CapacitySeatObservation(nil), observations...)
		for i := range sameFamily {
			sameFamily[i].ObservationID += "-same-pool"
			sameFamily[i].RawSHA256 += "-same-pool"
			sameFamily[i].FetchedAt = now.Add(4 * time.Minute)
			if sameFamily[i].SeatID == buildSeat.ID {
				sameFamily[i].Provider, sameFamily[i].Source, sameFamily[i].BillingPeriodActive = "grok", "live_billing", true
				sameFamily[i].Windows = []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 10}}
			}
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE seats SET agent_family='grok' WHERE id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET profile_id='grok_builder'
			WHERE project_id='mail' AND seat_id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-same-pool-family",
			StartedAt: now.Add(4 * time.Minute), ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID,
				unrelatedReview.ID}, Observations: sameFamily}, now.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		status, err = st.ProjectActivation(ctx, "mail", now.Add(5*time.Minute), 5*time.Minute)
		if err != nil || status.LiveReady || status.BuilderDistinctReviewerReadyTargets != 0 ||
			!activationHasHold(status, "builder_distinct_reviewer_unavailable") {
			t.Fatalf("same-family reviewer pool false-greened=%+v err=%v", status, err)
		}

		// Restore the legacy fixture for the remaining independent lenses.
		st.EnableEpicDedicatedWorkersV2 = false
		if _, err := st.DB.ExecContext(ctx, `UPDATE seats SET agent_family='codex' WHERE id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET profile_id='codex-builder'
			WHERE project_id='mail' AND seat_id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `DELETE FROM builder_driver_targets
			WHERE project_id='mail' AND seat_id=?`, reviewSeat.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET state='active'
			WHERE project_id='mail' AND role='code_reviewer'`); err != nil {
			t.Fatal(err)
		}
		restored := append([]store.CapacitySeatObservation(nil), observations...)
		for i := range restored {
			restored[i].ObservationID += "-post-dedicated"
			restored[i].RawSHA256 += "-post-dedicated"
			restored[i].FetchedAt = now.Add(6 * time.Minute)
		}
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-post-dedicated",
			StartedAt: now.Add(6 * time.Minute), ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID,
				unrelatedReview.ID}, Observations: restored}, now.Add(6*time.Minute)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("same-family-only reviewer cannot green builder readiness", func(t *testing.T) {
		sameFamilyBuild := store.Seat{Box: "", AgentFamily: "grok", ConfigDir: "/grok/same-family-build"}
		if err := st.AddSeat(ctx, sameFamilyBuild, now); err != nil {
			t.Fatal(err)
		}
		sameFamilyBuild.ID = sameFamilyBuild.ComposeID()
		if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{SeatID: sameFamilyBuild.ID,
			HostID: meta.HostID, AccountKey: "grok-build-account", CredentialLineage: "grok-build-lineage",
			ReservePct: 10}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "mail",
			SeatID: sameFamilyBuild.ID, InstanceRef: "local-driver", TmuxServerDomainID: "flowbee",
			TmuxServerInstanceID: "server-1", ProfileID: "grok-builder", WorkspaceRootID: "projects",
			WorkspaceRelativeBase: "mail-same-family", Enabled: true}, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET enabled=0
			WHERE project_id='mail' AND seat_id=?`, buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		sameFamilyObservations := append([]store.CapacitySeatObservation(nil), observations...)
		for i := range sameFamilyObservations {
			sameFamilyObservations[i].ObservationID += "-same-family"
			sameFamilyObservations[i].RawSHA256 += "-same-family"
			sameFamilyObservations[i].FetchedAt = now.Add(time.Minute)
		}
		sameFamilyObservations = append(sameFamilyObservations, store.CapacitySeatObservation{
			ObservationID: "obs-same-family-build", SeatID: sameFamilyBuild.ID, HostID: meta.HostID,
			Provider: "grok", AccountKey: "grok-build-account", CredentialLineage: "grok-build-lineage",
			CollectorID: "collector", Source: "live_billing", TrustState: "verified", IntegrityState: "verified",
			BillingPeriodActive: true,
			Windows:             []capacity.RouteWindow{{Kind: "monthly", Applicable: true, Known: true, Percent: 10}},
			FetchedAt:           now.Add(time.Minute), RawSHA256: "sha256:same-family-build", AdapterVersion: "fixture/v1",
		})
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-same-family",
			StartedAt: now.Add(time.Minute), ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID,
				unrelatedReview.ID, sameFamilyBuild.ID}, Observations: sameFamilyObservations}, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(2*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || status.BuilderDistinctReviewerReadyTargets != 0 ||
			!activationHasHold(status, "builder_distinct_reviewer_unavailable") {
			t.Fatalf("same-family-only reviewer false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET enabled=CASE seat_id WHEN ? THEN 1 ELSE 0 END
			WHERE project_id='mail' AND seat_id IN (?,?)`, buildSeat.ID, buildSeat.ID, sameFamilyBuild.ID); err != nil {
			t.Fatal(err)
		}
		restoredObservations := append([]store.CapacitySeatObservation(nil), observations...)
		for i := range restoredObservations {
			restoredObservations[i].ObservationID += "-restored"
			restoredObservations[i].RawSHA256 += "-restored"
			restoredObservations[i].FetchedAt = now.Add(2 * time.Minute)
		}
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-restored",
			StartedAt: now.Add(2 * time.Minute), ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID,
				unrelatedReview.ID}, Observations: restoredObservations}, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("actor role topology cannot collapse onto managed domain", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET lifecycle_ownership='driver_managed'
			WHERE project_id='mail' AND role='interactor' AND state='active'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "interactor_endpoint_topology_invalid") {
			t.Fatalf("managed Interactor false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET lifecycle_ownership='external_observed'
			WHERE project_id='mail' AND role='interactor' AND state='active'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("managed actor topology requires exact reserved presentation", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles SET presentation_name='wrong-name'
			WHERE project_id='mail' AND role='orchestrator' AND actor_id='orchestrator-mail'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "orchestrator_endpoint_topology_invalid") {
			t.Fatalf("wrong managed presentation false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE project_actor_lifecycles SET presentation_name='mail-orchestrator'
			WHERE project_id='mail' AND role='orchestrator' AND actor_id='orchestrator-mail'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reviewer must stay on managed dedicated topology", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET lifecycle_ownership='external_observed'
			WHERE project_id='mail' AND role='code_reviewer' AND state='active'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "reviewer_endpoint_topology_invalid") {
			t.Fatalf("external reviewer false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_bindings SET lifecycle_ownership='driver_managed'
			WHERE project_id='mail' AND role='code_reviewer' AND state='active'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET tmux_server_ownership='external'
			WHERE instance_ref='local-driver'`); err != nil {
			t.Fatal(err)
		}
		status, err = st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "reviewer_endpoint_topology_invalid") {
			t.Fatalf("reviewer on externally-owned non-default endpoint false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET tmux_server_ownership='managed_dedicated'
			WHERE instance_ref='local-driver'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("builder target must stay on managed dedicated topology", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET tmux_server_domain_id='default'
			WHERE project_id='mail'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "builder_endpoint_topology_invalid") {
			t.Fatalf("external builder target false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets SET tmux_server_domain_id='flowbee'
			WHERE project_id='mail'`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET tmux_server_ownership='external'
			WHERE instance_ref='local-driver'`); err != nil {
			t.Fatal(err)
		}
		status, err = st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || !activationHasHold(status, "builder_endpoint_topology_invalid") {
			t.Fatalf("builder on externally-owned non-default endpoint false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET tmux_server_ownership='managed_dedicated'
			WHERE instance_ref='local-driver'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("endpoint capability is required for each role", func(t *testing.T) {
		externalEndpointReady = false
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || !activationHasHold(status, "interactor_endpoint_control_unavailable") {
			t.Fatalf("revoked external endpoint false-greened: %+v", status)
		}
		externalEndpointReady = true

		managedEndpointReady = false
		status, err = st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || !activationHasHold(status, "orchestrator_endpoint_control_unavailable") ||
			!activationHasHold(status, "reviewer_endpoint_control_unavailable") ||
			!activationHasHold(status, "builder_endpoint_control_unavailable") {
			t.Fatalf("revoked managed endpoint false-greened: %+v", status)
		}
		managedEndpointReady = true
	})

	t.Run("unrelated incomplete seat does not block project", func(t *testing.T) {
		if !live.LiveReady || live.EnabledSeats != 1 || live.CapacityBoundSeats != 1 {
			t.Fatalf("global incomplete seat leaked into project readiness: %+v", live)
		}
	})

	t.Run("stale builder tmux server incarnation cannot green project", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets
			SET tmux_server_instance_id='server-replaced' WHERE project_id='mail' AND seat_id=?`,
			buildSeat.ID); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || status.Configured || status.CurrentBuilderTargets != 0 {
			t.Fatalf("stale builder server incarnation remained authoritative: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE builder_driver_targets
			SET tmux_server_instance_id='server-1' WHERE project_id='mail' AND seat_id=?`,
			buildSeat.ID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("reviewer binding family must have bound capacity", func(t *testing.T) {
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_projections SET provider='claude'
			WHERE session_id='reviewer-session'`); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.Configured || status.LiveReady || len(status.Reviewers) != 1 ||
			status.Reviewers[0].FamilyCapacityConfigured {
			t.Fatalf("reviewer with an unprovisioned family false-greened: %+v", status)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_projections SET provider='grok'
			WHERE session_id='reviewer-session'`); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("idle projection remains current while Driver ingestion is fresh", func(t *testing.T) {
		stale := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_session_projections SET updated_at=?`, stale); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(2*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if !status.LiveReady || status.CurrentReviewerBindings != 1 ||
			!status.Actors[0].BindingCurrent || !status.Actors[1].BindingCurrent {
			t.Fatalf("idle exact projection was confused with stale ingestion: %+v", status)
		}
	})

	t.Run("stale Driver ingestion cannot green exact projections or reviewers", func(t *testing.T) {
		stale := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_observation_cursors SET updated_at=?`, stale); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(2*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || status.CurrentReviewerBindings != 0 ||
			status.Actors[0].BindingCurrent || status.Actors[1].BindingCurrent {
			t.Fatalf("stale ingestion left old projections authoritative: %+v", status)
		}
		fresh := now.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_observation_cursors SET updated_at=?`, fresh); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("stale Driver instance cannot green actors or reviewers", func(t *testing.T) {
		stale := now.Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET updated_at=?`, stale); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(2*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || status.CurrentReviewerBindings != 0 ||
			status.Actors[0].BindingCurrent || status.Actors[1].BindingCurrent {
			t.Fatalf("stale Driver instance remained authoritative: %+v", status)
		}
		fresh := now.Add(2 * time.Minute).UTC().Format(time.RFC3339Nano)
		if _, err := st.DB.ExecContext(ctx, `UPDATE driver_instances SET updated_at=?`, fresh); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unrelated routable reviewer seat cannot green project", func(t *testing.T) {
		second := append([]store.CapacitySeatObservation(nil), observations...)
		second[0].ObservationID = "obs-build-routable"
		second[0].FetchedAt = now.Add(2 * time.Minute)
		second[0].RawSHA256 = "sha256:build-routable"
		second[1].ObservationID = "obs-review-held"
		second[1].RateLimited = true
		second[1].FetchedAt = now.Add(2 * time.Minute)
		second[1].RawSHA256 = "sha256:review-held"
		second[2].ObservationID = "obs-unrelated-review-routable"
		second[2].FetchedAt = now.Add(2 * time.Minute)
		second[2].RawSHA256 = "sha256:unrelated-review-routable"
		if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{ID: "generation-2",
			StartedAt:       now.Add(2 * time.Minute),
			ExpectedSeatIDs: []string{buildSeat.ID, reviewSeat.ID, unrelatedReview.ID},
			Observations:    second}, now.Add(2*time.Minute)); err != nil {
			t.Fatal(err)
		}
		status, err := st.ProjectActivation(ctx, "mail", now.Add(3*time.Minute), 5*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if status.LiveReady || status.RoutableSeats != 1 || len(status.Reviewers) != 1 ||
			status.Reviewers[0].FamilyCapacityRoutable {
			t.Fatalf("unrelated reviewer seat incorrectly greened exact reviewer route: %+v", status)
		}
	})
}

func activationHasHold(status store.ProjectActivationStatus, want string) bool {
	for _, hold := range status.Holds {
		if hold == want {
			return true
		}
	}
	return false
}

func TestProjectActivationRejectsLegacyActorBindingWithoutLifecycle(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "legacy", Name: "Legacy"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterProjectActor(ctx, store.ProjectActorRoute{ProjectID: "legacy",
		Role: store.DriverInteractorRole, ActorID: "legacy-interactor"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertDriverSessionBinding(ctx, store.DriverSessionBinding{ProjectID: "legacy",
		WorkerIdentity: "legacy-interactor", Role: store.DriverInteractorRole,
		HostID: "mac", StoreID: "legacy-store", TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "legacy-server", LifecycleOwnership: "driver_managed",
		LifecycleKey: "legacy-interactor", TargetEpoch: 1, ProfileID: "claude",
		WorkspaceRootID: "projects", WorkspaceRelativePath: "legacy", SessionID: "legacy-session",
		PaneInstanceID: "legacy-pane", AgentRunID: "legacy-run", Provider: "claude", ObservedAt: now,
	}, now); err != nil {
		t.Fatal(err)
	}
	status, err := st.ProjectActivation(ctx, "legacy", now, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if status.Configured || len(status.Actors) == 0 || status.Actors[0].LifecycleReady {
		t.Fatalf("legacy actor binding produced false-green activation: %+v", status)
	}
	found := false
	for _, hold := range status.Holds {
		found = found || hold == "missing_interactor_lifecycle"
	}
	if !found {
		t.Fatalf("missing lifecycle was not visible: %+v", status.Holds)
	}
}
