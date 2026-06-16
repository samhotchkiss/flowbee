package reconcile_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/clock"
	gh "github.com/samhotchkiss/flowbee/internal/github"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/reconcile"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seed(t *testing.T, st *store.Store, id string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := st.BindPRNumber(ctx, id, pr); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

// TestSweepMatchesScriptedRepo: a sweep populates Domain-B columns to match the
// scripted fakeGitHub repo, and records the rate-limit gauge (I-14). The DONE-WHEN
// "sweep populates Domain-B columns to match a test repo".
func TestSweepMatchesScriptedRepo(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jx", 100)
	seed(t, st, "jy", 101)

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 100, HeadRefOid: "ha", BaseRefOid: "ba", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(10, 0)})
	f.SetPR(gh.PullRequest{Number: 101, HeadRefOid: "hb", BaseRefOid: "bb", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(11, 0)})
	f.SetRateLimit(gh.RateLimit{Limit: 5000, Remaining: 4321, ResetAt: time.Unix(99999, 0)})

	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	src := store.DBFactSource{DB: st.DB}
	fa, _, _ := src.Facts(ctx, "jx")
	if fa.HeadSHA != "ha" || fa.BaseSHA != "ba" || !fa.CIGreen || !fa.PRExists {
		t.Fatalf("jx facts: %+v", fa)
	}
	fb, _, _ := src.Facts(ctx, "jy")
	if fb.HeadSHA != "hb" || fb.CIGreen {
		t.Fatalf("jy facts: %+v", fb)
	}
	g, _ := st.RateLimit(ctx)
	if g.Remaining != 4321 || g.Limit != 5000 {
		t.Fatalf("budget gauge not recorded: %+v", g)
	}
}

// TestSweepSupersedesOnNewCommit: a new commit (head SHA move) to an open PR whose
// job holds a SHA-bound verdict -> superseded + re-armed (the DONE-WHEN "new commit
// to an open PR -> job superseded + re-armed").
func TestSweepSupersedesOnNewCommit(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jc", 200)

	f := gh.NewFake()
	f.SetPR(gh.PullRequest{Number: 200, HeadRefOid: "h1", BaseRefOid: "b1", CIRollup: gh.CISuccess, UpdatedAt: time.Unix(10, 0)})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(20, 0)), nil)
	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("sweep1: %v", err)
	}
	// move the job to mergeable with a verdict bound to (h1,b1).
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", "b1")
	mustExec(t, st, `UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1' WHERE id='jc'`, mustJSON(t, v))

	// a NEW commit lands: head moves to h2.
	f.SetPR(gh.PullRequest{Number: 200, HeadRefOid: "h2", BaseRefOid: "b1", CIRollup: gh.CIPending, UpdatedAt: time.Unix(30, 0)})
	outs, err := rec.Sweep(ctx)
	if err != nil {
		t.Fatalf("sweep2: %v", err)
	}
	var superseded bool
	for _, o := range outs {
		if o.JobID == "jc" && o.Superseded {
			superseded = true
		}
	}
	if !superseded {
		t.Fatalf("new commit did not supersede: outs=%+v", outs)
	}
	j, _ := st.GetJob(ctx, "jc")
	if j.State != job.StateReady || j.Verdict != nil || j.Role != job.RoleEngWorker {
		t.Fatalf("not re-armed: %+v", j)
	}
}

// TestRefetchReadsRealState: a targeted refetch reads the REAL scripted PR state.
// Even if a webhook CLAIMED a PR was approved/merged, the refetch reconciles the
// truth (here: still open, CI red) and cannot fast-track (§8.1.3).
func TestRefetchReadsRealState(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seed(t, st, "jr", 300)

	f := gh.NewFake()
	// the REAL state: open, CI red, NOT merged.
	f.SetPR(gh.PullRequest{Number: 300, HeadRefOid: "h", BaseRefOid: "b", CIRollup: gh.CIFailure, UpdatedAt: time.Unix(5, 0)})
	rec := reconcile.New(st, f, clock.NewFake(time.Unix(10, 0)), nil)

	_, reconciled, err := rec.Refetch(ctx, 300)
	if err != nil || !reconciled {
		t.Fatalf("refetch reconciled=%v err=%v", reconciled, err)
	}
	fa, _, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jr")
	if fa.Merged || fa.CIGreen {
		t.Fatalf("refetch fast-tracked a lie: facts=%+v", fa)
	}
	j, _ := st.GetJob(ctx, "jr")
	if j.State == job.StateDone {
		t.Fatalf("refetch merged an un-merged PR")
	}
}

func mustExec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(), q, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}
