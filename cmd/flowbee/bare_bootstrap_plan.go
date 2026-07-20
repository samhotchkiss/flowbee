package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type bareBootstrapTopology struct {
	project                            *config.BootstrapProjectConfig
	byRef                              map[string]config.DriverEndpoint
	external, managed, controlEndpoint config.DriverEndpoint
}

// resolveBareBootstrapTopology is a read-only, pre-mutation fence. Bootstrap is
// intentionally local-only in P1: the adopted Interactor, managed
// Orchestrator/control plane, and every seat must resolve to the exact same
// Driver host. A remote coordinate is rejected before the project-init marker,
// checkpoint, service ensure, or control-plane action can be written.
func resolveBareBootstrapTopology(cfg config.Config, inventory config.DriverEndpointInventory,
	projectID string) (bareBootstrapTopology, error) {
	var topology bareBootstrapTopology
	for i := range cfg.BootstrapProjects {
		if cfg.BootstrapProjects[i].ProjectID == projectID {
			topology.project = &cfg.BootstrapProjects[i]
			break
		}
	}
	if topology.project == nil {
		return topology, errors.New("bare bootstrap project is not configured")
	}
	topology.byRef = make(map[string]config.DriverEndpoint, len(inventory.Endpoints))
	for _, endpoint := range inventory.Endpoints {
		topology.byRef[endpoint.InstanceRef] = endpoint
	}
	project := topology.project
	var ok bool
	topology.external, ok = topology.byRef[project.Interactor.InstanceRef]
	if !ok || topology.external.ExpectedTmuxServerOwnership != "external" ||
		topology.external.ExpectedTmuxServerDomainID != "default" {
		return topology, errors.New("Interactor does not resolve to the exact external/default Driver endpoint")
	}
	topology.managed, ok = topology.byRef[project.Orchestrator.InstanceRef]
	if !ok || topology.managed.ExpectedTmuxServerOwnership != "managed_dedicated" ||
		topology.managed.ExpectedTmuxServerDomainID != "flowbee" {
		return topology, errors.New("Orchestrator does not resolve to the exact managed_dedicated Driver endpoint")
	}
	topology.controlEndpoint, ok = topology.byRef[project.ControlPlane.InstanceRef]
	if !ok || topology.controlEndpoint.ExpectedTmuxServerOwnership != "managed_dedicated" ||
		topology.controlEndpoint.ExpectedTmuxServerDomainID != "flowbee" ||
		project.ControlPlane.ProfileID != "flowbee_control" {
		return topology, errors.New("control plane does not resolve to the exact managed_dedicated Driver endpoint/profile")
	}
	localHostID := topology.external.ExpectedHostID
	if localHostID == "" || topology.managed.ExpectedHostID != localHostID ||
		topology.controlEndpoint.ExpectedHostID != localHostID {
		return topology, errors.New("bare bootstrap is local-only: Interactor, Orchestrator, and control endpoints must share one exact host_id")
	}
	for _, seat := range project.LocalSeats {
		endpoint, exists := topology.byRef[seat.InstanceRef]
		if !exists || endpoint.ExpectedTmuxServerOwnership != "managed_dedicated" ||
			endpoint.ExpectedHostID != seat.HostID || endpoint.ExpectedTmuxServerDomainID != seat.TmuxServerDomainID {
			return topology, fmt.Errorf("seat %s does not resolve to its exact managed Driver endpoint", seat.SeatID)
		}
		if seat.HostID != localHostID || endpoint.ExpectedHostID != localHostID {
			return topology, fmt.Errorf("bare bootstrap is local-only: seat %s is not on host_id %s", seat.SeatID, localHostID)
		}
	}
	return topology, nil
}

func buildBareServerActionPlan(cfg config.Config, inventory config.DriverEndpointInventory,
	preflight bareBootstrapPreflight, init bootstrap.ProjectInit) (bareServerActionPlan, error) {
	topology, err := resolveBareBootstrapTopology(cfg, inventory, preflight.ProjectID)
	if err != nil {
		return bareServerActionPlan{}, err
	}
	project := topology.project
	if project == nil || init.ProjectID != preflight.ProjectID || init.CWD != preflight.RepoRoot ||
		init.RepositoryOrigin != preflight.Origin {
		return bareServerActionPlan{}, errors.New("bare bootstrap exact project initialization changed")
	}
	byRef, external, managed, controlEndpoint := topology.byRef, topology.external, topology.managed, topology.controlEndpoint
	bootstrapID := "bootstrap-" + project.ProjectID
	plan := bareServerActionPlan{BootstrapID: bootstrapID, ProjectID: project.ProjectID,
		CWD: init.CWD, RepositoryOrigin: init.RepositoryOrigin,
		ControlPlane: bareControlPlaneSpec{InstanceRef: controlEndpoint.InstanceRef,
			HostID: controlEndpoint.ExpectedHostID, StoreID: controlEndpoint.ExpectedStoreID,
			TmuxServerDomainID:   controlEndpoint.ExpectedTmuxServerDomainID,
			TmuxServerInstanceID: project.ControlPlane.TmuxServerInstanceID,
			LifecycleKey:         project.ControlPlane.LifecycleKey, TargetEpoch: project.ControlPlane.TargetEpoch,
			ProfileID: project.ControlPlane.ProfileID, WorkspaceRootID: project.ControlPlane.WorkspaceRootID,
			WorkspaceRelativePath: project.ControlPlane.WorkspaceRelativePath},
		Attach: bootstrap.AttachIntentSpec{ID: "attach-" + project.ProjectID,
			InteractorActorID: project.Interactor.ActorID, TmuxServerDomainID: "default",
			PresentationName: project.Interactor.PresentationName}}
	appendAction := func(key, kind string, payload any) error {
		action, err := makeBareBootstrapAction(bootstrapID, project.ProjectID, key, kind, payload)
		if err == nil {
			plan.Actions = append(plan.Actions, action)
		}
		return err
	}
	if err := appendAction("project", "project_upsert", store.PortfolioProject{ID: project.ProjectID,
		Name: project.Name, State: "active", Priority: 100, SchedulerWeight: 1}); err != nil {
		return bareServerActionPlan{}, err
	}
	repositoryIDs := append([]string(nil), project.RepositoryIDs...)
	sort.Strings(repositoryIDs)
	for _, repoID := range repositoryIDs {
		if err := appendAction("repository:"+repoID, "repository_attach",
			map[string]string{"project_id": project.ProjectID, "repo_id": repoID}); err != nil {
			return bareServerActionPlan{}, err
		}
	}
	interactorRoute := store.ProjectActorRoute{ProjectID: project.ProjectID,
		Role: store.DriverInteractorRole, ActorID: project.Interactor.ActorID}
	if err := appendAction("actor-route:interactor", "actor_route", interactorRoute); err != nil {
		return bareServerActionPlan{}, err
	}
	interactorOwnership := "external_observed"
	if project.Interactor.Operation == "ensure" {
		interactorOwnership = "driver_managed"
	}
	interactorLifecycle := store.ProjectActorLifecycleCommand{ProjectID: project.ProjectID,
		Role: store.DriverInteractorRole, ActorID: project.Interactor.ActorID,
		Operation: project.Interactor.Operation, InstanceRef: external.InstanceRef, TargetHostID: external.ExpectedHostID,
		TargetStoreID: external.ExpectedStoreID, TargetServerDomainID: external.ExpectedTmuxServerDomainID,
		TargetServerID: project.Interactor.TmuxServerInstanceID, LifecycleOwnership: interactorOwnership,
		LifecycleKey: project.Interactor.LifecycleKey, TargetEpoch: project.Interactor.TargetEpoch,
		ProfileID: project.Interactor.ProfileID, ExternalWatchID: project.Interactor.ExternalWatchID,
		WorkspaceRootID: project.Interactor.WorkspaceRootID, WorkspaceRelativePath: project.Interactor.WorkspaceRelativePath,
		ManagedRecoveryProfileID:             project.Interactor.RecoveryProfileID,
		ManagedRecoveryWorkspaceRootID:       project.Interactor.RecoveryWorkspaceRootID,
		ManagedRecoveryWorkspaceRelativePath: project.Interactor.RecoveryWorkspaceRelativePath,
		ExpectedSessionID:                    project.Interactor.ExistingSessionID,
		ExpectedPaneInstanceID:               project.Interactor.ExpectedPaneInstanceID,
		ExpectedAgentRunID:                   project.Interactor.ExpectedAgentRunID}
	if err := appendAction("actor-lifecycle:interactor", "actor_lifecycle", interactorLifecycle); err != nil {
		return bareServerActionPlan{}, err
	}
	orchestratorRoute := store.ProjectActorRoute{ProjectID: project.ProjectID,
		Role: store.DriverOrchestratorRole, ActorID: project.Orchestrator.ActorID}
	if err := appendAction("actor-route:orchestrator", "actor_route", orchestratorRoute); err != nil {
		return bareServerActionPlan{}, err
	}
	orchestratorLifecycle := store.ProjectActorLifecycleCommand{ProjectID: project.ProjectID,
		Role: store.DriverOrchestratorRole, ActorID: project.Orchestrator.ActorID,
		Operation: "ensure", InstanceRef: managed.InstanceRef, TargetHostID: managed.ExpectedHostID,
		TargetStoreID: managed.ExpectedStoreID, TargetServerDomainID: managed.ExpectedTmuxServerDomainID,
		TargetServerID: project.Orchestrator.TmuxServerInstanceID, LifecycleOwnership: "driver_managed",
		LifecycleKey: project.Orchestrator.LifecycleKey, TargetEpoch: project.Orchestrator.TargetEpoch,
		ProfileID: project.Orchestrator.ProfileID, WorkspaceRootID: project.Orchestrator.WorkspaceRootID,
		WorkspaceRelativePath: project.Orchestrator.WorkspaceRelativePath}
	if err := appendAction("actor-lifecycle:orchestrator", "actor_lifecycle", orchestratorLifecycle); err != nil {
		return bareServerActionPlan{}, err
	}
	localSeats := append([]config.BootstrapSeatConfig(nil), project.LocalSeats...)
	sort.Slice(localSeats, func(i, j int) bool { return localSeats[i].SeatID < localSeats[j].SeatID })
	for _, configuredSeat := range localSeats {
		endpoint, exists := byRef[configuredSeat.InstanceRef]
		if !exists || endpoint.ExpectedTmuxServerOwnership != "managed_dedicated" ||
			endpoint.ExpectedHostID != configuredSeat.HostID || endpoint.ExpectedTmuxServerDomainID != configuredSeat.TmuxServerDomainID {
			return bareServerActionPlan{}, fmt.Errorf("seat %s does not resolve to its exact managed Driver endpoint", configuredSeat.SeatID)
		}
		seat := store.Seat{ID: configuredSeat.SeatID, Box: configuredSeat.Box, AgentFamily: configuredSeat.AgentFamily,
			AccountKey: configuredSeat.AccountKey, ConfigDir: configuredSeat.ConfigDir,
			CodexHome: configuredSeat.CodexHome, MaxConcurrent: configuredSeat.MaxConcurrent, Enabled: true}
		if seat.ComposeID() != configuredSeat.SeatID {
			return bareServerActionPlan{}, fmt.Errorf("seat %s is not the deterministic runtime identity", configuredSeat.SeatID)
		}
		payload := bootstrapSeatBindPayload{ProjectID: project.ProjectID, Seat: seat,
			Capacity: store.CapacitySeatIdentity{SeatID: configuredSeat.SeatID, HostID: configuredSeat.HostID,
				AccountKey: configuredSeat.AccountKey, CredentialLineage: configuredSeat.CredentialLineage,
				ReservePct: float64(configuredSeat.ReservePct), AccountMaximum: configuredSeat.AccountMaximum},
			Target: store.BuilderDriverTarget{ProjectID: project.ProjectID, SeatID: configuredSeat.SeatID,
				InstanceRef: configuredSeat.InstanceRef, TmuxServerDomainID: configuredSeat.TmuxServerDomainID,
				TmuxServerInstanceID: configuredSeat.TmuxServerInstanceID, ProfileID: configuredSeat.ProfileID,
				WorkspaceRootID:       configuredSeat.WorkspaceRootID,
				WorkspaceRelativeBase: configuredSeat.WorkspaceRelativeBase, Enabled: true}}
		if err := appendAction("seat:"+configuredSeat.SeatID, "seat_bind", payload); err != nil {
			return bareServerActionPlan{}, err
		}
	}
	// Bootstrap identity is a deterministic generation, not a project singleton.
	// The seed uses the complete normalized immutable plan while it still carries
	// stable per-key provisional action IDs. Any authorized target epoch or plan
	// change therefore creates a new checkpoint and disjoint action IDs; an exact
	// replay returns to the original generation. Prior checkpoints remain immutable.
	generationBody, err := json.Marshal(plan)
	if err != nil {
		return bareServerActionPlan{}, err
	}
	generationSum := sha256.Sum256(generationBody)
	generationID := bootstrapID + "-" + hex.EncodeToString(generationSum[:12])
	for i := range plan.Actions {
		oldActionID := plan.Actions[i].ActionID
		actionSum := sha256.Sum256([]byte(generationID + "\x00server-action-generation\x00" + oldActionID))
		plan.Actions[i].BootstrapID = generationID
		plan.Actions[i].ActionID = "bootstrap-" + hex.EncodeToString(actionSum[:16])
	}
	plan.BootstrapID = generationID
	return plan, nil
}

func makeBareBootstrapAction(bootstrapID, projectID, key, kind string, payload any) (api.BootstrapAction, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return api.BootstrapAction{}, err
	}
	idSum := sha256.Sum256([]byte(bootstrapID + "\x00server-action\x00" + key))
	payloadSum := sha256.Sum256(body)
	return api.BootstrapAction{FormatVersion: api.BootstrapActionFormat, BootstrapID: bootstrapID,
		ProjectID: projectID, ActionID: "bootstrap-" + hex.EncodeToString(idSum[:16]), Kind: kind,
		PayloadSHA256: "sha256:" + hex.EncodeToString(payloadSum[:]), Payload: body}, nil
}
