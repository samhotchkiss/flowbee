package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func createAttentionProject(t *testing.T, st *store.Store, id string, now time.Time) {
	t.Helper()
	if _, err := st.CreatePortfolioProject(context.Background(), store.PortfolioProject{ID: id, Name: id}, now); err != nil {
		t.Fatal(err)
	}
}

func seedAttentionEpic(t *testing.T, st *store.Store, projectID, epicID string, now time.Time) {
	t.Helper()
	repoID := projectID + "-repo"
	if err := st.RegisterRepo(context.Background(), store.Repo{ID: repoID, Owner: "fixture", Repo: repoID, Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(context.Background(), projectID, repoID, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(context.Background(), `INSERT INTO epics
		(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
		VALUES (?,?,?,'running',?,?,?, ?,?)`, epicID, repoID, "epics/shared.md",
		projectID, "shared", "admit-"+epicID, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionDedupAndOperationsAreProjectIsolated(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC)
	for _, projectID := range []string{"mail", "calendar"} {
		createAttentionProject(t, st, projectID, now)
		seedAttentionEpic(t, st, projectID, projectID+"-epic", now)
	}

	const sharedDedup = "same-human-condition"
	for _, tc := range []struct{ projectID, epicID, itemID string }{
		{"mail", "mail-epic", "mail-attention"},
		{"calendar", "calendar-epic", "calendar-attention"},
	} {
		created, id, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
			ID: tc.itemID, EpicID: tc.epicID, Kind: "needs_input", DedupKey: sharedDedup,
		}, now)
		if err != nil || !created || id != tc.itemID {
			t.Fatalf("upsert %s created=%t id=%q err=%v", tc.projectID, created, id, err)
		}
		got, err := st.GetAttentionItemForProject(ctx, tc.projectID, tc.itemID)
		if err != nil || got.ProjectID != tc.projectID {
			t.Fatalf("get %s=%+v err=%v", tc.projectID, got, err)
		}
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items
		WHERE dedup_key=? AND state='open'`, sharedDedup).Scan(&count); err != nil || count != 2 {
		t.Fatalf("project-local active dedup rows=%d err=%v", count, err)
	}
	if _, err := st.GetAttentionItemForProject(ctx, "mail", "calendar-attention"); !errors.Is(err, store.ErrAttentionNotFound) {
		t.Fatalf("mail read calendar item err=%v", err)
	}

	reg := registerMaster(t, st, ctx, "project-master", now)
	leased, err := st.LeaseAttentionForProject(ctx, "mail", reg.MasterID, reg.Epoch, 5, nil, time.Minute, now.Add(time.Minute))
	if err != nil || len(leased) != 1 || leased[0].ProjectID != "mail" {
		t.Fatalf("mail lease=%+v err=%v", leased, err)
	}
	calendar, err := st.GetAttentionItemForProject(ctx, "calendar", "calendar-attention")
	if err != nil || calendar.State != "open" {
		t.Fatalf("calendar item was touched by mail lease: %+v err=%v", calendar, err)
	}
	if err := st.ResolveAttentionForProject(ctx, "mail", "calendar-attention", "dismissed", now.Add(2*time.Minute)); !errors.Is(err, store.ErrAttentionNotFound) {
		t.Fatalf("mail resolved calendar attention err=%v", err)
	}

	if _, err := st.DB.ExecContext(ctx, `UPDATE attention_items SET state='awaiting_ack'
		WHERE project_id='calendar' AND id='calendar-attention'`); err != nil {
		t.Fatal(err)
	}
	if err := st.AckAttentionForProject(ctx, "mail", "calendar-attention", now.Add(3*time.Minute)); !errors.Is(err, store.ErrAttentionNotFound) {
		t.Fatalf("mail acked calendar attention err=%v", err)
	}
	calendar, _ = st.GetAttentionItemForProject(ctx, "calendar", "calendar-attention")
	if calendar.State != "awaiting_ack" {
		t.Fatalf("cross-project ack changed calendar state=%q", calendar.State)
	}

	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: "spoof", ProjectID: "mail", EpicID: "calendar-epic", Kind: "needs_input", DedupKey: "spoof",
	}, now); !errors.Is(err, store.ErrAttentionProject) {
		t.Fatalf("caller project overrode epic owner err=%v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE attention_items SET project_id='calendar'
		WHERE project_id='mail' AND id='mail-attention'`); err == nil {
		t.Fatal("database allowed attention ownership mutation")
	}
}

func TestEpiclessAttentionProducersPersistOwningProject(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)
	createAttentionProject(t, st, "mail", now)

	req := store.CapacityPoolRequirement{ProjectID: "mail", Pool: "review", Provider: "codex", QueuedWork: 1}
	if _, err := st.ReconcileCapacityPools(ctx, []store.CapacityPoolRequirement{req}, now, time.Minute, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	var capacityProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM attention_items
		WHERE kind='capacity_pool_exhausted'`).Scan(&capacityProject); err != nil {
		t.Fatal(err)
	}
	if capacityProject != "mail" {
		t.Fatalf("capacity attention project=%q", capacityProject)
	}

	intent, err := st.CreateWorkIntent(ctx, store.CreateWorkIntentInput{
		ProjectID: "mail", SourceMessageID: "mail-message", SourceMessageVersion: 1,
		InteractorIncarnationID: "mail-interactor", Title: "Mail intent",
		ArtifactRef: "artifact://mail/intent", ArtifactSHA256: workIntentSHA, IntentVersion: 1,
		DefinitionComplete: true, OwnerActorID: "mail-interactor",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReconcileWorkIntents(ctx, now.Add(2*time.Minute), 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	var intentProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM attention_items
		WHERE kind='work_intent_promotion_stalled' AND dedup_key=?`,
		"work_intent_promotion_stalled:"+intent.ID).Scan(&intentProject); err != nil {
		t.Fatal(err)
	}
	if intentProject != "mail" {
		t.Fatalf("work-intent attention project=%q", intentProject)
	}
}
