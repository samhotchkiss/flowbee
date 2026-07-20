package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func addAdmissionProject(t *testing.T, st *store.Store, projectID string, now time.Time) {
	t.Helper()
	if _, err := st.CreatePortfolioProject(context.Background(), store.PortfolioProject{
		ID: projectID, Name: projectID,
	}, now); err != nil {
		t.Fatal(err)
	}
}

func addAdmissionRepo(t *testing.T, st *store.Store, repoID string) {
	t.Helper()
	if err := st.RegisterRepo(context.Background(), store.Repo{
		ID: repoID, Owner: "fixture", Repo: repoID, DefaultBranch: "main", Active: true,
	}); err != nil {
		t.Fatal(err)
	}
}

func bindAdmissionRepoOnly(t *testing.T, st *store.Store, projectID, repoID string, now time.Time) {
	t.Helper()
	if err := st.AddProjectRepo(context.Background(), projectID, repoID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(context.Background(), `UPDATE project_repos SET state='paused'
		WHERE project_id='default' AND repo_id=?`, repoID); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyRepoAdmissionAmbiguityIsHeldAndVisible(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	addAdmissionRepo(t, st, "shared") // RegisterRepo creates the default mapping.
	addAdmissionProject(t, st, "mail", now)
	if err := st.AddProjectRepo(ctx, "mail", "shared", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ResolveRepoAdmissionProjectAndHold(ctx, "shared", now); !errors.Is(err, store.ErrRepoAdmissionAmbiguous) {
		t.Fatalf("ambiguous route err=%v", err)
	}
	hold, err := st.GetRepoAdmissionHold(ctx, "shared")
	if err != nil || hold.State != "pending" || len(hold.CandidateProjects) != 2 {
		t.Fatalf("hold=%+v err=%v", hold, err)
	}
	items, err := st.ListOpenAttentionForProject(ctx, "default", "", []string{"repo_admission_routing_hold"}, "shared")
	if err != nil || len(items) != 1 || !items[0].Blocking {
		t.Fatalf("visible attention=%+v err=%v", items, err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE project_repos SET state='paused'
		WHERE project_id='default' AND repo_id='shared'`); err != nil {
		t.Fatal(err)
	}
	owner, err := st.ResolveRepoAdmissionProjectAndHold(ctx, "shared", now.Add(time.Minute))
	if err != nil || owner != "mail" {
		t.Fatalf("repaired owner=%q err=%v", owner, err)
	}
	hold, _ = st.GetRepoAdmissionHold(ctx, "shared")
	items, _ = st.ListOpenAttentionForProject(ctx, "default", "", []string{"repo_admission_routing_hold"}, "shared")
	if hold.State != "resolved" || len(items) != 0 {
		t.Fatalf("resolved hold=%+v open=%+v", hold, items)
	}
}

func TestLegacyProjectScopedAdoptionIsolatesEqualGitHubNumbers(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 13, 0, 0, 0, time.UTC)
	for _, pair := range [][2]string{{"mail", "mail-repo"}, {"calendar", "calendar-repo"}} {
		addAdmissionProject(t, st, pair[0], now)
		addAdmissionRepo(t, st, pair[1])
		bindAdmissionRepoOnly(t, st, pair[0], pair[1], now)
		issueID, err := st.AdoptIssueAsBuildForProject(ctx, pair[0], pair[1], 42,
			"same issue number", "body", "base", 5, now)
		if err != nil || issueID == "" {
			t.Fatalf("adopt issue %v id=%q err=%v", pair, issueID, err)
		}
		prID, _, err := st.AdoptPRForReviewInProject(ctx, pair[0], pair[1], 77,
			"base", "head", "diff", false, false, true, false, now, now)
		if err != nil || prID == "" {
			t.Fatalf("adopt pr %v id=%q err=%v", pair, prID, err)
		}
	}
	for _, pair := range [][2]string{{"mail", "mail-repo"}, {"calendar", "calendar-repo"}} {
		for _, column := range []string{"issue_number", "pr_number"} {
			var project string
			number := 42
			if column == "pr_number" {
				number = 77
			}
			if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM jobs WHERE repo=? AND `+column+`=?`,
				pair[1], number).Scan(&project); err != nil || project != pair[0] {
				t.Fatalf("repo=%s column=%s project=%q err=%v", pair[1], column, project, err)
			}
		}
	}
}

func TestLegacyAdmissionRechecksOwnershipInsideInsertTransaction(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
	addAdmissionProject(t, st, "mail", now)
	addAdmissionRepo(t, st, "shared")
	bindAdmissionRepoOnly(t, st, "mail", "shared", now)
	if owner, err := st.ResolveRepoAdmissionProjectAndHold(ctx, "shared", now); err != nil || owner != "mail" {
		t.Fatalf("initial owner=%q err=%v", owner, err)
	}
	addAdmissionProject(t, st, "calendar", now)
	if err := st.AddProjectRepo(ctx, "calendar", "shared", now); err != nil {
		t.Fatal(err)
	}
	if id, err := st.AdoptIssueAsBuildForProject(ctx, "mail", "shared", 99, "late race", "", "base", 5, now); !errors.Is(err, store.ErrRepoAdmissionAmbiguous) || id != "" {
		t.Fatalf("runtime ownership change id=%q err=%v", id, err)
	}
	var jobs int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE repo='shared' AND issue_number=99`).Scan(&jobs)
	hold, _ := st.GetRepoAdmissionHold(ctx, "shared")
	if jobs != 0 || hold.State != "pending" {
		t.Fatalf("jobs=%d hold=%+v", jobs, hold)
	}
}

func TestV2EpicAdmissionRequiresExactProjectRepoMembership(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	addAdmissionProject(t, st, "mail", now) // makes the default carve-out multi-project and strict.
	addAdmissionRepo(t, st, "registered")
	err := st.AddEpicRun(ctx, store.EpicRun{ID: "wrong-default", ProjectID: "default",
		Repo: "unregistered", Branch: "epic/wrong-default"}, 1, now)
	if !errors.Is(err, store.ErrRepoAdmissionWrongOwner) {
		t.Fatalf("default v2 wrong-repo err=%v", err)
	}
	err = st.AddEpicRun(ctx, store.EpicRun{ID: "wrong-mail", ProjectID: "mail",
		Repo: "registered", Branch: "epic/wrong-mail"}, 1, now)
	if !errors.Is(err, store.ErrRepoAdmissionWrongOwner) {
		t.Fatalf("nondefault wrong membership err=%v", err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "registered", now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "right-mail", ProjectID: "mail",
		Repo: "registered", Branch: "epic/right-mail"}, 1, now); err != nil {
		t.Fatalf("valid shared membership: %v", err)
	}
	addAdmissionProject(t, st, "calendar", now)
	if err := st.AddProjectRepo(ctx, "calendar", "registered", now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "right-calendar", ProjectID: "calendar",
		Repo: "registered", Branch: "epic/right-calendar"}, 1, now); err != nil {
		t.Fatalf("second project sharing repo: %v", err)
	}
}

func TestSingleDefaultProjectRetainsP1UnregisteredRepoCompatibility(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 15, 30, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "p1-default", ProjectID: "default",
		Repo: "legacy-unregistered", Branch: "epic/p1-default"}, 1, now); err != nil {
		t.Fatalf("single-default P1 compatibility: %v", err)
	}
}
