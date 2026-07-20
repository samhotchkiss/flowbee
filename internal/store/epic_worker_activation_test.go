package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func addPreDedicatedEpic(t *testing.T, st *store.Store, id string, now time.Time) {
	t.Helper()
	if err := st.AddEpicRun(context.Background(), store.EpicRun{ID: id, Slug: id,
		Repo: "russ", Branch: "epic/" + id, FilePath: "epics/" + id + ".md",
		Title: id, BuilderModelFamily: "codex"}, 20, now); err != nil {
		t.Fatalf("add pre-dedicated epic %s: %v", id, err)
	}
	// This fixture models a P1 epic whose independently selected reviewer family
	// was already persisted. Backfill is forbidden from inventing one.
	if _, err := st.DB.Exec(`UPDATE epic_deliveries SET reviewer_model_family='grok'
		WHERE epic_id=?`, id); err != nil {
		t.Fatalf("seed authoritative reviewer family for %s: %v", id, err)
	}
}

func TestEnableDedicatedWorkersBackfillsAllNonterminalP1EpicsAtomically(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	installTestEpicWorkerMaterialProvider(st)
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	addPreDedicatedEpic(t, st, "already-building", now)
	addPreDedicatedEpic(t, st, "awaiting-review", now.Add(time.Second))
	addPreDedicatedEpic(t, st, "terminal", now.Add(2*time.Second))
	if _, err := st.DB.ExecContext(ctx, `UPDATE epics SET state='done' WHERE id='awaiting-review';
		UPDATE epic_deliveries SET state='awaiting_review_dispatch' WHERE epic_id='awaiting-review';
		UPDATE epics SET state='abandoned' WHERE id='terminal';
		UPDATE epic_deliveries SET state='abandoned' WHERE epic_id='terminal'`); err != nil {
		t.Fatal(err)
	}

	if err := st.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, epicID := range []string{"already-building", "awaiting-review"} {
		var workers, credentials, distinctFamilies int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT model_family)
			FROM epic_worker_sessions WHERE epic_id=?`, epicID).Scan(&workers, &distinctFamilies); err != nil {
			t.Fatal(err)
		}
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_credentials
			WHERE epic_id=?`, epicID).Scan(&credentials); err != nil {
			t.Fatal(err)
		}
		if workers != 2 || credentials != 2 || distinctFamilies != 2 {
			t.Fatalf("%s backfill workers=%d credentials=%d families=%d", epicID,
				workers, credentials, distinctFamilies)
		}
	}
	var terminalWorkers int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='terminal'`).Scan(&terminalWorkers); err != nil {
		t.Fatal(err)
	}
	if terminalWorkers != 0 {
		t.Fatalf("terminal epic was backfilled with %d worker plans", terminalWorkers)
	}
	if enabled, err := st.DurableEpicDedicatedWorkersV2(ctx); err != nil || !enabled {
		t.Fatalf("durable activation enabled=%v err=%v", enabled, err)
	}

	// A later admission observes the durable flag in its own transaction even
	// when this Store's legacy in-memory boolean remains false.
	addPreDedicatedEpic(t, st, "post-activation", now.Add(2*time.Minute))
	var postWorkers int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='post-activation'`).Scan(&postWorkers); err != nil {
		t.Fatal(err)
	}
	if postWorkers != 2 {
		t.Fatalf("post-activation admission worker plans=%d, want 2", postWorkers)
	}
}

func TestEnableDedicatedWorkersIsConcurrentAndIdempotentWithAdmission(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 20)
	st := h.st
	now := h.now
	addPreDedicatedEpic(t, st, "before-race", now)

	start := make(chan struct{})
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- st.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(time.Minute))
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		errs <- st.AddEpicRun(ctx, store.EpicRun{ID: "racing-admission", Slug: "racing-admission",
			Repo: "russ", Branch: "epic/racing-admission", FilePath: "epics/racing-admission.md",
			BuilderModelFamily: "codex"}, 20, now.Add(2*time.Minute))
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, epicID := range []string{"before-race", "racing-admission"} {
		var workers, credentials int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
			WHERE epic_id=?`, epicID).Scan(&workers); err != nil {
			t.Fatal(err)
		}
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_credentials
			WHERE epic_id=?`, epicID).Scan(&credentials); err != nil {
			t.Fatal(err)
		}
		if workers != 2 || credentials != 2 {
			t.Fatalf("%s plans=%d materials=%d after concurrent activation", epicID, workers, credentials)
		}
	}
}

func TestDedicatedWorkerActivationCannotDisableWithDurableObligations(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	installTestEpicWorkerMaterialProvider(st)
	now := time.Date(2026, 7, 19, 19, 45, 0, 0, time.UTC)
	addPreDedicatedEpic(t, st, "one-way-worker-boundary", now)
	if err := st.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDurableEpicDedicatedWorkersV2(ctx, false, now.Add(2*time.Minute)); err == nil ||
		!strings.Contains(err.Error(), "one-way") {
		t.Fatalf("disable after materialization error=%v, want one-way refusal", err)
	}
	if enabled, err := st.DurableEpicDedicatedWorkersV2(ctx); err != nil || !enabled {
		t.Fatalf("refused disable changed durable activation enabled=%v err=%v", enabled, err)
	}
}

func TestDedicatedWorkerBackfillNeverGuessesMissingFamilies(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	installTestEpicWorkerMaterialProvider(st)
	now := time.Date(2026, 7, 19, 19, 50, 0, 0, time.UTC)
	addPreDedicatedEpic(t, st, "missing-authority", now)
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET reviewer_model_family=''
		WHERE epic_id='missing-authority'`); err != nil {
		t.Fatal(err)
	}
	err := st.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "no authoritative distinct reviewer family") {
		t.Fatalf("missing reviewer authority error=%v", err)
	}
	var workers int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='missing-authority'`).Scan(&workers); err != nil {
		t.Fatal(err)
	}
	if workers != 0 {
		t.Fatalf("missing authority guessed and persisted %d worker plans", workers)
	}
	if enabled, err := st.DurableEpicDedicatedWorkersV2(ctx); err != nil || enabled {
		t.Fatalf("missing authority leaked activation enabled=%v err=%v", enabled, err)
	}

	st2 := testutil.NewStore(t)
	installTestEpicWorkerMaterialProvider(st2)
	addPreDedicatedEpic(t, st2, "ambiguous-authority", now)
	if _, err := st2.DB.ExecContext(ctx, `UPDATE epics SET builder_model_family='claude'
		WHERE id='ambiguous-authority'`); err != nil {
		t.Fatal(err)
	}
	err = st2.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "ambiguous builder family authority") {
		t.Fatalf("ambiguous builder authority error=%v", err)
	}
	if err := st2.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='ambiguous-authority'`).Scan(&workers); err != nil {
		t.Fatal(err)
	}
	if workers != 0 {
		t.Fatalf("ambiguous authority guessed and persisted %d worker plans", workers)
	}
}

func TestDedicatedWorkerBackfillPersistsCapacityProvenReviewerFamily(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 2)
	addPreDedicatedEpic(t, h.st, "capacity-proven-family", h.now)
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET reviewer_model_family=''
		WHERE epic_id='capacity-proven-family'`); err != nil {
		t.Fatal(err)
	}
	if err := h.st.SetDurableEpicDedicatedWorkersV2(ctx, true, h.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var deliveryFamily, planFamily string
	if err := h.st.DB.QueryRowContext(ctx, `SELECT reviewer_model_family FROM epic_deliveries
		WHERE epic_id='capacity-proven-family'`).Scan(&deliveryFamily); err != nil {
		t.Fatal(err)
	}
	if err := h.st.DB.QueryRowContext(ctx, `SELECT model_family FROM epic_worker_sessions
		WHERE epic_id='capacity-proven-family' AND worker_role='reviewer'`).Scan(&planFamily); err != nil {
		t.Fatal(err)
	}
	if deliveryFamily == "" || deliveryFamily != planFamily {
		t.Fatalf("capacity proof did not bind delivery family=%q worker plan=%q",
			deliveryFamily, planFamily)
	}
}

func TestEnableDedicatedWorkersRollsBackFlagAndBackfillOnPartialPlan(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	installTestEpicWorkerMaterialProvider(st)
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	st.EnableEpicDedicatedWorkersV2 = true
	addPreDedicatedEpic(t, st, "corrupt-existing", now)
	st.EnableEpicDedicatedWorkersV2 = false
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM epic_worker_credentials
		WHERE epic_id='corrupt-existing' AND worker_role='reviewer'`); err != nil {
		t.Fatal(err)
	}
	addPreDedicatedEpic(t, st, "must-roll-back", now.Add(time.Minute))

	err := st.SetDurableEpicDedicatedWorkersV2(ctx, true, now.Add(2*time.Minute))
	if err == nil || !strings.Contains(err.Error(), "credential material") {
		t.Fatalf("activation error=%v, want missing material", err)
	}
	if enabled, readErr := st.DurableEpicDedicatedWorkersV2(ctx); readErr != nil || enabled {
		t.Fatalf("failed activation leaked durable flag enabled=%v err=%v", enabled, readErr)
	}
	var leaked int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_worker_sessions
		WHERE epic_id='must-roll-back'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("failed activation leaked %d backfilled worker plans", leaked)
	}
}

func TestBuilderLaunchCannotCommitWithoutExactlyTwoWorkerPlans(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 2)
	h.st.EnableEpicDedicatedWorkersV2 = true
	h.addEpic(t, "missing-reviewer-material")
	if _, err := h.st.DB.ExecContext(ctx, `DELETE FROM epic_worker_credentials
		WHERE epic_id='missing-reviewer-material' AND worker_role='reviewer'`); err != nil {
		t.Fatal(err)
	}

	_, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute), 5*time.Minute, "codex", 5)
	if err == nil || !strings.Contains(err.Error(), "exact dedicated worker plan") &&
		!strings.Contains(err.Error(), "credential material") {
		t.Fatalf("builder launch error=%v, want exact two-plan invariant", err)
	}
	var actions, assigned int
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='missing-reviewer-material' AND kind='builder_launch'`).Scan(&actions)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics
		WHERE id='missing-reviewer-material' AND seat_id<>''`).Scan(&assigned)
	if actions != 0 || assigned != 0 {
		t.Fatalf("failed invariant leaked builder action=%d assignment=%d", actions, assigned)
	}
}
