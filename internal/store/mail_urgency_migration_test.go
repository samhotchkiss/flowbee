package store

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	_ "modernc.org/sqlite"
)

func TestMailUrgencyCleanupRunsThroughMigrationFramework(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	createMailUrgencyMigrationSchema(t, db)
	markMigrationsAppliedBefore(t, db, "0023_mail_urgency_source_gate_cleanup.sql")

	execMailUrgencyMigrationSQL(t, db, `INSERT INTO tenant_domains (tenant_id, domain) VALUES ('t', 'russhq.net')`)
	insertMailUrgencyMigrationMessage(t, db, "sent", "sent", "sam@example.com", "", "")
	insertMailUrgencyMigrationMessage(t, db, "draft", "draft", "sam@example.com", "", "")
	insertMailUrgencyMigrationMessage(t, db, "first", "received", "sam@sub.russhq.net", "", "")
	insertMailUrgencyMigrationMessage(t, db, "bulk", "received", "news@example.com", "", "List-Unsubscribe: <mailto:u@example.com>")
	insertMailUrgencyMigrationMessage(t, db, "missing", "received", "vendor@example.com", "", "")
	insertMailUrgencyMigrationMessage(t, db, "valid", "received", "vendor@example.com", "", "")
	execMailUrgencyMigrationSQL(t, db, `INSERT INTO mail_comprehensions (id, tenant_id, message_id, status, verdict_recorded, personal_ask) VALUES
		('c-bulk', 't', 'bulk', 'completed', 1, 0),
		('c-valid', 't', 'valid', 'completed', 1, 1)`)
	insertMailUrgencyMigrationAttention(t, db, "a-sent", "sent", "p0", "bad", "", "regex")
	insertMailUrgencyMigrationAttention(t, db, "a-draft", "draft", "p1", "bad", "", "regex_v1")
	insertMailUrgencyMigrationAttention(t, db, "a-first", "first", "p1", "bad", "", "legacy_regex")
	insertMailUrgencyMigrationAttention(t, db, "a-bulk", "bulk", "p1", "bad", "", "regex")
	insertMailUrgencyMigrationAttention(t, db, "a-missing", "missing", "p1", "bad", "", "regex")
	insertMailUrgencyMigrationAttention(t, db, "a-valid", "valid", "p1", "real", "c-valid", "llm")
	insertMailUrgencyMigrationNeed(t, db, "n-sent", "sent", "p0", "bad", "", "message.received")
	insertMailUrgencyMigrationNeed(t, db, "n-draft", "draft", "p1", "bad", "", "message.received")
	insertMailUrgencyMigrationNeed(t, db, "n-first", "first", "p1", "bad", "", "legacy_regex")
	insertMailUrgencyMigrationNeed(t, db, "n-bulk", "bulk", "p1", "bad", "", "message.received")
	insertMailUrgencyMigrationNeed(t, db, "n-missing", "missing", "p1", "bad", "", "regex")
	insertMailUrgencyMigrationNeed(t, db, "n-valid", "valid", "p1", "real", "c-valid", "stage3.completed")

	if err := MigrateUp(ctx, db); err != nil {
		t.Fatal(err)
	}

	wantReasons := map[string]string{
		"a-sent":    "sent_or_draft",
		"a-draft":   "sent_or_draft",
		"a-first":   "first_party_sender",
		"a-bulk":    "bulk_without_personal_ask",
		"a-missing": "missing_llm_verdict",
	}
	for id, want := range wantReasons {
		var priority, impact sql.NullString
		var visible int
		var reason string
		if err := db.QueryRowContext(ctx, `SELECT priority, impact_statement, user_visible, invalidated_reason FROM mail_attention_items WHERE id = ?`, id).Scan(&priority, &impact, &visible, &reason); err != nil {
			t.Fatal(err)
		}
		if priority.Valid || impact.Valid || visible != 0 || reason != want {
			t.Fatalf("%s not neutralized: priority=%v impact=%v visible=%d reason=%q", id, priority, impact, visible, reason)
		}
	}
	wantNeedReasons := map[string]string{
		"n-sent":    "sent_or_draft",
		"n-draft":   "sent_or_draft",
		"n-first":   "first_party_sender",
		"n-bulk":    "bulk_without_personal_ask",
		"n-missing": "missing_llm_verdict",
	}
	for id, want := range wantNeedReasons {
		var priority, impact sql.NullString
		var visible int
		var reason string
		if err := db.QueryRowContext(ctx, `SELECT priority, impact_statement, user_visible, invalidated_reason FROM mail_need_items WHERE id = ?`, id).Scan(&priority, &impact, &visible, &reason); err != nil {
			t.Fatal(err)
		}
		if priority.Valid || impact.Valid || visible != 0 || reason != want {
			t.Fatalf("%s not neutralized: priority=%v impact=%v visible=%d reason=%q", id, priority, impact, visible, reason)
		}
	}

	var visible int
	var reason sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT user_visible, invalidated_reason FROM mail_attention_items WHERE id = 'a-valid'`).Scan(&visible, &reason); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || reason.Valid {
		t.Fatalf("valid LLM-confirmed row changed: visible=%d reason=%v", visible, reason)
	}
	if err := db.QueryRowContext(ctx, `SELECT user_visible, invalidated_reason FROM mail_need_items WHERE id = 'n-valid'`).Scan(&visible, &reason); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || reason.Valid {
		t.Fatalf("valid LLM-confirmed need changed: visible=%d reason=%v", visible, reason)
	}

	if err := MigrateUp(ctx, db); err != nil {
		t.Fatalf("second migration run: %v", err)
	}
}

func markMigrationsAppliedBefore(t *testing.T, db *sql.DB, stop string) {
	t.Helper()
	execMailUrgencyMigrationSQL(t, db, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if name == stop {
			return
		}
		execMailUrgencyMigrationSQL(t, db, `INSERT INTO schema_migrations (version) VALUES (?)`, name)
	}
	t.Fatalf("migration %s not found", stop)
}

func createMailUrgencyMigrationSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	execMailUrgencyMigrationSQL(t, db, `CREATE TABLE mail_attention_items (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		source TEXT NOT NULL,
		priority TEXT NULL,
		impact_statement TEXT NULL,
		llm_verdict_id TEXT NULL,
		classification_source TEXT NULL,
		user_visible INTEGER NOT NULL DEFAULT 1,
		invalidated_reason TEXT NULL
	)`)
	execMailUrgencyMigrationSQL(t, db, `CREATE TABLE mail_messages (
		id TEXT NOT NULL,
		tenant_id TEXT NOT NULL,
		status TEXT NOT NULL,
		sender_email TEXT NOT NULL,
		sender_domain TEXT NOT NULL,
		headers TEXT NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`)
	execMailUrgencyMigrationSQL(t, db, `CREATE TABLE mail_comprehensions (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		status TEXT NOT NULL,
		verdict_recorded INTEGER NOT NULL,
		personal_ask INTEGER NOT NULL
	)`)
	execMailUrgencyMigrationSQL(t, db, `CREATE TABLE tenant_domains (
		tenant_id TEXT NOT NULL,
		domain TEXT NOT NULL
	)`)
	execMailUrgencyMigrationSQL(t, db, `CREATE TABLE mail_need_items (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		source TEXT NOT NULL,
		priority TEXT NULL,
		impact_statement TEXT NULL,
		llm_verdict_id TEXT NULL,
		derivation_source TEXT NULL,
		user_visible INTEGER NOT NULL DEFAULT 1,
		invalidated_reason TEXT NULL
	)`)
}

func insertMailUrgencyMigrationMessage(t *testing.T, db *sql.DB, id, status, senderEmail, senderDomain, headers string) {
	t.Helper()
	execMailUrgencyMigrationSQL(t, db, `INSERT INTO mail_messages (id, tenant_id, status, sender_email, sender_domain, headers) VALUES (?, 't', ?, ?, ?, ?)`,
		id, status, senderEmail, senderDomain, headers)
}

func insertMailUrgencyMigrationAttention(t *testing.T, db *sql.DB, id, messageID, priority, impact, verdictID, source string) {
	t.Helper()
	execMailUrgencyMigrationSQL(t, db, `INSERT INTO mail_attention_items (id, tenant_id, message_id, source, priority, impact_statement, llm_verdict_id, classification_source, user_visible) VALUES (?, 't', ?, 'mail', ?, ?, ?, ?, 1)`,
		id, messageID, priority, impact, verdictID, source)
}

func insertMailUrgencyMigrationNeed(t *testing.T, db *sql.DB, id, messageID, priority, impact, verdictID, source string) {
	t.Helper()
	execMailUrgencyMigrationSQL(t, db, `INSERT INTO mail_need_items (id, tenant_id, message_id, source, priority, impact_statement, llm_verdict_id, derivation_source, user_visible) VALUES (?, 't', ?, 'email', ?, ?, ?, ?, 1)`,
		id, messageID, priority, impact, verdictID, source)
}

func execMailUrgencyMigrationSQL(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
