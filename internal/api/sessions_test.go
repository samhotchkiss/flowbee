package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

// TestSessionsJSON: GET /v1/sessions serves the goal-session registry as a
// JSON array — the machine-readable `flowbee session list` — and an empty
// registry is [] (never null), so dashboard clients can iterate unguarded.
func TestSessionsJSON(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()

	srv := api.New(st, clock.Real{}, ulid.NewMinter(nil), api.Config{
		LeaseTTL: 5 * time.Minute, LeaseTTLS: 300,
	}, "test")
	ts := httptest.NewServer(srv.PrivateHandler())
	defer ts.Close()

	get := func() []map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + "/v1/sessions")
		if err != nil {
			t.Fatalf("GET /v1/sessions: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /v1/sessions: status %d, want 200", resp.StatusCode)
		}
		var rows []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rows == nil {
			t.Fatal("GET /v1/sessions: decoded to nil — body was null, want []")
		}
		return rows
	}

	// empty registry → [] (the make(..., 0, n) guarantee).
	if rows := get(); len(rows) != 0 {
		t.Fatalf("empty registry: got %d rows, want 0", len(rows))
	}

	// register one session and read it back through the wire shape.
	if err := st.AddGoalSession(ctx, store.GoalSession{
		ID: "mail-epic", Box: "mini-1", TmuxName: "russ-codex",
		Repo: "russ", Note: "overnight run",
	}, time.Unix(1000, 0)); err != nil {
		t.Fatalf("add goal session: %v", err)
	}

	rows := get()
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	for field, want := range map[string]any{
		"id": "mail-epic", "box": "mini-1", "tmux_name": "russ-codex",
		"repo": "russ", "note": "overnight run", "enabled": true,
	} {
		if got := row[field]; got != want {
			t.Errorf("row[%q] = %v, want %v", field, got, want)
		}
	}
	// a fresh registration has never been watched: state is the store default,
	// and the watchdog-internal fields must NOT leak onto the wire.
	if _, ok := row["state"]; !ok {
		t.Error("row is missing \"state\"")
	}
	for _, internal := range []string{"last_pane_hash", "resume_attempts", "consecutive_failures"} {
		if _, ok := row[internal]; ok {
			t.Errorf("watchdog-internal field %q leaked onto the wire", internal)
		}
	}
}
