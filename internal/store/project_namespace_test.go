package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestPhase2EpicBranchNamespacePreservesDefaultAndSeparatesProjects(t *testing.T) {
	if got := epicBranchForProject("default", "auth"); got != "epic/auth" {
		t.Fatalf("default compatibility branch=%q", got)
	}
	a := epicBranchForProject("mail", "auth")
	b := epicBranchForProject("calendar", "auth")
	if a != "epic/mail/auth" || b != "epic/calendar/auth" || a == b {
		t.Fatalf("project branches collided: %q %q", a, b)
	}
}

func TestPhase2EpicSessionNamespaceAlwaysQualifiesProject(t *testing.T) {
	if got := epicSessionNameForProject("default", "auth"); got != "flowbee-worker-codex-default-auth" {
		t.Fatalf("default-qualified session=%q", got)
	}
	a := epicSessionNameForProject("mail", "auth")
	b := epicSessionNameForProject("calendar", "auth")
	if a != "flowbee-worker-codex-mail-auth" || b != "flowbee-worker-codex-calendar-auth" || a == b {
		t.Fatalf("project sessions collided: %q %q", a, b)
	}
}

func TestPhase2TwoProjectsMayAdmitTheSameHumanSlugWithoutSharingAuthority(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "flowbee.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	for _, projectID := range []string{"mail", "calendar"} {
		if _, err := st.CreatePortfolioProject(ctx, PortfolioProject{ID: projectID, Name: projectID}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.RegisterRepo(ctx, Repo{ID: projectID, Owner: "fixture", Repo: projectID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, projectID, now); err != nil {
			t.Fatal(err)
		}
		epicID := "epic-" + projectID + "-auth"
		if err := st.AddEpicRun(ctx, EpicRun{ID: epicID, ProjectID: projectID, Slug: "auth",
			AdmissionKey: "intent:" + projectID + ":1", Repo: projectID,
			FilePath: "epics/auth.md", Title: "Auth", Branch: epicBranchForProject(projectID, "auth")}, 1, now); err != nil {
			t.Fatalf("admit %s: %v", projectID, err)
		}
	}
	mail, err := st.GetEpicRun(ctx, "epic-mail-auth")
	if err != nil {
		t.Fatal(err)
	}
	calendar, err := st.GetEpicRun(ctx, "epic-calendar-auth")
	if err != nil {
		t.Fatal(err)
	}
	if mail.ProjectID != "mail" || calendar.ProjectID != "calendar" || mail.Branch == calendar.Branch || mail.ID == calendar.ID {
		t.Fatalf("cross-project authority collision: mail=%+v calendar=%+v", mail, calendar)
	}
	var deliveries int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_deliveries d
		JOIN epics e ON e.id=d.epic_id
		WHERE e.slug='auth' AND d.project_id IN ('mail','calendar')`).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	if deliveries != 2 {
		t.Fatalf("same-slug delivery count=%d", deliveries)
	}
}
