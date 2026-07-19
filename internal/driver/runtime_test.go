package driver

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

type receiptOverridePort struct {
	*FakePort
	Receipt Receipt
}

func (p *receiptOverridePort) ReceiptByAction(_ context.Context, _ ReceiptExpectation) (Receipt, bool, error) {
	return p.Receipt, true, nil
}

func putActionInVerification(t *testing.T, s SQLActionStore, a Action, now time.Time) Action {
	t.Helper()
	ctx := context.Background()
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := s.ClaimNextAction(ctx, "crashed-runtime", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := s.MarkActionVerifying(ctx, claimed.ActionID, "crashed-runtime", claimed.Epoch,
		"response lost after Driver accepted delivery", now); err != nil {
		t.Fatal(err)
	}
	return claimed
}

func receiptForAction(a Action, status ReceiptStatus) Receipt {
	return Receipt{DeliveryID: "delivery-" + a.ActionID, ActionID: a.ActionID,
		GrantID: a.GrantID, GrantEpoch: a.Epoch,
		Sender:            Identity{SessionID: a.SenderSessionID, AgentRunID: a.SenderAgentRunID},
		SenderPrincipalID: a.SenderPrincipalID,
		Recipient:         Identity{SessionID: a.RecipientSessionID, PaneInstanceID: a.RecipientPaneInstanceID},
		PayloadSHA256:     a.PayloadSHA256, Status: status}
}

func TestRuntimeLiveControlCapabilityRevocationAndRecovery(t *testing.T) {
	s, first := seedSQLStoreEpic(t)
	first.Epoch = 0
	first = routedAction(first)
	ctx := context.Background()
	if err := s.CommitAction(ctx, first); err != nil {
		t.Fatal(err)
	}
	var available atomic.Bool
	available.Store(true)
	s.ControlOriginGate = available.Load
	fake := NewFake()
	now := time.Date(2026, 7, 19, 8, 0, 0, 0, time.UTC)
	runtime := Runtime{Port: fake, Store: s, Owner: "live-capability-runtime",
		Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return false, nil })}
	if rep, err := runtime.Tick(ctx, now); err != nil || rep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("ready tick=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}

	second := routedAction(NewAction("action-2", `{"task":"review-again"}`, 0))
	second.ProjectID, second.EpicID, second.Kind = first.ProjectID, first.EpicID, first.Kind
	second.DedupKey, second.HeadSHA, second.BaseSHA = "review:epic-1:head-2:base", "head-2", "base"
	if err := s.CommitAction(ctx, second); err != nil {
		t.Fatal(err)
	}
	available.Store(false)
	// Read-only receipt/stage recovery remains live while the second pending
	// mutation is fenced.
	runtime.Evidence = evidenceFunc(func(_ context.Context, a Action, _ Receipt) (bool, error) {
		return a.ActionID == first.ActionID, nil
	})
	if rep, err := runtime.Tick(ctx, now.Add(time.Minute)); err != nil || rep.Verified != 1 || fake.SendCalls != 1 {
		t.Fatalf("revoked verification=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	if rep, err := runtime.Tick(ctx, now.Add(2*time.Minute)); err != nil || rep.Delivered != 0 || fake.SendCalls != 1 {
		t.Fatalf("revoked pending claim=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var state string
	if err := s.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE id=?`, second.ActionID).Scan(&state); err != nil || state != "pending" {
		t.Fatalf("revoked action state=%q err=%v", state, err)
	}

	available.Store(true)
	if rep, err := runtime.Tick(ctx, now.Add(3*time.Minute)); err != nil || rep.Delivered != 1 || fake.SendCalls != 2 {
		t.Fatalf("restored tick=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
}

func routedAction(a Action) Action {
	a.TargetRole = "reviewer"
	a.TargetHostID, a.TargetStoreID, a.TargetServerID = "host-1", "store-1", "server-1"
	a.LifecycleKey, a.TargetEpoch = "reviewer-seat-1", 1
	a.ProfileID, a.WorkspaceRootID, a.WorkspaceRelativePath = "grok_reviewer", "flowbee", "repo"
	a.LeaseID, a.LeaseEpoch = "lease-1", 1
	a.SenderSessionID, a.SenderAgentRunID = "", ""
	a.SenderPrincipalID = "flowbee-control"
	a.RecipientSessionID, a.RecipientPaneInstanceID, a.RecipientAgentRunID = "reviewer", "pane-1", "run-1"
	a.GrantID = "grant-1"
	return a
}

func TestRuntimeTransportSuccessWaitsForSeparateStageEvidence(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	a.Epoch = 0
	a = routedAction(a)
	if err := s.CommitAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	fake := NewFake()
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	runtime := Runtime{Port: fake, Store: s, Owner: "runtime-1", Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return false, nil })}
	rep, err := runtime.Tick(context.Background(), now)
	if err != nil || rep.Delivered != 1 || fake.SendCalls != 1 {
		t.Fatalf("first tick=%+v sends=%d err=%v", rep, fake.SendCalls, err)
	}
	var state string
	if err := s.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, a.ActionID).Scan(&state); err != nil || state != "verifying" {
		t.Fatalf("state=%s err=%v, receipt must not prove stage", state, err)
	}
	// Receipt lookup plus absent stage evidence must not resend.
	if _, err := runtime.Tick(context.Background(), now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if fake.SendCalls != 1 {
		t.Fatalf("verification resent payload: %d", fake.SendCalls)
	}
	runtime.Evidence = evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return true, nil })
	rep, err = runtime.Tick(context.Background(), now.Add(2*time.Minute))
	if err != nil || rep.Verified != 1 {
		t.Fatalf("stage verification=%+v err=%v", rep, err)
	}
	if err := s.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, a.ActionID).Scan(&state); err != nil || state != "acknowledged" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestRuntimeCrashAfterClaimNeverBlindlyResends(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	a.Epoch = 0
	a = routedAction(a)
	ctx := context.Background()
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	claimed, ok, err := s.ClaimNextAction(ctx, "dead-runtime", now, time.Minute)
	if err != nil || !ok || claimed.Epoch != 1 {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	fake := NewFake()
	runtime := Runtime{Port: fake, Store: s, Owner: "new-runtime"}
	rep, err := runtime.Tick(ctx, now.Add(2*time.Minute))
	if err != nil || rep.Reclaimed != 1 {
		t.Fatalf("recovery=%+v err=%v", rep, err)
	}
	if fake.SendCalls != 0 {
		t.Fatalf("expired uncertain claim resent payload: %d", fake.SendCalls)
	}
	var state string
	if err := s.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, a.ActionID).Scan(&state); err != nil || state != "verifying" {
		t.Fatalf("state=%s err=%v", state, err)
	}
}

func TestRuntimeRestartRecoversInFlightReceiptToSubmittedWithoutResend(t *testing.T) {
	for _, initial := range []ReceiptStatus{ReceiptAccepted, ReceiptDelivering} {
		t.Run(string(initial), func(t *testing.T) {
			s, a := seedSQLStoreEpic(t)
			a.Epoch = 0
			a = routedAction(a)
			now := time.Date(2026, 7, 19, 10, 30, 0, 0, time.UTC)
			claimed := putActionInVerification(t, s, a, now)
			if err := s.PersistReceipt(context.Background(), claimed, receiptForAction(claimed, initial)); err != nil {
				t.Fatal(err)
			}
			port := &receiptOverridePort{FakePort: NewFake(), Receipt: receiptForAction(claimed, ReceiptSubmitted)}
			runtime := Runtime{Port: port, Store: s, Owner: "restarted-runtime",
				Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return true, nil })}
			report, err := runtime.Tick(context.Background(), now.Add(time.Minute))
			if err != nil || report.Verified != 1 || port.SendCalls != 0 {
				t.Fatalf("report=%+v sends=%d err=%v", report, port.SendCalls, err)
			}
			var actionState, receiptState string
			if err := s.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, claimed.ActionID).Scan(&actionState); err != nil {
				t.Fatal(err)
			}
			if err := s.DB.QueryRow(`SELECT status FROM driver_receipts WHERE action_id=?`, claimed.ActionID).Scan(&receiptState); err != nil {
				t.Fatal(err)
			}
			if actionState != "acknowledged" || receiptState != string(ReceiptSubmitted) {
				t.Fatalf("action=%s receipt=%s", actionState, receiptState)
			}
		})
	}
}

func TestRuntimeRejectsRecoveredReceiptIdentityBeforePersistenceOrEvidence(t *testing.T) {
	tests := []struct {
		name          string
		sessionOrigin bool
		mutate        func(*Receipt)
	}{
		{name: "wrong control principal", mutate: func(r *Receipt) { r.SenderPrincipalID = "other-control" }},
		{name: "mixed origin", mutate: func(r *Receipt) {
			r.Sender = Identity{SessionID: "forged-session", AgentRunID: "forged-run"}
		}},
		{name: "wrong session agent run", sessionOrigin: true, mutate: func(r *Receipt) {
			r.Sender.AgentRunID = "run-2"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, a := seedSQLStoreEpic(t)
			a.Epoch = 0
			a = routedAction(a)
			if tc.sessionOrigin {
				a.SenderPrincipalID = ""
				a.SenderSessionID, a.SenderAgentRunID = "builder-session", "run-1"
			}
			now := time.Date(2026, 7, 19, 10, 45, 0, 0, time.UTC)
			claimed := putActionInVerification(t, s, a, now)
			receipt := receiptForAction(claimed, ReceiptSubmitted)
			tc.mutate(&receipt)
			evidenceCalled := false
			port := &receiptOverridePort{FakePort: NewFake(), Receipt: receipt}
			runtime := Runtime{Port: port, Store: s, Owner: "restarted-runtime",
				Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) {
					evidenceCalled = true
					return true, nil
				})}
			_, err := runtime.Tick(context.Background(), now.Add(time.Minute))
			if !errors.Is(err, ErrIdentityMismatch) || evidenceCalled || port.SendCalls != 0 {
				t.Fatalf("err=%v evidence=%v sends=%d", err, evidenceCalled, port.SendCalls)
			}
			var state string
			var receipts int
			_ = s.DB.QueryRow(`SELECT state FROM epic_actions WHERE id=?`, claimed.ActionID).Scan(&state)
			_ = s.DB.QueryRow(`SELECT COUNT(*) FROM driver_receipts WHERE action_id=?`, claimed.ActionID).Scan(&receipts)
			if state != "verifying" || receipts != 0 {
				t.Fatalf("invalid recovery advanced action=%s receipts=%d", state, receipts)
			}
		})
	}
}

func TestRuntimeDeadLetterCreatesDurableControlAlert(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	a.Epoch = 0
	// Deliberately leave the route incomplete: this is a definite configuration
	// failure, not an uncertain send.
	if err := s.CommitAction(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)
	rep, err := (Runtime{Port: NewFake(), Store: s, Owner: "runtime", MaximumTries: 1}).Tick(context.Background(), now)
	if err != nil || rep.DeadLettered != 1 {
		t.Fatalf("runtime=%+v err=%v", rep, err)
	}
	var alerts int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM control_alerts WHERE epic_id='epic-1' AND kind='action_dead_letter' AND state='pending'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("alerts=%d err=%v", alerts, err)
	}
}
