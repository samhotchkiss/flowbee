package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestPhase2ProjectCommandsArePayloadBoundAndAtomic(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	for _, repo := range []store.Repo{
		{ID: "mail-repo", Owner: "acme", Repo: "mail", Active: true},
		{ID: "calendar-repo", Owner: "acme", Repo: "calendar", Active: true},
	} {
		if err := st.RegisterRepo(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}

	create := store.PortfolioProject{ID: "mail", Name: "Mail", Priority: 20, SchedulerWeight: 3}
	first, err := st.CreatePortfolioProjectCommand(ctx, create, "create-mail", now)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := st.CreatePortfolioProjectCommand(ctx, create, "create-mail", now.Add(time.Minute))
	if err != nil || replayed.ID != first.ID || replayed.StateVersion != first.StateVersion {
		t.Fatalf("create replay=%+v err=%v", replayed, err)
	}
	if _, err := st.CreatePortfolioProjectCommand(ctx,
		store.PortfolioProject{ID: "calendar", Name: "Calendar"}, "create-mail", now); !errors.Is(err, store.ErrProjectCommandConflict) {
		t.Fatalf("changed create payload err=%v", err)
	}

	paused, err := st.SetPortfolioProjectStateCommand(ctx, "mail", "paused", "maintenance", 1, "pause-mail", now)
	if err != nil || paused.StateVersion != 2 {
		t.Fatalf("pause=%+v err=%v", paused, err)
	}
	replayedPause, err := st.SetPortfolioProjectStateCommand(ctx, "mail", "paused", "maintenance", 1, "pause-mail", now.Add(time.Minute))
	if err != nil || replayedPause.StateVersion != 2 {
		t.Fatalf("pause replay mutated twice: project=%+v err=%v", replayedPause, err)
	}
	if _, err := st.SetPortfolioProjectStateCommand(ctx, "mail", "paused", "different", 1, "pause-mail", now); !errors.Is(err, store.ErrProjectCommandConflict) {
		t.Fatalf("changed state payload err=%v", err)
	}

	// The command insert and mutation share one transaction: a stale precondition
	// rolls back the command identity, so a corrected retry may reuse its key.
	if _, err := st.SetPortfolioProjectStateCommand(ctx, "mail", "active", "", 1, "resume-mail", now); !errors.Is(err, store.ErrProjectConflict) {
		t.Fatalf("stale state precondition err=%v", err)
	}
	var staleCommands int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_commands
		WHERE scope_id='mail' AND idempotency_key='resume-mail'`).Scan(&staleCommands); err != nil || staleCommands != 0 {
		t.Fatalf("failed mutation persisted command count=%d err=%v", staleCommands, err)
	}
	if _, err := st.SetPortfolioProjectStateCommand(ctx, "mail", "active", "", 2, "resume-mail", now); err != nil {
		t.Fatalf("corrected command after rollback: %v", err)
	}

	if err := st.AddProjectRepoCommand(ctx, "mail", "mail-repo", "attach-repo", now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepoCommand(ctx, "mail", "mail-repo", "attach-repo", now.Add(time.Minute)); err != nil {
		t.Fatalf("repo replay: %v", err)
	}
	if err := st.AddProjectRepoCommand(ctx, "mail", "calendar-repo", "attach-repo", now); !errors.Is(err, store.ErrProjectCommandConflict) {
		t.Fatalf("changed repo payload err=%v", err)
	}

	route := store.ProjectActorRoute{ProjectID: "mail", Role: store.DriverInteractorRole, ActorID: "interactor-v1"}
	actor, err := st.RegisterProjectActorCommand(ctx, route, "bind-interactor", now)
	if err != nil {
		t.Fatal(err)
	}
	replayedActor, err := st.RegisterProjectActorCommand(ctx, route, "bind-interactor", now.Add(time.Minute))
	if err != nil || replayedActor.StateVersion != actor.StateVersion {
		t.Fatalf("actor replay mutated twice: actor=%+v err=%v", replayedActor, err)
	}
	route.ActorID = "interactor-v2"
	if _, err := st.RegisterProjectActorCommand(ctx, route, "bind-interactor", now); !errors.Is(err, store.ErrProjectCommandConflict) {
		t.Fatalf("changed actor payload err=%v", err)
	}
}

func TestPhase2ProjectCommandRollsBackWhenDomainMutationFails(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepoCommand(ctx, "mail", "missing-repo", "attach-missing", now); err == nil {
		t.Fatal("foreign-key failure unexpectedly succeeded")
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_commands
		WHERE scope_id='mail' AND idempotency_key='attach-missing'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed mutation persisted command count=%d err=%v", count, err)
	}
}
