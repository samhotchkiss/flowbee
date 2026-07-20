package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const bootstrapActionReceiptFormat = "flowbee.bootstrap-action-receipt/v1"

type serverBootstrapIntake struct {
	Store *store.Store
	Now   func() time.Time
}

func (i *serverBootstrapIntake) CommitBootstrapAction(ctx context.Context, action api.BootstrapAction,
	_ string) (api.BootstrapActionReceipt, error) {
	if i == nil || i.Store == nil {
		return api.BootstrapActionReceipt{}, errors.New("bootstrap action store is unavailable")
	}
	now := time.Now()
	if i.Now != nil {
		now = i.Now()
	}
	record, err := i.Store.CommitBootstrapAction(ctx, store.BootstrapActionInput{
		ID: action.ActionID, BootstrapID: action.BootstrapID, ProjectID: action.ProjectID,
		Kind: action.Kind, PayloadJSON: string(action.Payload), PayloadSHA256: action.PayloadSHA256,
	}, now)
	if errors.Is(err, store.ErrBootstrapActionConflict) {
		return api.BootstrapActionReceipt{}, api.ErrBootstrapActionConflict
	}
	if err != nil {
		return api.BootstrapActionReceipt{}, err
	}
	return api.BootstrapActionReceipt{FormatVersion: bootstrapActionReceiptFormat,
		ActionID: record.ID, ReceiptID: bootstrapReceiptID(record.ID), State: record.State}, nil
}

type bootstrapActionRuntime struct {
	Store *store.Store
	Owner string
}

type bootstrapSeatBindPayload struct {
	ProjectID string                     `json:"project_id"`
	Seat      store.Seat                 `json:"seat"`
	Capacity  store.CapacitySeatIdentity `json:"capacity"`
	Target    store.BuilderDriverTarget  `json:"target"`
}

func (r bootstrapActionRuntime) Tick(ctx context.Context, now time.Time) error {
	if r.Store == nil || strings.TrimSpace(r.Owner) == "" {
		return errors.New("bootstrap action runtime is incomplete")
	}
	if _, err := r.Store.RecoverExpiredBootstrapClaims(ctx, now); err != nil {
		return err
	}
	verifying, err := r.Store.ListBootstrapActionsForVerification(ctx, 20)
	if err != nil {
		return err
	}
	for _, action := range verifying {
		ready, evidence, err := r.fact(ctx, action)
		if err != nil {
			return err
		}
		if ready {
			if _, err := r.Store.CompleteBootstrapAction(ctx, action.ID, action.ActionEpoch, evidence, now); err != nil &&
				!errors.Is(err, store.ErrBootstrapActionStale) {
				return err
			}
		}
	}
	action, err := r.Store.ClaimNextBootstrapAction(ctx, r.Owner, now, time.Minute)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.execute(ctx, action, now)
}

func (r bootstrapActionRuntime) execute(ctx context.Context, action store.BootstrapActionRecord, now time.Time) error {
	var err error
	switch action.Kind {
	case "project_upsert":
		var project store.PortfolioProject
		if decodeStrictBootstrapPayload(action.PayloadJSON, &project) != nil || project.ID != action.ProjectID {
			err = errors.New("project_upsert payload does not own the action project")
		} else {
			_, err = r.Store.CreatePortfolioProjectCommand(ctx, project, action.ID, now)
		}
	case "repository_attach":
		var payload struct {
			ProjectID string `json:"project_id"`
			RepoID    string `json:"repo_id"`
		}
		if decodeStrictBootstrapPayload(action.PayloadJSON, &payload) != nil || payload.ProjectID != action.ProjectID || payload.RepoID == "" {
			err = errors.New("repository_attach payload is invalid or crosses projects")
		} else {
			err = r.Store.AddProjectRepoCommand(ctx, action.ProjectID, payload.RepoID, action.ID, now)
		}
	case "actor_route":
		var route store.ProjectActorRoute
		if decodeStrictBootstrapPayload(action.PayloadJSON, &route) != nil || route.ProjectID != action.ProjectID ||
			(route.Role != store.DriverInteractorRole && route.Role != store.DriverOrchestratorRole) || route.ActorID == "" {
			err = errors.New("actor_route payload is invalid or crosses projects")
		} else {
			_, err = r.Store.RegisterProjectActorCommand(ctx, route, action.ID, now)
		}
	case "actor_lifecycle":
		var command store.ProjectActorLifecycleCommand
		if decodeStrictBootstrapPayload(action.PayloadJSON, &command) != nil || command.ProjectID != action.ProjectID ||
			(command.IdempotencyKey != "" && command.IdempotencyKey != action.ID) {
			err = errors.New("actor_lifecycle payload is invalid or crosses projects")
		} else {
			route, routeErr := r.Store.GetProjectActor(ctx, command.ProjectID, command.Role)
			if routeErr != nil || route.ActorID != command.ActorID || route.State != "active" {
				err = errors.New("actor_lifecycle has no exact active actor route")
				break
			}
			// The actor_route action is separately durable and may replay over a
			// valid pre-existing route whose version is greater than one. Resolve
			// that exact post-route fact here; never guess version 1 in the client.
			command.ExpectedRouteStateVersion = int64(route.StateVersion)
			command.IdempotencyKey = action.ID
			_, _, err = r.Store.CommitProjectActorLifecycleIntent(ctx, command, now)
		}
	case "seat_bind":
		var payload bootstrapSeatBindPayload
		if decodeStrictBootstrapPayload(action.PayloadJSON, &payload) != nil || payload.ProjectID != action.ProjectID ||
			payload.Target.ProjectID != action.ProjectID {
			err = errors.New("seat_bind payload is invalid or crosses projects")
		} else {
			err = r.Store.BindBootstrapSeat(ctx, store.BootstrapSeatBinding{ProjectID: payload.ProjectID,
				Seat: payload.Seat, Capacity: payload.Capacity, Target: payload.Target}, now)
		}
	case "managed_topology":
		// Driver currently advertises no utility-console lifecycle operation.
		// Preserve the exact desired action as a visible hold instead of creating
		// a raw tmux session/rename or pretending the topology is complete.
		err = errors.New("Driver utility topology capability is not advertised")
	default:
		err = errors.New("bootstrap action kind is unsupported")
	}
	if err != nil {
		_, holdErr := r.Store.HoldBootstrapAction(ctx, action.ID, r.Owner, action.ClaimEpoch,
			err.Error(), now.Add(5*time.Minute), false, now)
		if holdErr != nil {
			return holdErr
		}
		return nil
	}
	_, err = r.Store.RecordBootstrapActionReceipt(ctx, action.ID, r.Owner, action.ClaimEpoch,
		bootstrapReceiptID(action.ID), "intent_committed", false, now)
	return err
}

func (r bootstrapActionRuntime) fact(ctx context.Context, action store.BootstrapActionRecord) (bool, string, error) {
	switch action.Kind {
	case "project_upsert":
		project, err := r.Store.GetPortfolioProject(ctx, action.ProjectID)
		if errors.Is(err, store.ErrProjectNotFound) {
			return false, "", nil
		}
		return err == nil, "project:" + project.ID, err
	case "repository_attach":
		var payload struct {
			ProjectID string `json:"project_id"`
			RepoID    string `json:"repo_id"`
		}
		if err := decodeStrictBootstrapPayload(action.PayloadJSON, &payload); err != nil {
			return false, "", err
		}
		repos, err := r.Store.ProjectRepoIDs(ctx, action.ProjectID, false)
		if err != nil {
			return false, "", err
		}
		for _, repo := range repos {
			if repo == payload.RepoID {
				return true, "repository:" + repo, nil
			}
		}
		return false, "", nil
	case "actor_lifecycle":
		var command store.ProjectActorLifecycleCommand
		if err := decodeStrictBootstrapPayload(action.PayloadJSON, &command); err != nil {
			return false, "", err
		}
		lifecycle, err := r.Store.GetProjectActorLifecycle(ctx, action.ProjectID, command.Role, command.ActorID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		if err != nil {
			return false, "", err
		}
		want := "active"
		if command.Operation == "stop" {
			want = "stopped"
		} else if command.Operation == "release" {
			want = "released"
		}
		return lifecycle.State == want, "actor:" + lifecycle.ActorID + ":" + lifecycle.State, nil
	case "actor_route":
		var route store.ProjectActorRoute
		if err := decodeStrictBootstrapPayload(action.PayloadJSON, &route); err != nil {
			return false, "", err
		}
		current, err := r.Store.GetProjectActor(ctx, action.ProjectID, route.Role)
		if err != nil {
			return false, "", err
		}
		ready := current.ProjectID == action.ProjectID && current.Role == route.Role &&
			current.ActorID == route.ActorID && current.State == "active"
		return ready, "actor_route:" + route.Role + ":" + route.ActorID, nil
	case "seat_bind":
		var payload bootstrapSeatBindPayload
		if err := decodeStrictBootstrapPayload(action.PayloadJSON, &payload); err != nil {
			return false, "", err
		}
		var host, account, lineage, instanceRef, domain, serverID, profile, root, base string
		var enabled, accountMaximum int
		var reserve float64
		err := r.Store.DB.QueryRowContext(ctx, `SELECT s.expected_host_id,s.expected_account_key,
			s.expected_credential_lineage,s.capacity_reserve_pct,s.account_max_concurrent,
			t.instance_ref,t.tmux_server_domain_id,t.tmux_server_instance_id,
			t.profile_id,t.workspace_root_id,t.workspace_relative_base,t.enabled FROM seats s
			JOIN builder_driver_targets t ON t.seat_id=s.id AND t.project_id=? WHERE s.id=?`,
			payload.ProjectID, payload.Capacity.SeatID).Scan(&host, &account, &lineage, &reserve, &accountMaximum, &instanceRef,
			&domain, &serverID, &profile, &root, &base, &enabled)
		if errors.Is(err, sql.ErrNoRows) {
			return false, "", nil
		}
		ready := err == nil && host == payload.Capacity.HostID && account == payload.Capacity.AccountKey &&
			lineage == payload.Capacity.CredentialLineage && reserve == payload.Capacity.ReservePct &&
			accountMaximum == payload.Capacity.AccountMaximum && instanceRef == payload.Target.InstanceRef &&
			domain == payload.Target.TmuxServerDomainID && serverID == payload.Target.TmuxServerInstanceID &&
			profile == payload.Target.ProfileID && root == payload.Target.WorkspaceRootID &&
			base == payload.Target.WorkspaceRelativeBase && enabled == 1
		return ready, "seat:" + payload.Capacity.SeatID, err
	case "managed_topology":
		return false, "", nil
	default:
		return false, "", errors.New("unknown bootstrap action kind")
	}
}

func decodeStrictBootstrapPayload(raw string, value any) error {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("bootstrap action payload has trailing or malformed JSON")
	}
	return nil
}

func bootstrapReceiptID(actionID string) string {
	sum := sha256.Sum256([]byte("flowbee-bootstrap-receipt/v1\x00" + actionID))
	return "bootstrap-receipt-" + hex.EncodeToString(sum[:16])
}

func (r bootstrapActionRuntime) String() string { return fmt.Sprintf("bootstrap-runtime[%s]", r.Owner) }
