package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// runSeed hand-seeds a `ready` build job (the M1 manual seed).
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	baseSHA := fs.String("base-sha", "deadbeef", "base SHA the build applies to")
	priority := fs.Int("priority", 0, "scheduling priority")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		return err
	}

	id := ulid.New()
	j, err := st.SeedJob(ctx, store.SeedParams{
		ID:       id,
		Kind:     job.KindBuild,
		Flow:     "build",
		Stage:    "build",
		Role:     job.RoleEngWorker,
		BaseSHA:  *baseSHA,
		Priority: *priority,
		Now:      clock.Real{}.Now(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("seeded ready build job %s (state=%s base_sha=%s)\n", j.ID, j.State, j.BaseSHA)
	return nil
}
