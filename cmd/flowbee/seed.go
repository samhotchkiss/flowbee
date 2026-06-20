package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/intake"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// runSeed hand-seeds a `ready` build job (the M1 manual seed). F1: it can carry a
// task/spec/acceptance the lease grant ships to the worker, set directly via
// flags OR parsed (stub) from a GitHub issue body file via -from-issue-body-file.
func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	baseSHA := fs.String("base-sha", "deadbeef", "base SHA the build applies to")
	priority := fs.Int("priority", 0, "scheduling priority")
	taskText := fs.String("task", "", "task text the agent must satisfy (F1)")
	specText := fs.String("spec", "", "spec/design context for the task (F1)")
	acceptance := fs.String("acceptance", "", "acceptance criteria / done-when (F1)")
	fromIssueBody := fs.String("from-issue-body-file", "", "stub intake: parse task/spec/acceptance from a GitHub issue body file (F1)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// F1 stub intake: a GitHub issue body file -> task/spec/acceptance. Explicit
	// flags still win over a parsed section (a manual override).
	if *fromIssueBody != "" {
		raw, err := os.ReadFile(*fromIssueBody)
		if err != nil {
			return fmt.Errorf("read issue body: %w", err)
		}
		t := intake.TaskFromIssueBody(string(raw))
		if *taskText == "" {
			*taskText = t.Text
		}
		if *specText == "" {
			*specText = t.Spec
		}
		if *acceptance == "" {
			*acceptance = t.AcceptanceCriteria
		}
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
		ID:                 id,
		Kind:               job.KindBuild,
		Flow:               "build",
		Stage:              "build",
		Role:               job.RoleEngWorker,
		BaseSHA:            *baseSHA,
		Priority:           job.NormalizePriority(*priority),
		TaskText:           *taskText,
		SpecText:           *specText,
		AcceptanceCriteria: *acceptance,
		Now:                clock.Real{}.Now(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("seeded ready build job %s (state=%s base_sha=%s task=%q)\n", j.ID, j.State, j.BaseSHA, j.TaskText)
	return nil
}
