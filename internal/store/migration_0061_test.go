package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestMigration0061BackfillsArtifactsAndCostEventsFromParents(t *testing.T) {
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
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0061_" {
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

	now := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)
	seedArtifactCostProject(t, st, "mail", "repo-mail", now)
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epics
		(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
		VALUES ('mail-epic','repo-mail','epics/mail.md','launching','mail','mail','mail:epic',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_artifacts
		(epic_id,project_id,repo,branch,created_at,updated_at)
		VALUES ('mail-epic','mail','repo-mail','epic/mail',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-job", ProjectID: "mail",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epics
		(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
		VALUES ('legacy-epic','','epics/legacy.md','launching','default','legacy','legacy:epic',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_artifacts
		(epic_id,project_id,repo,branch,created_at,updated_at)
		VALUES ('legacy-epic','default','','epic/legacy',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "legacy-job", ProjectID: "default",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	// Reproduce the compatibility-default attribution before 0061.
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_artifacts SET project_id='default' WHERE epic_id='mail-epic'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE job_events SET project_id='default' WHERE job_id='mail-job'`); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("migrations/0061_phase2_artifact_cost_attribution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 0061: %v", err)
	}
	for table, idColumn := range map[string]string{"epic_artifacts": "epic_id", "job_events": "job_id"} {
		var projectID string
		if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE `+idColumn+`=? LIMIT 1`,
			map[string]string{"epic_artifacts": "mail-epic", "job_events": "mail-job"}[table]).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		if projectID != "mail" {
			t.Fatalf("%s project=%q want mail", table, projectID)
		}
	}
	for table, idColumn := range map[string]string{"epic_artifacts": "epic_id", "job_events": "job_id"} {
		var projectID string
		if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE `+idColumn+`=? LIMIT 1`,
			map[string]string{"epic_artifacts": "legacy-epic", "job_events": "legacy-job"}[table]).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		if projectID != "default" {
			t.Fatalf("legacy %s project=%q want default", table, projectID)
		}
	}
}
