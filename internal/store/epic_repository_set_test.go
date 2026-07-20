package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func addEpicRepositoryProject(t *testing.T, st *store.Store, projectID string, repos []string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
		t.Fatal(err)
	}
	for _, repoID := range repos {
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "fixture", Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEpicRepositorySetAdmitsManyWithExactlyOneDelivery(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	addEpicRepositoryProject(t, st, "alpha", []string{"alpha-app", "alpha-docs", "alpha-extra"}, now)
	epic := store.EpicRun{ID: "alpha-multi", ProjectID: "alpha", Slug: "multi",
		AdmissionKey: "alpha:multi:v1", ContractHash: "sha256:contract-v1",
		Repositories: []string{"alpha-docs", "alpha-app"}, DeliveryRepo: "alpha-app",
		Branch: "epic/alpha/multi", Scope: []string{"internal/**"}}
	if err := st.AddEpicRun(ctx, epic, 1, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEpicRun(ctx, epic.ID)
	if err != nil || got.Repo != "alpha-app" || got.DeliveryRepo != "alpha-app" ||
		got.RepositorySetMode != "explicit" || !equalStrings(got.Repositories, []string{"alpha-app", "alpha-docs"}) {
		t.Fatalf("epic=%+v err=%v", got, err)
	}
	var total, deliveries, validated int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*),SUM(is_delivery),SUM(membership_validated)
		FROM epic_repositories WHERE epic_id=?`, epic.ID).Scan(&total, &deliveries, &validated); err != nil {
		t.Fatal(err)
	}
	if total != 2 || deliveries != 1 || validated != 2 {
		t.Fatalf("repository rows total=%d deliveries=%d validated=%d", total, deliveries, validated)
	}
	var deliveryProjection string
	if err := st.DB.QueryRowContext(ctx, `SELECT delivery_repo FROM epic_deliveries WHERE epic_id=?`, epic.ID).Scan(&deliveryProjection); err != nil {
		t.Fatal(err)
	}
	if deliveryProjection != "alpha-app" {
		t.Fatalf("delivery projection=%q", deliveryProjection)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_repositories SET repo_id='other'
		WHERE epic_id=? AND repo_id='alpha-docs'`, epic.ID); err == nil {
		t.Fatal("admitted repository set mutated")
	}
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM epic_repositories
		WHERE epic_id=? AND repo_id='alpha-docs'`, epic.ID); err == nil {
		t.Fatal("admitted repository set member deleted")
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_repositories
		(epic_id,project_id,repo_id,is_delivery,membership_validated,created_at)
		VALUES (?,?,?,0,1,?)`, epic.ID, "alpha", "alpha-extra", now.Format(time.RFC3339Nano)); err == nil {
		t.Fatal("admitted repository set accepted a late member")
	}
}

func TestEpicRepositorySetRejectsUnownedMemberAndChangedLostAckReplay(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	addEpicRepositoryProject(t, st, "alpha", []string{"alpha-app", "alpha-docs"}, now)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "foreign", Owner: "fixture", Repo: "foreign", Active: true}); err != nil {
		t.Fatal(err)
	}
	bad := store.EpicRun{ID: "bad", ProjectID: "alpha", Repositories: []string{"alpha-app", "foreign"},
		DeliveryRepo: "alpha-app", Repo: "alpha-app", Branch: "epic/alpha/bad"}
	if err := st.AddEpicRun(ctx, bad, 1, now); !errors.Is(err, store.ErrRepoAdmissionWrongOwner) {
		t.Fatalf("unowned member err=%v", err)
	}
	var badCount int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE id='bad'`).Scan(&badCount)
	if badCount != 0 {
		t.Fatalf("unowned repository admitted %d epic", badCount)
	}

	epic := store.EpicRun{ID: "lost-ack", ProjectID: "alpha", Slug: "lost-ack",
		AdmissionKey: "alpha:lost-ack:v1", ContractHash: "sha256:same-contract",
		Repositories: []string{"alpha-app", "alpha-docs"}, DeliveryRepo: "alpha-app",
		Branch: "epic/alpha/lost-ack"}
	if err := st.AddEpicRun(ctx, epic, 1, now); err != nil {
		t.Fatal(err)
	}
	replay := epic
	replay.ID = "different-generated-id"
	replay.Repositories = []string{"alpha-docs", "alpha-app"}
	if err := st.AddEpicRun(ctx, replay, 1, now.Add(time.Minute)); err != nil {
		t.Fatalf("exact lost-ack replay: %v", err)
	}
	var count int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epics WHERE admission_key=?`, epic.AdmissionKey).Scan(&count)
	if count != 1 {
		t.Fatalf("lost-ack replay admitted %d epics", count)
	}
	changed := replay
	changed.Repositories = []string{"alpha-app"}
	if err := st.AddEpicRun(ctx, changed, 1, now.Add(2*time.Minute)); !errors.Is(err, store.ErrEpicAdmissionConflict) {
		t.Fatalf("changed repository replay err=%v", err)
	}
}

func TestEpicRepositorySetRejectsDuplicateOrMissingDelivery(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 15, 0, 0, time.UTC)
	addEpicRepositoryProject(t, st, "alpha", []string{"alpha-app", "alpha-docs"}, now)
	for name, epic := range map[string]store.EpicRun{
		"duplicate": {
			ID: "duplicate", ProjectID: "alpha", Repositories: []string{"alpha-app", "alpha-app"},
			DeliveryRepo: "alpha-app", Branch: "epic/alpha/duplicate",
		},
		"missing delivery member": {
			ID: "missing", ProjectID: "alpha", Repositories: []string{"alpha-docs"},
			DeliveryRepo: "alpha-app", Branch: "epic/alpha/missing",
		},
	} {
		if err := st.AddEpicRun(ctx, epic, 1, now); !errors.Is(err, store.ErrEpicRepositorySetInvalid) {
			t.Fatalf("%s err=%v", name, err)
		}
	}
}

func TestEpicRepositorySetAllowsSharedRepoOnlyThroughEachProjectMembership(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 20, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "shared", Owner: "fixture", Repo: "shared", Active: true}); err != nil {
		t.Fatal(err)
	}
	for _, projectID := range []string{"alpha", "beta"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.AddProjectRepo(ctx, "alpha", "shared", now); err != nil {
		t.Fatal(err)
	}
	alpha := store.EpicRun{ID: "alpha-shared", ProjectID: "alpha", Repositories: []string{"shared"},
		DeliveryRepo: "shared", Branch: "epic/alpha/shared"}
	if err := st.AddEpicRun(ctx, alpha, 1, now); err != nil {
		t.Fatalf("alpha shared membership: %v", err)
	}
	beta := store.EpicRun{ID: "beta-shared", ProjectID: "beta", Repositories: []string{"shared"},
		DeliveryRepo: "shared", Branch: "epic/beta/shared"}
	if err := st.AddEpicRun(ctx, beta, 1, now); !errors.Is(err, store.ErrRepoAdmissionWrongOwner) {
		t.Fatalf("beta without membership err=%v", err)
	}
	if err := st.AddProjectRepo(ctx, "beta", "shared", now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEpicRun(ctx, beta, 1, now); err != nil {
		t.Fatalf("beta explicit shared membership: %v", err)
	}
}

func TestEpicRepositorySetPreservesDefaultLegacyRepoProjection(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 20, 0, 30, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "legacy-default", ProjectID: "default",
		Repo: "legacy-unregistered", Branch: "epic/legacy-default"}, 1, now); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetEpicRun(ctx, "legacy-default")
	if err != nil || got.Repo != "legacy-unregistered" || got.DeliveryRepo != got.Repo ||
		got.RepositorySetMode != "legacy" || !equalStrings(got.Repositories, []string{"legacy-unregistered"}) {
		t.Fatalf("legacy epic=%+v err=%v", got, err)
	}
	var validated int
	if err := st.DB.QueryRowContext(ctx, `SELECT membership_validated FROM epic_repositories
		WHERE epic_id='legacy-default' AND is_delivery=1`).Scan(&validated); err != nil || validated != 0 {
		t.Fatalf("legacy membership=%d err=%v", validated, err)
	}
	if err := st.DeleteEpicRun(ctx, "legacy-default"); err != nil {
		t.Fatalf("pre-launch rollback must cascade repository set: %v", err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
