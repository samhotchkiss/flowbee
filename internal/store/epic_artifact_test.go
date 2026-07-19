package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestEpicArtifactRealGreenEntersDurableReviewDispatchState(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-green", Repo: "repo", Branch: "epic/green", BuilderModelFamily: "codex"}, 1, now); err != nil {
		t.Fatal(err)
	}
	fact := store.EpicArtifactFact{EpicID: "epic-green", Repo: "repo", Branch: "epic/green", PRNumber: 42, PROpen: true, HeadSHA: "head", BaseSHA: "base", CIState: "green", CIHasRealSuccess: true, RequiredChecksPresentPassed: true}
	if err := st.ObserveEpicArtifactFact(ctx, fact, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var state, ci, greenAt string
	var version int
	if err := st.DB.QueryRowContext(ctx, `SELECT state,ci_state,ci_green_observed_at,artifact_version FROM epic_deliveries WHERE epic_id='epic-green'`).Scan(&state, &ci, &greenAt, &version); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_review_dispatch" || ci != "green" || greenAt == "" || version != 1 {
		t.Fatalf("state=%s ci=%s green_at=%s version=%d", state, ci, greenAt, version)
	}
}

func TestEpicArtifactNeverTreatsGreenByAbsenceAsGreen(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-skipped", Repo: "repo", Branch: "epic/skipped"}, 1, now); err != nil {
		t.Fatal(err)
	}
	fact := store.EpicArtifactFact{EpicID: "epic-skipped", Repo: "repo", Branch: "epic/skipped", PRNumber: 43, PROpen: true, HeadSHA: "head", BaseSHA: "base", CIState: "green", RequiredChecksPresentPassed: true}
	if err := st.ObserveEpicArtifactFact(ctx, fact, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var state, ci string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,ci_state FROM epic_deliveries WHERE epic_id='epic-skipped'`).Scan(&state, &ci); err != nil {
		t.Fatal(err)
	}
	if state != "awaiting_ci" || ci == "green" {
		t.Fatalf("green-by-absence accepted: state=%s ci=%s", state, ci)
	}
}

func TestEpicArtifactHeadAdvanceCancelsOldEffectAndRequiresFreshCI(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 2, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "epic-move", Repo: "repo", Branch: "epic/move"}, 1, now); err != nil {
		t.Fatal(err)
	}
	green := store.EpicArtifactFact{EpicID: "epic-move", Repo: "repo", Branch: "epic/move", PRNumber: 44, PROpen: true, HeadSHA: "h1", BaseSHA: "b1", CIState: "green", CIHasRealSuccess: true, RequiredChecksPresentPassed: true}
	if err := st.ObserveEpicArtifactFact(ctx, green, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,state,dedup_key,payload_json,payload_sha256,head_sha,base_sha,created_at,updated_at)
		VALUES ('old','default','epic-move','review_dispatch','pending','old-key','{}','hash','h1','b1',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	moved := green
	moved.HeadSHA, moved.CIState, moved.CIHasRealSuccess, moved.RequiredChecksPresentPassed = "h2", "pending", false, false
	if err := st.ObserveEpicArtifactFact(ctx, moved, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var deliveryState, actionState, verdict string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,verdict FROM epic_deliveries WHERE epic_id='epic-move'`).Scan(&deliveryState, &verdict); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT state FROM epic_actions WHERE id='old'`).Scan(&actionState); err != nil {
		t.Fatal(err)
	}
	if deliveryState != "awaiting_ci" || actionState != "cancelled_superseded" || verdict != "" {
		t.Fatalf("delivery=%s action=%s verdict=%s", deliveryState, actionState, verdict)
	}
}
