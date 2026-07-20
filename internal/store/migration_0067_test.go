package store_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestMigration0067AddsDurablePreEffectCertificateAndWorkerCleanupFence(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"epic_lifecycle_pre_effect_failures", "epic_worker_sessions"} {
		var found int
		if err := st.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&found); err != nil || found != 1 {
			t.Fatalf("table %s found=%d err=%v", table, found, err)
		}
	}
	rows, err := st.DB.Query(`PRAGMA table_info(epic_worker_sessions)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "cleanup_action_id" {
			found = notNull == 1 && dflt.Valid && dflt.String == "''"
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("0067 cleanup_action_id was not installed as a non-null empty-default fence")
	}
}
