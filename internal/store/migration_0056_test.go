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

func TestMigration0056BackfillsOutboxAndAuditFromOwningJob(t *testing.T) {
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
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0056_" {
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

	now := time.Date(2026, 7, 19, 22, 30, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-build", ProjectID: "mail",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	// Reproduce the pre-0056 writer: both inserts omit project_id and receive the
	// compatibility default even though their job belongs to mail.
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO outbox
		(job_id,action,head_sha,status) VALUES ('mail-build','pulls.create','head-old','sent')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO audit_log
		(job_id,action,head_sha,detail) VALUES ('mail-build','pulls.create','head-old','pr=42')`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO outbox
		(job_id,action,head_sha,status) VALUES ('legacy-orphan','issues.create','','abandoned')`); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile("migrations/0056_phase2_outbox_project_attribution.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 0056: %v", err)
	}
	for _, table := range []string{"outbox", "audit_log"} {
		var projectID string
		if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM `+table+` WHERE job_id='mail-build'`).Scan(&projectID); err != nil {
			t.Fatal(err)
		}
		if projectID != "mail" {
			t.Fatalf("%s project=%q want mail", table, projectID)
		}
	}
	var orphanProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM outbox WHERE job_id='legacy-orphan'`).Scan(&orphanProject); err != nil {
		t.Fatal(err)
	}
	if orphanProject != "default" {
		t.Fatalf("legacy orphan project=%q want default", orphanProject)
	}
}
