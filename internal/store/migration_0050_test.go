package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestMigration0050AddsFailClosedTmuxServerDomainColumns(t *testing.T) {
	st := testutil.NewStore(t)
	want := map[string][]string{
		"driver_instances":           {"tmux_server_domain_id", "tmux_server_ownership"},
		"builder_driver_targets":     {"tmux_server_domain_id"},
		"driver_session_bindings":    {"tmux_server_domain_id", "lifecycle_ownership"},
		"epic_actions":               {"target_server_domain_id"},
		"driver_session_projections": {"tmux_server_domain_id"},
	}
	for table, columns := range want {
		rows, err := st.DB.QueryContext(context.Background(), "PRAGMA table_info("+table+")")
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]string{}
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var defaultValue any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			if text, ok := defaultValue.(string); ok {
				found[name] = text
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		for _, column := range columns {
			if found[column] != "''" {
				t.Fatalf("%s.%s default=%q; legacy authority must remain empty", table, column, found[column])
			}
		}
	}
}
