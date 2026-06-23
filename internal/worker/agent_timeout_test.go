package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/client"
)

// TestHungAgentKilledOnContextEnd is the regression for a fleet-availability wedge: a
// hung or slow agent must NEVER block the worker loop forever — that would silently
// remove the worker from the fleet (cmd.Wait would never return). runAgentHeartbeatIO
// now bounds the agent by the lease TTL AND cancels it when the CP revokes the lease;
// both reduce to run-context cancellation. Here we cancel the context while a `sleep`
// agent is running and assert the helper returns promptly instead of waiting it out.
func TestHungAgentKilledOnContextEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"directive":"continue"}`))
	}))
	defer srv.Close()
	c := client.New(srv.URL)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		// a hung agent that FORKS a child holding the stdout pipe (`| cat`) — the real
		// case: killing only the direct child leaves the orphan pinning cmd.Wait(). The
		// fix kills the whole process group, so Wait returns when the GROUP dies.
		_, err := runAgentHeartbeatIO(ctx, c, nil, "j", 1, 3600, t.TempDir(), "sleep 30 | cat", nil, true)
		done <- err
	}()

	// let the agent actually start, then simulate the lease timeout / CP revoke.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		// returned promptly — the hung agent was killed and the worker is free.
		if err == nil {
			t.Fatal("expected an abort error when the lease ends mid-agent")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runAgentHeartbeatIO blocked after the context ended — a hung agent would wedge the worker forever")
	}
}

func TestAgentHeartbeatRefreshesRegistration(t *testing.T) {
	oldMin, oldMax := agentHeartbeatMinS, agentHeartbeatMaxS
	agentHeartbeatMinS, agentHeartbeatMaxS = 1, 1
	t.Cleanup(func() {
		agentHeartbeatMinS, agentHeartbeatMaxS = oldMin, oldMax
	})

	var registers atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/workers/register":
			registers.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(client.RegisterResponse{WorkerID: "wid"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/j/heartbeat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"directive":"continue"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	reg := client.Registration{Identity: "builder-1", Capabilities: []string{"role:eng_worker"}}
	_, err := runAgentHeartbeatIO(context.Background(), client.New(srv.URL), &reg, "j", 1, 300,
		t.TempDir(), "sleep 2", nil, true)
	if err != nil {
		t.Fatalf("runAgentHeartbeatIO: %v", err)
	}
	if got := registers.Load(); got == 0 {
		t.Fatal("expected heartbeat loop to refresh worker registration while the agent was running")
	}
	if reg.WorkerID != "wid" {
		t.Fatalf("registration refresh should carry returned worker_id forward, got %q", reg.WorkerID)
	}
}
