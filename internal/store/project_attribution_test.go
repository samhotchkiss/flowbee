package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// Legacy child writers are intentionally still accepted during the Phase-2
// compatibility window, but their durable rows must inherit the parent's real
// project rather than silently falling back to the default project.
func TestPhase2DerivedChildRecordsInheritProjectOwnership(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-job", Kind: job.KindBuild,
		Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='mail' WHERE id='mail-job'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO leases
		(lease_id,job_id,lease_epoch,identity,ttl_s,deadline) VALUES
		('mail-lease','mail-job',1,'builder',60,?)`, now.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var leaseProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM leases WHERE lease_id='mail-lease'`).Scan(&leaseProject); err != nil {
		t.Fatal(err)
	}
	if leaseProject != "mail" {
		t.Fatalf("legacy lease writer silently misattributed project=%q", leaseProject)
	}

	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "mail-epic", ProjectID: "mail", Slug: "auth",
		Repo: "mail", FilePath: "epics/auth.md", Title: "Auth", Branch: "epic/mail/auth"}, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,kind,epic_id,dedup_key,state,created_at,updated_at,first_seen_at,last_seen_at)
		VALUES ('mail-attention','needs_input','mail-epic','mail-attention','open',?,?,?,?)`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var attentionProject string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM attention_items WHERE id='mail-attention'`).Scan(&attentionProject); err != nil {
		t.Fatal(err)
	}
	if attentionProject != "mail" {
		t.Fatalf("legacy attention writer silently misattributed project=%q", attentionProject)
	}
}
