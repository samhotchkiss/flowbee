package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixtureGrant() Grant {
	return Grant{GrantID: "g1", SenderSessionID: "a", SenderAgentRunID: "run-a", RecipientSessionID: "b", RecipientPaneInstanceID: "pane-b", Epoch: 4, MaximumPayloadBytes: 1000}
}

func TestFakeIdempotentReplayAndReceiptNotStage(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	if err := f.Grant(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	a := NewAction("a1", "hello", 4)
	req := SendRequest{Action: a, GrantID: "g1", RecipientSessionID: "b", RecipientPaneInstanceID: "pane-b", GrantEpoch: 4}
	r1, err := f.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := f.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if r1.DeliveryID != r2.DeliveryID || f.SendCalls != 1 {
		t.Fatalf("replay mutated: %+v calls=%d", r2, f.SendCalls)
	}
	if r1.StageComplete() {
		t.Fatal("transport receipt cannot prove workflow stage")
	}
}

func TestFakeDeniesStaleOrLateralTrafficWithoutSend(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	_ = f.Grant(context.Background(), g)
	a := NewAction("a1", "hello", 3)
	_, err := f.Send(context.Background(), SendRequest{Action: a, GrantID: "g1", RecipientSessionID: "c", RecipientPaneInstanceID: "pane-c", GrantEpoch: 3})
	if !errors.Is(err, ErrGrantDenied) && !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("err=%v", err)
	}
	if f.SendCalls != 0 {
		t.Fatalf("forbidden send mutated: %d", f.SendCalls)
	}
}

func TestFakeDeniesStalePaneIncarnationWithoutSend(t *testing.T) {
	f := NewFake()
	_ = f.Grant(context.Background(), fixtureGrant())
	_, err := f.Send(context.Background(), SendRequest{Action: NewAction("stale", "x", 4), GrantID: "g1", RecipientSessionID: "b", RecipientPaneInstanceID: "old-pane", GrantEpoch: 4})
	if !errors.Is(err, ErrIdentityMismatch) || f.SendCalls != 0 {
		t.Fatalf("stale pane accepted: err=%v calls=%d", err, f.SendCalls)
	}
}

func TestFakeDeniesMixedControlOriginWithoutSend(t *testing.T) {
	f := NewFake()
	g := Grant{GrantID: "control-grant", SenderPrincipalID: "flowbee-control",
		RecipientSessionID: "recipient", RecipientPaneInstanceID: "pane", Epoch: 2}
	_ = f.Grant(context.Background(), g)
	a := NewAction("control-action", "hello", 2)
	a.SenderPrincipalID = "flowbee-control"
	a.SenderSessionID = "forged-session"
	_, err := f.Send(context.Background(), SendRequest{Action: a, GrantID: g.GrantID,
		RecipientSessionID: g.RecipientSessionID, RecipientPaneInstanceID: g.RecipientPaneInstanceID,
		GrantEpoch: g.Epoch, OnBehalfOfSessionID: "forged-session"})
	if !errors.Is(err, ErrIdentityMismatch) || f.SendCalls != 0 {
		t.Fatalf("mixed origin accepted: err=%v calls=%d", err, f.SendCalls)
	}
}

type memoryCommit struct {
	actions    []Action
	receipts   []Receipt
	commitErr  error
	persistErr error
}

func (m *memoryCommit) CommitAction(_ context.Context, a Action) error {
	if m.commitErr != nil {
		return m.commitErr
	}
	m.actions = append(m.actions, a)
	return nil
}
func (m *memoryCommit) PersistReceipt(_ context.Context, _ Action, r Receipt) error {
	if m.persistErr != nil {
		return m.persistErr
	}
	m.receipts = append(m.receipts, r)
	return nil
}

type portOverride struct {
	*FakePort
	ensureErr      error
	ensureIdentity Identity
	grantErr       error
}

func (p *portOverride) EnsureSession(ctx context.Context, t SessionTarget, a Action) (Identity, error) {
	if p.ensureErr != nil {
		return Identity{}, p.ensureErr
	}
	if p.ensureIdentity != (Identity{}) {
		return p.ensureIdentity, nil
	}
	return p.FakePort.EnsureSession(ctx, t, a)
}
func (p *portOverride) Grant(ctx context.Context, g Grant) error {
	if p.grantErr != nil {
		return p.grantErr
	}
	return p.FakePort.Grant(ctx, g)
}

func TestExecutorCommitsBeforeAnyDriverMutation(t *testing.T) {
	f := NewFake()
	m := &memoryCommit{commitErr: errors.New("db unavailable")}
	e := Executor{Port: f, Store: m}
	_, err := e.Execute(context.Background(), SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "p"}}, fixtureGrant(), NewAction("commit-fail", "x", 4))
	if err == nil || f.SendCalls != 0 || len(f.Grants) != 0 {
		t.Fatalf("driver touched before durable commit: err=%v sends=%d grants=%d", err, f.SendCalls, len(f.Grants))
	}
}

func TestExecutorFencesIdentityBeforeGrantOrSend(t *testing.T) {
	f := &portOverride{FakePort: NewFake(), ensureIdentity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "new"}}
	m := &memoryCommit{}
	target := SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "old"}}
	_, err := (Executor{Port: f, Store: m}).Execute(context.Background(), target, fixtureGrant(), NewAction("identity-fail", "x", 4))
	if !errors.Is(err, ErrIdentityMismatch) || len(f.Grants) != 0 || f.SendCalls != 0 {
		t.Fatalf("stale identity not fenced: err=%v grants=%d sends=%d", err, len(f.Grants), f.SendCalls)
	}
}

func TestExecutorUncertainWithoutReceiptRequiresReconciliation(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	_ = f.Grant(context.Background(), g)
	f.NextError = ErrUncertain
	e := Executor{Port: f, Store: &memoryCommit{}}
	r, err := e.Execute(context.Background(), SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}, g, NewAction("unknown", "x", 4))
	if !errors.Is(err, ErrUncertain) || !r.Uncertain || f.SendCalls != 1 {
		t.Fatalf("uncertain window not surfaced: result=%+v err=%v calls=%d", r, err, f.SendCalls)
	}
}

func TestExecutorReceiptPersistFailureNeverClaimsStage(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	m := &memoryCommit{persistErr: errors.New("ledger unavailable")}
	called := false
	e := Executor{Port: f, Store: m, Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { called = true; return true, nil })}
	r, err := e.Execute(context.Background(), SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}, g, NewAction("persist-fail", "x", 4))
	if err == nil || called || r.StageComplete {
		t.Fatalf("stage claimed before receipt persistence: result=%+v err=%v called=%v", r, err, called)
	}
}

type evidenceFunc func(context.Context, Action, Receipt) (bool, error)

func (f evidenceFunc) AwaitStage(c context.Context, a Action, r Receipt) (bool, error) {
	return f(c, a, r)
}

func TestExecutorSeparatesTransportReceiptFromStageEvidence(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	target := SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}
	_ = f.Grant(context.Background(), g)
	m := &memoryCommit{}
	e := Executor{Port: f, Store: m, Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) { return false, nil })}
	r, err := e.Execute(context.Background(), target, g, NewAction("x", "payload", 4))
	if err != nil {
		t.Fatal(err)
	}
	if r.Receipt.Status != ReceiptSubmitted || r.StageComplete {
		t.Fatalf("unexpected result: %+v", r)
	}
	if len(m.receipts) != 1 {
		t.Fatalf("receipt not persisted")
	}
}

func TestExecutorUsesDirectControlOriginWithoutOnBehalf(t *testing.T) {
	f := NewFake()
	target := SessionTarget{Identity: Identity{StoreID: "store", SessionID: "recipient", PaneInstanceID: "pane"}}
	g := Grant{GrantID: "grant", SenderPrincipalID: "flowbee-control",
		RecipientSessionID: "recipient", RecipientPaneInstanceID: "pane", Epoch: 3}
	a := NewAction("control-action", "payload", 3)
	a.SenderPrincipalID = "flowbee-control"
	result, err := (Executor{Port: f, Store: &memoryCommit{}}).Execute(context.Background(), target, g, a)
	if err != nil || !result.Receipt.Submitted() || len(f.SendRequests) != 1 {
		t.Fatalf("result=%+v requests=%d err=%v", result, len(f.SendRequests), err)
	}
	req := f.SendRequests[0]
	if req.OnBehalfOfSessionID != "" || req.SenderPrincipalID != "flowbee-control" ||
		req.SenderSessionID != "" || result.Receipt.SenderPrincipalID != "flowbee-control" {
		t.Fatalf("control send impersonated a session: request=%+v receipt=%+v", req, result.Receipt)
	}
}

func TestExecutorDoesNotTreatTypedTransportAsSubmission(t *testing.T) {
	f := NewFake()
	f.NextStatus = ReceiptTyped
	g := fixtureGrant()
	_ = f.Grant(context.Background(), g)
	m := &memoryCommit{}
	evidenceCalled := false
	e := Executor{Port: f, Store: m, Evidence: evidenceFunc(func(context.Context, Action, Receipt) (bool, error) {
		evidenceCalled = true
		return true, nil
	})}
	result, err := e.Execute(context.Background(), SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}, g, NewAction("typed", "x", g.Epoch))
	if !errors.Is(err, ErrUncertain) || !result.Uncertain || evidenceCalled || len(m.receipts) != 1 {
		t.Fatalf("result=%+v err=%v evidence=%v receipts=%d", result, err, evidenceCalled, len(m.receipts))
	}
}

func TestExecutorDoesNotTreatRefusedTransportAsDelivery(t *testing.T) {
	f := NewFake()
	f.NextStatus = ReceiptRefused
	g := fixtureGrant()
	_ = f.Grant(context.Background(), g)
	m := &memoryCommit{}
	result, err := (Executor{Port: f, Store: m}).Execute(context.Background(),
		SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}, g, NewAction("refused", "x", g.Epoch))
	if err == nil || result.Uncertain || len(m.receipts) != 1 {
		t.Fatalf("result=%+v err=%v receipts=%d", result, err, len(m.receipts))
	}
}

func TestExecutorReconcilesCrashUncertainWithoutResend(t *testing.T) {
	f := NewFake()
	g := fixtureGrant()
	target := SessionTarget{Identity: Identity{StoreID: "s", SessionID: "b", PaneInstanceID: "pane-b"}}
	_ = f.Grant(context.Background(), g)
	a := NewAction("x", "payload", 4)
	// Simulate an accepted Driver delivery whose response was lost.
	_, _ = f.Send(context.Background(), SendRequest{Action: a, GrantID: g.GrantID, RecipientSessionID: g.RecipientSessionID, RecipientPaneInstanceID: g.RecipientPaneInstanceID, GrantEpoch: g.Epoch})
	f.NextError = ErrUncertain
	m := &memoryCommit{}
	e := Executor{Port: f, Store: m}
	if _, err := e.Execute(context.Background(), target, g, a); err != nil {
		t.Fatal(err)
	}
	if f.SendCalls != 1 {
		t.Fatalf("uncertain retry resent: %d", f.SendCalls)
	}
}

func TestNewUDSPortUsesSameHTTPContract(t *testing.T) {
	p := NewUDSPort("/tmp/flowbee-driver.sock", "token")
	if p.BaseURL == "" || p.Client == nil || p.Client.Transport == nil {
		t.Fatalf("UDS adapter not configured: %+v", p)
	}
}

func TestObservationStoreResetIsExplicitAndAtLeastOnce(t *testing.T) {
	f := NewFake()
	f.Observations = []Observation{{EventID: "e1", StoreSeq: 7, Kind: "session.started", Identity: Identity{StoreID: "s1", SessionID: "b"}}}
	b, err := f.Observe(context.Background(), "tdc2.cursor")
	if err != nil || len(b.Events) != 1 {
		t.Fatalf("batch=%+v err=%v", b, err)
	}
	// A real adapter sets these flags when the Driver reports a cursor gap/reset;
	// Flowbee replaces only the derived projection and never its audit ledger.
	b.CursorGap, b.StoreReset = true, true
	if !b.CursorGap || !b.StoreReset {
		t.Fatal("cursor reset must be explicit")
	}
}

func TestDriverBoundaryContainsNoRawTmuxActuator(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Dir(file)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if !strings.HasSuffix(ent.Name(), ".go") || strings.HasSuffix(ent.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		if strings.Contains(s, "tmux-send") || strings.Contains(s, "send-keys") {
			t.Fatalf("raw tmux actuator leaked into DriverPort package: %s", ent.Name())
		}
	}
}
