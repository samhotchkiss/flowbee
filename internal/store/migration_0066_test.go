package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestMigration0066AddsNoGuessedAdoptedInteractorRecoveryPolicy(t *testing.T) {
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := store.MigrateUp(context.Background(), st.DB); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"project_id": true, "role": true, "actor_id": true, "profile_id": true,
		"workspace_root_id": true, "workspace_relative_path": true,
		"source_intent_payload_sha256": true, "created_at": true, "updated_at": true,
	}
	rows, err := st.DB.Query(`PRAGMA table_info(project_actor_managed_recovery_policies)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for column := range want {
		if !seen[column] {
			t.Fatalf("0066 missing recovery policy column %q", column)
		}
	}
	var policies int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM project_actor_managed_recovery_policies`).Scan(&policies); err != nil {
		t.Fatal(err)
	}
	if policies != 0 {
		t.Fatalf("0066 guessed/backfilled recovery authority: rows=%d", policies)
	}
}
