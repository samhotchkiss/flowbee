package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestMigration0063BackfillsExactlyOneDeliveryRepositoryPerEpic(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0063_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}

	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, store.Repo{ID: "mail-app", Owner: "fixture", Repo: "mail-app", Active: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "mail-app", now); err != nil {
		t.Fatal(err)
	}
	stamp := now.Format(time.RFC3339Nano)
	for _, row := range []struct {
		id, projectID, repo string
	}{
		{"mail-legacy", "mail", "mail-app"},
		{"default-legacy", "default", "unregistered-legacy"},
	} {
		if _, err := st.DB.ExecContext(ctx, `INSERT INTO epics
			(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
			VALUES (?,?,?,'launching',?,?,?, ?,?)`, row.id, row.repo, "epics/"+row.id+".md",
			row.projectID, row.id, row.projectID+":"+row.id, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	body, err := os.ReadFile("migrations/0063_phase2_epic_repository_sets.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 0063: %v", err)
	}

	for _, tc := range []struct {
		id, projectID, repo string
		validated           int
	}{
		{"mail-legacy", "mail", "mail-app", 1},
		{"default-legacy", "default", "unregistered-legacy", 0},
	} {
		var projectID, repo, mode string
		var finalized, total, deliveries, validated int
		if err := st.DB.QueryRowContext(ctx, `SELECT e.project_id,e.repo,e.repository_set_mode,e.repository_set_finalized,
			COUNT(er.repo_id),SUM(er.is_delivery),SUM(er.membership_validated)
			FROM epics e JOIN epic_repositories er ON er.epic_id=e.id
			WHERE e.id=? GROUP BY e.id`, tc.id).
			Scan(&projectID, &repo, &mode, &finalized, &total, &deliveries, &validated); err != nil {
			t.Fatal(err)
		}
		if projectID != tc.projectID || repo != tc.repo || mode != "legacy" ||
			finalized != 1 || total != 1 || deliveries != 1 || validated != tc.validated {
			t.Fatalf("%s project=%q repo=%q mode=%q finalized=%d total=%d delivery=%d validated=%d",
				tc.id, projectID, repo, mode, finalized, total, deliveries, validated)
		}
	}
}
