package multirepo_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/multirepo"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestManagerLegacyRepoOriginFailsClosedAndPersistsAmbiguousRoute(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "shared", Owner: "fixture", Repo: "shared", Active: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: "mail", Name: "Mail"}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.AddProjectRepo(ctx, "mail", "shared", now); err != nil {
		t.Fatal(err)
	}
	fake := gh.NewFake()
	_, err := multirepo.New(ctx, st, clock.NewFake(now), nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if !errors.Is(err, store.ErrRepoAdmissionAmbiguous) {
		t.Fatalf("manager ambiguity err=%v", err)
	}
	hold, holdErr := st.GetRepoAdmissionHold(ctx, "shared")
	if holdErr != nil || hold.State != "pending" {
		t.Fatalf("durable hold=%+v err=%v", hold, holdErr)
	}
}

func TestManagerV2AllowsSharedRepoBecauseEpicCarriesProjectAuthority(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	st.EnableEpicReviewHandoffV2 = true
	now := time.Date(2026, 7, 19, 17, 30, 0, 0, time.UTC)
	if err := st.RegisterRepo(ctx, store.Repo{ID: "shared", Owner: "fixture", Repo: "shared", Active: true}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mail", "calendar"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: id, Name: id}, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddProjectRepo(ctx, id, "shared", now); err != nil {
			t.Fatal(err)
		}
	}
	fake := gh.NewFake()
	mgr, err := multirepo.New(ctx, st, clock.NewFake(now), nil,
		func(store.Repo) (gh.Client, gh.Writer, error) { return fake, fake, nil })
	if err != nil {
		t.Fatalf("v2 shared repo manager: %v", err)
	}
	if got := mgr.Repos(); len(got) != 1 || got[0] != "shared" {
		t.Fatalf("repos=%v", got)
	}
}
