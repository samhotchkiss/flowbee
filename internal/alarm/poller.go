// Package alarm runs the single durable-timer polling goroutine (project
// override #2). It replaces River's cadence role: one goroutine wakes on an
// interval, claims due timers from the store, and fires them epoch-guarded. M2's
// only timer is no_eligible_worker (I-6). M8 adds the liveness timers — the
// per-phase soft deadline + the absolute lease cap (Rung-3) — plus a periodic
// Rung-2 sweep + a two-rung kill evaluation pass over every active lease (§10).
package alarm

import (
	"context"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// Publisher is the SSE hook the poller uses to surface a fired alarm live
// (satisfied by *api.Broker). Optional; nil disables publishing.
type Publisher interface {
	PublishAlarm(jobID, kind string)
}

// LivenessPublisher surfaces a fired stall kill / revoke live (satisfied by
// *api.Broker via PublishReconcile-style hooks). Optional.
type LivenessPublisher interface {
	PublishLiveness(jobID, event string)
}

// FactSource is the reconciled Domain-B fact reader the Rung-2 sweep consumes (the
// store's DBFactSource). Passed in so the poller never touches GitHub directly.
type FactSource = store.FactSource

// Poller is the single-goroutine durable-timer driver.
type Poller struct {
	store    *store.Store
	clock    clock.Clock
	interval time.Duration
	pub      Publisher

	// M8 liveness wiring. When livenessCfg.AbsoluteCap (or PhaseBudget) is set the
	// poller drives the Rung-3 deadline checks + a periodic Rung-2 sweep + the
	// two-rung evaluation pass. facts is the reconciled-fact source Rung-2 reads.
	livenessOn  bool
	livenessCfg store.LivenessConfig
	facts       FactSource
	livePub     LivenessPublisher
	// breakerTripped is the last fleet-wide Rung-2 circuit-breaker state, refreshed
	// each Rung-2 sweep and fed into the deadline-driven evaluations.
	breakerTripped bool

	// M11 compensation wiring (§6.5.4, I-12). When set, a kill that revokes a lease
	// triggers compensate(job, dead_epoch): drop the dead epoch's ref, cancel its CI,
	// draft-back any PR opened for the dead attempt. The mirror is the shared bare repo
	// the dead epoch ref lives on (nil-safe: records the compensation intent).
	compensateOn bool
	mirror       *gitops.Mirror
}

// New builds a poller. interval is how often it scans for due timers.
func New(st *store.Store, clk clock.Clock, interval time.Duration, pub Publisher) *Poller {
	if interval <= 0 {
		interval = 100 * time.Millisecond
	}
	return &Poller{store: st, clock: clk, interval: interval, pub: pub}
}

// WithLiveness enables the M8 liveness driving (§10): the Rung-3 deadline checks,
// the periodic Rung-2 sweep, and the two-rung kill evaluation pass. facts is the
// reconciled-fact source Rung-2 reads; cfg carries the deadlines / governor ceiling.
func (p *Poller) WithLiveness(cfg store.LivenessConfig, facts FactSource, pub LivenessPublisher) *Poller {
	p.livenessOn = true
	p.livenessCfg = cfg
	p.facts = facts
	p.livePub = pub
	return p
}

// WithCompensation enables the M11 explicit compensation on every lease revocation
// (§6.5.4, I-12): a kill drops the dead epoch's ref, cancels its (job, epoch) CI, and
// drafts-back any PR opened for the dead attempt. The mirror may be nil (the ref-drop
// is then recorded intent only — the epoch bump from the revoke is itself the fence).
func (p *Poller) WithCompensation(m *gitops.Mirror) *Poller {
	p.compensateOn = true
	p.mirror = m
	return p
}

// compensateKill runs compensation for a kill result (the dead epoch is NewEpoch-1).
func (p *Poller) compensateKill(ctx context.Context, res store.LivenessResult, now time.Time) {
	if !p.compensateOn || !res.Killed || res.NewEpoch <= 0 {
		return
	}
	_, _ = p.store.Compensate(ctx, store.CompensateParams{
		JobID: res.JobID, DeadEpoch: res.NewEpoch - 1, Reason: res.Reason,
		Mirror: p.mirror, EnqueueDraftBack: true, Now: now,
	})
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
		case store.TimerLeaseDeadline, store.TimerPhaseDeadline:
			if !p.livenessOn {
				_ = p.store.MarkTimerFired(ctx, d.ID)
				continue
			}
			res, err := p.store.FireLeaseDeadline(ctx, d, now, p.livenessCfg, p.breakerTripped)
			if err == nil && res.Killed {
				p.compensateKill(ctx, res, now)
				if p.livePub != nil {
					p.livePub.PublishLiveness(res.JobID, "stall_revoked")
				}
			}
		}
	}
}

// Rung2Tick runs ONE Rung-2 sweep + the two-rung evaluation pass over every active
// lease (§10). Separated from Tick so it can run on the slower reconcile cadence
// (the external oracle only updates on the sweep, §10.2) and be driven directly in
// tests. It refreshes the fleet-wide circuit-breaker state, then evaluates each
// active job's ladder (catching a soft-deadline + Rung-2-stalled agreement that the
// deadline timer alone could not, since Rung-2 lags the clock).
func (p *Poller) Rung2Tick(ctx context.Context) {
	if !p.livenessOn {
		return
	}
	now := p.clock.Now()
	tripped, err := p.store.Rung2Sweep(ctx, p.facts, now, p.livenessCfg)
	if err != nil {
		return
	}
	p.breakerTripped = tripped
	ids, err := p.store.ActiveLeaseJobs(ctx)
	if err != nil {
		return
	}
	for _, id := range ids {
		res, err := p.store.EvaluateLiveness(ctx, id, now, p.livenessCfg, tripped)
		if err == nil && res.Killed {
			p.compensateKill(ctx, res, now)
			if p.livePub != nil {
				p.livePub.PublishLiveness(res.JobID, "stall_revoked")
			}
		}
	}
}
