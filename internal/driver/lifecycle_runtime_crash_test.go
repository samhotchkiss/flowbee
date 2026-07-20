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

type lostLifecycleResponsePort struct{ *FakePort }

func (p *lostLifecycleResponsePort) EnsureLifecycleSession(ctx context.Context, target SessionTarget, action Action) (LifecycleReceipt, error) {
	if _, err := p.FakePort.EnsureLifecycleSession(ctx, target, action); err != nil {
		return LifecycleReceipt{}, err
	}
	return LifecycleReceipt{}, errors.New("connection closed after Driver committed lifecycle effect")
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
