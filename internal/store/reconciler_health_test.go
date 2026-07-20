package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestReconcilerWatchdogPersistsOneAlertAndFencesOldIncarnation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)

	lease, err := st.BeginReconciler(ctx, "review_handoff", "serve-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReconcilerSuccess(ctx, lease, now.Add(10*time.Second), store.ReconcilerProgress{Cursor: "pr:4951", LedgerSeq: 42}); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.ReconcileStaleReconcilers(ctx, now.Add(69*time.Second)); err != nil || rep.Alerted != 0 {
		t.Fatalf("early watchdog=%+v err=%v", rep, err)
	}
	staleAt := now.Add(71 * time.Second)
	// Active attention deduplication is project-local. A project may legitimately
	// use the same key as a global control-plane condition without being mutated
	// or resolved by the watchdog.
	createAttentionProject(t, st, "alpha", now)
	dedup := "reconciler_dead:review_handoff:1:1"
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,state,dedup_key,occurrences,first_seen_at,last_seen_at,created_at,updated_at)
		VALUES ('alpha-same-dedup','alpha','needs_input','open',?,1,?,?,?,?)`, dedup, stamp, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.ReconcileStaleReconcilers(ctx, staleAt); err != nil || rep.Alerted != 1 {
		t.Fatalf("stale watchdog=%+v err=%v", rep, err)
	}
	health, err := st.GetReconcilerHealth(ctx, "review_handoff")
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "stale" || health.Cursor != "pr:4951" || health.LedgerSeq != 42 || health.StaleEpoch != 1 {
		t.Fatalf("health after stale=%+v", health)
	}
	var alerts, openAttention int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE kind='reconciler_dead' AND state='pending'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("alerts=%d err=%v", alerts, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE kind='reconciler_dead' AND state='open'`).Scan(&openAttention); err != nil || openAttention != 1 {
		t.Fatalf("open attention=%d err=%v", openAttention, err)
	}
	var alphaState string
	var alphaOccurrences int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,occurrences FROM attention_items
		WHERE project_id='alpha' AND dedup_key=?`, dedup).Scan(&alphaState, &alphaOccurrences); err != nil {
		t.Fatal(err)
	}
	if alphaState != "open" || alphaOccurrences != 1 {
		t.Fatalf("global watchdog mutated project attention state=%q occurrences=%d", alphaState, alphaOccurrences)
	}
	// Repeated watchdog passes are idempotent for the same stale generation.
	if rep, err := st.ReconcileStaleReconcilers(ctx, staleAt.Add(time.Hour)); err != nil || rep.Alerted != 0 {
		t.Fatalf("repeat watchdog=%+v err=%v", rep, err)
	}

	newLease, err := st.BeginReconciler(ctx, "review_handoff", "serve-b", staleAt.Add(time.Hour), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if newLease.Epoch != lease.Epoch+1 {
		t.Fatalf("replacement epoch=%d want %d", newLease.Epoch, lease.Epoch+1)
	}
	if err := st.HeartbeatReconciler(ctx, lease, staleAt.Add(time.Hour), store.ReconcilerProgress{}); !errors.Is(err, store.ErrStaleReconcilerLease) {
		t.Fatalf("old incarnation heartbeat err=%v, want stale lease", err)
	}
	if err := st.MarkReconcilerSuccess(ctx, newLease, staleAt.Add(time.Hour), store.ReconcilerProgress{LedgerSeq: 43}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE kind='reconciler_dead' AND state='resolved'`).Scan(&openAttention); err != nil || openAttention != 1 {
		t.Fatalf("resolved attention=%d err=%v", openAttention, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM attention_items
		WHERE project_id='alpha' AND dedup_key=?`, dedup).Scan(&alphaState); err != nil || alphaState != "open" {
		t.Fatalf("global recovery resolved project attention state=%q err=%v", alphaState, err)
	}
}

func TestPoisonFactIsQuarantinedWhileLoopRemainsHealthy(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	lease, err := st.BeginReconciler(ctx, "artifact_ingestion", "serve-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordReconcilerPoisonFact(ctx, lease.Name, "repo:russ/pr:broken", "invalid artifact payload", now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordReconcilerPoisonFact(ctx, lease.Name, "repo:russ/pr:broken", "invalid artifact payload", now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReconcilerSuccess(ctx, lease, now.Add(3*time.Second), store.ReconcilerProgress{Cursor: "store-seq:9", LedgerSeq: 99}); err != nil {
		t.Fatal(err)
	}
	health, err := st.GetReconcilerHealth(ctx, lease.Name)
	if err != nil {
		t.Fatal(err)
	}
	if health.State != "healthy" || health.Cursor != "store-seq:9" || health.LedgerSeq != 99 {
		t.Fatalf("poison stopped healthy loop: %+v", health)
	}
	var occurrences, alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT occurrences FROM reconciler_poison_facts
		WHERE reconciler_name=? AND fact_key=?`, lease.Name, "repo:russ/pr:broken").Scan(&occurrences); err != nil || occurrences != 2 {
		t.Fatalf("occurrences=%d err=%v", occurrences, err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE kind='reconciler_poison_fact'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("deduped poison alerts=%d err=%v", alerts, err)
	}
	if err := st.ResolveReconcilerPoisonFact(ctx, lease.Name, "repo:russ/pr:broken", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordReconcilerPoisonFact(ctx, lease.Name, "repo:russ/pr:broken", "bad again", now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE kind='reconciler_poison_fact'`).Scan(&alerts); err != nil || alerts != 2 {
		t.Fatalf("recurrence alerts=%d err=%v", alerts, err)
	}
}

func TestRecoveredReconcilerPanicIsDurableAndDoesNotFenceNextTick(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	lease, err := st.BeginReconciler(ctx, "driver_executor", "serve-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReconcilerPanic(ctx, lease, now.Add(time.Second), store.ReconcilerProgress{}, "boom"); err != nil {
		t.Fatal(err)
	}
	health, err := st.GetReconcilerHealth(ctx, lease.Name)
	if err != nil || health.State != "panicked" || health.ConsecutiveFailures != 1 || health.LastPanicAt.IsZero() {
		t.Fatalf("panic health=%+v err=%v", health, err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts WHERE kind='reconciler_panic'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("panic alerts=%d err=%v", alerts, err)
	}
	if err := st.MarkReconcilerSuccess(ctx, lease, now.Add(2*time.Second), store.ReconcilerProgress{}); err != nil {
		t.Fatal(err)
	}
	health, _ = st.GetReconcilerHealth(ctx, lease.Name)
	if health.State != "healthy" || health.ConsecutiveFailures != 0 {
		t.Fatalf("next tick did not recover: %+v", health)
	}
	if err := st.MarkReconcilerFailure(ctx, lease, now.Add(3*time.Second), store.ReconcilerProgress{}, "temporary transport failure"); err != nil {
		t.Fatal(err)
	}
	health, _ = st.GetReconcilerHealth(ctx, lease.Name)
	if health.State != "degraded" || health.ConsecutiveFailures != 1 || health.LastFailureAt.IsZero() {
		t.Fatalf("failure health=%+v", health)
	}
}

func TestDeliveryBackstopPoisonRowIsQuarantinedInsteadOfKillingPass(t *testing.T) {
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "poison-backstop", Repo: "russ", Branch: "epic/poison-backstop"}, 1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET state='building',state_due_at=?
		WHERE epic_id='poison-backstop'`, now.Add(-time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// Simulate one poison row without poisoning the operational attention used to
	// quarantine it. The pass must continue and return normally.
	if _, err := st.DB.ExecContext(ctx, `CREATE TRIGGER poison_delivery_attention
		BEFORE INSERT ON attention_items WHEN NEW.epic_id='poison-backstop'
		BEGIN SELECT RAISE(ABORT,'poison delivery row'); END`); err != nil {
		t.Fatal(err)
	}
	rep, err := st.ReconcileEpicDeliveryBackstops(ctx, now)
	if err != nil || rep.Scanned != 1 {
		t.Fatalf("pass=%+v err=%v", rep, err)
	}
	var poisonState string
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM reconciler_poison_facts
		WHERE reconciler_name='delivery_backstop' AND fact_key='epic:poison-backstop'`).Scan(&poisonState); err != nil || poisonState != "open" {
		t.Fatalf("poison state=%q err=%v", poisonState, err)
	}
	var alerts int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_alerts
		WHERE kind='reconciler_poison_fact'`).Scan(&alerts); err != nil || alerts != 1 {
		t.Fatalf("poison alerts=%d err=%v", alerts, err)
	}
}

func TestReconcilerSummaryIsAnIndependentDueClockDeadMan(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	if _, err := st.BeginReconciler(ctx, "healthy", "serve-1", now, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.BeginReconciler(ctx, "reconciler_watchdog", "serve-1", now, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	summary, err := st.ReconcilerSummary(ctx, now.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 2 || summary.Overdue != 1 || len(summary.OverdueNames) != 1 || summary.OverdueNames[0] != "reconciler_watchdog" {
		t.Fatalf("summary=%+v", summary)
	}
}
