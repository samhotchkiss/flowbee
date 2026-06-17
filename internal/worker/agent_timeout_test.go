package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
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
		// a hung agent: sleeps far longer than the test would ever wait.
		_, err := runAgentHeartbeatIO(ctx, c, "j", 1, 3600, t.TempDir(), "sleep 30", nil, true)
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
