package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestProjectDashboardFoldsResidencyHumanDemandAndOldestBlocker(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{
		ID: "mail", Name: "Mail", Priority: 10, SchedulerWeight: 3, ConcurrencyCap: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	for _, epic := range []store.EpicRun{
		{ID: "mail-active", ProjectID: "mail", Repo: "russ", Title: "Active", Branch: "epic/mail-active"},
		{ID: "mail-parked", ProjectID: "mail", Repo: "russ", Title: "Parked", Branch: "epic/mail-parked"},
	} {
		if err := st.AddEpicRun(ctx, epic, 2, now); err != nil {
			t.Fatal(err)
		}
		if err := st.MarkEpicLaunched(ctx, epic.ID, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.UpsertEpicStatus(ctx, "mail-parked", epicspec.StatusBlock{
		UpdatedRaw: now.Add(time.Minute).Format(time.RFC3339), State: "done",
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: "mail-blocker", EpicID: "mail-active", Kind: "review_dispatch_overdue",
		DedupKey: "mail:review", Priority: 1, Blocking: true, Detail: "review was not dispatched",
	}, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("mail-plan"))
	if _, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "mail-decision", ProjectID: "mail", Kind: workintent.DecisionQuestion,
		Title: "Choose behavior", Prompt: "Which behavior?", Options: json.RawMessage(`[]`),
		ResponseSchema: json.RawMessage(`{}`), ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseAnswer},
		RequestedBy: "orchestrator:mail", RouteTo: "human:sam", SubjectArtifactRef: "artifact://mail-plan",
		SubjectVersion: 1, SubjectSHA256: "sha256:" + hex.EncodeToString(hash[:]),
	}, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ProjectDashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mail *store.ProjectDashboardRow
	for i := range rows {
		if rows[i].Project.ID == "mail" {
			mail = &rows[i]
		}
	}
	if mail == nil {
		t.Fatalf("mail missing from dashboard: %+v", rows)
	}
	if mail.ActiveEpics != 1 || mail.ParkedEpics != 1 || mail.NeedsYou != 1 {
		t.Fatalf("project counts=%+v", *mail)
	}
	if mail.OldestBlocker != "review was not dispatched" || mail.BlockerKind != "review_dispatch_overdue" || mail.BlockedSince.IsZero() {
		t.Fatalf("oldest blocker=%+v", *mail)
	}
}
