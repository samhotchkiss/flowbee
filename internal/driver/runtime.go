package driver

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Runtime struct {
	Port         DriverPort
	Resolver     *EndpointResolver
	Store        SQLActionStore
	Evidence     StageEvidence
	Owner        string
	ClaimTTL     time.Duration
	MaximumTries int
}

type RuntimeReport struct{ Reclaimed, Verified, Delivered, Retried, DeadLettered int }

func (r Runtime) Tick(ctx context.Context, now time.Time) (RuntimeReport, error) {
	var out RuntimeReport
	if (r.Resolver == nil && nilDriverPort(r.Port)) || r.Store.DB == nil || r.Owner == "" {
		return out, errors.New("driver runtime requires port, store, and owner")
	}
	if r.ClaimTTL <= 0 {
		r.ClaimTTL = time.Minute
	}
	if r.MaximumTries <= 0 {
		r.MaximumTries = 5
	}
	n, err := r.Store.ReclaimExpiredActions(ctx, now)
	if err != nil {
		return out, err
	}
	out.Reclaimed = int(n)
	if a, ok, err := r.Store.ClaimNextVerifying(ctx, r.Owner, now, r.ClaimTTL); err != nil {
		return out, err
	} else if ok {
		return r.verify(ctx, a, now, out)
	}
	a, ok, err := r.Store.ClaimNextAction(ctx, r.Owner, now, r.ClaimTTL)
	if err != nil || !ok {
		return out, err
	}
	if err := validateRuntimeRoute(a); err != nil {
		return r.retryOrDeadLetter(ctx, a, err, now, out)
	}
	if err := validateSessionOriginEndpoint(a); err != nil {
		return r.retryOrDeadLetter(ctx, a, err, now, out)
	}
	port, err := resolveRuntimeSendPort(r.Resolver, r.Port, a)
	if err != nil {
		return r.retryOrDeadLetter(ctx, a, err, now, out)
	}
	exec := Executor{Port: port, Store: r.Store, Evidence: r.Evidence}
	result, err := exec.ExecuteClaimed(ctx, a.SessionTarget(), a.RouteGrant(), a)
	if err != nil {
		if result.Uncertain || errors.Is(err, ErrUncertain) {
			if markErr := r.Store.MarkActionVerifying(ctx, a.ActionID, r.Owner, a.Epoch, err.Error(), now); markErr != nil {
				return out, markErr
			}
			return out, nil
		}
		return r.retryOrDeadLetter(ctx, a, err, now, out)
	}
	if result.StageComplete {
		if err := r.Store.AcknowledgeAction(ctx, a.ActionID, r.Owner, a.Epoch, now); err != nil {
			return out, err
		}
		out.Delivered++
		return out, nil
	}
	if err := r.Store.MarkActionVerifying(ctx, a.ActionID, r.Owner, a.Epoch,
		"transport accepted; awaiting independent stage evidence", now); err != nil {
		return out, err
	}
	out.Delivered++
	return out, nil
}

func (r Runtime) verify(ctx context.Context, a Action, now time.Time, out RuntimeReport) (RuntimeReport, error) {
	port, err := resolveRuntimePort(r.Resolver, r.Port, a)
	if err != nil {
		_ = r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch, err.Error(), now)
		return out, err
	}
	receipt, ok, err := port.ReceiptByAction(ctx, a.ExpectedReceipt())
	if err != nil {
		_ = r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch, err.Error(), now)
		return out, err
	}
	if !ok {
		if err := r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch,
			"no durable Driver receipt; manual/mechanical evidence required before retry", now); err != nil {
			return out, err
		}
		return out, nil
	}
	if err := a.ExpectedReceipt().Validate(receipt); err != nil {
		_ = r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch, err.Error(), now)
		return out, err
	}
	if err := r.Store.PersistReceipt(ctx, a, receipt); err != nil {
		return out, err
	}
	complete := false
	if r.Evidence != nil {
		complete, err = r.Evidence.AwaitStage(ctx, a, receipt)
		if err != nil {
			_ = r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch, err.Error(), now)
			return out, err
		}
	}
	if !complete {
		if err := r.Store.ReleaseVerifying(ctx, a.ActionID, r.Owner, a.Epoch,
			"receipt exists; awaiting independent stage evidence", now); err != nil {
			return out, err
		}
		return out, nil
	}
	if err := r.Store.AcknowledgeVerifying(ctx, a.ActionID, r.Owner, a.Epoch, now); err != nil {
		return out, err
	}
	out.Verified++
	return out, nil
}

func (r Runtime) retryOrDeadLetter(ctx context.Context, a Action, cause error, now time.Time, out RuntimeReport) (RuntimeReport, error) {
	var attempts int
	if err := r.Store.DB.QueryRowContext(ctx, `SELECT attempts FROM epic_actions WHERE id=?`, a.ActionID).Scan(&attempts); err != nil {
		return out, err
	}
	if attempts >= r.MaximumTries {
		if err := r.Store.DeadLetterAction(ctx, a.ActionID, r.Owner, a.Epoch, cause.Error(), now); err != nil {
			return out, err
		}
		out.DeadLettered++
		return out, nil
	}
	backoff := time.Minute << min(attempts-1, 3)
	if err := r.Store.RetryAction(ctx, a.ActionID, r.Owner, a.Epoch, cause.Error(), now.Add(backoff), now); err != nil {
		return out, err
	}
	out.Retried++
	return out, nil
}

func validateRuntimeRoute(a Action) error {
	for name, value := range map[string]string{
		"target_host_id": a.TargetHostID, "target_store_id": a.TargetStoreID,
		"target_server_domain_id": a.TargetServerDomainID,
		"target_server_id":        a.TargetServerID, "lease_id": a.LeaseID,
		"recipient_session_id":       a.RecipientSessionID,
		"recipient_pane_instance_id": a.RecipientPaneInstanceID, "grant_id": a.GrantID,
	} {
		if value == "" {
			return fmt.Errorf("driver action missing %s", name)
		}
	}
	controlOrigin := a.SenderPrincipalID != "" && a.SenderSessionID == "" && a.SenderAgentRunID == ""
	sessionOrigin := a.SenderPrincipalID == "" && a.SenderSessionID != "" && a.SenderAgentRunID != ""
	if !controlOrigin && !sessionOrigin {
		return errors.New("driver action has incomplete or mixed sender origin")
	}
	if controlOrigin {
		if err := validatePrincipalID(a.SenderPrincipalID, "sender_principal_id"); err != nil {
			return err
		}
	}
	managed := a.LifecycleKey != "" || a.TargetEpoch != 0 || a.ProfileID != "" ||
		a.WorkspaceRootID != "" || a.WorkspaceRelativePath != ""
	if managed && (a.LifecycleKey == "" || a.TargetEpoch < 1 || a.ProfileID == "") {
		return errors.New("driver action has incomplete lifecycle target")
	}
	if (a.WorkspaceRootID == "") != (a.WorkspaceRelativePath == "") {
		return errors.New("driver action has incomplete lifecycle workspace")
	}
	if a.TargetLifecycleOwnership == "external_observed" {
		if a.ExternalWatchID == "" || a.WorkspaceRootID != "" {
			return errors.New("driver action has incomplete external lifecycle target")
		}
	} else if managed && a.WorkspaceRootID == "" {
		return errors.New("driver action has incomplete managed lifecycle target")
	}
	if a.LeaseEpoch < 1 || a.Epoch < 1 {
		return errors.New("driver action has incomplete epochs")
	}
	return nil
}
