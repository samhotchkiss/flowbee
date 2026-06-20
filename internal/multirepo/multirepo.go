// Package multirepo is the F9 multi-repo control plane (build-list F9): ONE
// Flowbee, a SET of GitHub repos, a SHARED fleet. It wires, per managed repo, a
// repo-scoped reconcile-IN loop and a repo-scoped project-OUT loop — each over the
// repo's OWN github.Client/Writer (real or fake) — while the scheduler and the
// worker fleet stay GLOBAL and repo-agnostic:
//
//   - reconcile-IN runs PER repo: each repo's BoardSweep binds its swept PRs back
//     to jobs only within that repo (PR numbers are repo-scoped, so #1000 in repo A
//     never cross-binds to #1000 in repo B).
//   - project-OUT runs PER repo: each repo's sender drains only its own repo's
//     outbox rows, over that repo's writer, against that repo's integration branch.
//   - the SCHEDULER is shared: the store's ReadyCandidates returns the UNION of all
//     repos' ready jobs, and the existing priority/aging ranking is the cross-repo
//     prioritization — any repo's ready work routes to any capable worker.
//   - workers stay repo-AGNOSTIC: they advertise capabilities, never a repo, so the
//     same box can build repo A then review repo B.
//
// This is NOT a deterministic-core package (it owns the per-repo GitHub loops and a
// clock); archcheck forbids the core from importing it.
package multirepo

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/project"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/scheduler"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// Clock is the injected clock (Flowbee is the sole clock); passed to the per-repo
// reconcile/project loops.
type Clock interface{ Now() time.Time }

// Publisher surfaces a per-repo loop outcome on the SSE feed (optional, nil-safe).
type Publisher interface {
	PublishReconcile(jobID, event string)
}

// GitHubFactory builds the per-repo GitHub client+writer for a registered repo.
// In production this returns a *github.RealClient bearing that repo's installation
// token; in tests it returns a per-repo *github.Fake. Returning the same value for
// both Client and Writer (a Fake, or a RealClient) is the normal case.
type GitHubFactory func(r store.Repo) (gh.Client, gh.Writer, error)

// repoLoop bundles one repo's two scoped loops.
type repoLoop struct {
	repo   store.Repo
	rec    *reconcile.Reconciler
	sender *project.Sender
	// unsticker is the repo's client viewed as the optional #214 merge_handoff un-stick
	// surface (fast-forward a behind, reviewed, green PR so it stops rotting). nil when the
	// repo's client doesn't implement it (e.g. a minimal test fake) — the un-stick is then
	// skipped for that repo.
	unsticker gh.MergeUnsticker
}

// Manager owns the per-repo loops over one shared store. It is built from the
// repos registry and is the single object the runtime ticks (SweepAll/DrainAll) and
// the single place a webhook hint is routed to the right repo's reconciler.
type Manager struct {
	store *store.Store
	clk   Clock
	pub   Publisher
	loops map[string]*repoLoop // keyed by repos.id
	order []string             // repo ids in stable (sorted) order
}

// HistoryWriter is the per-repo LOCAL-git writer that lands the F11 issue-archive
// commit (build-list §F). Aliased from project so callers wiring the Manager need
// not import project directly. Satisfied by *gitops.Mirror.
type HistoryWriter = project.HistoryWriter

// HistoryFactory builds the per-repo LOCAL-git history writer for the F11
// issue-archive projection (build-list §F): given a repo, it returns the writer
// that lands the dedicated post-merge `docs/history/<id>.md` + TOC commit on the
// repo's integration branch, or nil to disable the archive for that repo. In
// production this returns the repo's bare mirror (gitops.Open(mirrorPath)).
type HistoryFactory func(r store.Repo) HistoryWriter

// Option configures the Manager at construction (functional options keep New's
// signature stable for existing callers/tests).
type Option func(*managerConfig)

type managerConfig struct {
	history        HistoryFactory
	allowOwnSource map[string]bool
	archiveHistory map[string]bool
	logger         *slog.Logger
}

// WithHistory wires the F11 history writer per repo so each repo's project-OUT
// sender lands the dedicated post-merge archive commit. Without it, history.write
// rows drain as audited no-ops (the ledger stays canonical; the markdown is simply
// not materialized).
func WithHistory(f HistoryFactory) Option {
	return func(c *managerConfig) { c.history = f }
}

// WithAllowOwnSource marks the repo ids (by registry id) whose own source is NOT the
// Flowbee control plane's, relaxing the flowbee_source content class for their merge
// cross-check — so their internal//cmd/ changes self-merge instead of forced handoff.
// MUST be the SAME set as store.AllowOwnSourceRepos so the two gate sites agree. Empty
// (default) = every repo fully protected. NEVER include the repo that IS Flowbee.
func WithAllowOwnSource(repos map[string]bool) Option {
	return func(c *managerConfig) { c.allowOwnSource = repos }
}

// WithArchiveHistory opts the named repo ids into the durable §F history archive (every
// merge lands docs/history/*.md on the repo's integration branch via the Contents API).
// Per-repo opt-in (default off) because it commits to the repo's main on every merge.
func WithArchiveHistory(repos map[string]bool) Option {
	return func(c *managerConfig) { c.archiveHistory = repos }
}

// WithLogger wires a logger into each repo's project-OUT sender so dead-lettered GitHub
// writes are recorded durably in the serve log (alongside the flowbee_outbox_abandoned metric).
func WithLogger(l *slog.Logger) Option {
	return func(c *managerConfig) { c.logger = l }
}

// New builds a Manager over every ACTIVE registered repo, constructing each repo's
// scoped reconcile-IN + project-OUT loop via the factory. Parked (active=0) repos
// are skipped — their loops do not run and their jobs are not dispatched.
func New(ctx context.Context, st *store.Store, clk Clock, pub Publisher, factory GitHubFactory, opts ...Option) (*Manager, error) {
	var cfg managerConfig
	for _, o := range opts {
		o(&cfg)
	}
	repos, err := st.ListRepos(ctx, true)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	m := &Manager{store: st, clk: clk, pub: pub, loops: map[string]*repoLoop{}}
	for _, r := range repos {
		client, writer, ferr := factory(r)
		if ferr != nil {
			return nil, fmt.Errorf("build github for repo %q: %w", r.ID, ferr)
		}
		recClk := reconcileClock{clk}
		projClk := projectClock{clk}
		sender := project.NewForRepo(r.ID, r.DefaultBranch, st, writer, projClk, asProjectPub(pub))
		// relax flowbee_source for a non-control-plane repo (mirrors store.AllowOwnSourceRepos).
		sender.SetAllowOwnSource(cfg.allowOwnSource[r.ID])
		sender.SetArchiveHistory(cfg.archiveHistory[r.ID])
		sender.SetLogger(cfg.logger)
		rec := reconcile.NewForRepo(r.ID, st, client, recClk, asReconcilePub(pub))
		// F11 (build-list §F): wire the per-repo history writer so the merged->done
		// post-merge archive commit lands on the repo's integration branch. The same
		// mirror also resolves base_sha for GitHub-issue intake (adopt a labeled issue
		// as a build cut from current main).
		if cfg.history != nil {
			if hw := cfg.history(r); hw != nil {
				branch := r.DefaultBranch
				if branch == "" {
					branch = "main"
				}
				sender = sender.WithHistory(hw, branch)
				rec = rec.WithIntake(hw, branch)
			}
		}
		unsticker, _ := client.(gh.MergeUnsticker) // nil if the client doesn't support it
		m.loops[r.ID] = &repoLoop{
			repo:      r,
			rec:       rec,
			sender:    sender,
			unsticker: unsticker,
		}
		m.order = append(m.order, r.ID)
	}
	sort.Strings(m.order)
	return m, nil
}

// Repos returns the managed repo ids in stable order.
func (m *Manager) Repos() []string { return append([]string(nil), m.order...) }

// SweepAll runs one reconcile-IN sweep PER managed repo (the §8.1 floor, per repo).
// Each repo's sweep reads its own board and binds its PRs only within that repo. It
// returns the per-repo outcome counts and stops on the first error (the caller
// retries on the next tick).
func (m *Manager) SweepAll(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{}
	for _, id := range m.order {
		outs, err := m.loops[id].rec.Sweep(ctx)
		if err != nil {
			return counts, fmt.Errorf("sweep repo %q: %w", id, err)
		}
		counts[id] = len(outs)
	}
	return counts, nil
}

// DrainAll drains the project-OUT outbox PER managed repo (each sender drains only
// its own repo's rows, over its own writer). Returns the per-repo sent counts.
func (m *Manager) DrainAll(ctx context.Context) (map[string]int, error) {
	counts := map[string]int{}
	for _, id := range m.order {
		n, err := m.loops[id].sender.DrainOnce(ctx)
		if err != nil {
			return counts, fmt.Errorf("drain repo %q: %w", id, err)
		}
		counts[id] = n
	}
	return counts, nil
}

// UnstickAll fast-forwards every merge_handoff PR that is BEHIND its base, per repo, over the
// repo's own client (#214). It NEVER merges — only brings the PR up to date (server-side FF,
// re-triggering CI) — so a reviewed, green PR stops rotting behind a base that keeps moving
// (the "each other merge pushes the waiting ones further behind, they never converge"
// cascade); a human (or a later self-merge) then lands a CURRENT PR. It acts ONLY on a
// definitive "behind" — GitHub reports that state only when the repo REQUIRES up-to-date
// branches, so this self-scopes to exactly the repos that need it; "unknown" (async, not yet
// computed), "clean", "dirty" (a real conflict — a human resolves) are left alone. A per-PR
// error is skipped (best-effort), never fatal. Returns the per-repo count of branches updated.
func (m *Manager) UnstickAll(ctx context.Context) (map[string]int, error) {
	rows, err := m.store.MergeHandoffView(ctx)
	if err != nil {
		return nil, err
	}
	byRepo := map[string][]store.MergeHandoffRow{}
	for _, r := range rows {
		byRepo[r.Repo] = append(byRepo[r.Repo], r)
	}
	counts := map[string]int{}
	for _, id := range m.order {
		loop := m.loops[id]
		if loop.unsticker == nil {
			continue
		}
		for _, row := range byRepo[id] {
			if row.PRNumber == 0 {
				continue
			}
			state, ok, serr := loop.unsticker.PullMergeableState(ctx, row.PRNumber)
			if serr != nil || !ok || state != "behind" {
				continue // act ONLY on a definitive behind
			}
			if uerr := loop.unsticker.UpdateBranch(ctx, row.PRNumber); uerr != nil {
				continue // 422 conflict / transient — a human resolves a real conflict
			}
			counts[id]++
		}
	}
	return counts, nil
}

// RefetchHint routes a webhook hint to the named repo's reconciler (a targeted,
// repo-scoped single-PR refetch). Unknown repo => false (best-effort hint).
func (m *Manager) RefetchHint(ctx context.Context, repoID string, prNumber int) bool {
	l, ok := m.loops[repoID]
	if !ok {
		return false
	}
	return l.rec.RefetchHint(ctx, prNumber)
}

// GlobalReadyOrder returns the cross-repo offer order for a worker with the given
// attested capabilities: the UNION of every repo's ready jobs, ranked by the shared
// scheduler (priority + aging), filtered to what the worker can win. This IS the
// cross-repo prioritization — there is one global queue, not one per repo, so the
// highest-priority/oldest job wins regardless of which repo it belongs to.
func (m *Manager) GlobalReadyOrder(ctx context.Context, attested []string, now time.Time) ([]scheduler.Candidate, error) {
	cands, err := m.store.ReadyCandidates(ctx)
	if err != nil {
		return nil, err
	}
	return scheduler.Order(cands, attested, now), nil
}

// ── clock/publisher adapters (the reconcile/project packages each declare their
// own tiny Clock/Publisher interface; these satisfy both from the Manager's). ──

type reconcileClock struct{ c Clock }

func (r reconcileClock) Now() time.Time { return r.c.Now() }

type projectClock struct{ c Clock }

func (p projectClock) Now() time.Time { return p.c.Now() }

// asReconcilePub / asProjectPub adapt the Manager's Publisher to each loop's
// Publisher (identical method set; nil stays nil so publishing is disabled).
func asReconcilePub(p Publisher) reconcile.Publisher {
	if p == nil {
		return nil
	}
	return p
}

func asProjectPub(p Publisher) project.Publisher {
	if p == nil {
		return nil
	}
	return p
}
