package driver

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Runtime struct {
	Port         DriverPort
	Store        SQLActionStore
	Evidence     StageEvidence
	Owner        string
	ClaimTTL     time.Duration
	MaximumTries int
}

type RuntimeReport struct{ Reclaimed, Verified, Delivered, Retried, DeadLettered int }

func (r Runtime) Tick(ctx context.Context, now time.Time) (RuntimeReport, error) {
	var out RuntimeReport
	if r.Port == nil || r.Store.DB == nil || r.Owner == "" {
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
	exec := Executor{Port: r.Port, Store: r.Store, Evidence: r.Evidence}
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
	receipt, ok, err := r.Port.ReceiptByAction(ctx, a.ExpectedReceipt())
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
		"target_server_id": a.TargetServerID, "lifecycle_key": a.LifecycleKey,
		"profile_id": a.ProfileID, "workspace_root_id": a.WorkspaceRootID,
		"workspace_relative_path": a.WorkspaceRelativePath, "lease_id": a.LeaseID,
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
	if a.TargetEpoch < 1 || a.LeaseEpoch < 1 || a.Epoch < 1 {
		return errors.New("driver action has incomplete epochs")
	}
	return nil
}
