package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestOutboxAndAuditDeriveOwningJobProject(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 21, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-build", ProjectID: "mail",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}

	action := store.OutboxRow{JobID: "mail-build", Action: store.ActionOpenPR, HeadSHA: "head-1"}
	if err := st.EnqueueOutbox(ctx, action); err != nil {
		t.Fatalf("enqueue without caller project: %v", err)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil || !ok {
		t.Fatalf("pending row ok=%t err=%v", ok, err)
	}
	if row.ProjectID != "mail" {
		t.Fatalf("outbox project=%q want mail", row.ProjectID)
	}

	// An idempotent replay derives the same owner and does not duplicate either
	// the effect or its eventual audit row.
	action.ProjectID = "mail"
	if err := st.EnqueueOutbox(ctx, action); err != nil {
		t.Fatalf("matching replay: %v", err)
	}
	spoofedReplay := action
	spoofedReplay.ProjectID = "calendar"
	if err := st.EnqueueOutbox(ctx, spoofedReplay); err == nil || !strings.Contains(err.Error(), "does not own job") {
		t.Fatalf("idempotency conflict bypassed project fence: %v", err)
	}
	if err := st.MarkOutboxSent(ctx, row.ID, "pr=42"); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkOutboxSent(ctx, row.ID, "replayed"); err != nil {
		t.Fatalf("sent replay: %v", err)
	}
	var outboxCount int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE job_id='mail-build'`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if outboxCount != 1 {
		t.Fatalf("outbox replay rows=%d want 1", outboxCount)
	}
	audit, err := st.AuditLog(ctx, "mail-build")
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].ProjectID != "mail" {
		t.Fatalf("audit=%+v want one mail row", audit)
	}
	all, err := st.AllAudit(ctx)
	if err != nil || len(all) != 1 || all[0].ProjectID != "mail" {
		t.Fatalf("all audit=%+v err=%v", all, err)
	}
}

func TestOutboxProjectSpoofAndCrossProjectMutationFailClosed(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 21, 30, 0, 0, time.UTC)
	for _, id := range []string{"mail", "calendar"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: id, Name: id}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "mail-build", ProjectID: "mail",
		Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}

	err := st.EnqueueOutbox(ctx, store.OutboxRow{JobID: "mail-build", ProjectID: "calendar",
		Action: store.ActionOpenPR, HeadSHA: "head-spoof"})
	if err == nil || !strings.Contains(err.Error(), "does not own job") {
		t.Fatalf("caller project spoof err=%v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO outbox
		(job_id,project_id,action,head_sha,status) VALUES
		('mail-build','calendar','pulls.create','head-sql-spoof','pending')`); err == nil {
		t.Fatal("database accepted cross-project outbox")
	}

	if err := st.EnqueueOutbox(ctx, store.OutboxRow{JobID: "mail-build", Action: store.ActionComment,
		HeadSHA: "comment-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO audit_log
		(job_id,project_id,action,head_sha,detail) VALUES
		('mail-build','calendar','pulls.comment','comment-1','spoof')`); err == nil {
		t.Fatal("database accepted cross-project audit row")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE outbox SET project_id='calendar'
		WHERE job_id='mail-build' AND action='pulls.comment'`); err == nil {
		t.Fatal("database allowed an outbox identity to change projects")
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET project_id='calendar' WHERE id='mail-build'`); err == nil {
		t.Fatal("database allowed a job with durable effects to change projects")
	}
}

func TestOutboxLegacyDefaultCompatibility(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 22, 0, 0, 0, time.UTC)
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: "legacy-default", Kind: job.KindBuild,
		Flow: "build", Stage: "build", Role: job.RoleEngWorker, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueOutbox(ctx, store.OutboxRow{JobID: "legacy-default",
		Action: store.ActionCreateIssue, HeadSHA: "legacy"}); err != nil {
		t.Fatal(err)
	}
	row, ok, err := st.NextPendingOutbox(ctx)
	if err != nil || !ok || row.ProjectID != "default" {
		t.Fatalf("legacy pending=%+v ok=%t err=%v", row, ok, err)
	}
	if err := st.MarkOutboxSent(ctx, row.ID, "issue=1"); err != nil {
		t.Fatal(err)
	}
	audit, err := st.AuditLog(ctx, "legacy-default")
	if err != nil || len(audit) != 1 || audit[0].ProjectID != "default" {
		t.Fatalf("legacy audit=%+v err=%v", audit, err)
	}

	// Pre-Phase-2 diagnostic/test writers sometimes emitted default-project rows
	// before a job projection existed. Keep those rows readable on upgrade.
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO outbox
		(job_id,action,head_sha,status) VALUES ('legacy-orphan','issues.create','orphan','abandoned')`); err != nil {
		t.Fatalf("legacy default orphan compatibility: %v", err)
	}
	var projectID string
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM outbox WHERE job_id='legacy-orphan'`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if projectID != "default" {
		t.Fatalf("legacy orphan project=%q", projectID)
	}
}
