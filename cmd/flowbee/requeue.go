package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runRequeue re-arms a job to `ready` with a fresh attempt budget. Two forms:
//
//	flowbee requeue [--force] <job-id>              # one job
//	flowbee requeue --state needs_human [--repo X]  # every job in that state (bulk)
//
// The bulk form is the operator answer to "N jobs bounced out to needs_human and don't
// auto-retry" — re-enter them all at once instead of one curl/command per id. It SKIPS jobs a
// human deliberately closed (escalation_reason=pr_closed): requeuing a rejected PR rebuilds
// work the human said no to. Requeue those individually by id if you truly intend to.
func runRequeue(args []string) error {
	fs := flag.NewFlagSet("requeue", flag.ContinueOnError)
	force := fs.Bool("force", false, "requeue even if the job is actively leased (fences the live worker, discarding its in-flight work)")
	state := fs.String("state", "", "requeue ALL jobs in this state (e.g. needs_human) instead of a single job-id")
	repo := fs.String("repo", "", "with --state: limit to this repo id")
	if err := fs.Parse(args); err != nil {
		return err
	}

	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070")
	c := client.NewWithToken(url, os.Getenv("FLOWBEE_WORKER_TOKEN"))

	if *state != "" {
		if fs.NArg() > 0 {
			return fmt.Errorf("give EITHER a <job-id> OR --state, not both")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		db, err := store.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return err
		}
		defer db.Close()
		return requeueByState(c, db, *state, *repo, *force)
	}

	if fs.NArg() < 1 {
		return fmt.Errorf("usage: flowbee requeue [--force] <job-id>   |   flowbee requeue --state <state> [--repo X]")
	}
	return requeueOne(c, fs.Arg(0), *force)
}

func requeueOne(c *client.Client, jobID string, force bool) error {
	st, err := c.Requeue(context.Background(), jobID, force)
	if err != nil {
		return err
	}
	if st == 409 {
		return fmt.Errorf("job %s is actively leased — a worker is building/reviewing it now; "+
			"re-run with --force to requeue anyway (this discards the live worker's work)", jobID)
	}
	if st != 200 {
		return fmt.Errorf("requeue status %d", st)
	}
	fmt.Printf("requeued %s -> ready (fresh attempt budget)\n", jobID)
	return nil
}

// requeueByState reads the matching job ids from the local control-plane DB (read-only) and
// requeues each through the same API the single-job form uses.
func requeueByState(c *client.Client, db *store.Store, state, repo string, force bool) error {
	ctx := context.Background()
	query := `SELECT id, COALESCE(escalation_reason,'') FROM jobs WHERE state = ?`
	qargs := []any{state}
	if repo != "" {
		query += ` AND COALESCE(repo,'') = ?`
		qargs = append(qargs, repo)
	}
	query += ` ORDER BY enqueued_at`
	rows, err := db.DB.QueryContext(ctx, query, qargs...)
	if err != nil {
		return fmt.Errorf("list %s jobs: %w", state, err)
	}
	type job struct{ id, reason string }
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.reason); err != nil {
			rows.Close()
			return err
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(jobs) == 0 {
		fmt.Printf("no jobs in state %q%s\n", state, repoSuffix(repo))
		return nil
	}

	var requeued, skipped, failed int
	for _, j := range jobs {
		if j.reason == "pr_closed" {
			skipped++
			fmt.Printf("skip %s (pr_closed — a human closed this PR; requeue by id to override)\n", j.id)
			continue
		}
		if err := requeueOne(c, j.id, force); err != nil {
			failed++
			fmt.Printf("FAILED %s: %v\n", j.id, err)
			continue
		}
		requeued++
	}
	fmt.Printf("\nrequeued %d, skipped %d (pr_closed), failed %d — of %d %s job(s)%s\n",
		requeued, skipped, failed, len(jobs), state, repoSuffix(repo))
	if failed > 0 {
		return fmt.Errorf("%d requeue(s) failed", failed)
	}
	return nil
}

func repoSuffix(repo string) string {
	if repo == "" {
		return ""
	}
	return " in repo " + repo
}
