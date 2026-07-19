package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestMigration0051AddsFailClosedExternalWatchAuthority(t *testing.T) {
	st := testutil.NewStore(t)
	want := map[string][]string{
		"driver_session_bindings":   {"external_watch_id"},
		"epic_actions":              {"external_watch_id", "sender_host_id", "sender_store_id", "sender_server_domain_id", "sender_server_id"},
		"driver_grants":             {"expected_recipient_agent_run_id"},
		"driver_receipts":           {"expected_recipient_agent_run_id"},
		"driver_lifecycle_receipts": {"tmux_server_domain_id", "external_watch_id"},
	}
	for table, required := range want {
		rows, err := st.DB.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
		if err != nil {
			t.Fatal(err)
		}
		found, rawPaneColumn := map[string]string{}, false
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			for _, column := range required {
				if name == column {
					found[column], _ = defaultValue.(string)
				}
			}
			if name == "pane_id" || name == "raw_pane_id" {
				rawPaneColumn = true
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, column := range required {
			if found[column] != "''" {
				t.Fatalf("%s.%s default=%q", table, column, found[column])
			}
		}
		if rawPaneColumn {
			t.Fatalf("%s persisted raw pane selector", table)
		}
	}
}
