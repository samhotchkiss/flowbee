package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type bareBootstrapActionClient interface {
	Commit(context.Context, api.BootstrapAction) (api.BootstrapActionReceipt, error)
	Status(context.Context, string) (api.BootstrapActionStatus, error)
	Activation(context.Context, string) (store.ProjectActivationStatus, error)
}

type bareServerActionPlan struct {
	BootstrapID, ProjectID, CWD, RepositoryOrigin string
	ControlPlane                                  bareControlPlaneSpec
	Actions                                       []api.BootstrapAction
	Attach                                        bootstrap.AttachIntentSpec
}

type bareControlPlaneSpec struct {
	InstanceRef, HostID, StoreID, TmuxServerDomainID, TmuxServerInstanceID string
	LifecycleKey, ProfileID, WorkspaceRootID, WorkspaceRelativePath        string
	TargetEpoch                                                            int64
}

type bareServerActionRunner struct {
	Store        bootstrap.CheckpointStore
	Client       bareBootstrapActionClient
	PollInterval time.Duration
	Attach       func(bootstrap.AttachIntentSpec) error
	FinalReady   func(context.Context) (bool, error)
}

func (r bareServerActionRunner) Run(ctx context.Context, plan bareServerActionPlan) error {
	if r.Store == nil || r.Client == nil || r.Attach == nil || plan.BootstrapID == "" || plan.ProjectID == "" ||
		plan.CWD == "" || plan.RepositoryOrigin == "" || len(plan.Actions) == 0 {
		return errors.New("bare server bootstrap plan is incomplete")
	}
	cp, err := initializeBarePlanCheckpoint(ctx, r.Store, plan)
	if err != nil {
		return err
	}
	if cp.Done {
		activation, err := r.Client.Activation(ctx, plan.ProjectID)
		if err != nil {
			return err
		}
		if !activation.LiveReady {
			return fmt.Errorf("completed bootstrap is no longer live-ready: %v", activation.Holds)
		}
		if r.FinalReady != nil {
			ready, readyErr := r.FinalReady(ctx)
			if readyErr != nil {
				return readyErr
			}
			if !ready {
				return errors.New("completed bootstrap control plane is not final LiveReady")
			}
		}
		return r.Attach(plan.Attach)
	}
	for _, action := range plan.Actions {
		key := "server_action:" + action.ActionID
		for {
			if cp.Prepared[key] == "" {
				cp, err = r.advance(ctx, cp, func(next *bootstrap.Checkpoint) { next.Prepared[key] = action.ActionID })
				if err != nil {
					return err
				}
				continue
			}
			if cp.Prepared[key] != action.ActionID {
				return errors.New("bare bootstrap prepared action identity changed")
			}
			if cp.Issued[key] == "" {
				receipt, commitErr := r.Client.Commit(ctx, action)
				if commitErr != nil {
					return commitErr
				}
				cp, err = r.advance(ctx, cp, func(next *bootstrap.Checkpoint) { next.Issued[key] = receipt.ReceiptID })
				if err != nil {
					return err
				}
				continue
			}
			status, statusErr := r.Client.Status(ctx, action.ActionID)
			if statusErr != nil {
				return statusErr
			}
			switch status.State {
			case "succeeded":
				cp, err = r.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
					next.Completed[key], next.LastHold = "mechanical_status:succeeded", ""
				})
				if err != nil {
					return err
				}
				goto nextAction
			case "held", "uncertain", "dead_letter":
				_, _ = r.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
					next.LastHold = action.ActionID + ":" + status.State + ":" + status.LastError
				})
				return fmt.Errorf("bootstrap action %s is visibly %s: %s", action.ActionID, status.State, status.LastError)
			case "pending", "claimed", "verifying":
				if err := waitBootstrapPoll(ctx, r.PollInterval); err != nil {
					return err
				}
				continue
			default:
				return errors.New("bootstrap action returned unknown state")
			}
		}
	nextAction:
	}
	for {
		activation, err := r.Client.Activation(ctx, plan.ProjectID)
		if err != nil {
			return err
		}
		if activation.LiveReady {
			break
		}
		if err := waitBootstrapPoll(ctx, r.PollInterval); err != nil {
			return fmt.Errorf("project activation remains held (%v): %w", activation.Holds, err)
		}
	}
	if r.FinalReady != nil {
		for {
			ready, err := r.FinalReady(ctx)
			if err != nil {
				return err
			}
			if ready {
				break
			}
			if err := waitBootstrapPoll(ctx, r.PollInterval); err != nil {
				return fmt.Errorf("control plane remains short of final LiveReady: %w", err)
			}
		}
	}
	cp, err = r.advance(ctx, cp, func(next *bootstrap.Checkpoint) {
		next.Done, next.LastHold = true, ""
	})
	if err != nil {
		return err
	}
	return r.Attach(plan.Attach)
}

func initializeBarePlanCheckpoint(ctx context.Context, checkpointStore bootstrap.CheckpointStore,
	plan bareServerActionPlan) (bootstrap.Checkpoint, error) {
	if checkpointStore == nil {
		return bootstrap.Checkpoint{}, errors.New("bare bootstrap checkpoint store is required")
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return bootstrap.Checkpoint{}, err
	}
	sum := sha256.Sum256(body)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	cp, ok, err := checkpointStore.Load(ctx, plan.BootstrapID)
	if err != nil {
		return bootstrap.Checkpoint{}, err
	}
	if !ok {
		cp, err = checkpointStore.Create(ctx, bootstrap.Checkpoint{BootstrapID: plan.BootstrapID,
			PlanSHA256: digest, ProjectID: plan.ProjectID, CWD: plan.CWD,
			RepositoryOrigin: plan.RepositoryOrigin, Prepared: map[string]string{}, Issued: map[string]string{},
			Completed: map[string]string{}})
		if errors.Is(err, bootstrap.ErrCheckpointConflict) {
			cp, ok, err = checkpointStore.Load(ctx, plan.BootstrapID)
		}
		if err != nil || !ok && cp.BootstrapID == "" {
			return bootstrap.Checkpoint{}, fmt.Errorf("create or reload bare bootstrap checkpoint: %w", err)
		}
	}
	if cp.PlanSHA256 != digest || cp.ProjectID != plan.ProjectID || cp.CWD != plan.CWD ||
		cp.RepositoryOrigin != plan.RepositoryOrigin {
		return bootstrap.Checkpoint{}, errors.New("bare bootstrap checkpoint belongs to a different immutable plan")
	}
	return cp, nil
}

func (r bareServerActionRunner) advance(ctx context.Context, cp bootstrap.Checkpoint,
	mutate func(*bootstrap.Checkpoint)) (bootstrap.Checkpoint, error) {
	for attempts := 0; attempts < 16; attempts++ {
		next := cp
		next.Prepared = cloneStringMap(cp.Prepared)
		next.Issued = cloneStringMap(cp.Issued)
		next.Completed = cloneStringMap(cp.Completed)
		mutate(&next)
		updated, err := r.Store.CompareAndSwap(ctx, next, cp.Version)
		if err == nil {
			return updated, nil
		}
		if !errors.Is(err, bootstrap.ErrCheckpointConflict) {
			return bootstrap.Checkpoint{}, err
		}
		var ok bool
		cp, ok, err = r.Store.Load(ctx, cp.BootstrapID)
		if err != nil || !ok {
			return bootstrap.Checkpoint{}, fmt.Errorf("reload concurrent bare bootstrap: %w", err)
		}
	}
	return bootstrap.Checkpoint{}, errors.New("bare bootstrap checkpoint contention exceeded retry bound")
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func waitBootstrapPoll(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
