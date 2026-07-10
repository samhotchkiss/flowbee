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
	"fmt"
	"strconv"
	"strings"
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
	// mirror resolves the integration-branch tip so a label-opted issue can be
	// adopted as a build cut from current main (GitHub-issue intake). Nil disables
	// issue intake for this repo (e.g. tests with no local mirror).
	mirror RepoMirror
	branch string
	// requiredChecks is the repo's REQUIRED status-check contexts (server-side branch
	// protection / ruleset), cached after the first successful fetch. A PR whose every
	// required check has passed is CI-green for the merge gate even if the AGGREGATE
	// rollup is UNSTABLE from a NON-required (e.g. cosmetic post-merge) check — matching
	// GitHub's own merge policy. nil + requiredFetched=false = not yet fetched; an empty
	// (non-nil via requiredFetched=true) list means "no required checks configured", in
	// which case the gate falls back to the conservative full-rollup-green rule.
	requiredChecks  []string
	requiredFetched bool
}

// RepoMirror resolves a ref to a commit SHA (satisfied by *gitops.Mirror) so the
// reconciler can cut an adopted issue's build from the current integration tip.
type RepoMirror interface {
	HeadSHA(ref string) (string, error)
}

// IntakeLabel is the opt-in label a human adds to a GitHub issue to hand it to
// Flowbee (distinct from the `flowbee` umbrella label Flowbee puts on issues it
// MATERIALIZES, so a materialized issue is never re-adopted).
const IntakeLabel = "flowbee:build"

// WithIntake wires the integration-branch mirror + branch so Sweep adopts
// label-opted issues as builds. Returns the reconciler for chaining.
func (r *Reconciler) WithIntake(m RepoMirror, branch string) *Reconciler {
	r.mirror = m
	if branch == "" {
		branch = "main"
	}
	r.branch = branch
	return r
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
	mainCIRed := r.mainCIRed(ctx)
	_ = r.store.RecordMainCIRed(ctx, r.repo, mainCIRed) // surface a red main on /metrics + status
	r.ensureRequiredChecks(ctx)                         // warm the required-checks cache once per sweep (not per PR)
	var outs []store.ReconcileOutcome
	for _, pr := range snap.PullRequests {
		out, applied, err := r.ingest(ctx, pr, now, mainCIRed)
		if err != nil {
			return outs, err
		}
		if applied {
			outs = append(outs, out)
		}
	}
	// GitHub-issue intake (build-list): adopt every label-opted, not-yet-tracked
	// issue as a build cut from the current integration tip. Idempotent in the store
	// (a job already tracking the issue is a no-op), so re-sweeps never duplicate.
	if r.mirror != nil {
		base, berr := r.mirror.HeadSHA("refs/heads/" + r.branch)
		for _, iss := range snap.Issues {
			if berr != nil || !hasLabel(iss.Labels, IntakeLabel) {
				continue
			}
			id, aerr := r.store.AdoptIssueAsBuild(ctx, r.repo, iss.Number, iss.Title, iss.Body, base, priorityFromLabels(iss.Labels), now)
			if aerr == nil && id != "" && r.pub != nil {
				r.pub.PublishReconcile(id, "issue_adopted")
			}
		}
	}
	return outs, nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// priorityFromLabels reads an optional `flowbee:p<N>` label (N in 1..10, LOWER = more
// urgent — `flowbee:p1` = drop-everything, `flowbee:p10` = nice-to-have) that a human can
// add alongside the intake label to rank an issue. Returns 0 when absent — the store's
// NormalizePriority maps that to the default 5, and clamps any out-of-band N into 1..10.
func priorityFromLabels(labels []string) int {
	for _, l := range labels {
		if rest, ok := strings.CutPrefix(l, "flowbee:p"); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				return n
			}
		}
	}
	return 0
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
	return r.ingest(ctx, pr, now, r.mainCIRed(ctx))
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

// AdoptPR imports a single pre-existing PR (one Flowbee did not originate — e.g. an
// external agent-pool branch) into this repo's review pipeline: it reads the PR's
// REAL state from GitHub (never trusting the caller), then binds it to an opted-in
// adopted code_reviewer job in review_pending via store.AdoptPRForReview. The normal
// reconcile + project-out machinery drives it from there (review -> self-merge on
// green, or needs_human on changes_requested). Idempotent: a PR already tracked by a
// non-cancelled job returns ("", false, nil) with no new job. If an already-adopted
// PR's authoritative base/head moved, the existing job is re-armed and returned with
// rearmed=true. Returns the new/re-armed job id (or "" if it was already tracked /
// the PR does not exist).
func (r *Reconciler) AdoptPR(ctx context.Context, prNumber int) (string, bool, error) {
	pr, ok, err := r.gh.PullRequest(ctx, prNumber)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, fmt.Errorf("pr #%d not found", prNumber)
	}
	if pr.Merged {
		return "", false, fmt.Errorf("pr #%d is already merged", prNumber)
	}
	if pr.ClosedUnmerged {
		return "", false, fmt.Errorf("pr #%d is closed unmerged", prNumber)
	}
	ciGreen := pr.CIRollup == gh.CISuccess && pr.CIHasRealSuccess
	differ, ok := r.gh.(gh.PRDiffer)
	if !ok {
		return "", false, fmt.Errorf("pr #%d cannot be adopted for review: github client cannot provide an authoritative diff", prNumber)
	}
	diff, err := differ.PullRequestDiff(ctx, prNumber, pr.BaseRefOid, pr.HeadRefOid)
	if err != nil {
		return "", false, fmt.Errorf("fetch adopted pr diff repo=%q pr=%d base=%s head=%s: %w",
			r.repo, prNumber, pr.BaseRefOid, pr.HeadRefOid, err)
	}
	id, rearmed, err := r.store.AdoptPRForReview(ctx, r.repo, prNumber, pr.BaseRefOid, pr.HeadRefOid,
		diff, diff == "",
		pr.Merged, ciGreen, pr.IsDraft, pr.UpdatedAt, r.clock.Now())
	if err != nil {
		return "", false, err
	}
	if id != "" && r.pub != nil {
		if rearmed {
			r.pub.PublishReconcile(id, "adopt_rearmed")
		} else {
			r.pub.PublishReconcile(id, "adopted")
		}
	}
	return id, rearmed, nil
}

// IntakeSweep runs a reconcile sweep so a freshly labeled/opened issue is adopted on
// the webhook, not just on the floor poll (webhook.Refetcher). Returns whether it ran.
func (r *Reconciler) IntakeSweep(ctx context.Context) bool {
	if _, err := r.Sweep(ctx); err != nil {
		return false
	}
	return true
}

// ingest maps one PR's Domain-B facts to its bound job and applies them under the
// I-3 guards. An un-bound PR (no job for that number) is a no-op (applied=false).
func (r *Reconciler) ingest(ctx context.Context, pr gh.PullRequest, now time.Time, mainCIRed bool) (store.ReconcileOutcome, bool, error) {
	jobID, ok, err := r.store.JobIDForPRInRepo(ctx, r.repo, pr.Number)
	if err != nil {
		return store.ReconcileOutcome{}, false, err
	}
	if !ok {
		return store.ReconcileOutcome{}, false, nil
	}
	out, err := r.store.ApplyReconciledPR(ctx, jobID, toReconciled(pr, mainCIRed, r.requiredChecks), now)
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
// mainCIRed reports whether the integration branch's CI is DEFINITIVELY red — the green-main
// signal threaded onto every reconciled PR this sweep so a review_pending CI failure inherited
// from a broken main does not bounce a good PR. A client that can't read branch CI, or any
// read error, degrades to false (bounce as before) — a missing signal never WITHHOLDS a bounce.
func (r *Reconciler) mainCIRed(ctx context.Context) bool {
	br, ok := r.gh.(gh.BranchCIReader)
	if !ok {
		return false
	}
	branch := r.branch
	if branch == "" {
		branch = "main"
	}
	st, err := br.BranchCIState(ctx, branch)
	if err != nil {
		return false
	}
	return st == gh.CIFailure || st == gh.CIError
}

// ensureRequiredChecks warms r.requiredChecks (the repo's required status-check contexts)
// once via branch protection, then caches it — called at the start of each Sweep, NOT in
// the per-PR / webhook-refetch path (so a targeted refetch issues no extra GitHub call).
// A client that can't read protection, or any error, leaves it nil and the gate uses the
// conservative full-rollup-green rule — a missing signal never LOOSENS the gate.
func (r *Reconciler) ensureRequiredChecks(ctx context.Context) {
	if r.requiredFetched {
		return
	}
	branch := r.branch
	if branch == "" {
		branch = "main"
	}
	// Prefer the rules API (covers modern rulesets — what russ uses). A repo enforcing
	// required checks via a ruleset has NO classic branch protection (that API 404s), so
	// BranchProtection alone would report zero required checks and silently disable the
	// required-checks CI gate. Fall back to classic branch protection for repos that use it.
	if rr, ok := r.gh.(gh.RequiredChecksReader); ok {
		checks, err := rr.BranchRequiredChecks(ctx, branch)
		if err != nil {
			return // transient: leave unfetched so a later sweep retries
		}
		r.requiredFetched = true
		r.requiredChecks = checks
		if len(checks) > 0 {
			return
		}
		// empty from rules — fall through to classic protection (belt-and-suspenders).
	}
	if br, ok := r.gh.(gh.BranchProtectionReader); ok {
		prot, ok, err := br.BranchProtection(ctx, branch)
		if err != nil {
			return
		}
		r.requiredFetched = true
		if ok && len(prot.RequiredChecks) > 0 {
			r.requiredChecks = prot.RequiredChecks
		}
		return
	}
	r.requiredFetched = true // no protection surface at all; never retry
}

// requiredChecksGreen reports whether EVERY required check has passed at the head. Empty
// required => false (caller falls back to the full-rollup rule). A required check that is
// pending/missing/failing is simply absent from PassedChecks, so this stays false until it
// genuinely concludes SUCCESS — it can only ever make the gate match GitHub's required-
// checks policy, never approve a PR whose required check has not passed.
// anyIn reports whether any name in xs is also in ys (set intersection non-empty) —
// used to detect whether a REQUIRED check is among the definitively-failed checks.
func anyIn(xs, ys []string) bool {
	if len(xs) == 0 || len(ys) == 0 {
		return false
	}
	set := make(map[string]bool, len(ys))
	for _, y := range ys {
		set[y] = true
	}
	for _, x := range xs {
		if set[x] {
			return true
		}
	}
	return false
}

func requiredChecksGreen(pr gh.PullRequest, required []string) bool {
	if len(required) == 0 {
		return false
	}
	passed := make(map[string]bool, len(pr.PassedChecks))
	for _, n := range pr.PassedChecks {
		passed[n] = true
	}
	for _, req := range required {
		if !passed[req] {
			return false
		}
	}
	return true
}

func toReconciled(pr gh.PullRequest, mainCIRed bool, requiredChecks []string) store.ReconciledPR {
	// Green/failed are evaluated against the repo's REQUIRED checks when known, else the
	// aggregate rollup. Both paths require a real (non-skipped) passing check — GitHub
	// rolls an ALL-SKIPPED head up to SUCCESS (no test ran), so the aggregate alone would
	// let a paths-filtered or hostile-workflow PR pass on tests that never executed.
	var ciGreen, ciFailed bool
	if len(requiredChecks) > 0 {
		// Required-checks policy (matches GitHub's own merge gate): green iff EVERY required
		// check passed; failed iff a REQUIRED check definitively failed. A non-required
		// (e.g. cosmetic post-merge) check that is pending/failing makes the AGGREGATE
		// UNSTABLE but neither blocks green nor bounces the build — which is the whole point.
		ciGreen = pr.CIHasRealSuccess && requiredChecksGreen(pr, requiredChecks)
		ciFailed = anyIn(pr.FailingChecks, requiredChecks)
	} else {
		// Unknown required checks: the conservative aggregate-only rule (unchanged behavior).
		ciGreen = pr.CIHasRealSuccess && pr.CIRollup == gh.CISuccess
		ciFailed = pr.CIRollup == gh.CIFailure || pr.CIRollup == gh.CIError
	}
	return store.ReconciledPR{
		Number:      pr.Number,
		UpdatedAt:   pr.UpdatedAt,
		IsDraft:     pr.IsDraft,
		Merged:      pr.Merged,
		HeadSHA:     pr.HeadRefOid,
		BaseSHA:     pr.BaseRefOid,
		MergeCommit: pr.MergeCommit,
		MainCIRed:   mainCIRed,
		CIGreen:     ciGreen,
		CIFailed:    ciFailed,
		// the NAMES of the failed checks, carried to a bounced build so the rebuild brief
		// tells the agent exactly which gate to re-run + fix (not a generic "CI was red").
		FailingChecks:    pr.FailingChecks,
		FailingCheckURLs: pr.FailingCheckURLs,
		ClosedUnmerged:   pr.ClosedUnmerged,
	}
}
