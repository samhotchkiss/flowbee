package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProjectActivationStatus is the operator-facing proof that a project has the
// durable inventory required by the v2 control plane. Configured covers facts an
// operator can establish while serve is stopped. LiveReady additionally requires
// a complete, fresh capacity generation produced by the running collector.
type ProjectActivationStatus struct {
	Project                             PortfolioProject            `json:"project"`
	RepositoryIDs                       []string                    `json:"repository_ids"`
	Actors                              []ProjectActorActivation    `json:"actors"`
	EnabledSeats                        int                         `json:"enabled_seats"`
	CapacityBoundSeats                  int                         `json:"capacity_bound_seats"`
	EnabledBuilderTargets               int                         `json:"enabled_builder_targets"`
	CurrentBuilderTargets               int                         `json:"current_builder_targets"`
	BuilderTopologyReadyTargets         int                         `json:"builder_topology_ready_targets"`
	BuilderEndpointControlReadyTargets  int                         `json:"builder_endpoint_control_ready_targets"`
	BuilderDistinctReviewerReadyTargets int                         `json:"builder_distinct_reviewer_ready_targets"`
	CurrentReviewerBindings             int                         `json:"current_reviewer_bindings"`
	ReviewerIdentities                  []string                    `json:"reviewer_identities"`
	Reviewers                           []ProjectReviewerActivation `json:"reviewers"`
	ActiveCapacityGeneration            string                      `json:"active_capacity_generation,omitempty"`
	RoutableSeats                       int                         `json:"routable_seats"`
	Configured                          bool                        `json:"configured"`
	LiveReady                           bool                        `json:"live_ready"`
	Holds                               []string                    `json:"holds"`
}

// ProjectReviewerActivation proves more than binding presence: the exact live
// Driver incarnation supplies the family, and an operator-bound capacity seat
// for that family must exist and be routable before activation can turn green.
type ProjectReviewerActivation struct {
	WorkerIdentity           string `json:"worker_identity"`
	SeatID                   string `json:"seat_id,omitempty"`
	ModelFamily              string `json:"model_family,omitempty"`
	TmuxServerDomainID       string `json:"tmux_server_domain_id,omitempty"`
	LifecycleOwnership       string `json:"lifecycle_ownership,omitempty"`
	TmuxServerOwnership      string `json:"tmux_server_ownership,omitempty"`
	TopologyReady            bool   `json:"topology_ready"`
	EndpointControlReady     bool   `json:"endpoint_control_ready"`
	FamilyCapacityConfigured bool   `json:"family_capacity_configured"`
	FamilyCapacityRoutable   bool   `json:"family_capacity_routable"`
}

type ProjectActorActivation struct {
	Role                 string `json:"role"`
	ActorID              string `json:"actor_id,omitempty"`
	RouteState           string `json:"route_state,omitempty"`
	BindingID            string `json:"binding_id,omitempty"`
	SessionID            string `json:"session_id,omitempty"`
	HostID               string `json:"host_id,omitempty"`
	StoreID              string `json:"store_id,omitempty"`
	TmuxServerDomainID   string `json:"tmux_server_domain_id,omitempty"`
	LifecycleOwnership   string `json:"lifecycle_ownership,omitempty"`
	TmuxServerOwnership  string `json:"tmux_server_ownership,omitempty"`
	BindingCurrent       bool   `json:"binding_current"`
	TopologyReady        bool   `json:"topology_ready"`
	EndpointControlReady bool   `json:"endpoint_control_ready"`
	LifecycleState       string `json:"lifecycle_state,omitempty"`
	LifecycleOperation   string `json:"lifecycle_operation,omitempty"`
	LifecycleHoldKind    string `json:"lifecycle_hold_kind,omitempty"`
	LifecycleHoldReason  string `json:"lifecycle_hold_reason,omitempty"`
	LifecycleReady       bool   `json:"lifecycle_ready"`
}

// ProjectActivation reports configuration and live routing evidence without
// mutating either. Exact-current means the binding's store/session/pane/run
// incarnation is present in the live Driver projection; names, CWDs, PIDs, and
// timestamps are never fallback authority.
func (s *Store) ProjectActivation(ctx context.Context, projectID string, now time.Time,
	capacityFreshFor time.Duration) (ProjectActivationStatus, error) {
	project, err := s.GetPortfolioProject(ctx, projectID)
	if err != nil {
		return ProjectActivationStatus{}, err
	}
	out := ProjectActivationStatus{Project: project}
	out.RepositoryIDs, err = s.ProjectRepoIDs(ctx, projectID, true)
	if err != nil {
		return ProjectActivationStatus{}, err
	}
	if project.State != "active" {
		out.Holds = append(out.Holds, "project_not_active")
	}
	if len(out.RepositoryIDs) == 0 {
		out.Holds = append(out.Holds, "no_active_repository")
	}

	for _, role := range []string{DriverInteractorRole, DriverOrchestratorRole} {
		actor := ProjectActorActivation{Role: role}
		route, routeErr := s.GetProjectActor(ctx, projectID, role)
		if routeErr == nil {
			actor.ActorID, actor.RouteState = route.ActorID, route.State
			lifecycle, lifecycleErr := s.GetProjectActorLifecycle(ctx, projectID, role, route.ActorID)
			if lifecycleErr == nil {
				actor.LifecycleState, actor.LifecycleOperation = lifecycle.State, lifecycle.DesiredOperation
				actor.LifecycleHoldKind, actor.LifecycleHoldReason = lifecycle.HoldKind, lifecycle.HoldReason
			} else if !errors.Is(lifecycleErr, sql.ErrNoRows) {
				return ProjectActivationStatus{}, lifecycleErr
			}
			binding, bindErr := s.ActiveDriverSessionBinding(ctx, projectID, route.ActorID, role)
			if bindErr == nil {
				actor.BindingID, actor.SessionID = binding.BindingID, binding.SessionID
				actor.HostID, actor.StoreID = binding.HostID, binding.StoreID
				actor.TmuxServerDomainID, actor.LifecycleOwnership = binding.TmuxServerDomainID, binding.LifecycleOwnership
				actor.BindingCurrent, err = currentDriverBinding(ctx, s.DB, binding, now, capacityFreshFor)
				if err != nil {
					return ProjectActivationStatus{}, err
				}
				actor.EndpointControlReady = s.HasDriverControlOriginForBinding(binding)
				actor.TmuxServerOwnership, err = driverInstanceOwnership(ctx, s.DB, binding.HostID, binding.StoreID)
				if err != nil {
					return ProjectActivationStatus{}, err
				}
				switch role {
				case DriverInteractorRole:
					actor.TopologyReady = binding.LifecycleOwnership == "external_observed" &&
						binding.TmuxServerDomainID == "default" && actor.TmuxServerOwnership == "external"
				case DriverOrchestratorRole:
					actor.TopologyReady = binding.LifecycleOwnership == "driver_managed" &&
						binding.TmuxServerDomainID != "default" && actor.TmuxServerOwnership == "managed_dedicated"
				}
				actor.LifecycleReady = actor.LifecycleState == "active" && lifecycleErr == nil &&
					lifecycle.ActiveBindingID == binding.BindingID
			}
		}
		if routeErr != nil && !errors.Is(routeErr, ErrProjectNotFound) {
			return ProjectActivationStatus{}, routeErr
		}
		if actor.ActorID == "" || actor.RouteState != "active" {
			out.Holds = append(out.Holds, "missing_"+role+"_route")
		} else if actor.LifecycleState == "" {
			out.Holds = append(out.Holds, "missing_"+role+"_lifecycle")
		} else if !actor.LifecycleReady {
			out.Holds = append(out.Holds, role+"_lifecycle_"+actor.LifecycleState)
		} else if !actor.BindingCurrent {
			out.Holds = append(out.Holds, "missing_current_"+role+"_binding")
		} else if !actor.TopologyReady {
			out.Holds = append(out.Holds, role+"_endpoint_topology_invalid")
		} else if !actor.EndpointControlReady {
			out.Holds = append(out.Holds, role+"_endpoint_control_unavailable")
		}
		out.Actors = append(out.Actors, actor)
	}

	// A project's capacity inventory is the set it explicitly routes through its
	// builder targets. Global seats are shared fleet inventory, not implicit
	// project membership: an unrelated incomplete seat must not block this project,
	// and an unrelated routable seat must never make it ready.
	targetRows, err := s.DB.QueryContext(ctx, `SELECT t.seat_id,t.instance_ref,t.tmux_server_domain_id,t.tmux_server_instance_id,
		COALESCE(s.enabled,0),COALESCE(s.agent_family,''),COALESCE(s.expected_host_id,''),COALESCE(s.expected_account_key,''),
		COALESCE(s.expected_credential_lineage,''),COALESCE(i.store_id,''),COALESCE(i.tmux_server_ownership,'')
		FROM builder_driver_targets t LEFT JOIN seats s ON s.id=t.seat_id
		LEFT JOIN driver_instances i ON i.instance_ref=t.instance_ref
		WHERE t.project_id=? AND t.enabled=1 ORDER BY t.seat_id`, projectID)
	if err != nil {
		return ProjectActivationStatus{}, err
	}
	type projectTarget struct {
		seatID, instanceRef, domainID, serverID, family, hostID, accountKey, lineage, storeID, ownership string
		enabled                                                                                          bool
	}
	var targets []projectTarget
	for targetRows.Next() {
		var target projectTarget
		var enabled int
		if err := targetRows.Scan(&target.seatID, &target.instanceRef, &target.domainID, &target.serverID, &enabled,
			&target.family,
			&target.hostID, &target.accountKey, &target.lineage, &target.storeID, &target.ownership); err != nil {
			targetRows.Close()
			return ProjectActivationStatus{}, err
		}
		target.enabled = enabled == 1
		targets = append(targets, target)
	}
	if err := targetRows.Close(); err != nil {
		return ProjectActivationStatus{}, err
	}
	var seatIDs []string
	for _, target := range targets {
		out.EnabledBuilderTargets++
		if !target.enabled {
			continue
		}
		out.EnabledSeats++
		seatIDs = append(seatIDs, target.seatID)
		if target.hostID != "" && target.accountKey != "" && target.lineage != "" {
			out.CapacityBoundSeats++
		}
		current, currentErr := currentDriverInstance(ctx, s.DB, target.instanceRef,
			target.hostID, target.domainID, target.serverID, now, capacityFreshFor)
		if currentErr != nil {
			return ProjectActivationStatus{}, currentErr
		}
		if current {
			out.CurrentBuilderTargets++
		}
		if target.domainID != "" && target.domainID != "default" && target.ownership == "managed_dedicated" {
			out.BuilderTopologyReadyTargets++
		}
		if target.hostID != "" && target.storeID != "" && target.domainID != "" &&
			s.DriverControlOriginEndpointGate != nil &&
			s.DriverControlOriginEndpointGate(target.hostID, target.storeID, target.domainID) {
			out.BuilderEndpointControlReadyTargets++
		}
	}
	if out.EnabledBuilderTargets == 0 {
		out.Holds = append(out.Holds, "no_enabled_builder_target")
	} else if out.EnabledSeats < out.EnabledBuilderTargets {
		out.Holds = append(out.Holds, "builder_target_seat_disabled")
	}
	if out.EnabledSeats == 0 {
		out.Holds = append(out.Holds, "no_enabled_seat")
	} else if out.CapacityBoundSeats < out.EnabledSeats {
		out.Holds = append(out.Holds, "seat_capacity_identity_incomplete")
	}
	if out.CurrentBuilderTargets < out.EnabledBuilderTargets {
		out.Holds = append(out.Holds, "builder_target_driver_unavailable")
	}
	if out.BuilderTopologyReadyTargets < out.EnabledBuilderTargets {
		out.Holds = append(out.Holds, "builder_endpoint_topology_invalid")
	}
	if out.BuilderEndpointControlReadyTargets < out.EnabledBuilderTargets {
		out.Holds = append(out.Holds, "builder_endpoint_control_unavailable")
	}
	reviewerRows, err := s.DB.QueryContext(ctx, `SELECT worker_identity FROM driver_session_bindings
		WHERE project_id=? AND role=? AND state='active' ORDER BY worker_identity`, projectID, DriverReviewerRole)
	if err != nil {
		return ProjectActivationStatus{}, err
	}
	var reviewerCandidates []string
	for reviewerRows.Next() {
		var identity string
		if err := reviewerRows.Scan(&identity); err != nil {
			reviewerRows.Close()
			return ProjectActivationStatus{}, err
		}
		reviewerCandidates = append(reviewerCandidates, identity)
	}
	if err := reviewerRows.Close(); err != nil {
		return ProjectActivationStatus{}, err
	}
	for _, identity := range reviewerCandidates {
		binding, bindErr := s.ActiveDriverSessionBinding(ctx, projectID, identity, DriverReviewerRole)
		if bindErr != nil {
			if errors.Is(bindErr, sql.ErrNoRows) {
				continue
			}
			return ProjectActivationStatus{}, bindErr
		}
		current, currentErr := currentDriverBinding(ctx, s.DB, binding, now, capacityFreshFor)
		if currentErr != nil {
			return ProjectActivationStatus{}, currentErr
		}
		if current {
			out.ReviewerIdentities = append(out.ReviewerIdentities, identity)
			var family string
			if err := s.DB.QueryRowContext(ctx, `SELECT provider FROM driver_session_projections
				WHERE store_id=? AND session_id=? AND pane_instance_id=? AND agent_run_id=?
				AND tmux_server_instance_id=?`, binding.StoreID, binding.SessionID,
				binding.PaneInstanceID, binding.AgentRunID, binding.TmuxServerInstanceID).Scan(&family); err != nil {
				return ProjectActivationStatus{}, err
			}
			endpointOwnership, err := driverInstanceOwnership(ctx, s.DB, binding.HostID, binding.StoreID)
			if err != nil {
				return ProjectActivationStatus{}, err
			}
			reviewer := ProjectReviewerActivation{WorkerIdentity: identity, SeatID: binding.SeatID,
				ModelFamily: family, TmuxServerDomainID: binding.TmuxServerDomainID,
				LifecycleOwnership: binding.LifecycleOwnership, TmuxServerOwnership: endpointOwnership,
				TopologyReady: binding.LifecycleOwnership == "driver_managed" && binding.TmuxServerDomainID != "default" &&
					endpointOwnership == "managed_dedicated",
				EndpointControlReady: s.HasDriverControlOriginForBinding(binding)}
			if family != "" && binding.SeatID != "" {
				var configured int
				if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM seats WHERE id=? AND enabled=1
					AND agent_family=? AND expected_host_id=? AND expected_account_key<>''
					AND expected_credential_lineage<>''`, binding.SeatID, family, binding.HostID).Scan(&configured); err != nil {
					return ProjectActivationStatus{}, err
				}
				reviewer.FamilyCapacityConfigured = configured > 0
			}
			if family == "" {
				out.Holds = append(out.Holds, "reviewer_family_missing")
			} else if binding.SeatID == "" {
				out.Holds = append(out.Holds, "reviewer_capacity_seat_unbound")
			} else if !reviewer.FamilyCapacityConfigured {
				out.Holds = append(out.Holds, "reviewer_capacity_seat_mismatch")
			}
			if !reviewer.TopologyReady {
				out.Holds = append(out.Holds, "reviewer_endpoint_topology_invalid")
			}
			if !reviewer.EndpointControlReady {
				out.Holds = append(out.Holds, "reviewer_endpoint_control_unavailable")
			}
			out.Reviewers = append(out.Reviewers, reviewer)
		}
	}
	out.CurrentReviewerBindings = len(out.ReviewerIdentities)
	if out.CurrentReviewerBindings == 0 {
		out.Holds = append(out.Holds, "no_current_reviewer_binding")
	}

	err = s.DB.QueryRowContext(ctx, `SELECT g.generation_id FROM capacity_active_generation a
		JOIN capacity_generations g ON g.generation_id=a.generation_id AND g.state='active'
		WHERE a.singleton=1`).Scan(&out.ActiveCapacityGeneration)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return ProjectActivationStatus{}, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		out.Holds = append(out.Holds, "no_active_capacity_generation")
	}
	for _, id := range seatIDs {
		decision, routeErr := s.CapacityRouteForSeat(ctx, id, now, capacityFreshFor)
		if routeErr != nil {
			return ProjectActivationStatus{}, fmt.Errorf("capacity route for seat %s: %w", id, routeErr)
		}
		if decision.Routable {
			out.RoutableSeats++
		}
	}
	if out.RoutableSeats == 0 {
		out.Holds = append(out.Holds, "no_routable_seat")
	}
	allReviewerFamiliesConfigured := len(out.Reviewers) > 0
	allReviewerFamiliesRoutable := len(out.Reviewers) > 0
	allReviewerTopologyReady := len(out.Reviewers) > 0
	allReviewerEndpointControlReady := len(out.Reviewers) > 0
	for i := range out.Reviewers {
		if out.Reviewers[i].FamilyCapacityConfigured {
			decision, err := s.CapacityRouteForSeat(ctx, out.Reviewers[i].SeatID, now, capacityFreshFor)
			if err != nil {
				return ProjectActivationStatus{}, fmt.Errorf("reviewer capacity route for seat %s: %w",
					out.Reviewers[i].SeatID, err)
			}
			if decision.Routable {
				out.Reviewers[i].FamilyCapacityRoutable = true
			}
		}
		allReviewerFamiliesConfigured = allReviewerFamiliesConfigured &&
			out.Reviewers[i].ModelFamily != "" && out.Reviewers[i].FamilyCapacityConfigured
		allReviewerFamiliesRoutable = allReviewerFamiliesRoutable && out.Reviewers[i].FamilyCapacityRoutable
		allReviewerTopologyReady = allReviewerTopologyReady && out.Reviewers[i].TopologyReady
		allReviewerEndpointControlReady = allReviewerEndpointControlReady && out.Reviewers[i].EndpointControlReady
	}
	if out.ActiveCapacityGeneration != "" && !allReviewerFamiliesRoutable {
		out.Holds = append(out.Holds, "reviewer_family_capacity_not_routable")
	}
	for _, target := range targets {
		if !target.enabled {
			continue
		}
		for _, reviewer := range out.Reviewers {
			if target.family != "" && reviewer.ModelFamily != "" && reviewer.ModelFamily != target.family &&
				reviewer.TopologyReady && reviewer.EndpointControlReady && reviewer.FamilyCapacityRoutable {
				out.BuilderDistinctReviewerReadyTargets++
				break
			}
		}
	}
	if out.BuilderDistinctReviewerReadyTargets < out.EnabledBuilderTargets {
		out.Holds = append(out.Holds, "builder_distinct_reviewer_unavailable")
	}

	out.Configured = project.State == "active" && len(out.RepositoryIDs) > 0 &&
		len(out.Actors) == 2 && out.Actors[0].BindingCurrent && out.Actors[0].LifecycleReady && out.Actors[0].TopologyReady &&
		out.Actors[1].BindingCurrent && out.Actors[1].LifecycleReady && out.Actors[1].TopologyReady &&
		out.EnabledSeats > 0 && out.CapacityBoundSeats == out.EnabledSeats &&
		out.EnabledBuilderTargets > 0 && out.EnabledSeats == out.EnabledBuilderTargets &&
		out.CurrentBuilderTargets == out.EnabledBuilderTargets &&
		out.BuilderTopologyReadyTargets == out.EnabledBuilderTargets && out.CurrentReviewerBindings > 0 &&
		allReviewerFamiliesConfigured && allReviewerTopologyReady
	out.LiveReady = out.Configured && out.Actors[0].EndpointControlReady && out.Actors[1].EndpointControlReady &&
		out.BuilderEndpointControlReadyTargets == out.EnabledBuilderTargets &&
		out.BuilderDistinctReviewerReadyTargets == out.EnabledBuilderTargets &&
		out.ActiveCapacityGeneration != "" && out.RoutableSeats > 0 && allReviewerFamiliesRoutable &&
		allReviewerEndpointControlReady
	return out, nil
}

func currentDriverBinding(ctx context.Context, db *sql.DB, binding DriverSessionBinding, now time.Time,
	freshFor time.Duration) (bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT i.updated_at,c.updated_at FROM driver_instances i
		JOIN driver_observation_cursors c ON c.store_id=i.store_id AND c.instance_ref=i.instance_ref AND c.active=1
		JOIN driver_session_projections p ON p.store_id=i.store_id
		WHERE i.state='live' AND i.host_id=? AND i.store_id=? AND p.session_id=?
		 AND p.host_id=?
		 AND p.pane_instance_id=? AND p.agent_run_id=? AND p.tmux_server_domain_id=?
		 AND p.tmux_server_instance_id=?
		 AND p.lifecycle<>'ended'`, binding.HostID, binding.StoreID, binding.SessionID,
		binding.HostID, binding.PaneInstanceID, binding.AgentRunID, binding.TmuxServerDomainID,
		binding.TmuxServerInstanceID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	count, fresh := 0, false
	for rows.Next() {
		var instanceUpdated, cursorUpdated string
		if err := rows.Scan(&instanceUpdated, &cursorUpdated); err != nil {
			return false, err
		}
		count++
		fresh = driverObservationFresh(instanceUpdated, now, freshFor) &&
			driverObservationFresh(cursorUpdated, now, freshFor)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return count == 1 && fresh, nil
}

func currentDriverInstance(ctx context.Context, db *sql.DB, instanceRef, hostID, domainID, serverID string, now time.Time,
	freshFor time.Duration) (bool, error) {
	if instanceRef == "" || hostID == "" || domainID == "" || serverID == "" {
		return false, nil
	}
	var state, instanceUpdated, cursorUpdated string
	var serverPresent int
	err := db.QueryRowContext(ctx, `SELECT i.state,i.updated_at,c.updated_at,
		EXISTS(SELECT 1 FROM driver_session_projections p WHERE p.store_id=i.store_id
			AND p.host_id=i.host_id AND p.tmux_server_domain_id=?
			AND p.tmux_server_instance_id=? AND p.lifecycle<>'ended')
		FROM driver_instances i
		JOIN driver_observation_cursors c ON c.store_id=i.store_id AND c.instance_ref=i.instance_ref AND c.active=1
		WHERE i.instance_ref=? AND i.host_id=?`, domainID, serverID, instanceRef, hostID).
		Scan(&state, &instanceUpdated, &cursorUpdated, &serverPresent)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return state == "live" && serverPresent == 1 && driverObservationFresh(instanceUpdated, now, freshFor) &&
		driverObservationFresh(cursorUpdated, now, freshFor), nil
}

func driverInstanceOwnership(ctx context.Context, db *sql.DB, hostID, storeID string) (string, error) {
	if hostID == "" || storeID == "" {
		return "", nil
	}
	rows, err := db.QueryContext(ctx, `SELECT tmux_server_ownership FROM driver_instances
		WHERE host_id=? AND store_id=? AND state='live'`, hostID, storeID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ownership string
	count := 0
	for rows.Next() {
		if err := rows.Scan(&ownership); err != nil {
			return "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if count != 1 {
		return "", nil
	}
	return ownership, nil
}

func driverObservationFresh(raw string, now time.Time, freshFor time.Duration) bool {
	if freshFor <= 0 || raw == "" {
		return false
	}
	for _, layout := range []string{rfc3339, time.RFC3339, "2006-01-02 15:04:05"} {
		observed, err := time.Parse(layout, raw)
		if err == nil {
			return !observed.Before(now.Add(-freshFor))
		}
	}
	return false
}
