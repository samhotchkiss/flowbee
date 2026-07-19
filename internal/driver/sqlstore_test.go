package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedSQLStoreEpic(t *testing.T) (SQLActionStore, Action) {
	t.Helper()
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := "2026-07-19T00:00:00Z"
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO epics
		 (id, repo, file_path, state, project_id, slug, admission_key, created_at, updated_at)
		VALUES ('epic-1', 'repo', 'epics/epic-1.md', 'running', 'default', 'epic-1', 'test:epic-1', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		INSERT INTO epic_deliveries
		 (epic_id, project_id, state, created_at, updated_at)
		VALUES ('epic-1', 'default', 'awaiting_review_dispatch', ?, ?)`, now, now); err != nil {
		t.Fatal(err)
	}
	a := NewAction("action-1", `{"task":"review"}`, 3)
	a.ProjectID, a.EpicID, a.Kind = "default", "epic-1", "review_dispatch"
	a.DedupKey, a.HeadSHA, a.BaseSHA = "review:epic-1:head:base", "head", "base"
	a.SenderPrincipalID = "flowbee-control"
	return SQLActionStore{DB: st.DB, Now: func() time.Time { return time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC) }, ControlOriginAvailable: true}, a
}

func TestSQLActionStoreCommitsImmutableActionIdempotently(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	changed := a
	changed.Payload = `{"task":"different"}`
	changed.PayloadSHA256 = NewAction(a.ActionID, changed.Payload, a.Epoch).PayloadSHA256
	if err := s.CommitAction(ctx, changed); !errors.Is(err, ErrIdempotencyBody) {
		t.Fatalf("changed replay err=%v", err)
	}
	var count int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestSQLActionStoreControlOriginDisabledNeverClaimsPreexistingMessage(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	s.ControlOriginAvailable = false
	claimed, ok, err := s.ClaimNextAction(ctx, "disabled-runtime", time.Now(), time.Minute)
	if err != nil || ok || claimed.ActionID != "" {
		t.Fatalf("disabled claim action=%+v ok=%v err=%v", claimed, ok, err)
	}
	var state string
	var epoch int64
	if err := s.DB.QueryRowContext(ctx, `SELECT state,action_epoch FROM epic_actions WHERE id=?`, a.ActionID).
		Scan(&state, &epoch); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || epoch != a.Epoch {
		t.Fatalf("disabled claim mutated state=%s epoch=%d want pending/%d", state, epoch, a.Epoch)
	}
}

func TestSQLActionStorePersistsReceiptIdempotentlyAndRejectsMutation(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	a.GrantID, a.GrantEpoch = "grant-1", a.Epoch
	a.RecipientSessionID, a.RecipientPaneInstanceID = "reviewer", "pane-1"
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	r := Receipt{DeliveryID: "delivery-1", ActionID: a.ActionID, GrantID: "grant-1", GrantEpoch: a.Epoch,
		SenderPrincipalID: "flowbee-control", Recipient: Identity{SessionID: "reviewer", PaneInstanceID: "pane-1"},
		PayloadSHA256: a.PayloadSHA256, Status: ReceiptSubmitted}
	if err := s.PersistReceipt(ctx, a, r); err != nil {
		t.Fatal(err)
	}
	if err := s.PersistReceipt(ctx, a, r); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	changed := r
	changed.GrantID = "grant-2"
	if err := s.PersistReceipt(ctx, a, changed); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("changed replay err=%v", err)
	}
}

func TestSQLActionStoreReceiptProgressesMonotonicallyAndFencesIdentity(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	a.GrantID, a.GrantEpoch = "grant-1", a.Epoch
	a.RecipientSessionID, a.RecipientPaneInstanceID = "reviewer", "pane-1"
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	accepted := Receipt{DeliveryID: "delivery-1", ActionID: a.ActionID, GrantID: a.GrantID,
		GrantEpoch: a.GrantEpoch, SenderPrincipalID: a.SenderPrincipalID,
		Recipient:     Identity{SessionID: a.RecipientSessionID, PaneInstanceID: a.RecipientPaneInstanceID},
		PayloadSHA256: a.PayloadSHA256, Status: ReceiptAccepted}
	if err := s.PersistReceipt(ctx, a, accepted); err != nil {
		t.Fatal(err)
	}
	delivering := accepted
	delivering.Status = ReceiptDelivering
	if err := s.PersistReceipt(ctx, a, delivering); err != nil {
		t.Fatalf("accepted -> delivering: %v", err)
	}
	submitted := accepted
	submitted.Status, submitted.CompatibilityCode = ReceiptSubmitted, 0
	if err := s.PersistReceipt(ctx, a, submitted); err != nil {
		t.Fatalf("delivering -> submitted: %v", err)
	}
	if err := s.PersistReceipt(ctx, a, delivering); !errors.Is(err, ErrIdempotencyBody) {
		t.Fatalf("terminal receipt regressed: %v", err)
	}
	wrongPrincipal := submitted
	wrongPrincipal.SenderPrincipalID = "other-control"
	if err := s.PersistReceipt(ctx, a, wrongPrincipal); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong principal accepted: %v", err)
	}
	mixed := submitted
	mixed.Sender = Identity{SessionID: "forged", AgentRunID: "run"}
	if err := s.PersistReceipt(ctx, a, mixed); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("mixed origin accepted: %v", err)
	}
	wrongRecipient := submitted
	wrongRecipient.Recipient.PaneInstanceID = "reused-pane"
	if err := s.PersistReceipt(ctx, a, wrongRecipient); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong recipient accepted: %v", err)
	}
}

func TestReceiptExpectationFencesSessionAgentRun(t *testing.T) {
	a := NewAction("session-action", "payload", 2)
	a.GrantID, a.GrantEpoch = "grant", 2
	a.SenderSessionID, a.SenderAgentRunID = "sender", "run-1"
	a.RecipientSessionID, a.RecipientPaneInstanceID = "recipient", "pane"
	r := Receipt{DeliveryID: "delivery", ActionID: a.ActionID, GrantID: a.GrantID,
		GrantEpoch: 2, Sender: Identity{SessionID: "sender", AgentRunID: "run-2"},
		Recipient:     Identity{SessionID: "recipient", PaneInstanceID: "pane"},
		PayloadSHA256: a.PayloadSHA256, Status: ReceiptSubmitted}
	if err := a.ExpectedReceipt().Validate(r); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("wrong agent run accepted: %v", err)
	}
}

func TestSQLActionStoreLifecycleReceiptResolvesUncertainWithoutChangingIntent(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	uncertain := LifecycleReceipt{LifecycleReceiptID: "lifecycle-1", ActionID: a.ActionID,
		ActionEpoch: a.Epoch, Operation: "ensure", LifecycleKey: "builder-1", TargetEpoch: 2,
		LeaseID: "builder-affinity:epic-1", LeaseEpoch: 2, Status: "uncertain"}
	if err := s.PersistLifecycleReceipt(ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	resolved := uncertain
	resolved.Status = "ensured"
	resolved.IdentityAfter = Identity{HostID: "host", StoreID: "store",
		TmuxServerInstanceID: "server", LifecycleKey: "builder-1", TargetEpoch: 2,
		SessionID: "session-2", PaneInstanceID: "pane-2", AgentRunID: "run-2"}
	if err := s.PersistLifecycleReceipt(ctx, resolved); err != nil {
		t.Fatalf("canonical uncertain receipt did not resolve: %v", err)
	}
	if err := s.PersistLifecycleReceipt(ctx, resolved); err != nil {
		t.Fatalf("resolved replay: %v", err)
	}
	mutated := resolved
	mutated.TargetEpoch = 3
	if err := s.PersistLifecycleReceipt(ctx, mutated); !errors.Is(err, ErrIdempotencyBody) {
		t.Fatalf("changed lifecycle intent err=%v", err)
	}
	regressed := resolved
	regressed.Status = "uncertain"
	if err := s.PersistLifecycleReceipt(ctx, regressed); !errors.Is(err, ErrIdempotencyBody) {
		t.Fatalf("terminal lifecycle receipt regressed: %v", err)
	}
}

func TestSQLActionStoreClaimsAndFencesActionEpoch(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	a.Epoch = 0
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	claimed, ok, err := s.ClaimNextAction(ctx, "executor-a", now, time.Minute)
	if err != nil || !ok || claimed.ActionID != a.ActionID || claimed.Epoch != 1 {
		t.Fatalf("claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	if _, ok, err := s.ClaimNextAction(ctx, "executor-b", now, time.Minute); err != nil || ok {
		t.Fatalf("double claim ok=%v err=%v", ok, err)
	}
	if err := s.AcknowledgeAction(ctx, a.ActionID, "executor-a", 0, now); !errors.Is(err, ErrStaleActionEpoch) {
		t.Fatalf("stale epoch accepted: %v", err)
	}
	if err := s.AcknowledgeAction(ctx, a.ActionID, "executor-a", claimed.Epoch, now); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := s.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE id=?`, a.ActionID).Scan(&state); err != nil || state != "acknowledged" {
		t.Fatalf("state=%q err=%v", state, err)
	}
}

func TestSQLActionStoreDeadLetterRearmsSameRowWithinBudget(t *testing.T) {
	s, a := seedSQLStoreEpic(t)
	ctx := context.Background()
	a.Epoch = 0
	if err := s.CommitAction(ctx, a); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 1, 0, 0, 0, time.UTC)
	claimed, ok, err := s.ClaimNextAction(ctx, "executor", now, time.Minute)
	if err != nil || !ok {
		t.Fatalf("claim ok=%v err=%v", ok, err)
	}
	if err := s.DeadLetterAction(ctx, a.ActionID, "executor", claimed.Epoch, "transient 5xx", now); err != nil {
		t.Fatal(err)
	}
	if err := s.RearmDeadLetter(ctx, a.ActionID, 1, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	reclaimed, ok, err := s.ClaimNextAction(ctx, "executor", now.Add(time.Minute), time.Minute)
	if err != nil || !ok {
		t.Fatalf("reclaim ok=%v err=%v", ok, err)
	}
	if reclaimed.GrantID == claimed.GrantID || reclaimed.Epoch != claimed.Epoch+1 {
		t.Fatalf("retry reused Driver grant identity: first=%+v retry=%+v", claimed, reclaimed)
	}
	var grants, distinct int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT grant_id)
		FROM driver_grants WHERE action_id=?`, a.ActionID).Scan(&grants, &distinct); err != nil || grants != 2 || distinct != 2 {
		t.Fatalf("grant history count=%d distinct=%d err=%v", grants, distinct, err)
	}
	if err := s.DeadLetterAction(ctx, a.ActionID, "executor", reclaimed.Epoch, "still failing", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RearmDeadLetter(ctx, a.ActionID, 1, now.Add(2*time.Minute)); !errors.Is(err, ErrStaleActionEpoch) {
		t.Fatalf("recovery cap not enforced: %v", err)
	}
	var count, recoveries int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*), MAX(recovery_count) FROM epic_actions WHERE dedup_key=?`, a.DedupKey).Scan(&count, &recoveries); err != nil || count != 1 || recoveries != 1 {
		t.Fatalf("count=%d recoveries=%d err=%v", count, recoveries, err)
	}
}
