package driver

import (
	"context"
	"errors"
	"testing"
	"time"
)

type lifecycleProjectionRecorder struct{ calls int }

func (p *lifecycleProjectionRecorder) ProjectLifecycleResult(context.Context, Action, LifecycleReceipt, time.Time) error {
	p.calls++
	return nil
}

type flakyLifecycleProjection struct{ calls int }

func (p *flakyLifecycleProjection) ProjectLifecycleResult(context.Context, Action, LifecycleReceipt, time.Time) error {
	p.calls++
	if p.calls == 1 {
		return errors.New("crash seam after local cleanup")
	}
	return nil
}

type lostLifecycleResponsePort struct{ *FakePort }

type rejectingLifecycleWorkspaces struct {
	prepareCalls, finalizeCalls int
	prepareErr, finalizeErr     error
}

func (w *rejectingLifecycleWorkspaces) PrepareLifecycleWorkspace(context.Context, Action, time.Time) error {
	w.prepareCalls++
	return w.prepareErr
}
func (w *rejectingLifecycleWorkspaces) FinalizeLifecycleWorkspace(context.Context, Action,
	LifecycleReceipt, time.Time) error {
	w.finalizeCalls++
	return w.finalizeErr
}
func (w *rejectingLifecycleWorkspaces) FinalizePreEffectLifecycleWorkspace(context.Context, Action, time.Time) error {
	w.finalizeCalls++
	return w.finalizeErr
}

type positiveStopPort struct {
	*FakePort
	stopCalls int
}

func (p *positiveStopPort) StopSession(_ context.Context, _ SessionTarget, action Action) (LifecycleReceipt, error) {
	p.stopCalls++
	return LifecycleReceipt{FormatVersion: "tmux-driver.lifecycle-receipt/v3",
		LifecycleReceiptID: "stop-receipt", Operation: "stop", ActionID: action.ActionID,
		ActionEpoch: action.Epoch, LifecycleKey: action.LifecycleKey, TargetEpoch: action.TargetEpoch,
		TmuxServerDomainID: action.TargetServerDomainID,
		Status:             "target_absent", AbsenceObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
}

func (p *lostLifecycleResponsePort) EnsureLifecycleSession(ctx context.Context, target SessionTarget, action Action) (LifecycleReceipt, error) {
	if _, err := p.FakePort.EnsureLifecycleSession(ctx, target, action); err != nil {
		return LifecycleReceipt{}, err
	}
	return LifecycleReceipt{}, errors.New("connection closed after Driver committed lifecycle effect")
}

func TestLifecycleRuntimeWorkspacePrepareFailureMakesZeroDriverCalls(t *testing.T) {
	store, action := seedSQLStoreEpic(t)
	action = routedAction(action)
	action.Epoch = 0
	action.Kind = "reviewer_launch"
	action.ExecutorKind = "driver_lifecycle"
	if err := store.CommitAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	workspaces := &rejectingLifecycleWorkspaces{prepareErr: errors.New("unsafe workspace root")}
	runtime := LifecycleRuntime{Port: fake, Store: store, Projector: &lifecycleProjectionRecorder{},
		Workspaces: workspaces, Owner: "workspace-preflight", ClaimTTL: time.Minute}
	report, err := runtime.Tick(context.Background(), time.Date(2026, 7, 19, 23, 40, 0, 0, time.UTC))
	if err != nil || report.Retried != 1 || workspaces.prepareCalls != 1 || fake.EnsureCalls != 0 {
		t.Fatalf("report=%+v prepare=%d Driver Ensures=%d err=%v",
			report, workspaces.prepareCalls, fake.EnsureCalls, err)
	}
}

func TestLifecycleRuntimeCleansWorkspaceAfterStopBeforeStoreProjection(t *testing.T) {
	store, action := seedSQLStoreEpic(t)
	action = routedAction(action)
	action.Epoch = 0
	action.Kind = "worker_stop"
	action.ExecutorKind = "driver_lifecycle"
	if err := store.CommitAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	port := &positiveStopPort{FakePort: NewFake()}
	workspaces := &rejectingLifecycleWorkspaces{finalizeErr: errors.New("ambiguous workspace marker")}
	projector := &lifecycleProjectionRecorder{}
	runtime := LifecycleRuntime{Port: port, Store: store, Projector: projector,
		Workspaces: workspaces, Owner: "workspace-finalize", ClaimTTL: time.Minute}
	_, err := runtime.Tick(context.Background(), time.Date(2026, 7, 19, 23, 50, 0, 0, time.UTC))
	if err == nil || port.stopCalls != 1 || workspaces.finalizeCalls != 1 || projector.calls != 0 {
		t.Fatalf("stop=%d finalize=%d projections=%d err=%v",
			port.stopCalls, workspaces.finalizeCalls, projector.calls, err)
	}
}

func TestLifecycleRuntimeReplaysPreEffectWorkspaceCleanupWithoutDriverVerification(t *testing.T) {
	store, action := seedSQLStoreEpic(t)
	action = routedAction(action)
	action.Epoch = 0
	action.Kind = "worker_workspace_cleanup"
	action.ExecutorKind = "driver_lifecycle"
	if err := store.CommitAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	workspaces := &rejectingLifecycleWorkspaces{}
	projector := &flakyLifecycleProjection{}
	runtime := LifecycleRuntime{Port: fake, Store: store, Projector: projector,
		Workspaces: workspaces, Owner: "workspace-local-replay", ClaimTTL: time.Minute}
	now := time.Date(2026, 7, 20, 0, 5, 0, 0, time.UTC)
	if _, err := runtime.Tick(context.Background(), now); err == nil || workspaces.finalizeCalls != 1 {
		t.Fatalf("first local cleanup err=%v finalizer=%d", err, workspaces.finalizeCalls)
	}
	if rep, err := runtime.Tick(context.Background(), now.Add(time.Minute)); err != nil || rep.Executed != 1 ||
		workspaces.finalizeCalls != 2 || fake.EnsureCalls != 0 || fake.StopCalls != 0 {
		t.Fatalf("replay=%+v err=%v finalizer=%d ensure=%d stop=%d", rep, err,
			workspaces.finalizeCalls, fake.EnsureCalls, fake.StopCalls)
	}
}

func TestLifecycleRuntimeLostResponseRecoversByActionWithoutBlindResend(t *testing.T) {
	store, action := seedSQLStoreEpic(t)
	action = routedAction(action)
	action.Epoch = 0
	action.Kind = "conflict_resolution"
	action.ExecutorKind = "driver_lifecycle"
	if err := store.CommitAction(context.Background(), action); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	port := &lostLifecycleResponsePort{FakePort: fake}
	projector := &lifecycleProjectionRecorder{}
	runtime := LifecycleRuntime{Port: port, Store: store, Projector: projector,
		Owner: "lifecycle-crash", ClaimTTL: time.Minute}
	now := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)

	report, err := runtime.Tick(context.Background(), now)
	if err != nil || report.Retried != 0 || report.Executed != 0 || fake.EnsureCalls != 1 || projector.calls != 0 {
		t.Fatalf("lost response report=%+v ensure=%d projections=%d err=%v",
			report, fake.EnsureCalls, projector.calls, err)
	}
	var state string
	if err := store.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, action.ActionID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "verifying" {
		t.Fatalf("lost-response action state=%q, want verifying", state)
	}

	report, err = runtime.Tick(context.Background(), now.Add(time.Minute))
	if err != nil || report.Verified != 1 || fake.EnsureCalls != 1 || projector.calls != 1 {
		t.Fatalf("recovery report=%+v ensure=%d projections=%d err=%v",
			report, fake.EnsureCalls, projector.calls, err)
	}
}
