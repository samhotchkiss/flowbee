package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

type Orchestrator struct {
	Store CheckpointStore
	Ports Ports
}

func (o Orchestrator) Run(ctx context.Context, plan Plan) (Result, error) {
	if o.Store == nil {
		return Result{}, errors.New("bootstrap checkpoint store is required")
	}
	plan = sortedPlan(plan)
	if err := validatePlan(plan, o.Ports); err != nil {
		return Result{}, err
	}
	digest, err := planDigest(plan)
	if err != nil {
		return Result{}, err
	}
	cp, ok, err := o.Store.Load(ctx, plan.ID)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		cp, err = o.Store.Create(ctx, Checkpoint{BootstrapID: plan.ID, PlanSHA256: digest,
			ProjectID: plan.ProjectID, Prepared: map[string]string{}, Issued: map[string]string{}, Completed: map[string]string{}})
		if err != nil {
			if !errors.Is(err, ErrCheckpointConflict) {
				return Result{}, err
			}
			cp, ok, err = o.Store.Load(ctx, plan.ID)
			if err != nil || !ok {
				return Result{}, fmt.Errorf("reload raced bootstrap checkpoint: %w", err)
			}
		}
	}
	if cp.PlanSHA256 != digest || cp.ProjectID != plan.ProjectID {
		return Result{}, errors.New("bootstrap id is already bound to a different immutable plan")
	}
	serviceReconcileEligible := make(map[string]bool, len(plan.Endpoints))
	for _, endpoint := range plan.Endpoints {
		key := "driver_endpoint:" + endpoint.key()
		serviceReconcileEligible[key] = cp.Issued[key] != ""
	}
	for attempts := 0; attempts < 256; attempts++ {
		result, progressed, err := o.step(ctx, plan, cp, serviceReconcileEligible)
		if errors.Is(err, ErrCheckpointConflict) {
			// Another bare invocation advanced the same immutable plan. Treat the
			// version conflict as useful progress: reload the durable checkpoint
			// and resume from its facts instead of failing or preparing a second
			// action. The plan digest check above fences unrelated callers.
			cp, ok, err = o.Store.Load(ctx, plan.ID)
			if err != nil || !ok {
				return Result{}, fmt.Errorf("reload concurrently advanced bootstrap checkpoint: %w", err)
			}
			if cp.PlanSHA256 != digest || cp.ProjectID != plan.ProjectID {
				return Result{}, errors.New("concurrently advanced bootstrap checkpoint changed immutable identity")
			}
			for _, endpoint := range plan.Endpoints {
				key := "driver_endpoint:" + endpoint.key()
				serviceReconcileEligible[key] = serviceReconcileEligible[key] || cp.Issued[key] != ""
			}
			continue
		}
		if err != nil || result.Complete || result.Hold != "" || !progressed {
			return result, err
		}
		cp, ok, err = o.Store.Load(ctx, plan.ID)
		if err != nil || !ok {
			return Result{}, fmt.Errorf("reload bootstrap checkpoint: %w", err)
		}
	}
	return Result{}, errors.New("bootstrap exceeded deterministic step bound")
}

func (o Orchestrator) step(ctx context.Context, plan Plan, cp Checkpoint,
	serviceReconcileEligible map[string]bool) (Result, bool, error) {
	result := func(phase, hold string, complete bool) Result {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Hold: hold,
			Complete: complete, Version: cp.Version}
	}
	if cp.Done {
		return o.revalidateDone(ctx, plan, cp)
	}
	if cp.Completed["driver_endpoints:both_ready"] == "" {
		// Probe BOTH exact domains before any launchd/td Ensure mutation. A single
		// default endpoint can never stand in for the managed flowbee domain.
		missing := make([]EndpointRef, 0, len(plan.Endpoints))
		for _, endpoint := range plan.Endpoints {
			ready, err := o.Ports.Driver.EndpointReady(ctx, endpoint)
			if err != nil {
				return result("driver_endpoints", "", false), false, err
			}
			if !ready {
				missing = append(missing, endpoint)
			}
		}
		if len(missing) == 0 {
			return o.complete(ctx, cp, "driver_endpoints:both_ready", "observed", "driver_endpoints")
		}
		// First prepare/submit every missing endpoint once. Once an endpoint has a
		// durable accepted or uncertain receipt, a later bare invocation must call
		// the SAME action again so Driver's service manager can reconcile a crash
		// after accept. This branch returns a hold after one pass, preventing the
		// Run loop from hammering the manager while preserving recovery on restart.
		for _, endpoint := range missing {
			key := "driver_endpoint:" + endpoint.key()
			if cp.Prepared[key] == "" || cp.Issued[key] == "" {
				return o.issue(ctx, cp, key, "driver_endpoints", func(req EffectRequest) (EffectReceipt, error) {
					return o.Ports.Driver.EnsureEndpoint(ctx, endpoint, req)
				})
			}
		}
		for _, endpoint := range missing {
			if !serviceReconcileEligible["driver_endpoint:"+endpoint.key()] {
				return o.hold(ctx, cp, "driver_endpoints", "driver_endpoints:awaiting_both_live_facts")
			}
		}
		allReady := true
		for _, endpoint := range missing {
			key := "driver_endpoint:" + endpoint.key()
			receipt, err := o.Ports.Driver.EnsureEndpoint(ctx, endpoint, EffectRequest{
				ActionID: cp.Prepared[key], ProjectID: cp.ProjectID, CWD: cp.CWD,
			})
			if err != nil {
				return result("driver_endpoints", "", false), false, err
			}
			if receipt.ID == "" || receipt.ID != cp.Issued[key] {
				return result("driver_endpoints", "", false), false,
					errors.New("Driver service reconciliation returned a different durable receipt")
			}
			ready, err := o.Ports.Driver.EndpointReady(ctx, endpoint)
			if err != nil {
				return result("driver_endpoints", "", false), false, err
			}
			allReady = allReady && ready
		}
		if allReady {
			return o.complete(ctx, cp, "driver_endpoints:both_ready", "observed", "driver_endpoints")
		}
		return o.hold(ctx, cp, "driver_endpoints", "driver_endpoints:awaiting_both_live_facts")
	}
	if key := "control_plane:" + plan.ControlPlane.ID; cp.Completed[key] == "" {
		return o.ensureFact(ctx, cp, key, "control_plane", func() (bool, error) {
			return o.Ports.Control.LiveReady(ctx, plan.ControlPlane)
		}, func(req EffectRequest) (EffectReceipt, error) {
			return o.Ports.Control.Ensure(ctx, plan.ControlPlane, req)
		})
	}
	if cp.CWD == "" {
		init, err := o.Ports.Init.ResolveProjectInit(ctx, plan.ProjectID)
		if err != nil {
			return result("resolve_project", "", false), false, err
		}
		if init.ProjectID != plan.ProjectID || init.CWD == "" || init.RepositoryOrigin == "" {
			return o.hold(ctx, cp, "resolve_project", "project_marker_unresolved")
		}
		next := cloneCheckpoint(cp)
		next.CWD, next.RepositoryOrigin, next.LastHold = init.CWD, init.RepositoryOrigin, ""
		updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
		if err != nil {
			return result("resolve_project", "", false), false, err
		}
		return Result{BootstrapID: cp.BootstrapID, Phase: "resolve_project", Version: updated.Version}, true, nil
	}
	init := ProjectInit{ProjectID: cp.ProjectID, RepositoryOrigin: cp.RepositoryOrigin, CWD: cp.CWD}
	if key := "project:create"; cp.Completed[key] == "" {
		return o.ensureFact(ctx, cp, key, "project_provision", func() (bool, error) {
			return o.Ports.Projects.ProjectExists(ctx, init)
		}, func(req EffectRequest) (EffectReceipt, error) {
			return o.Ports.Projects.AddProject(ctx, init, req)
		})
	}
	if key := "project_repo:attach"; cp.Completed[key] == "" {
		return o.ensureFact(ctx, cp, key, "project_provision", func() (bool, error) {
			return o.Ports.Projects.RepositoryAttached(ctx, init)
		}, func(req EffectRequest) (EffectReceipt, error) {
			return o.Ports.Projects.AttachRepository(ctx, init, req)
		})
	}
	if cp.Completed["project_activation:authorized"] == "" {
		activation, err := o.Ports.Activation.ExactProjectActivation(ctx, plan.ProjectID)
		if err != nil {
			return result("project_activation", "", false), false, err
		}
		if activation.ProjectID != plan.ProjectID || !activation.Exists {
			return o.hold(ctx, cp, "project_activation", "exact_project_mismatch")
		}
		if !activation.BootstrapAllowed {
			return o.hold(ctx, cp, "project_activation", "project_activation_held:"+joinHolds(activation.Holds))
		}
		return o.complete(ctx, cp, "project_activation:authorized", "observed", "project_activation")
	}
	for _, actor := range plan.Actors {
		key := "actor:" + actor.ID
		if cp.Completed[key] == "" {
			return o.ensureFact(ctx, cp, key, "actors", func() (bool, error) {
				return o.Ports.Actors.ActorReady(ctx, actor)
			}, func(req EffectRequest) (EffectReceipt, error) {
				return o.Ports.Actors.EnsureOrAdoptActor(ctx, actor, req)
			})
		}
	}
	for _, group := range plan.Groups {
		key := "group:" + group.ID
		if cp.Completed[key] == "" {
			return o.ensureFact(ctx, cp, key, "actors", func() (bool, error) {
				return o.Ports.Groups.GroupReady(ctx, group)
			}, func(req EffectRequest) (EffectReceipt, error) {
				return o.Ports.Groups.EnsureGroup(ctx, group, req)
			})
		}
	}
	for _, seat := range plan.LocalSeats {
		key := "seat:" + seat.ID
		if cp.Completed[key] == "" {
			return o.ensureFact(ctx, cp, key, "seats", func() (bool, error) {
				return o.Ports.Seats.SeatBound(ctx, seat)
			}, func(req EffectRequest) (EffectReceipt, error) {
				return o.Ports.Seats.BindLocalSeat(ctx, seat, req)
			})
		}
	}
	if cp.Completed["project_activation:live"] == "" {
		activation, err := o.Ports.Activation.ExactProjectActivation(ctx, plan.ProjectID)
		if err != nil {
			return result("project_live_ready", "", false), false, err
		}
		if activation.ProjectID != plan.ProjectID || !activation.Exists || !activation.LiveReady {
			hold := "project_not_live_ready:" + joinHolds(activation.Holds)
			if activation.ProjectID != plan.ProjectID || !activation.Exists {
				hold = "exact_project_mismatch"
			}
			return o.hold(ctx, cp, "project_live_ready", hold)
		}
		return o.complete(ctx, cp, "project_activation:live", "observed", "project_live_ready")
	}
	if key := "attach:" + plan.AttachIntent.ID; cp.Completed[key] == "" {
		return o.ensureFact(ctx, cp, key, "attach_intent", func() (bool, error) {
			return o.Ports.Interactor.IntentAttached(ctx, plan.AttachIntent)
		}, func(req EffectRequest) (EffectReceipt, error) {
			return o.Ports.Interactor.AttachIntent(ctx, plan.AttachIntent, req)
		})
	}
	next := cloneCheckpoint(cp)
	next.Done, next.LastHold = true, ""
	updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
	if err != nil {
		return result("complete", "", false), false, err
	}
	return Result{BootstrapID: cp.BootstrapID, Phase: "complete", Complete: true,
		Version: updated.Version}, true, nil
}

func (o Orchestrator) ensureFact(ctx context.Context, cp Checkpoint, key, phase string,
	ready func() (bool, error), issue func(EffectRequest) (EffectReceipt, error)) (Result, bool, error) {
	isReady, err := ready()
	if err != nil {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
	}
	if isReady {
		return o.complete(ctx, cp, key, "observed", phase)
	}
	if cp.Issued[key] != "" {
		return o.hold(ctx, cp, phase, key+":awaiting_live_fact")
	}
	return o.issue(ctx, cp, key, phase, issue)
}

func (o Orchestrator) issue(ctx context.Context, cp Checkpoint, key, phase string,
	issue func(EffectRequest) (EffectReceipt, error)) (Result, bool, error) {
	id := actionID(cp.BootstrapID, key)
	if cp.Prepared[key] == "" {
		next := cloneCheckpoint(cp)
		next.Prepared[key], next.LastHold = id, ""
		updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
		if err != nil {
			return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
		}
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: updated.Version}, true, nil
	}
	if cp.Prepared[key] != id {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false,
			errors.New("bootstrap prepared action identity mismatch")
	}
	receipt, err := issue(EffectRequest{ActionID: id,
		ProjectID: cp.ProjectID, CWD: cp.CWD})
	if err != nil {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
	}
	if receipt.ID == "" {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false,
			errors.New("bootstrap effect returned an empty durable receipt")
	}
	next := cloneCheckpoint(cp)
	next.Issued[key], next.LastHold = receipt.ID, ""
	updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
	if err != nil {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
	}
	return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: updated.Version}, true, nil
}

func (o Orchestrator) complete(ctx context.Context, cp Checkpoint, key, evidence,
	phase string) (Result, bool, error) {
	next := cloneCheckpoint(cp)
	next.Completed[key], next.LastHold = evidence, ""
	updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
	if err != nil {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
	}
	return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: updated.Version}, true, nil
}

func (o Orchestrator) hold(ctx context.Context, cp Checkpoint, phase,
	hold string) (Result, bool, error) {
	if cp.LastHold == hold {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Hold: hold,
			Version: cp.Version}, false, nil
	}
	next := cloneCheckpoint(cp)
	next.LastHold = hold
	updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
	if err != nil {
		return Result{BootstrapID: cp.BootstrapID, Phase: phase, Version: cp.Version}, false, err
	}
	return Result{BootstrapID: cp.BootstrapID, Phase: phase, Hold: hold,
		Version: updated.Version}, true, nil
}

func (o Orchestrator) revalidateDone(ctx context.Context, plan Plan, cp Checkpoint) (Result, bool, error) {
	hold := func(phase, reason string) (Result, bool, error) { return o.hold(ctx, cp, phase, reason) }
	for _, endpoint := range plan.Endpoints {
		ready, err := o.Ports.Driver.EndpointReady(ctx, endpoint)
		if err != nil {
			return Result{BootstrapID: cp.BootstrapID, Phase: "revalidate", Version: cp.Version}, false, err
		}
		if !ready {
			return hold("revalidate", "completed_bootstrap_endpoint_not_ready:"+endpoint.key())
		}
	}
	ready, err := o.Ports.Control.LiveReady(ctx, plan.ControlPlane)
	if err != nil {
		return Result{}, false, err
	}
	if !ready {
		return hold("revalidate", "completed_bootstrap_control_not_ready")
	}
	init, err := o.Ports.Init.ResolveProjectInit(ctx, plan.ProjectID)
	if err != nil {
		return Result{}, false, err
	}
	if init.ProjectID != cp.ProjectID || init.CWD != cp.CWD || init.RepositoryOrigin != cp.RepositoryOrigin {
		return hold("revalidate", "completed_bootstrap_project_marker_mismatch")
	}
	if ready, err = o.Ports.Projects.ProjectExists(ctx, init); err != nil {
		return Result{}, false, err
	} else if !ready {
		return hold("revalidate", "completed_bootstrap_project_missing")
	}
	if ready, err = o.Ports.Projects.RepositoryAttached(ctx, init); err != nil {
		return Result{}, false, err
	} else if !ready {
		return hold("revalidate", "completed_bootstrap_repository_detached")
	}
	activation, err := o.Ports.Activation.ExactProjectActivation(ctx, plan.ProjectID)
	if err != nil {
		return Result{}, false, err
	}
	if activation.ProjectID != plan.ProjectID || !activation.Exists || !activation.BootstrapAllowed || !activation.LiveReady {
		return hold("revalidate", "completed_bootstrap_project_not_live")
	}
	for _, actor := range plan.Actors {
		if ready, err = o.Ports.Actors.ActorReady(ctx, actor); err != nil {
			return Result{}, false, err
		} else if !ready {
			return hold("revalidate", "completed_bootstrap_actor_not_ready:"+actor.ID)
		}
	}
	for _, group := range plan.Groups {
		if ready, err = o.Ports.Groups.GroupReady(ctx, group); err != nil {
			return Result{}, false, err
		} else if !ready {
			return hold("revalidate", "completed_bootstrap_topology_not_ready:"+group.ID)
		}
	}
	for _, seat := range plan.LocalSeats {
		if ready, err = o.Ports.Seats.SeatBound(ctx, seat); err != nil {
			return Result{}, false, err
		} else if !ready {
			return hold("revalidate", "completed_bootstrap_seat_not_ready:"+seat.ID)
		}
	}
	if ready, err = o.Ports.Interactor.IntentAttached(ctx, plan.AttachIntent); err != nil {
		return Result{}, false, err
	} else if !ready {
		return hold("revalidate", "completed_bootstrap_human_not_attached")
	}
	if cp.LastHold != "" {
		next := cloneCheckpoint(cp)
		next.LastHold = ""
		updated, err := o.Store.CompareAndSwap(ctx, next, cp.Version)
		if err != nil {
			return Result{}, false, err
		}
		return Result{BootstrapID: cp.BootstrapID, Phase: "complete", Complete: true, Version: updated.Version}, true, nil
	}
	return Result{BootstrapID: cp.BootstrapID, Phase: "complete", Complete: true, Version: cp.Version}, false, nil
}

func planDigest(plan Plan) (string, error) {
	body, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

func actionID(bootstrapID, key string) string {
	h := sha256.Sum256([]byte(bootstrapID + "\x00" + key))
	return "bootstrap-" + hex.EncodeToString(h[:16])
}

func cloneCheckpoint(in Checkpoint) Checkpoint {
	out := in
	out.Prepared = make(map[string]string, len(in.Prepared))
	out.Issued = make(map[string]string, len(in.Issued))
	out.Completed = make(map[string]string, len(in.Completed))
	for k, v := range in.Prepared {
		out.Prepared[k] = v
	}
	for k, v := range in.Issued {
		out.Issued[k] = v
	}
	for k, v := range in.Completed {
		out.Completed[k] = v
	}
	return out
}

func joinHolds(holds []string) string {
	if len(holds) == 0 {
		return "unspecified"
	}
	body, _ := json.Marshal(holds)
	return string(body)
}
