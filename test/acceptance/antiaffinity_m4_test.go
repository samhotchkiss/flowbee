// M4 acceptance: enforced anti-affinity at lease time (I-10, §5.5 / §6.3.1),
// proven end-to-end over the real HTTP surface against a real SQLite store.
//
// DONE-WHEN (each proven below by a real, non-skipped test):
//   - a worker that BUILT job J never wins J's code_review lease (0 rows / 204);
//   - a same-model_family worker is excluded from the code_review lease;
//   - a DISTINCT-identity, DISTINCT-family reviewer DOES win (the term is an
//     exclusion, not a blanket block — independence is satisfiable);
//   - with ONLY model:codex workers, the review stage raises no_eligible_worker;
//   - the exclusion holds under -race.
//
// The anti-affinity input is the BUILDER's identity + model_family, persisted
// durably when the build result lands review_pending (the live bound_* columns
// are cleared at that point). The §6.3.1 claim's NOT EXISTS reads those durable
// columns; a claim that would violate independence returns 0 rows (ErrLostRace),
// the job stays review_pending, and its no_eligible_worker alarm can fire.
package acceptance

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/alarm"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// registerReviewer enrolls a worker that OFFERS the code_reviewer role with the
// given identity + model_family (so capability match is satisfied and only
// anti-affinity can exclude it).
func registerReviewer(t *testing.T, ctx context.Context, url, identity, family string) *client.Client {
	t.Helper()
	c := client.New(url)
	if _, err := c.Register(ctx, client.Registration{
		WorkerID: "wk-" + identity, Identity: identity, Host: "test",
		Capabilities: []string{"role:code_reviewer", "model_family:" + family},
	}); err != nil {
		t.Fatalf("register reviewer %s: %v", identity, err)
	}
	return c
}

// seedAndBuild seeds a build job and has the named builder (identity+family)
// produce a result, landing it in review_pending with the builder identity
// persisted durably. Returns nothing; the job is left review_pending.
func seedAndBuild(t *testing.T, ctx context.Context, st *store.Store, url, jobID, builderID, builderFamily string) {
	t.Helper()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base1",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed %s: %v", jobID, err)
	}
	builder := registerWorker(t, ctx, url, builderID, builderFamily)
	g, ok, err := builder.Lease(ctx, builderID, builderFamily, "")
	if err != nil || !ok || g.JobID != jobID {
		t.Fatalf("builder lease ok=%v err=%v job=%s", ok, err, g.JobID)
	}
	if _, _, err := builder.Result(ctx, jobID, g.LeaseEpoch, "build-1", map[string]any{"kind": "patch", "base_sha": "base1"}); err != nil {
		t.Fatalf("builder result: %v", err)
	}
	j, _ := st.GetJob(ctx, jobID)
	if j.State != job.StateReviewPending {
		t.Fatalf("after build state=%s want review_pending", j.State)
	}
	// reconcile green facts so the review gate OFFERS the job (the §6.3.1
	// anti-affinity exclusions still apply at CLAIM time; ci_green gates candidacy).
	if err := st.UpsertDomainBFacts(ctx, jobID, job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "head1", BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatalf("seed green facts: %v", err)
	}
}

func newM4Server(st *store.Store, clk clock.Clock) *api.Server {
	return api.New(st, clk, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 300 * time.Millisecond,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "m4")
}

// TestM4BuilderNeverWinsItsOwnReview proves the no-self-review term: the worker
// that built J — same identity — is excluded from J's code_review lease (0 rows
// -> 204), the job stays review_pending, and an independent reviewer still wins.
func TestM4BuilderNeverWinsItsOwnReview(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM4Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-self"
	seedAndBuild(t, ctx, st, ts.URL, jobID, "builder-alice", "codex")

	// the BUILDER offers to review (its identity also attests role:code_reviewer).
	// It must NOT win its own review — the §6.3.1 identity term excludes it.
	selfReviewer := client.New(ts.URL)
	if _, err := selfReviewer.Register(ctx, client.Registration{
		WorkerID: "wk-builder-alice", Identity: "builder-alice", Host: "test",
		Capabilities: []string{"role:eng_worker", "role:code_reviewer", "model_family:codex"},
	}); err != nil {
		t.Fatalf("re-register builder as reviewer: %v", err)
	}
	if g, ok, err := selfReviewer.Lease(ctx, "builder-alice", "codex", string(job.RoleCodeReviewer)); err != nil || ok {
		t.Fatalf("builder must NOT win its own review (I-10): ok=%v job=%s err=%v", ok, g.JobID, err)
	}
	// the job is untouched — still review_pending, awaiting an independent reviewer.
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("after rejected self-review state=%s want review_pending", j.State)
	}

	// a DISTINCT-identity, DISTINCT-family reviewer DOES win (independence holds).
	reviewer := registerReviewer(t, ctx, ts.URL, "reviewer-bob", "opus")
	rg, ok, err := reviewer.Lease(ctx, "reviewer-bob", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != jobID {
		t.Fatalf("independent reviewer must win: ok=%v err=%v job=%s", ok, err, rg.JobID)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateCodeReview {
		t.Fatalf("after independent review lease state=%s want code_review", j.State)
	}
}

// TestM4SameModelFamilyExcluded proves the model_family term: a reviewer with a
// DISTINCT identity but the SAME model_family as the builder is excluded
// (uncorrelated failure modes, §5.5) — yet a different family wins.
func TestM4SameModelFamilyExcluded(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM4Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-family"
	seedAndBuild(t, ctx, st, ts.URL, jobID, "builder-alice", "codex")

	// a distinct-IDENTITY reviewer, but SAME model_family (codex) as the builder.
	// It must NOT win — the model_family anti-affinity term excludes it.
	sameFam := registerReviewer(t, ctx, ts.URL, "reviewer-carol", "codex")
	if g, ok, err := sameFam.Lease(ctx, "reviewer-carol", "codex", string(job.RoleCodeReviewer)); err != nil || ok {
		t.Fatalf("same-model_family reviewer must NOT win (I-10): ok=%v job=%s err=%v", ok, g.JobID, err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("after rejected same-family review state=%s want review_pending", j.State)
	}

	// a distinct family DOES win.
	diff := registerReviewer(t, ctx, ts.URL, "reviewer-dave", "opus")
	rg, ok, err := diff.Lease(ctx, "reviewer-dave", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != jobID {
		t.Fatalf("distinct-family reviewer must win: ok=%v err=%v job=%s", ok, err, rg.JobID)
	}
}

// TestM4LegacyReviewPendingWithUnknownBuilderFamilyDrains pins the live #225 backlog
// shape: old review_pending rows had the reviewer capability but no persisted
// builder_model_family. Unknown builder family must fail open for a DIFFERENT reviewer;
// otherwise adding a second-family reviewer cannot drain historical review backlog.
func TestM4LegacyReviewPendingWithUnknownBuilderFamilyDrains(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM4Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "legacy-review", Kind: job.KindBuild, Flow: "build", Stage: "review",
		Role: job.RoleEngWorker, BaseSHA: "base1",
		RequiredCapabilities: []string{"role:code_reviewer"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed legacy review: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs
		   SET state='review_pending',
		       required_capabilities='["role:code_reviewer"]',
		       builder_identity=NULL,
		       builder_model_family=NULL,
		       bound_identity=NULL,
		       bound_model_family=NULL,
		       eng_worker_job=id
		 WHERE id='legacy-review'`); err != nil {
		t.Fatalf("shape legacy review row: %v", err)
	}
	if err := st.UpsertDomainBFacts(ctx, "legacy-review", job.DomainBFacts{
		PRExists: true, PRNumber: 1, HeadSHA: "head1", BaseSHA: "base1", CIGreen: true,
	}); err != nil {
		t.Fatalf("seed facts: %v", err)
	}

	reviewer := registerReviewer(t, ctx, ts.URL, "reviewer-claude", "opus")
	rg, ok, err := reviewer.Lease(ctx, "reviewer-claude", "opus", string(job.RoleCodeReviewer))
	if err != nil || !ok || rg.JobID != "legacy-review" {
		t.Fatalf("cross-family reviewer must drain legacy unknown-family review: ok=%v err=%v job=%s", ok, err, rg.JobID)
	}
	if j, _ := st.GetJob(ctx, "legacy-review"); j.State != job.StateCodeReview {
		t.Fatalf("after legacy review lease state=%s want code_review", j.State)
	}
}

// TestM4SingleProviderFleetRaisesNoEligibleWorker proves the §5.6 surface: with
// ONLY model:codex workers (the builder built codex; the only reviewer offering
// is also codex), the model_family term is unsatisfiable, no reviewer can claim
// the review_pending job, and the review stage raises no_eligible_worker (I-6)
// rather than silently collapsing review independence.
func TestM4SingleProviderFleetRaisesNoEligibleWorker(t *testing.T) {
	st := testutil.NewStore(t)
	st.NoEligibleWorkerDelay = 30 * time.Second
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM4Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-mono"
	seedAndBuild(t, ctx, st, ts.URL, jobID, "builder-alice", "codex")

	// the only reviewer-offering worker is ALSO codex — same family as the builder,
	// so the model_family term excludes it. It cannot win.
	monoReviewer := registerReviewer(t, ctx, ts.URL, "reviewer-codex2", "codex")
	if g, ok, err := monoReviewer.Lease(ctx, "reviewer-codex2", "codex", string(job.RoleCodeReviewer)); err != nil || ok {
		t.Fatalf("codex-only fleet must not satisfy review independence: ok=%v job=%s err=%v", ok, g.JobID, err)
	}
	if j, _ := st.GetJob(ctx, jobID); j.State != job.StateReviewPending {
		t.Fatalf("review job state=%s want still review_pending", j.State)
	}

	// drive the durable-timer poller past the alarm window: the review stage alarms.
	poller := alarm.New(st, clk, time.Hour, srv.Broker())
	clk.Advance(31 * time.Second)
	poller.Tick(ctx)

	if ok, _ := st.AlarmFired(ctx, jobID, store.TimerNoEligibleWorker); !ok {
		t.Fatal("single-provider review stage must raise no_eligible_worker (I-6/§5.6)")
	}
	// and it is in the ledger (reconstructable by replay), at review_pending.
	evs, _ := st.LoadEvents(ctx, jobID)
	var alarmed bool
	for _, e := range evs {
		if e.Kind == ledger.KindNoEligibleWorker {
			alarmed = true
			if e.ToState != job.StateReviewPending {
				t.Fatalf("alarm event to_state=%s want review_pending", e.ToState)
			}
		}
	}
	if !alarmed {
		t.Fatal("no_eligible_worker event missing from the review job's ledger")
	}
}

// TestM4SelfReviewExclusionUnderRace hammers the claim: the builder and an
// independent reviewer race for the same review_pending job concurrently. The
// builder must NEVER win (anti-affinity holds under -race); exactly the
// independent reviewer wins, and the job lands code_review bound to the
// independent identity. Run with -race -count to exercise the serialized claim.
func TestM4SelfReviewExclusionUnderRace(t *testing.T) {
	st := testutil.NewStore(t)
	clk := clock.NewFake(time.Unix(5000, 0))
	srv := newM4Server(st, clk)
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := "job-race"
	seedAndBuild(t, ctx, st, ts.URL, jobID, "builder-alice", "codex")

	// builder also offers to review (must always lose); reviewer-bob is independent.
	selfReviewer := client.New(ts.URL)
	if _, err := selfReviewer.Register(ctx, client.Registration{
		WorkerID: "wk-builder-alice", Identity: "builder-alice", Host: "test",
		Capabilities: []string{"role:eng_worker", "role:code_reviewer", "model_family:codex"},
	}); err != nil {
		t.Fatalf("re-register builder: %v", err)
	}
	reviewer := registerReviewer(t, ctx, ts.URL, "reviewer-bob", "opus")

	type res struct {
		who string
		g   client.LeaseGrant
		ok  bool
	}
	var (
		mu      sync.Mutex
		results []res
		wg      sync.WaitGroup
	)
	race := func(who string, c *client.Client, fam string) {
		defer wg.Done()
		g, ok, err := c.Lease(ctx, who, fam, string(job.RoleCodeReviewer))
		if err != nil {
			return
		}
		mu.Lock()
		results = append(results, res{who: who, g: g, ok: ok})
		mu.Unlock()
	}
	wg.Add(2)
	go race("builder-alice", selfReviewer, "codex")
	go race("reviewer-bob", reviewer, "opus")
	wg.Wait()

	winners := 0
	for _, r := range results {
		if !r.ok {
			continue
		}
		winners++
		if r.who == "builder-alice" {
			t.Fatalf("builder won its own review under race (I-10 violated): %+v", r.g)
		}
		if r.g.JobID != jobID {
			t.Fatalf("winner leased %s want %s", r.g.JobID, jobID)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one reviewer to win, got %d", winners)
	}
	j, _ := st.GetJob(ctx, jobID)
	if j.State != job.StateCodeReview || j.BoundIdentity != "reviewer-bob" {
		t.Fatalf("job state=%s bound=%s want code_review/reviewer-bob", j.State, j.BoundIdentity)
	}
}
