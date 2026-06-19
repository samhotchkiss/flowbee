package store_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedBuildPR(t *testing.T, st *store.Store, id string, pr int) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base0",
		RequiredCapabilities: []string{"role:eng_worker"}, Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if pr > 0 {
		if err := st.BindPRNumber(ctx, id, pr); err != nil {
			t.Fatalf("bind pr: %v", err)
		}
	}
}

// markMergeable moves a seeded job into `mergeable` — a realistic pre-merge state that
// legitimately owns a reviewable/merging PR. A merged PR completes a job only from such
// a state (prBoundActive), never from bare `ready`/`building` or a parked `needs_human`.
func markMergeable(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if _, err := st.DB.ExecContext(context.Background(),
		`UPDATE jobs SET state='mergeable' WHERE id=?`, id); err != nil {
		t.Fatalf("mark mergeable %s: %v", id, err)
	}
}

// TestReconcileWritesDomainBFacts: a sweep populates the Domain-B fact columns to
// match the scripted PR. (The DONE-WHEN "sweep populates Domain-B columns".)
func TestReconcileWritesDomainBFacts(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "j1", 7)

	now := time.Unix(2000, 0)
	out, err := st.ApplyReconciledPR(ctx, "j1", store.ReconciledPR{
		Number: 7, UpdatedAt: now, HeadSHA: "h1", BaseSHA: "b1", CIGreen: true,
	}, now)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !out.Applied {
		t.Fatalf("facts not applied")
	}
	facts, ok, err := store.DBFactSource{DB: st.DB}.Facts(ctx, "j1")
	if err != nil || !ok {
		t.Fatalf("read facts ok=%v err=%v", ok, err)
	}
	if !facts.PRExists || facts.PRNumber != 7 || facts.HeadSHA != "h1" || facts.BaseSHA != "b1" || !facts.CIGreen {
		t.Fatalf("facts mismatch: %+v", facts)
	}
}

// TestReconcileNeverWritesDomainAField: the DONE-WHEN keystone. reconcile-IN
// ingesting facts must NEVER write a Domain-A field (stage/role/lens/verdict/
// counters). We snapshot the Domain-A fields of a job in code_review with a
// verdict-relevant state, run a NON-superseding reconcile (same SHA, CI flip), and
// assert every Domain-A field is byte-identical afterward.
func TestReconcileNeverWritesDomainAField(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jA", 11)

	// establish a reconciled baseline (head h1) and drive the job into code_review
	// via the normal worker path so it carries Domain-A state worth protecting.
	now := time.Unix(2000, 0)
	if err := st.SetReconciledFacts(ctx, "jA", store.ReconciledPR{
		Number: 11, UpdatedAt: now, HeadSHA: "h1", BaseSHA: "b1", CIGreen: false,
	}); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// manually move the job to code_review with a bound reviewer + bumped counters,
	// to give the Domain-A snapshot teeth (these are the fields reconcile may not touch).
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='code_review', role='code_reviewer', stage='review',
		       bounces=2, attempts=1, bound_lens='critical_reviewer',
		       lease_epoch=5 WHERE id='jA'`); err != nil {
		t.Fatalf("setup domain-A: %v", err)
	}

	before := snapshotDomainA(t, st, "jA")

	// a reconcile that writes Domain-B facts (CI flips green) at the SAME head SHA:
	// NOT a supersession, NOT a merge — purely a Domain-B fact update.
	out, err := st.ApplyReconciledPR(ctx, "jA", store.ReconciledPR{
		Number: 11, UpdatedAt: now.Add(time.Minute), HeadSHA: "h1", BaseSHA: "b1", CIGreen: true,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !out.Applied || out.Superseded || out.Done || out.Frozen {
		t.Fatalf("unexpected outcome: %+v", out)
	}

	after := snapshotDomainA(t, st, "jA")
	if before != after {
		t.Fatalf("reconcile-IN wrote a Domain-A field:\n before=%+v\n after =%+v", before, after)
	}
	// and the Domain-B fact DID change (CI is now green).
	facts, _, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jA")
	if !facts.CIGreen {
		t.Fatalf("Domain-B CI fact not updated")
	}
}

// domainA captures every Domain-A field reconcile-IN must leave untouched.
type domainA struct {
	State, Role, Stage, Lens, Verdict string
	Bounces, Attempts, MaxBounces     int
}

func snapshotDomainA(t *testing.T, st *store.Store, id string) domainA {
	t.Helper()
	var d domainA
	var lens, verdict *string
	err := st.DB.QueryRowContext(context.Background(), `
		SELECT state, role, stage, COALESCE(bound_lens,''), COALESCE(verdict,''),
		       bounces, attempts, max_bounces FROM jobs WHERE id = ?`, id).
		Scan(&d.State, &d.Role, &d.Stage, &d.Lens, &d.Verdict, &d.Bounces, &d.Attempts, &d.MaxBounces)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	_ = lens
	_ = verdict
	return d
}

// TestSHAMonotonicGuard: an ingest OLDER than the recorded high-water-mark is
// ignored — late/out-of-order deliveries cannot rewind state (I-3).
func TestSHAMonotonicGuard(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jm", 3)

	t1 := time.Unix(5000, 0)
	if _, err := st.ApplyReconciledPR(ctx, "jm", store.ReconciledPR{
		Number: 3, UpdatedAt: t1, HeadSHA: "new", BaseSHA: "b", CIGreen: true,
	}, t1); err != nil {
		t.Fatalf("apply t1: %v", err)
	}
	// a LATE delivery (older updatedAt) carrying a stale head: must be ignored.
	t0 := t1.Add(-time.Hour)
	out, err := st.ApplyReconciledPR(ctx, "jm", store.ReconciledPR{
		Number: 3, UpdatedAt: t0, HeadSHA: "old", BaseSHA: "b", CIGreen: false,
	}, t0)
	if err != nil {
		t.Fatalf("apply t0: %v", err)
	}
	if out.Applied {
		t.Fatalf("stale ingest was applied; must be ignored (SHA-monotonic guard)")
	}
	facts, _, _ := store.DBFactSource{DB: st.DB}.Facts(ctx, "jm")
	if facts.HeadSHA != "new" || !facts.CIGreen {
		t.Fatalf("stale ingest rewound state: %+v", facts)
	}
}

// TestTerminalSHAGuard: once a job's merge commit is recorded (terminal Domain-B
// fact), NO later event re-dispatches it (I-3). The job is frozen at done.
func TestTerminalSHAGuard(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jt", 9)

	markMergeable(t, st, "jt")

	t1 := time.Unix(6000, 0)
	// reconcile a merged PR: terminal. The job goes done; merge_commit recorded.
	out, err := st.ApplyReconciledPR(ctx, "jt", store.ReconciledPR{
		Number: 9, UpdatedAt: t1, HeadSHA: "h", BaseSHA: "b", Merged: true, MergeCommit: "merge-sha",
	}, t1)
	if err != nil {
		t.Fatalf("apply merged: %v", err)
	}
	if !out.Done {
		t.Fatalf("merged PR did not transition job to done: %+v", out)
	}
	if j, _ := st.GetJob(ctx, "jt"); j.State != job.StateDone {
		t.Fatalf("state=%s want done", j.State)
	}
	// a later (even newer) event must NOT re-dispatch the settled job.
	out2, err := st.ApplyReconciledPR(ctx, "jt", store.ReconciledPR{
		Number: 9, UpdatedAt: t1.Add(time.Hour), HeadSHA: "h2", BaseSHA: "b", CIGreen: false,
	}, t1.Add(time.Hour))
	if err != nil {
		t.Fatalf("apply post-terminal: %v", err)
	}
	if !out2.Frozen || out2.Applied {
		t.Fatalf("terminal guard did not freeze: %+v", out2)
	}
	if j, _ := st.GetJob(ctx, "jt"); j.State != job.StateDone {
		t.Fatalf("settled job re-dispatched: state=%s", j.State)
	}
}

// TestSupersedeOnSHAMove: a new head SHA on an open PR (with the job past build)
// supersedes the SHA-bound verdict and re-arms to ready against the new base, with
// the lease epoch bumped (I-5, §6.2.4). And the ledger replays to the same row.
func TestSupersedeOnSHAMove(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "js", 21)

	t1 := time.Unix(7000, 0)
	if _, err := st.ApplyReconciledPR(ctx, "js", store.ReconciledPR{
		Number: 21, UpdatedAt: t1, HeadSHA: "h1", BaseSHA: "b1", CIGreen: true,
	}, t1); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// move the job to mergeable with a minted verdict bound to (h1,b1) and epoch 4.
	v := job.MintVerdict(job.VerdictApproved, job.DispositionHandoff, "h1", "b1")
	vjson := mustJSON(t, v)
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='mergeable', verdict=?, head_sha='h1', lease_epoch=4 WHERE id='js'`, vjson); err != nil {
		t.Fatalf("set mergeable: %v", err)
	}

	// reconcile-IN observes a NEW head SHA (h2): supersede + re-arm.
	t2 := t1.Add(time.Minute)
	out, err := st.ApplyReconciledPR(ctx, "js", store.ReconciledPR{
		Number: 21, UpdatedAt: t2, HeadSHA: "h2", BaseSHA: "b2", CIGreen: false,
	}, t2)
	if err != nil {
		t.Fatalf("apply move: %v", err)
	}
	if !out.Superseded {
		t.Fatalf("SHA move did not supersede: %+v", out)
	}
	j, _ := st.GetJob(ctx, "js")
	if j.State != job.StateReady {
		t.Fatalf("state=%s want ready (re-armed)", j.State)
	}
	if j.Role != job.RoleEngWorker {
		t.Fatalf("role=%s want eng_worker", j.Role)
	}
	if j.Verdict != nil {
		t.Fatalf("verdict not invalidated on supersession")
	}
	if j.BaseSHA != "b2" {
		t.Fatalf("base not re-armed to new base: %s", j.BaseSHA)
	}
	if j.LeaseEpoch != 5 {
		t.Fatalf("lease epoch=%d want 5 (bumped on revoke)", j.LeaseEpoch)
	}

	// replay determinism: Fold(events) == the projection (Domain-A subset).
	events, err := st.LoadEvents(ctx, "js")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	folded, err := ledger.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.State != j.State || folded.Role != j.Role || folded.BaseSHA != j.BaseSHA ||
		folded.LeaseEpoch != j.LeaseEpoch || folded.Verdict != nil {
		t.Fatalf("fold != projection:\n fold=%+v\n proj=%+v", folded, j)
	}
	// the supersede resets Stage + RequiredCapabilities too (the job re-arms as a
	// build): a one-sided change to supersedeTx or the KindSuperseded fold that drops
	// either would strand the re-armed job on a resync — guard both like the bounce path.
	if folded.Stage != j.Stage {
		t.Fatalf("stage diverged on supersede: fold=%q proj=%q", folded.Stage, j.Stage)
	}
	if !reflect.DeepEqual(folded.RequiredCapabilities, j.RequiredCapabilities) {
		t.Fatalf("required_capabilities diverged on supersede: fold=%v proj=%v", folded.RequiredCapabilities, j.RequiredCapabilities)
	}
}

// TestMergeHandoffNotSuperseded pins the loop fix: a job handed to a human
// (merge_handoff — e.g. a change to Flowbee's own source the flowbee_source denylist
// blocks from self-merge) must SETTLE, not get re-armed. The reviewer's empty
// findings-commit moves the branch head after the verdict bound to the reviewed head,
// so a supersedable merge_handoff looped handoff→supersede→rebuild→re-review forever
// (the live #41 stall). A head move must leave merge_handoff untouched.
func TestMergeHandoffNotSuperseded(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jh", 41)

	t1 := time.Unix(7000, 0)
	if _, err := st.ApplyReconciledPR(ctx, "jh", store.ReconciledPR{
		Number: 41, UpdatedAt: t1, HeadSHA: "h1", BaseSHA: "b1", CIGreen: true,
	}, t1); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `
		UPDATE jobs SET state='merge_handoff', head_sha='h1', lease_epoch=4 WHERE id='jh'`); err != nil {
		t.Fatalf("set merge_handoff: %v", err)
	}

	// the reviewer's empty findings-commit moved the head to h2: must NOT supersede.
	t2 := t1.Add(time.Minute)
	out, err := st.ApplyReconciledPR(ctx, "jh", store.ReconciledPR{
		Number: 41, UpdatedAt: t2, HeadSHA: "h2", BaseSHA: "b1", CIGreen: true,
	}, t2)
	if err != nil {
		t.Fatalf("apply move: %v", err)
	}
	if out.Superseded {
		t.Fatalf("merge_handoff was superseded on a head move — the human-merge loop bug")
	}
	j, _ := st.GetJob(ctx, "jh")
	if j.State != job.StateMergeHandoff {
		t.Fatalf("state=%s want merge_handoff (settled for the human)", j.State)
	}
}

// TestUnboundPRNoOp: a swept PR not bound to any job is a no-op (no error).
func TestUnboundPRNoOp(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	_, ok, err := st.JobIDForPR(ctx, 999)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if ok {
		t.Fatalf("unbound PR resolved to a job")
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

// TestBaseRefreshOnMerge: when a PR merges, every still-`ready` build in the repo
// advances its base_sha to the new main (the merge commit) so it builds on CURRENT
// code — the literal "base_sha refresh after merge". A KindBaseRefreshed event keeps
// projection == re-fold (base_sha is folded). The just-merged job is excluded.
func TestBaseRefreshOnMerge(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(8000, 0)

	// a ready build adopted at an OLD base (no PR).
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: "ready1", Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
		BaseSHA: "OLD", RequiredCapabilities: []string{"role:eng_worker"}, Now: now,
	}); err != nil {
		t.Fatal(err)
	}
	// a sibling that merges (in a realistic pre-merge state: a merged PR completes a job
	// only from a state that owns a reviewable/merging PR, not bare `ready`).
	seedBuildPR(t, st, "merged1", 9)
	markMergeable(t, st, "merged1")
	if _, err := st.ApplyReconciledPR(ctx, "merged1", store.ReconciledPR{
		Number: 9, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b", Merged: true, MergeCommit: "NEWMAIN",
	}, now); err != nil {
		t.Fatalf("apply merge: %v", err)
	}

	j, _ := st.GetJob(ctx, "ready1")
	if j.BaseSHA != "NEWMAIN" {
		t.Fatalf("ready job base=%s, want NEWMAIN (refreshed on merge)", j.BaseSHA)
	}
	// determinism: Fold(events) reproduces the refreshed base.
	evs, _ := st.LoadEvents(ctx, "ready1")
	folded, _ := ledger.Fold(evs)
	if folded.BaseSHA != "NEWMAIN" {
		t.Fatalf("fold base=%s != projection NEWMAIN — KindBaseRefreshed not folded", folded.BaseSHA)
	}
	// the merged job itself is done, not "refreshed".
	if m, _ := st.GetJob(ctx, "merged1"); m.State != job.StateDone {
		t.Fatalf("merged job state=%s want done", m.State)
	}
}

// TestReconcileParksOnClosedUnmergedPR: when a human CLOSES a job's PR without merging,
// reconcile parks the job at needs_human with the legible pr_closed reason — promptly,
// instead of waiting on a merge that never comes (the old behavior: a slow, misleading
// stall escalation ~4×lease_ttl later).
func TestReconcileParksOnClosedUnmergedPR(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	seedBuildPR(t, st, "jc", 7)
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='review_pending' WHERE id='jc'`); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(6000, 0)
	if _, err := st.ApplyReconciledPR(ctx, "jc", store.ReconciledPR{
		Number: 7, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b", ClosedUnmerged: true,
	}, now); err != nil {
		t.Fatalf("apply: %v", err)
	}
	j, _ := st.GetJob(ctx, "jc")
	if j.State != job.StateNeedsHuman {
		t.Fatalf("closed-unmerged PR must park the job at needs_human, got %s", j.State)
	}
	if j.EscalationReason != string(job.EscalationPRClosed) {
		t.Fatalf("escalation_reason=%q want %q", j.EscalationReason, job.EscalationPRClosed)
	}

	// a MERGED PR (not closed-unmerged) still goes to done, not parked.
	seedBuildPR(t, st, "jm2", 8)
	_, _ = st.DB.ExecContext(ctx, `UPDATE jobs SET state='merging' WHERE id='jm2'`)
	if _, err := st.ApplyReconciledPR(ctx, "jm2", store.ReconciledPR{
		Number: 8, UpdatedAt: now, HeadSHA: "h", BaseSHA: "b", Merged: true, MergeCommit: "m",
	}, now); err != nil {
		t.Fatalf("apply merged: %v", err)
	}
	if j2, _ := st.GetJob(ctx, "jm2"); j2.State != job.StateDone {
		t.Fatalf("merged PR must be done, got %s", j2.State)
	}
}

// TestMergedPRDoesNotCompleteIllegalStates locks the §6.2 edge-legality fix: a merged
// PR completes a job ONLY from a state that legitimately owns a reviewable/merging PR
// (prBoundActive). It must NOT drag a parked or pre-review job to `done` — most
// importantly a `needs_human` job (escalated, pr_number not cleared) whose PR a human
// merges, which would silently ERASE the §12.6.1 human gate; nor a superseded-back-to-
// `ready` job (skipping re-review).
func TestMergedPRDoesNotCompleteIllegalStates(t *testing.T) {
	ctx := context.Background()
	for _, st0 := range []string{"needs_human", "ready", "building"} {
		t.Run(st0, func(t *testing.T) {
			st := testutil.NewStore(t)
			seedBuildPR(t, st, "j", 9)
			if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state=? WHERE id='j'`, st0); err != nil {
				t.Fatal(err)
			}
			out, err := st.ApplyReconciledPR(ctx, "j", store.ReconciledPR{
				Number: 9, UpdatedAt: time.Unix(9000, 0), HeadSHA: "h", BaseSHA: "b",
				Merged: true, MergeCommit: "deadbeef",
			}, time.Unix(9000, 0))
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if out.Done {
				t.Fatalf("a merged PR illegally completed a %s job (review skipped / human gate erased)", st0)
			}
			if j, _ := st.GetJob(ctx, "j"); j.State != job.State(st0) {
				t.Fatalf("state mutated %s -> %s on an illegal merge-complete", st0, j.State)
			}
		})
	}
}
