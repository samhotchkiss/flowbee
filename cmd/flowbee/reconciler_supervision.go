package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// durableReconcilerSet is the shared per-iteration supervision wrapper for v2
// loops. Panics become durable failure facts and alerts, while the goroutine stays
// alive for the next iteration. Each status write is fenced by the serve
// incarnation's durable run epoch.
type durableReconcilerSet struct {
	store  *store.Store
	mu     sync.Mutex
	leases map[string]store.ReconcilerLease
}

func beginDurableReconcilers(ctx context.Context, st *store.Store, owner string, now time.Time, grace map[string]time.Duration) (*durableReconcilerSet, error) {
	out := &durableReconcilerSet{store: st, leases: make(map[string]store.ReconcilerLease, len(grace))}
	for name, maxSilence := range grace {
		lease, err := st.BeginReconciler(ctx, name, owner, now, maxSilence)
		if err != nil {
			return nil, fmt.Errorf("begin reconciler %s: %w", name, err)
		}
		out.leases[name] = lease
	}
	return out, nil
}

func (s *durableReconcilerSet) tick(ctx context.Context, name string, now time.Time, fn func() error) (err error) {
	s.mu.Lock()
	lease, ok := s.leases[name]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("reconciler %q is not registered", name)
	}
	progress := store.ReconcilerProgress{}
	if seq, seqErr := s.store.EpicDigestSeq(ctx); seqErr == nil {
		progress.LedgerSeq = seq
	}
	if err := s.store.HeartbeatReconciler(ctx, lease, now, progress); err != nil {
		return fmt.Errorf("record %s heartbeat: %w", name, err)
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			detail := fmt.Sprintf("%v", recovered)
			recordErr := s.store.MarkReconcilerPanic(ctx, lease, now, progress, detail)
			if recordErr != nil {
				err = fmt.Errorf("%s panic: %s (record panic: %w)", name, detail, recordErr)
			} else {
				err = fmt.Errorf("%s panic recovered: %s", name, detail)
			}
		}
	}()
	if err = fn(); err != nil {
		if recordErr := s.store.MarkReconcilerFailure(ctx, lease, now, progress, err.Error()); recordErr != nil {
			return fmt.Errorf("%s failed: %v (record failure: %w)", name, err, recordErr)
		}
		return err
	}
	if seq, seqErr := s.store.EpicDigestSeq(ctx); seqErr == nil {
		progress.LedgerSeq = seq
	}
	if err := s.store.MarkReconcilerSuccess(ctx, lease, now, progress); err != nil {
		return fmt.Errorf("record %s success: %w", name, err)
	}
	return nil
}
