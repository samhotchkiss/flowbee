package mailurgency

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestCleanupLegacyUrgencyNeutralizesInvalidRowsAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	createMailCleanupSchema(t, db)

	exec(t, db, `INSERT INTO tenant_domains (tenant_id, domain) VALUES ('t', 'russhq.net')`)
	insertMessage(t, db, "sent", "sent", "sam@external.com", "", "")
	insertMessage(t, db, "draft", "drafted", "sam@external.com", "", "")
	insertMessage(t, db, "first", "received", "sam@sub.russhq.net", "", "")
	insertMessage(t, db, "bulk", "received", "news@example.com", "", "List-Unsubscribe: <mailto:u@example.com>")
	insertMessage(t, db, "missing", "received", "vendor@example.com", "", "")
	insertMessage(t, db, "valid", "received", "vendor@example.com", "", "")

	exec(t, db, `INSERT INTO mail_comprehensions (id, tenant_id, message_id, status, verdict_recorded, personal_ask) VALUES
		('c-bulk', 't', 'bulk', 'completed', 1, 0),
		('c-valid', 't', 'valid', 'completed', 1, 1)`)

	insertAttention(t, db, "a-sent", "sent", "p0", "bad", "", "regex")
	insertAttention(t, db, "a-draft", "draft", "p1", "bad", "", "regex_v1")
	insertAttention(t, db, "a-first", "first", "p1", "bad", "", "legacy_regex")
	insertAttention(t, db, "a-bulk", "bulk", "p1", "bad", "", "regex")
	insertAttention(t, db, "a-missing", "missing", "p1", "bad", "", "regex")
	insertAttention(t, db, "a-valid", "valid", "p1", "real", "c-valid", "llm")
	insertNeed(t, db, "n-sent", "sent", "p0", "bad", "", "message.received")
	insertNeed(t, db, "n-draft", "draft", "p1", "bad", "", "message.received")
	insertNeed(t, db, "n-first", "first", "p1", "bad", "", "legacy_regex")
	insertNeed(t, db, "n-bulk", "bulk", "p1", "bad", "", "message.received")
	insertNeed(t, db, "n-missing", "missing", "p1", "bad", "", "regex")
	insertNeed(t, db, "n-valid", "valid", "p1", "real", "c-valid", "stage3.completed")

	n, err := CleanupLegacyUrgency(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Fatalf("neutralized rows=%d want 10", n)
	}

	wantReasons := map[string]string{
		"a-sent":    ReasonSentOrDraft,
		"a-draft":   ReasonSentOrDraft,
		"a-first":   ReasonFirstPartySender,
		"a-bulk":    ReasonBulkWithoutPersonalAsk,
		"a-missing": ReasonMissingLLMVerdict,
	}
	for id, want := range wantReasons {
		var priority, impact sql.NullString
		var visible int
		var reason string
		if err := db.QueryRowContext(ctx, `SELECT priority, impact_statement, user_visible, invalidated_reason FROM mail_attention_items WHERE id = ?`, id).Scan(&priority, &impact, &visible, &reason); err != nil {
			t.Fatal(err)
		}
		if priority.Valid || impact.Valid || visible != 0 || reason != want {
			t.Fatalf("%s not neutralized correctly: priority=%v impact=%v visible=%d reason=%q", id, priority, impact, visible, reason)
		}
	}
	wantNeedReasons := map[string]string{
		"n-sent":    ReasonSentOrDraft,
		"n-draft":   ReasonSentOrDraft,
		"n-first":   ReasonFirstPartySender,
		"n-bulk":    ReasonBulkWithoutPersonalAsk,
		"n-missing": ReasonMissingLLMVerdict,
	}
	for id, want := range wantNeedReasons {
		var priority, impact sql.NullString
		var visible int
		var reason string
		if err := db.QueryRowContext(ctx, `SELECT priority, impact_statement, user_visible, invalidated_reason FROM mail_need_items WHERE id = ?`, id).Scan(&priority, &impact, &visible, &reason); err != nil {
			t.Fatal(err)
		}
		if priority.Valid || impact.Valid || visible != 0 || reason != want {
			t.Fatalf("%s not neutralized correctly: priority=%v impact=%v visible=%d reason=%q", id, priority, impact, visible, reason)
		}
	}

	var visible int
	var reason sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT user_visible, invalidated_reason FROM mail_attention_items WHERE id = 'a-valid'`).Scan(&visible, &reason); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || reason.Valid {
		t.Fatalf("valid LLM-confirmed row was changed: visible=%d reason=%v", visible, reason)
	}
	if err := db.QueryRowContext(ctx, `SELECT user_visible, invalidated_reason FROM mail_need_items WHERE id = 'n-valid'`).Scan(&visible, &reason); err != nil {
		t.Fatal(err)
	}
	if visible != 1 || reason.Valid {
		t.Fatalf("valid LLM-confirmed need was changed: visible=%d reason=%v", visible, reason)
	}

	n, err = CleanupLegacyUrgency(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("second cleanup changed %d rows, want 0", n)
	}
}

func createMailCleanupSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	exec(t, db, `CREATE TABLE mail_attention_items (
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
	exec(t, db, `CREATE TABLE mail_messages (
		id TEXT NOT NULL,
		tenant_id TEXT NOT NULL,
		status TEXT NOT NULL,
		sender_email TEXT NOT NULL,
		sender_domain TEXT NOT NULL,
		headers TEXT NOT NULL,
		PRIMARY KEY (tenant_id, id)
	)`)
	exec(t, db, `CREATE TABLE mail_comprehensions (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		message_id TEXT NOT NULL,
		status TEXT NOT NULL,
		verdict_recorded INTEGER NOT NULL,
		personal_ask INTEGER NOT NULL
	)`)
	exec(t, db, `CREATE TABLE tenant_domains (
		tenant_id TEXT NOT NULL,
		domain TEXT NOT NULL
	)`)
	exec(t, db, `CREATE TABLE mail_need_items (
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

func insertMessage(t *testing.T, db *sql.DB, id, status, senderEmail, senderDomain, headers string) {
	t.Helper()
	exec(t, db, `INSERT INTO mail_messages (id, tenant_id, status, sender_email, sender_domain, headers) VALUES (?, 't', ?, ?, ?, ?)`,
		id, status, senderEmail, senderDomain, headers)
}

func insertAttention(t *testing.T, db *sql.DB, id, messageID, priority, impact, verdictID, source string) {
	t.Helper()
	exec(t, db, `INSERT INTO mail_attention_items (id, tenant_id, message_id, source, priority, impact_statement, llm_verdict_id, classification_source, user_visible) VALUES (?, 't', ?, 'mail', ?, ?, ?, ?, 1)`,
		id, messageID, priority, impact, verdictID, source)
}

func insertNeed(t *testing.T, db *sql.DB, id, messageID, priority, impact, verdictID, source string) {
	t.Helper()
	exec(t, db, `INSERT INTO mail_need_items (id, tenant_id, message_id, source, priority, impact_statement, llm_verdict_id, derivation_source, user_visible) VALUES (?, 't', ?, 'email', ?, ?, ?, ?, 1)`,
		id, messageID, priority, impact, verdictID, source)
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}
