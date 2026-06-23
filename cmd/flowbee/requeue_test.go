package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
	"github.com/samhotchkiss/flowbee/internal/job"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestRequeueByState: --state requeues every matching job EXCEPT pr_closed (human-rejected),
// scoped by repo, and never touches jobs in other states.
func TestRequeueByState(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	seed := func(id, repo, state, reason string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			Repo: repo, Now: time.Unix(1000, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state=?, escalation_reason=? WHERE id=?`, state, reason, id); err != nil {
			t.Fatal(err)
		}
	}
	seed("a", "russ", "needs_human", "")          // requeue
	seed("b", "russ", "needs_human", "")          // requeue
	seed("c", "russ", "needs_human", "pr_closed") // SKIP — human closed it
	seed("d", "russ", "ready", "")                // wrong state — untouched
	seed("e", "flowbee", "needs_human", "")       // wrong repo — untouched

	// fake control-plane API that records which ids got a requeue POST.
	var mu sync.Mutex
	got := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// path: /v1/jobs/<id>/requeue
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 4 && parts[3] == "requeue" {
			mu.Lock()
			got[parts[2]] = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := client.NewWithToken(srv.URL, "")
	if err := requeueByState(c, st, "needs_human", "russ", "", false); err != nil {
		t.Fatalf("requeueByState: %v", err)
	}

	if !got["a"] || !got["b"] {
		t.Errorf("the two open needs_human russ jobs must be requeued; got %v", got)
	}
	if got["c"] {
		t.Error("a pr_closed job must be SKIPPED (rebuilding a human-rejected PR)")
	}
	if got["d"] {
		t.Error("a ready job (wrong state) must not be requeued")
	}
	if got["e"] {
		t.Error("a flowbee job (wrong repo filter) must not be requeued")
	}
}

func TestRequeueByStateFiltersReason(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)

	seed := func(id, reason string) {
		if _, err := st.SeedJob(ctx, store.SeedParams{
			ID: id, Kind: job.KindBuild, Flow: "build", Stage: "build", Role: job.RoleEngWorker,
			Repo: "russ", Now: time.Unix(1000, 0),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='needs_human', escalation_reason=? WHERE id=?`, reason, id); err != nil {
			t.Fatal(err)
		}
	}
	seed("rule", `405 Repository rule violations - Required status check "Migration version guard" is expected`)
	seed("stall", "stall")

	var mu sync.Mutex
	got := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) == 4 && parts[3] == "requeue" {
			mu.Lock()
			got[parts[2]] = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		http.Error(w, "unexpected", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := client.NewWithToken(srv.URL, "")
	if err := requeueByState(c, st, "needs_human", "russ", "405 Repository rule", false); err != nil {
		t.Fatalf("requeueByState reason: %v", err)
	}
	if !got["rule"] || got["stall"] {
		t.Fatalf("reason filter should requeue only rule violation job, got %v", got)
	}
}
