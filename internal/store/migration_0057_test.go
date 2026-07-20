package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestMigration0057BackfillsEpiclessAttentionAndLocalizesDedup(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	entries, err := os.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0057_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	now := time.Date(2026, 7, 19, 23, 45, 0, 0, time.UTC)
	for _, id := range []string{"mail", "calendar"} {
		if _, err := st.CreatePortfolioProject(ctx, store.PortfolioProject{ID: id, Name: id}, now); err != nil {
			t.Fatal(err)
		}
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,kind,dedup_key,evidence_json,state,created_at,updated_at,first_seen_at,last_seen_at)
		VALUES ('mail-capacity','capacity_pool_exhausted','capacity-mail','{"project_id":"mail"}','open',?,?,?,?),
		       ('legacy','master_absent','legacy','{}','open',?,?,?,?),
		       ('legacy-malformed','master_absent','legacy-malformed','{','open',?,?,?,?)`,
		stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile("migrations/0057_phase2_attention_project_isolation.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, string(body)); err != nil {
		t.Fatalf("apply 0057: %v", err)
	}
	for id, want := range map[string]string{"mail-capacity": "mail", "legacy": "default", "legacy-malformed": "default"} {
		var got string
		if err := st.DB.QueryRowContext(ctx, `SELECT project_id FROM attention_items WHERE id=?`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s project=%q want %q", id, got, want)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,dedup_key,state,created_at,updated_at,first_seen_at,last_seen_at)
		VALUES ('mail-shared','mail','needs_input','shared','open',?,?,?,?),
		       ('calendar-shared','calendar','needs_input','shared','open',?,?,?,?)`,
		stamp, stamp, stamp, stamp, stamp, stamp, stamp, stamp); err != nil {
		t.Fatalf("same dedup in two projects: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO attention_items
		(id,project_id,kind,dedup_key,state,created_at,updated_at,first_seen_at,last_seen_at)
		VALUES ('mail-duplicate','mail','needs_input','shared','leased',?,?,?,?)`,
		stamp, stamp, stamp, stamp); err == nil {
		t.Fatal("same-project active duplicate passed 0057 unique index")
	}
}
