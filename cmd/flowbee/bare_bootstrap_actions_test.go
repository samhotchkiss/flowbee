package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/bootstrap"
	"github.com/samhotchkiss/flowbee/internal/store"
)

type bareActionClientFake struct {
	activation store.ProjectActivationStatus
	commits    int
}

func (f *bareActionClientFake) Commit(context.Context, api.BootstrapAction) (api.BootstrapActionReceipt, error) {
	f.commits++
	return api.BootstrapActionReceipt{}, errors.New("unexpected commit")
}
func (f *bareActionClientFake) Status(context.Context, string) (api.BootstrapActionStatus, error) {
	return api.BootstrapActionStatus{}, errors.New("unexpected status")
}
func (f *bareActionClientFake) Activation(context.Context, string) (store.ProjectActivationStatus, error) {
	return f.activation, nil
}

func TestCompletedBareBootstrapRevalidatesFinalHealthBeforeAttach(t *testing.T) {
	ctx := context.Background()
	db, checkpoints, err := bootstrap.OpenSQLiteCheckpointStore(ctx, filepath.Join(t.TempDir(), "bootstrap.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	action, err := makeBareBootstrapAction("bootstrap-russ", "russ", "project", "project_upsert",
		store.PortfolioProject{ID: "russ", Name: "Russ"})
	if err != nil {
		t.Fatal(err)
	}
	plan := bareServerActionPlan{BootstrapID: "bootstrap-russ", ProjectID: "russ", CWD: "/dev/russ",
		RepositoryOrigin: "github.com/sam/russ", Actions: []api.BootstrapAction{action},
		Attach: bootstrap.AttachIntentSpec{ID: "attach-russ", InteractorActorID: "russ-claude",
			TmuxServerDomainID: "default", PresentationName: "russ-interactor"}}
	cp, err := initializeBarePlanCheckpoint(ctx, checkpoints, plan)
	if err != nil {
		t.Fatal(err)
	}
	cp.Done = true
	if _, err := checkpoints.CompareAndSwap(ctx, cp, cp.Version); err != nil {
		t.Fatal(err)
	}
	client := &bareActionClientFake{activation: store.ProjectActivationStatus{LiveReady: true}}
	finalReady, attached := false, 0
	runner := bareServerActionRunner{Store: checkpoints, Client: client,
		FinalReady: func(context.Context) (bool, error) { return finalReady, nil },
		Attach:     func(bootstrap.AttachIntentSpec) error { attached++; return nil }}
	if err := runner.Run(ctx, plan); err == nil || attached != 0 || client.commits != 0 {
		t.Fatalf("stale Done revalidation err=%v attached=%d commits=%d", err, attached, client.commits)
	}
	finalReady = true
	if err := runner.Run(ctx, plan); err != nil || attached != 1 || client.commits != 0 {
		t.Fatalf("healthy Done resume err=%v attached=%d commits=%d", err, attached, client.commits)
	}
}
