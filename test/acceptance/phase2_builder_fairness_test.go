package acceptance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestPhase2BuilderFairnessIsAuthoritativeDurableAndRestartSafe(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "builder-fair.db")
	now := time.Date(2026, 7, 20, 1, 0, 0, 0, time.UTC)
	st := openPhase2Store(t, path)
	st.EnableDriverControlOrigin = true
	for _, projectID := range []string{"alpha", "beta"} {
		createPhase2Project(t, st, projectID, 1, now)
	}
	seat := store.Seat{Box: "builder-host", AgentFamily: "codex",
		CodexHome: "/codex/acceptance", AccountKey: "builder-account",
		Health: store.SeatReady, MaxConcurrent: 2}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	seat.ID = seat.ComposeID()
	if err := st.BindCapacitySeatIdentity(ctx, store.CapacitySeatIdentity{
		SeatID: seat.ID, HostID: seat.Box, AccountKey: "builder-account",
		CredentialLineage: "builder-lineage", ReservePct: 10, AccountMaximum: 2,
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.CommitCapacityGeneration(ctx, store.CapacityGeneration{
		ID: "builder-generation", StartedAt: now, ExpectedSeatIDs: []string{seat.ID},
		Observations: []store.CapacitySeatObservation{{
			ObservationID: "builder-observation", SeatID: seat.ID, HostID: seat.Box,
			Provider: "codex", AccountKey: "builder-account", CredentialLineage: "builder-lineage",
			CollectorID: "collector", Source: "live_app_server", TrustState: "verified",
			IntegrityState: "verified", FetchedAt: now, RawSHA256: "sha256:acceptance",
			AdapterVersion: "acceptance/v1", Windows: []capacity.RouteWindow{{
				Kind: "weekly", Applicable: true, Known: true, Percent: 10,
			}},
		}},
	}, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,tmux_server_domain_id,
		 tmux_server_ownership,state,created_at,updated_at)
		VALUES ('acceptance-driver','builder-host','builder-store','boot','flowbee',
		'managed_dedicated','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_observation_cursors
		(store_id,instance_ref,cursor,high_store_seq,uncertainty_epoch,last_event_id,active,updated_at)
		VALUES ('builder-store','acceptance-driver','tdc2.acceptance',1,0,'baseline',1,?)`, stamp); err != nil {
		t.Fatal(err)
	}
	for _, projectID := range []string{"alpha", "beta"} {
		if err := st.UpsertBuilderDriverTarget(ctx, store.BuilderDriverTarget{
			ProjectID: projectID, SeatID: seat.ID, InstanceRef: "acceptance-driver",
			TmuxServerDomainID: "flowbee", TmuxServerInstanceID: "builder-server",
			ProfileID: "codex-builder", WorkspaceRootID: "workspace",
			WorkspaceRelativeBase: "repos", Enabled: true,
		}, now); err != nil {
			t.Fatal(err)
		}
		repoID := projectID + "-repo"
		if err := st.RegisterRepo(ctx, store.Repo{ID: repoID, Owner: "fixture",
			Repo: repoID, Active: true}); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, projectID, repoID, now); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE project_repos SET state='paused'
			WHERE project_id='default' AND repo_id=?`, repoID); err != nil {
			t.Fatal(err)
		}
		if err := st.AddEpicRun(ctx, store.EpicRun{ID: projectID + "-epic",
			ProjectID: projectID, Repo: repoID, Branch: "epic/" + projectID,
			Slug: projectID, Title: projectID, FilePath: "epics/" + projectID + ".md",
		}, 1, now); err != nil {
			t.Fatal(err)
		}
	}
	// Beta is the continuously-eligible quiet project. The authoritative pass must
	// physically allocate it first even though alpha sorts first lexically.
	if _, err := st.DB.ExecContext(ctx, `UPDATE epic_deliveries SET created_at=?
		WHERE epic_id='beta-epic'`, now.Add(-16*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	st.EnableCapacityV2 = true
	if rep, err := st.ReconcileBuilderLaunches(ctx, now, 5*time.Minute, "codex", 5); err != nil || rep.ActionsCreated != 2 {
		t.Fatalf("authoritative builder pass=%+v err=%v", rep, err)
	}
	var firstProject string
	var forced int
	if err := st.DB.QueryRowContext(ctx, `SELECT project_id,forced_by_age
		FROM project_scheduler_effects ORDER BY seq LIMIT 1`).Scan(&firstProject, &forced); err != nil {
		t.Fatal(err)
	}
	if firstProject != "beta" || forced != 1 {
		t.Fatalf("first fair effect project=%q forced=%d", firstProject, forced)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openPhase2Store(t, path)
	defer st.Close()
	st.EnableCapacityV2 = true
	st.EnableDriverControlOrigin = true
	if _, err := st.ReconcileBuilderLaunches(ctx, now.Add(time.Minute), 5*time.Minute, "codex", 5); err != nil {
		t.Fatal(err)
	}
	var actions, effects int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM epic_actions
		WHERE kind='builder_launch'`).Scan(&actions)
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_scheduler_effects
		WHERE effect_kind='builder_launch'`).Scan(&effects)
	if actions != 2 || effects != 2 {
		t.Fatalf("restart replay actions=%d effects=%d", actions, effects)
	}
}
