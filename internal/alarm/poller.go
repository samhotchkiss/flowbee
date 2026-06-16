// Package alarm runs the single durable-timer polling goroutine (project
// override #2). It replaces River's cadence role: one goroutine wakes on an
// interval, claims due timers from the store, and fires them epoch-guarded. M2's
// only timer is no_eligible_worker (I-6): a `ready` job that no compliant worker
// has leased before its alarm window raises the alarm.
package alarm

import (
	"context"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// Publisher is the SSE hook the poller uses to surface a fired alarm live
// (satisfied by *api.Broker). Optional; nil disables publishing.
type Publisher interface {
	PublishAlarm(jobID, kind string)
}

// Poller is the single-goroutine durable-timer driver.
type Poller struct {
	store    *store.Store
	clock    clock.Clock
	interval time.Duration
	pub      Publisher
}

// New builds a poller. interval is how often it scans for due timers.
func New(st *store.Store, clk clock.Clock, interval time.Duration, pub Publisher) *Poller {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	return &Poller{store: st, clock: clk, interval: interval, pub: pub}
}

// Run blocks until ctx is cancelled, scanning for due timers each interval and
// firing them. Exactly one Poller should run per control plane (epoch-guarded so
// even a duplicate would be a no-op).
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

// Tick processes all currently-due timers once. Exposed for deterministic tests
// (drive it directly with a fake clock instead of waiting on the ticker).
func (p *Poller) Tick(ctx context.Context) {
	now := p.clock.Now()
	due, err := p.store.DueTimers(ctx, now)
	if err != nil {
		return
	}
	for _, d := range due {
		switch d.Kind {
		case store.TimerNoEligibleWorker:
			fired, err := p.store.FireNoEligibleWorker(ctx, d, now)
			if err == nil && fired && p.pub != nil {
				p.pub.PublishAlarm(d.JobID, d.Kind)
			}
		}
	}
}
