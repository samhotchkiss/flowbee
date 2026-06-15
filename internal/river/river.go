// Package river wires the in-process River job queue (DESIGN §12.3) for timers
// and Flowbee's own internal work (reconcile sweeps, deadline checks, alarms).
//
// IMPORTANT (DESIGN §3.5): an agent lease is NEVER a River-worked row. River runs
// internal, trusted, bounded units of Flowbee's own work only. M0 registers no
// workers; job kinds arrive with the milestones that need them.
package river

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	riverqueue "github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Migrate applies River's own schema migrations. Run before store.MigrateUp.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	m, err := rivermigrate.New(riverpgxv5.New(pool), nil)
	if err != nil {
		return fmt.Errorf("river migrate new: %w", err)
	}
	if _, err := m.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("river migrate up: %w", err)
	}
	return nil
}

// noopArgs is a placeholder job kind so the client can Start with a non-empty
// Workers bundle (River requires >=1 worker when a queue is configured). Real
// internal kinds — reconcile_sweep, lease_deadline_check, etc. (DESIGN §3.5) —
// replace/join it in later milestones.
type noopArgs struct{}

func (noopArgs) Kind() string { return "flowbee_noop" }

type noopWorker struct {
	riverqueue.WorkerDefaults[noopArgs]
}

func (noopWorker) Work(context.Context, *riverqueue.Job[noopArgs]) error { return nil }

// Workers builds the registry of internal River workers. M0: just the no-op.
func Workers() *riverqueue.Workers {
	w := riverqueue.NewWorkers()
	riverqueue.AddWorker(w, &noopWorker{})
	return w
}

// NewClient builds the in-process River client.
func NewClient(pool *pgxpool.Pool, maxWorkers int) (*riverqueue.Client[pgx.Tx], error) {
	return riverqueue.NewClient(riverpgxv5.New(pool), &riverqueue.Config{
		Queues: map[string]riverqueue.QueueConfig{
			riverqueue.QueueDefault: {MaxWorkers: maxWorkers},
		},
		Workers: Workers(),
	})
}
