package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/driver"
	"github.com/samhotchkiss/flowbee/internal/driverbridge"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestBuilderFairStarvationFenceServesQuietProjectFirst(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 8)
	h.addProject(t, "a-noisy", "active", 0)
	h.addProject(t, "z-quiet", "active", 0)
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE projects SET scheduler_weight=100
		WHERE id='a-noisy'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"noisy-1", "noisy-2", "noisy-3"} {
		h.addProjectEpic(t, "a-noisy", id)
	}
	h.addProjectEpic(t, "z-quiet", "quiet-1")
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET created_at=?
		WHERE epic_id='quiet-1'`, h.now.Add(-16*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	var projectID string
	var forced int
	if err := h.st.DB.QueryRowContext(ctx, `SELECT project_id,forced_by_age
		FROM project_scheduler_effects ORDER BY seq LIMIT 1`).Scan(&projectID, &forced); err != nil {
		t.Fatal(err)
	}
	if projectID != "z-quiet" || forced != 1 {
		t.Fatalf("first service project=%q forced=%d; quiet project was not age-fenced", projectID, forced)
	}
}

func TestBuilderFairWeightedServiceConvergesForNoisyProjects(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 8)
	h.addProject(t, "a-heavy", "active", 0)
	h.addProject(t, "z-light", "active", 0)
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE projects SET scheduler_weight=3
		WHERE id='a-heavy'`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		h.addProjectEpic(t, "a-heavy", "heavy-"+string(rune('0'+i)))
		h.addProjectEpic(t, "z-light", "light-"+string(rune('0'+i)))
	}
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	rows, err := h.st.DB.QueryContext(ctx, `SELECT project_id FROM project_scheduler_effects
		ORDER BY seq LIMIT 4`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		counts[projectID]++
	}
	if counts["a-heavy"] != 3 || counts["z-light"] != 1 {
		t.Fatalf("first complete 3:1 service round=%v", counts)
	}
}

func TestBuilderFairEffectFailureRollsBackAllocationAndDebit(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "fair-rollback")
	if _, err := h.st.DB.ExecContext(ctx, `CREATE TRIGGER fail_builder_fair_effect
		BEFORE INSERT ON project_scheduler_effects BEGIN
		SELECT RAISE(ABORT,'builder fair effect failpoint'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err == nil {
		t.Fatal("effect-ledger failure did not fail the transactional scheduling turn")
	}
	var assigned, actions, effects, states int
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics
		WHERE id='fair-rollback' AND seat_id<>''`).Scan(&assigned)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='fair-rollback' AND kind='builder_launch'`).Scan(&actions)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_scheduler_effects`).Scan(&effects)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_scheduler_state
		WHERE pool='build' AND last_served_at<>''`).Scan(&states)
	if assigned != 0 || actions != 0 || effects != 0 || states != 0 {
		t.Fatalf("failed turn leaked allocation=%d actions=%d effects=%d served_states=%d",
			assigned, actions, effects, states)
	}
	if _, err := h.st.DB.ExecContext(ctx, `DROP TRIGGER fail_builder_fair_effect`); err != nil {
		t.Fatal(err)
	}
	if rep, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil || rep.ActionsCreated != 1 {
		t.Fatalf("retry after rollback=%+v err=%v", rep, err)
	}
}

func TestBuilderLaunchCommitsImmutableWorkspaceSourceAndRoleQualifiedPath(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.st.EnableEpicDedicatedWorkersV2 = true
	installTestEpicWorkerMaterialProvider(h.st)
	envelopes := t.TempDir()
	materials := driver.SQLLifecycleLaunchMaterials{DB: h.st.DB, EnvelopeDirectory: envelopes,
		WorkerAuthSecret: []byte("workspace-source-test-secret-0123456789")}
	h.st.EpicWorkerCredentialMaterializer = materials.PrepareEnvelope
	if err := h.st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{ProjectID: "default",
		SeatID: h.seat.ID, InstanceRef: h.instanceRef, TmuxServerDomainID: "flowbee",
		TmuxServerInstanceID: "server-build", ProfileID: "codex_builder",
		WorkspaceRootID: "workspace-build", WorkspaceRelativeBase: "repos", Enabled: true}, h.now); err != nil {
		t.Fatal(err)
	}
	h.addEpic(t, "workspace-source")
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	var baseSHA, workspace, sourceSHA string
	if err := h.st.DB.QueryRowContext(ctx, `SELECT a.base_sha,a.workspace_relative_path,
		json_extract(w.bootstrap_payload,'$.source_commit_sha')
		FROM epic_actions a JOIN epic_worker_sessions w ON w.epic_id=a.epic_id
		AND w.worker_role='builder' WHERE a.epic_id='workspace-source' AND a.kind='builder_launch'`).
		Scan(&baseSHA, &workspace, &sourceSHA); err != nil {
		t.Fatal(err)
	}
	if baseSHA != sourceSHA || baseSHA != "1111111111111111111111111111111111111111" ||
		workspace != "repos/default/workspace-source/builder" {
		t.Fatalf("builder source/path=%q/%q plan_source=%q", baseSHA, workspace, sourceSHA)
	}
}

func TestBuilderFairRestartAndReplayDoNotDoubleDebit(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 1)
	h.addEpic(t, "fair-restart")
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	var dsn string
	if err := h.st.DB.QueryRowContext(ctx, `SELECT file FROM pragma_database_list
		WHERE name='main'`).Scan(&dsn); err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(dsn) {
		t.Fatalf("database path is not restartable: %q", dsn)
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close() })
	restarted.EnableCapacityV2 = true
	restarted.EnableDriverControlOrigin = true
	if _, err := restarted.ReconcileBuilderLaunches(ctx, h.now.Add(2*time.Minute),
		5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	var actions, effects int
	_ = restarted.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE epic_id='fair-restart' AND kind='builder_launch'`).Scan(&actions)
	_ = restarted.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_scheduler_effects
		WHERE resource_id='fair-restart'`).Scan(&effects)
	if actions != 1 || effects != 1 {
		t.Fatalf("restart replay actions=%d effects=%d", actions, effects)
	}
}

func TestBuilderFairSchedulesParkedReworkBeforeFreshWorkInProject(t *testing.T) {
	ctx := context.Background()
	h := newBuilderLaunchHarness(t, 3)
	h.st.EnableEpicReviewHandoffV2 = true
	// The helper's review chronology spans twelve minutes; keep that unrelated
	// reviewer-capacity proof out of this scheduler fixture, then restore the
	// production builder-capacity gate for the turn under test.
	h.st.EnableCapacityV2 = false
	action := seedRejectedBuilderLifecycle(t, h.st, "fair-rework", h.seat.ID,
		"account-build", 5201, h.now)
	h.st.EnableCapacityV2 = true
	if err := (driver.SQLActionStore{DB: h.st.DB}).RetryAction(ctx, action.ActionID,
		"fair-rework-claim", action.Epoch, "return to scheduler fixture",
		h.now.Add(14*time.Minute), h.now.Add(14*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// The fixture first exercises the legacy one-project claim path. Return the
	// immutable action to its pre-service epoch, then add a second active project
	// so the authoritative multi-project scheduler owns the compute acquisition.
	if _, err := h.st.DB.ExecContext(ctx, `UPDATE epic_actions SET action_epoch=0,
		claim_owner='',claim_deadline_at='',next_attempt_at=? WHERE id=?`,
		h.now.Add(14*time.Minute).Format(time.RFC3339Nano), action.ActionID); err != nil {
		t.Fatal(err)
	}
	h.addProject(t, "other", "active", 0)
	if err := h.st.RegisterRepo(ctx, store.Repo{ID: "russ", Owner: "fixture", Repo: "russ", Active: true}); err != nil {
		t.Fatal(err)
	}
	dedupHash := sha256.Sum256([]byte("fair-rework\x00" + h.seat.ID + "\x00builder_relaunch"))
	dedup := "builder_capacity_unavailable:fair-rework:" + hex.EncodeToString(dedupHash[:8])
	stamp := h.now.Add(14 * time.Minute).Format(time.RFC3339Nano)
	if _, err := h.st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,dedup_key,state,created_at,updated_at,first_seen_at,last_seen_at)
		VALUES ('fair-rework-own','default','capacity_pool_exhausted',?,'open',?,?,?,?),
		       ('fair-rework-other','other','capacity_pool_exhausted',?,'open',?,?,?,?)`,
		dedup, stamp, stamp, stamp, stamp, dedup, stamp, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	h.addEpic(t, "fair-fresh")
	if _, err := h.st.ReconcileBuilderLaunches(ctx, h.now.Add(15*time.Minute),
		30*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	rows, err := h.st.DB.QueryContext(ctx, `SELECT effect_kind,resource_id
		FROM project_scheduler_effects ORDER BY seq LIMIT 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]string
	for rows.Next() {
		var kind, id string
		if err := rows.Scan(&kind, &id); err != nil {
			t.Fatal(err)
		}
		got = append(got, [2]string{kind, id})
	}
	if len(got) != 2 || got[0] != [2]string{"builder_rework", "fair-rework"} ||
		got[1] != [2]string{"builder_launch", "fair-fresh"} {
		t.Fatalf("builder service order=%v", got)
	}
	var state, computeAction string
	if err := h.st.DB.QueryRowContext(ctx, `SELECT e.state,d.compute_lease_action_id
		FROM epics e JOIN epic_deliveries d ON d.epic_id=e.id WHERE e.id='fair-rework'`).
		Scan(&state, &computeAction); err != nil {
		t.Fatal(err)
	}
	if state != "launching" || computeAction != action.ActionID {
		t.Fatalf("rework compute acquisition state=%q action=%q", state, computeAction)
	}
	var ownAttention, otherAttention string
	_ = h.st.DB.QueryRowContext(ctx, `SELECT state FROM attention_items
		WHERE id='fair-rework-own'`).Scan(&ownAttention)
	_ = h.st.DB.QueryRowContext(ctx, `SELECT state FROM attention_items
		WHERE id='fair-rework-other'`).Scan(&otherAttention)
	if ownAttention != "resolved" || otherAttention != "open" {
		t.Fatalf("project-local capacity attention own=%q other=%q", ownAttention, otherAttention)
	}
	claimed, ok, err := (driver.SQLActionStore{DB: h.st.DB}).ClaimNextLifecycleAction(ctx,
		"fair-runtime", h.now.Add(16*time.Minute), time.Minute)
	if err != nil || !ok || claimed.ActionID != action.ActionID || claimed.Epoch != 1 {
		t.Fatalf("fair-prepared rework claim=%+v ok=%v err=%v", claimed, ok, err)
	}
	gate, err := (driverbridge.Projector{Store: h.st, CapacityFreshFor: 30 * time.Minute}).
		PrepareLifecycleAction(ctx, claimed, h.now.Add(16*time.Minute))
	if err != nil || !gate.Allowed {
		t.Fatalf("fair-prepared rework gate=%+v err=%v", gate, err)
	}
	var computeEpoch int64
	if err := h.st.DB.QueryRowContext(ctx, `SELECT compute_lease_action_epoch
		FROM epic_deliveries WHERE epic_id='fair-rework'`).Scan(&computeEpoch); err != nil {
		t.Fatal(err)
	}
	if computeEpoch != claimed.Epoch {
		t.Fatalf("compute epoch=%d claimed epoch=%d", computeEpoch, claimed.Epoch)
	}
}
