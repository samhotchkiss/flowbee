package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/lease"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func seedBuild(t *testing.T, st *store.Store, id string) job.Job {
	t.Helper()
	j, err := st.SeedJob(context.Background(), store.SeedParams{
		ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-" + id, Now: time.Unix(1000, 0),
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if j.State != job.StateReady {
		t.Fatalf("seeded state=%s want ready", j.State)
	}
	return j
}

func claim(st *store.Store, jobID, identity string) (*lease.Lease, error) {
	return st.ClaimReadyJob(context.Background(), store.ClaimParams{
		JobID: jobID, LeaseID: "lease-" + jobID + "-" + identity, Identity: identity,
		ModelFamily: "fam-" + identity, Role: job.RoleEngWorker,
		TTL: 5 * time.Minute, Now: time.Unix(2000, 0),
	})
}

// TestExactlyOneClaimWinsRace: N goroutines race one ready job. Exactly one wins;
// the rest get ErrLostRace.
func TestExactlyOneClaimWinsRace(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "raceJob")

	const N = 16
	var wins int32
	var mu sync.Mutex
	var winnerEpoch int
	var lostRace int
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			start.Wait()
			ls, err := claim(st, "raceJob", fmt.Sprintf("w%d", i))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
				winnerEpoch = ls.Epoch
			case errors.Is(err, lease.ErrLostRace):
				lostRace++
			default:
				t.Errorf("unexpected claim error: %v", err)
			}
		}(i)
	}
	start.Done()
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d", wins)
	}
	if lostRace != N-1 {
		t.Fatalf("expected %d lost-race, got %d", N-1, lostRace)
	}
	if winnerEpoch != 1 {
		t.Fatalf("winner epoch=%d want 1", winnerEpoch)
	}
	j, _ := st.GetJob(context.Background(), "raceJob")
	if j.State != job.StateLeased {
		t.Fatalf("job state=%s want leased", j.State)
	}
}

// TestNoDoubleLeaseUnderLoad: many goroutines / several jobs; each job leased to
// exactly one worker. Run with -race -count=N.
func TestNoDoubleLeaseUnderLoad(t *testing.T) {
	st := testutil.NewStore(t)
	const jobs = 5
	for j := 0; j < jobs; j++ {
		seedBuild(t, st, fmt.Sprintf("job%d", j))
	}

	var mu sync.Mutex
	winners := map[string]int{}
	var start sync.WaitGroup
	start.Add(1)
	var wg sync.WaitGroup
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			start.Wait()
			for j := 0; j < jobs; j++ {
				jobID := fmt.Sprintf("job%d", j)
				ls, err := claim(st, jobID, fmt.Sprintf("w%d", w))
				if err == nil {
					mu.Lock()
					winners[jobID]++
					mu.Unlock()
					_ = ls
				} else if !errors.Is(err, lease.ErrLostRace) {
					t.Errorf("claim %s: %v", jobID, err)
				}
			}
		}(w)
	}
	start.Done()
	wg.Wait()

	for j := 0; j < jobs; j++ {
		jobID := fmt.Sprintf("job%d", j)
		if winners[jobID] != 1 {
			t.Fatalf("job %s leased %d times, want exactly 1", jobID, winners[jobID])
		}
	}
}

// TestStaleEpochResult409 / heartbeat: a stale epoch is rejected with ErrStaleEpoch.
func TestStaleEpoch(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "j")
	ls, err := claim(st, "j", "w1")
	if err != nil {
		t.Fatal(err)
	}

	// current epoch heartbeat -> ok
	if _, err := st.Heartbeat(context.Background(), store.HeartbeatParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)}); err != nil {
		t.Fatalf("current heartbeat should succeed: %v", err)
	}
	// stale epoch heartbeat -> 409
	if _, err := st.Heartbeat(context.Background(), store.HeartbeatParams{JobID: "j", Epoch: ls.Epoch - 1, Now: time.Unix(3000, 0)}); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale heartbeat want ErrStaleEpoch, got %v", err)
	}
	// stale epoch result -> 409
	if _, err := st.Result(context.Background(), store.ResultParams{JobID: "j", Epoch: ls.Epoch - 1, Now: time.Unix(3000, 0)}); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("stale result want ErrStaleEpoch, got %v", err)
	}
}

// TestResultLandsReviewPending: a current-epoch result advances building->review_pending.
func TestResultLandsReviewPending(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "j")
	ls, _ := claim(st, "j", "w1")
	resp, err := st.Result(context.Background(), store.ResultParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Accepted || resp.JobState != string(job.StateReviewPending) {
		t.Fatalf("result resp=%+v", resp)
	}
	j, _ := st.GetJob(context.Background(), "j")
	if j.State != job.StateReviewPending {
		t.Fatalf("state=%s want review_pending", j.State)
	}
	if j.LeaseID != "" {
		t.Fatal("lease id must be cleared after result")
	}
	if j.LeaseEpoch != ls.Epoch {
		t.Fatalf("epoch must stay monotonic (%d), got %d", ls.Epoch, j.LeaseEpoch)
	}
	// a fresh lease attempt now finds no ready job.
	cands, _ := st.ReadyCandidates(context.Background())
	if len(cands) != 0 {
		t.Fatalf("no job should be ready, got %d candidates", len(cands))
	}
}

// TestIdempotentResultRetry: a duplicate idempotency key returns the identical
// response with exactly one applied result (one review_pending event).
func TestIdempotentResultRetry(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "j")
	ls, _ := claim(st, "j", "w1")

	first, err := st.Result(context.Background(), store.ResultParams{JobID: "j", Epoch: ls.Epoch, IdempotencyKey: "key-1", Now: time.Unix(3000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Result(context.Background(), store.ResultParams{JobID: "j", Epoch: ls.Epoch, IdempotencyKey: "key-1", Now: time.Unix(4000, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("idempotent retry differs: %+v vs %+v", first, second)
	}
	// exactly one result_accepted event.
	evs, _ := st.LoadEvents(context.Background(), "j")
	n := 0
	for _, e := range evs {
		if e.Kind == ledger.KindResultAccepted {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one result_accepted event, got %d", n)
	}
}

// TestReleaseReturnsToReady: release -> ready, attempts++.
func TestReleaseReturnsToReady(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "j")
	ls, _ := claim(st, "j", "w1")
	if err := st.Release(context.Background(), store.ReleaseParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)}); err != nil {
		t.Fatal(err)
	}
	j, _ := st.GetJob(context.Background(), "j")
	if j.State != job.StateReady {
		t.Fatalf("state=%s want ready", j.State)
	}
	if j.Attempts != 1 {
		t.Fatalf("attempts=%d want 1", j.Attempts)
	}
	// stale epoch release after re-ready -> 409
	if err := st.Release(context.Background(), store.ReleaseParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)}); !errors.Is(err, lease.ErrStaleEpoch) {
		t.Fatalf("release on ready job want ErrStaleEpoch, got %v", err)
	}
}

// TestFoldEqualsProjection: after driving a full lifecycle, Fold(events) deep-equals
// the jobs projection.
func TestFoldEqualsProjection(t *testing.T) {
	st := testutil.NewStore(t)
	seedBuild(t, st, "j")
	ls, _ := claim(st, "j", "w1")
	if _, err := st.Heartbeat(context.Background(), store.HeartbeatParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3000, 0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Result(context.Background(), store.ResultParams{JobID: "j", Epoch: ls.Epoch, Now: time.Unix(3500, 0)}); err != nil {
		t.Fatal(err)
	}

	evs, _ := st.LoadEvents(context.Background(), "j")
	folded, err := ledger.Fold(evs)
	if err != nil {
		t.Fatal(err)
	}
	proj, _ := st.GetJob(context.Background(), "j")

	if folded.State != proj.State {
		t.Fatalf("state: fold=%s proj=%s", folded.State, proj.State)
	}
	if folded.LeaseEpoch != proj.LeaseEpoch {
		t.Fatalf("epoch: fold=%d proj=%d", folded.LeaseEpoch, proj.LeaseEpoch)
	}
	if folded.LeaseID != proj.LeaseID {
		t.Fatalf("lease_id: fold=%q proj=%q", folded.LeaseID, proj.LeaseID)
	}
	if folded.BoundIdentity != proj.BoundIdentity {
		t.Fatalf("bound_identity: fold=%q proj=%q", folded.BoundIdentity, proj.BoundIdentity)
	}
	if folded.BaseSHA != proj.BaseSHA {
		t.Fatalf("base_sha: fold=%q proj=%q", folded.BaseSHA, proj.BaseSHA)
	}
	if folded.JobSeq != proj.JobSeq {
		t.Fatalf("job_seq: fold=%d proj=%d", folded.JobSeq, proj.JobSeq)
	}
	if folded.Kind != proj.Kind {
		t.Fatalf("kind: fold=%s proj=%s", folded.Kind, proj.Kind)
	}
}
