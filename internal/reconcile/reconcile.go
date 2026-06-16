// Package reconcile is the reconcile-IN loop (DESIGN §3.3, §8.1): the ONLY
// authority for Domain-B facts (I-1). It pulls GitHub-owned facts via the single
// installation identity's batched BoardSweep, applies the SHA-monotonic +
// terminal-SHA guards (I-3), and drives the §3.4 reconcile transitions (merged ->
// done; SHA move -> superseded + re-arm, I-5). Webhooks are HINTS only: a verified,
// deduped, write-ahead-logged delivery triggers a TARGETED refetch through the SAME
// code path — never a direct state change (so a forged/replayed webhook can at
// worst refetch the real state, never fast-track a merge, §8.1.3).
//
// It writes ONLY Domain-B fact-fields (enforced structurally by the store's
// ApplyReconciledPR). It is NOT a deterministic-core package (it reads a clock and
// talks GitHub); archcheck forbids the core from importing it.
package reconcile

import (
	"context"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// Clock is the injected clock (DESIGN: Flowbee is the sole clock).
type Clock interface{ Now() time.Time }

// Publisher surfaces a reconcile outcome live on the SSE feed (satisfied by
// *api.Broker). Optional; nil disables publishing.
type Publisher interface {
	PublishReconcile(jobID, event string)
}

// Reconciler runs reconcile-IN against a github.Client (real or fake).
type Reconciler struct {
	store *store.Store
	gh    gh.Client
	clock Clock
	pub   Publisher
	// repo is the F9 repo-scope handle this reconciler is bound to (a repos.id).
	// Empty is the legacy single-repo scope. Every swept PR is bound back to a job
	// ONLY within this repo (PR numbers are repo-scoped), so one control plane runs
	// one Reconciler per repo, each over its own github.Client.
	repo string
}

// New builds a Reconciler for the legacy single-repo scope (repo="").
func New(st *store.Store, client gh.Client, clk Clock, pub Publisher) *Reconciler {
	return &Reconciler{store: st, gh: client, clock: clk, pub: pub}
}

// NewForRepo builds a Reconciler bound to a specific F9 repo scope (a repos.id):
// its sweep binds swept PRs back to jobs only within that repo. One control plane
// holds one Reconciler per managed repo, each over the repo's own github.Client.
func NewForRepo(repo string, st *store.Store, client gh.Client, clk Clock, pub Publisher) *Reconciler {
	return &Reconciler{store: st, gh: client, clock: clk, pub: pub, repo: repo}
}

// Repo returns the repo-scope handle this reconciler is bound to ("" = legacy).
func (r *Reconciler) Repo() string { return r.repo }

// Sweep performs one batched BoardSweep (§8.1.1) and reconciles every PR whose
// number is bound to a job. It records the rate-limit gauge (I-14) every sweep.
// The sweep is the FLOOR (§8.1): even with zero webhooks ever delivered, it
// reconciles everything. Returns the per-job outcomes (for tests / publishing).
func (r *Reconciler) Sweep(ctx context.Context) ([]store.ReconcileOutcome, error) {
	now := r.clock.Now()
	snap, err := r.gh.BoardSweep(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.store.RecordRateLimit(ctx, snap.RateLimit, now); err != nil {
		return nil, err
	}
	var outs []store.ReconcileOutcome
	for _, pr := range snap.PullRequests {
		out, applied, err := r.ingest(ctx, pr, now)
		if err != nil {
			return outs, err
		}
		if applied {
			outs = append(outs, out)
		}
	}
	return outs, nil
}

// Refetch handles a webhook HINT (§8.1.3): a TARGETED single-PR refetch through
// the same ingest path as the sweep. The webhook never carries authority — this
// reads the REAL state from GitHub and reconciles it under the I-3 guards. A
// forged "approved"/"merged" delivery thus triggers a refetch that reads the
// un-approved/un-merged truth and cannot fast-track anything.
func (r *Reconciler) Refetch(ctx context.Context, prNumber int) (store.ReconcileOutcome, bool, error) {
	now := r.clock.Now()
	pr, ok, err := r.gh.PullRequest(ctx, prNumber)
	if err != nil {
		return store.ReconcileOutcome{}, false, err
	}
	if !ok {
		return store.ReconcileOutcome{}, false, nil // no such PR: nothing to reconcile
	}
	return r.ingest(ctx, pr, now)
}

// RefetchHint adapts Refetch to the webhook.Refetcher interface: a targeted
// refetch driven by a verified, deduped webhook HINT. It returns whether the PR
// was bound to a job (and thus reconciled). Errors are swallowed to a false return
// — a webhook is a best-effort hint; the floor sweep reconciles regardless (§8.1.4).
func (r *Reconciler) RefetchHint(ctx context.Context, prNumber int) bool {
	_, reconciled, err := r.Refetch(ctx, prNumber)
	if err != nil {
		return false
	}
	return reconciled
}

// ingest maps one PR's Domain-B facts to its bound job and applies them under the
// I-3 guards. An un-bound PR (no job for that number) is a no-op (applied=false).
func (r *Reconciler) ingest(ctx context.Context, pr gh.PullRequest, now time.Time) (store.ReconcileOutcome, bool, error) {
	jobID, ok, err := r.store.JobIDForPRInRepo(ctx, r.repo, pr.Number)
	if err != nil {
		return store.ReconcileOutcome{}, false, err
	}
	if !ok {
		return store.ReconcileOutcome{}, false, nil
	}
	out, err := r.store.ApplyReconciledPR(ctx, jobID, toReconciled(pr), now)
	if err != nil {
		return store.ReconcileOutcome{}, false, err
	}
	if r.pub != nil {
		switch {
		case out.Done:
			r.pub.PublishReconcile(jobID, "reconciled_done")
		case out.Superseded:
			r.pub.PublishReconcile(jobID, "superseded")
		case out.Frozen:
			r.pub.PublishReconcile(jobID, "terminal_frozen")
		case out.Applied:
			r.pub.PublishReconcile(jobID, "facts_reconciled")
		}
	}
	return out, true, nil
}

// toReconciled maps a github.PullRequest to the store's Domain-B fact-set. CI is
// "green" iff the rollup is SUCCESS (a pure mapping; gate logic consumes the bool).
func toReconciled(pr gh.PullRequest) store.ReconciledPR {
	return store.ReconciledPR{
		Number:      pr.Number,
		UpdatedAt:   pr.UpdatedAt,
		IsDraft:     pr.IsDraft,
		Merged:      pr.Merged,
		HeadSHA:     pr.HeadRefOid,
		BaseSHA:     pr.BaseRefOid,
		MergeCommit: pr.MergeCommit,
		CIGreen:     pr.CIRollup == gh.CISuccess,
	}
}
