// Package acceptance proves the M1 DONE-WHEN end-to-end thread over the real
// HTTP surface against a real SQLite store: seed -> lease(once) -> heartbeat ->
// result -> review_pending; loser gets 204; stale-epoch -> 409; Fold==projection;
// lifecycle visible via SSE. No GitHub, no LLM, no PR.
package acceptance

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/ledger"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestLeaseThreadEndToEnd(t *testing.T) {
	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: 2 * time.Second,
		LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	// subscribe to SSE BEFORE seeding so we capture the lifecycle live.
	sseLines := make(chan string, 64)
	sseCtx, sseCancel := context.WithCancel(ctx)
	defer sseCancel()
	go streamSSE(t, sseCtx, ts.URL+"/v1/events", sseLines)
	time.Sleep(100 * time.Millisecond) // let the SSE stream attach

	// 1) seed a ready build job.
	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{
		ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build",
		Role: job.RoleEngWorker, BaseSHA: "base-sha-1", Now: time.Unix(1000, 0),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// 2) two workers register with distinct identities + families, long-poll the
	//    SAME job concurrently.
	c1 := client.New(ts.URL)
	c2 := client.New(ts.URL)
	for _, c := range []*client.Client{c1, c2} {
		if _, err := c.Register(ctx, client.Registration{Identity: regIdentity(c, c1), Host: "t", Capabilities: []string{"role:eng_worker"}}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}

	type leaseRes struct {
		grant client.LeaseGrant
		ok    bool
		err   error
	}
	results := make(chan leaseRes, 2)
	var start sync.WaitGroup
	start.Add(1)
	for i, c := range []*client.Client{c1, c2} {
		go func(i int, c *client.Client) {
			start.Wait()
			g, ok, err := c.Lease(ctx, []string{"alice", "bob"}[i], []string{"codex", "opus"}[i], "")
			results <- leaseRes{g, ok, err}
		}(i, c)
	}
	start.Done()

	r1 := <-results
	r2 := <-results
	if r1.err != nil || r2.err != nil {
		t.Fatalf("lease errors: %v / %v", r1.err, r2.err)
	}

	// 3) exactly one got a lease (200), the other 204.
	wins := 0
	var winner client.LeaseGrant
	for _, r := range []leaseRes{r1, r2} {
		if r.ok {
			wins++
			winner = r.grant
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one winner, got %d", wins)
	}
	if winner.JobID != jobID {
		t.Fatalf("winner job=%s want %s", winner.JobID, jobID)
	}
	if winner.BaseSHA != "base-sha-1" {
		t.Fatalf("lease envelope base_sha=%q", winner.BaseSHA)
	}
	if winner.LeaseEpoch != 1 {
		t.Fatalf("first claim epoch=%d want 1", winner.LeaseEpoch)
	}

	// 4) winner heartbeats (valid epoch -> continue), posts result.
	dir, st200, err := c1.Heartbeat(ctx, jobID, winner.LeaseEpoch)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if st200 != http.StatusOK || dir != "continue" {
		t.Fatalf("heartbeat status=%d dir=%s", st200, dir)
	}
	res, rst, err := c1.Result(ctx, jobID, winner.LeaseEpoch, "idem-1", map[string]any{"kind": "patch", "base_sha": winner.BaseSHA})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if rst != http.StatusOK || !res.Accepted || res.JobState != string(job.StateReviewPending) {
		t.Fatalf("result status=%d resp=%+v", rst, res)
	}

	// 5) projection: review_pending, lease cleared, epoch monotonic.
	j, _ := st.GetJob(ctx, jobID)
	if j.State != job.StateReviewPending {
		t.Fatalf("state=%s want review_pending", j.State)
	}
	if j.LeaseID != "" {
		t.Fatalf("lease_id not cleared: %q", j.LeaseID)
	}
	if j.LeaseEpoch != 1 {
		t.Fatalf("epoch=%d want 1 (monotonic, not reset)", j.LeaseEpoch)
	}

	// 6) ledger: Fold(events) deep-equals the projection.
	evs, err := st.LoadEvents(ctx, jobID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	assertEventOrder(t, evs)
	folded, _ := ledger.Fold(evs)
	if folded.State != j.State || folded.LeaseEpoch != j.LeaseEpoch || folded.BaseSHA != j.BaseSHA ||
		folded.LeaseID != j.LeaseID || folded.JobSeq != j.JobSeq || folded.Kind != j.Kind {
		t.Fatalf("Fold != projection:\n fold=%+v\n proj=%+v", folded, j)
	}

	// 7) stale-epoch heartbeat AND result -> 409.
	if _, hs, _ := c1.Heartbeat(ctx, jobID, winner.LeaseEpoch); hs != http.StatusConflict {
		t.Fatalf("stale-epoch heartbeat status=%d want 409 (job left active state)", hs)
	}
	if _, rs, _ := c1.Result(ctx, jobID, winner.LeaseEpoch-1, "idem-stale", map[string]any{"kind": "patch"}); rs != http.StatusConflict {
		t.Fatalf("stale-epoch result status=%d want 409", rs)
	}

	// 8) a fresh lease attempt now returns 204 (job is review_pending, not ready).
	if _, ok, err := c2.Lease(ctx, "bob", "opus", ""); err != nil || ok {
		t.Fatalf("expected 204 (no ready job), ok=%v err=%v", ok, err)
	}

	// (5) lifecycle visible live via SSE: we should have observed the job reach
	//     lease_claimed and result_accepted/review_pending.
	assertSSESawLifecycle(t, sseLines, jobID)
}

// TestIdempotentResultRetryOverHTTP: duplicate idempotency key -> identical
// response, exactly one applied transition.
func TestIdempotentResultRetryOverHTTP(t *testing.T) {
	st := testutil.NewStore(t)
	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LongPollWait: time.Second, LeaseTTLS: 300, HeartbeatIntervalS: 30,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()
	ctx := context.Background()

	jobID := ulid.New()
	if _, err := st.SeedJob(ctx, store.SeedParams{ID: jobID, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker, BaseSHA: "b", Now: time.Unix(1000, 0)}); err != nil {
		t.Fatal(err)
	}
	c := client.New(ts.URL)
	if _, err := c.Register(ctx, client.Registration{Identity: "alice", Host: "t"}); err != nil {
		t.Fatal(err)
	}
	g, ok, err := c.Lease(ctx, "alice", "codex", "")
	if err != nil || !ok {
		t.Fatalf("lease ok=%v err=%v", ok, err)
	}

	first, s1, _ := c.Result(ctx, jobID, g.LeaseEpoch, "dup", map[string]any{"kind": "patch"})
	second, s2, _ := c.Result(ctx, jobID, g.LeaseEpoch, "dup", map[string]any{"kind": "patch"})
	if s1 != http.StatusOK || s2 != http.StatusOK {
		t.Fatalf("statuses %d/%d", s1, s2)
	}
	if first != second {
		t.Fatalf("idempotent retry differs: %+v vs %+v", first, second)
	}
	evs, _ := st.LoadEvents(ctx, jobID)
	n := 0
	for _, e := range evs {
		if e.Kind == ledger.KindResultAccepted {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one result_accepted, got %d", n)
	}
}

func assertEventOrder(t *testing.T, evs []ledger.Event) {
	t.Helper()
	var kinds []ledger.EventKind
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	want := []ledger.EventKind{
		ledger.KindJobCreated, ledger.KindLeaseClaimed, ledger.KindHeartbeat,
		ledger.KindWorkerStarted, ledger.KindResultAccepted,
	}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event[%d]=%s want %s (full=%v)", i, kinds[i], want[i], kinds)
		}
	}
	// per-job ordinals must be 1..N contiguous.
	for i, e := range evs {
		if e.JobSeq != i+1 {
			t.Fatalf("job_seq[%d]=%d want %d", i, e.JobSeq, i+1)
		}
	}
}

func streamSSE(t *testing.T, ctx context.Context, url string, out chan<- string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") {
			select {
			case out <- strings.TrimPrefix(line, "data: "):
			case <-ctx.Done():
				return
			}
		}
	}
}

func assertSSESawLifecycle(t *testing.T, lines chan string, jobID string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	sawClaim, sawResult := false, false
	for {
		select {
		case line := <-lines:
			if !strings.Contains(line, jobID) {
				continue
			}
			if strings.Contains(line, "lease_claimed") {
				sawClaim = true
			}
			if strings.Contains(line, "result_accepted") || strings.Contains(line, "review_pending") {
				sawResult = true
			}
			if sawClaim && sawResult {
				return
			}
		case <-deadline:
			t.Fatalf("SSE did not show full lifecycle (claim=%v result=%v)", sawClaim, sawResult)
		}
	}
}

// regIdentity assigns distinct identities to the two clients.
func regIdentity(c, c1 *client.Client) string {
	if c == c1 {
		return "alice"
	}
	return "bob"
}
