package mailtrace

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestTraceCompleteIncludesExactCorrelatedPayloads(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	seedMessage(t, db, "m1")
	exec(t, db, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m1', 'needs_llm', 'light_personal_open_loop',
		'{"known_contact":true,"contact_type":"personal","vip":false,"recipient_position":"to","first_party":true,"sender_lean":"reply_worthy","correlations":["thread"],"entity_match":{"type":"person"},"open_loop_candidate":true}',
		'important because it asks for a decision')`)
	exec(t, db, `INSERT INTO email_message_comprehension
		(message_id, prompt_version, content_class, scores, summary, key_points, quick_reply, open_loop, escalate, escalate_reason, parsed_json)
		VALUES ('m1','comprehension_v1','personal','{"urgency":0.8}','Please decide','["budget"]',NULL,'{"due":"today"}',1,'needs draft','{}')`)
	exec(t, db, `INSERT INTO email_message_comprehension_heavy
		(message_id, draft, options, belief_delta, push_verdict, context_bundle_manifest, escalation_reason, output_json)
		VALUES ('m1','Draft body','["approve","decline"]','{"belief":"changed"}','{"push":true}',
		'[{"type":"thread","source_id":"t1","title":"Prior thread","inclusion_reason":"open loop"}]','needs draft','{}')`)
	exec(t, db, `INSERT INTO model_invocation
		(id, message_id, stage, request_id, provider, model, model_version, started_at, completed_at, latency_ms, cost_amount, cost_currency, status, created_at)
		VALUES ('i-light','m1','mail_comprehension_light','r-light','openrouter','haiku','4.5','2026-07-04T00:00:00Z','2026-07-04T00:00:01Z',1000,0.001,'USD','succeeded','2026-07-04T00:00:01Z'),
		       ('i-heavy','m1','mail_comprehension_heavy','r-heavy','openrouter','opus','4.1','2026-07-04T00:00:02Z','2026-07-04T00:00:05Z',3000,0.01,'USD','succeeded','2026-07-04T00:00:05Z')`)
	exec(t, db, `INSERT INTO model_invocation_payload (model_invocation_id, request_text, response_text)
		VALUES ('i-light','LIGHT PROMPT','LIGHT RESPONSE'), ('i-heavy','HEAVY PROMPT','HEAVY RESPONSE')`)

	tr, err := NewService(db).Trace(ctx, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Deterministic.Status != StatusComplete || *tr.Deterministic.Stage2PromptKey != "light_personal_open_loop" {
		t.Fatalf("deterministic trace not complete: %+v", tr.Deterministic)
	}
	if got := tr.Deterministic.Facts["known_contact"].Value; got != true {
		t.Fatalf("known_contact=%v", got)
	}
	if tr.LightLLM.Status != StatusComplete || tr.LightLLM.RequestText == nil || *tr.LightLLM.RequestText != "LIGHT PROMPT" {
		t.Fatalf("light trace missing exact payload: %+v", tr.LightLLM)
	}
	if tr.HeavyLLM.Status != StatusComplete || tr.HeavyLLM.RequestText == nil || *tr.HeavyLLM.RequestText != "HEAVY PROMPT" {
		t.Fatalf("heavy trace missing exact payload: %+v", tr.HeavyLLM)
	}
	if len(tr.HeavyLLM.ContextBundleManifest) != 1 {
		t.Fatalf("manifest=%v", tr.HeavyLLM.ContextBundleManifest)
	}
	if len(tr.Invocations) != 2 {
		t.Fatalf("invocations=%d", len(tr.Invocations))
	}
}

func TestTraceSkippedLightFromDeterministicRoute(t *testing.T) {
	db := testDB(t)
	seedMessage(t, db, "m2")
	exec(t, db, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m2', 'archive', NULL, '{"known_contact":false}', 'no action needed')`)

	tr, err := NewService(db).Trace(context.Background(), "m2")
	if err != nil {
		t.Fatal(err)
	}
	if tr.LightLLM.Status != StatusSkipped || tr.LightLLM.SkipReason == nil || *tr.LightLLM.SkipReason != "deterministic_route_no_llm" {
		t.Fatalf("light skip=%+v", tr.LightLLM)
	}
}

func TestTraceSkippedHeavyWhenLightDoesNotEscalate(t *testing.T) {
	db := testDB(t)
	seedMessage(t, db, "m3")
	exec(t, db, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m3', 'needs_llm', 'light_newsletter', '{}', 'newsletter')`)
	exec(t, db, `INSERT INTO email_message_comprehension
		(message_id, prompt_version, content_class, scores, summary, key_points, quick_reply, open_loop, escalate, escalate_reason, parsed_json)
		VALUES ('m3','v1','newsletter','{}','FYI','[]',NULL,NULL,0,NULL,'{}')`)
	exec(t, db, `INSERT INTO model_invocation (id, message_id, stage, model, status, created_at)
		VALUES ('i3','m3','mail_comprehension_light','haiku','succeeded','2026-07-04T00:00:00Z')`)
	exec(t, db, `INSERT INTO model_invocation_payload (model_invocation_id, request_text, response_text)
		VALUES ('i3','prompt','response')`)

	tr, err := NewService(db).Trace(context.Background(), "m3")
	if err != nil {
		t.Fatal(err)
	}
	if tr.HeavyLLM.Status != StatusSkipped || tr.HeavyLLM.SkipReason == nil || *tr.HeavyLLM.SkipReason != "light_llm_did_not_escalate" {
		t.Fatalf("heavy skip=%+v", tr.HeavyLLM)
	}
}

func TestTracePendingAndFailedInvocations(t *testing.T) {
	db := testDB(t)
	seedMessage(t, db, "m4")
	exec(t, db, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m4', 'needs_llm', 'light_personal', '{}', 'needs look')`)
	exec(t, db, `INSERT INTO model_invocation (id, message_id, stage, model, status, error, created_at)
		VALUES ('i4','m4','mail_comprehension_light','haiku','failed','provider timeout','2026-07-04T00:00:00Z'),
		       ('i5','m4','mail_comprehension_heavy','opus','pending',NULL,'2026-07-04T00:00:01Z')`)

	tr, err := NewService(db).Trace(context.Background(), "m4")
	if err != nil {
		t.Fatal(err)
	}
	if tr.LightLLM.Status != StatusFailed || tr.LightLLM.Error == nil || *tr.LightLLM.Error != "provider timeout" {
		t.Fatalf("light failed=%+v", tr.LightLLM)
	}
	if tr.HeavyLLM.Status != StatusPending {
		t.Fatalf("heavy pending=%+v", tr.HeavyLLM)
	}
	if tr.HeavyLLM.RequestText == nil || *tr.HeavyLLM.RequestText != PayloadMissing {
		t.Fatalf("heavy payload status=%+v", tr.HeavyLLM.RequestText)
	}
}

func TestTraceLegacyParsedComprehensionDoesNotGuessInvocation(t *testing.T) {
	db := testDB(t)
	seedMessage(t, db, "m5")
	exec(t, db, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m5', 'needs_llm', 'light_personal', '{}', 'legacy')`)
	exec(t, db, `INSERT INTO email_message_comprehension
		(message_id, prompt_version, content_class, scores, summary, key_points, quick_reply, open_loop, escalate, escalate_reason, parsed_json)
		VALUES ('m5','v1','personal','{}','legacy summary','[]',NULL,NULL,0,NULL,'{}')`)
	// A tempting but uncorrelated invocation must not be matched by time, subject, prompt text, or anything else.
	exec(t, db, `INSERT INTO model_invocation (id, message_id, stage, model, status, created_at)
		VALUES ('other',NULL,'mail_comprehension_light','haiku','succeeded','2026-07-04T00:00:00Z')`)
	exec(t, db, `INSERT INTO model_invocation_payload (model_invocation_id, request_text, response_text)
		VALUES ('other','DO NOT USE','DO NOT USE')`)

	tr, err := NewService(db).Trace(context.Background(), "m5")
	if err != nil {
		t.Fatal(err)
	}
	if tr.LightLLM.Status != StatusComplete || tr.LightLLM.RequestText == nil || *tr.LightLLM.RequestText != LegacyUncorrelated {
		t.Fatalf("legacy light trace guessed payload: %+v", tr.LightLLM)
	}
	if len(tr.Invocations) != 0 {
		t.Fatalf("uncorrelated invocation leaked into trace: %+v", tr.Invocations)
	}
}

func TestCreateCorrelatedInvocationPersistsMessageAndStage(t *testing.T) {
	db := testDB(t)
	if err := CreateLightComprehensionInvocation(context.Background(), db, CorrelatedInvocationParams{
		ID: "i-write", MessageID: "m-write", Stage: "ignored", RequestID: "r", Provider: "p", Model: "m", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := CreateHeavyComprehensionInvocation(context.Background(), db, CorrelatedInvocationParams{
		ID: "i-heavy-write", MessageID: "m-write", RequestID: "r2", Provider: "p", Model: "m2",
	}); err != nil {
		t.Fatal(err)
	}
	var messageID, stage string
	if err := db.QueryRow(`SELECT message_id, stage FROM model_invocation WHERE id='i-write'`).Scan(&messageID, &stage); err != nil {
		t.Fatal(err)
	}
	if messageID != "m-write" || stage != StageLight {
		t.Fatalf("message_id/stage = %s/%s", messageID, stage)
	}
	if err := db.QueryRow(`SELECT message_id, stage FROM model_invocation WHERE id='i-heavy-write'`).Scan(&messageID, &stage); err != nil {
		t.Fatal(err)
	}
	if messageID != "m-write" || stage != StageHeavy {
		t.Fatalf("heavy message_id/stage = %s/%s", messageID, stage)
	}
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+t.Name()+"?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	exec(t, db, `CREATE TABLE email_message (
		id TEXT PRIMARY KEY,
		subject TEXT,
		from_address TEXT,
		to_addresses TEXT,
		cc_addresses TEXT,
		received_at TEXT,
		processing_status TEXT
	)`)
	exec(t, db, `CREATE TABLE mail_item_score (
		message_id TEXT PRIMARY KEY,
		stage1_band TEXT,
		stage2_prompt_key TEXT,
		details TEXT,
		rank_rationale TEXT
	)`)
	exec(t, db, `CREATE TABLE email_message_comprehension (
		message_id TEXT PRIMARY KEY,
		prompt_version TEXT,
		content_class TEXT,
		scores TEXT,
		summary TEXT,
		key_points TEXT,
		quick_reply TEXT,
		open_loop TEXT,
		escalate BOOLEAN,
		escalate_reason TEXT,
		parsed_json TEXT
	)`)
	exec(t, db, `CREATE TABLE email_message_comprehension_heavy (
		message_id TEXT PRIMARY KEY,
		draft TEXT,
		options TEXT,
		belief_delta TEXT,
		push_verdict TEXT,
		context_bundle_manifest TEXT,
		escalation_reason TEXT,
		output_json TEXT
	)`)
	exec(t, db, `CREATE TABLE model_invocation (
		id TEXT PRIMARY KEY,
		message_id TEXT,
		stage TEXT,
		request_id TEXT,
		provider TEXT,
		model TEXT,
		model_version TEXT,
		started_at TEXT,
		completed_at TEXT,
		latency_ms INTEGER,
		cost_amount REAL,
		cost_currency TEXT,
		status TEXT,
		error TEXT,
		created_at TEXT
	)`)
	exec(t, db, `CREATE TABLE model_invocation_payload (
		model_invocation_id TEXT PRIMARY KEY,
		request_text TEXT,
		response_text TEXT
	)`)
	return db
}

func seedMessage(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	exec(t, db, `INSERT INTO email_message (id, subject, from_address, to_addresses, cc_addresses, received_at, processing_status)
		VALUES (?, 'Subject', 'sender@example.com', '["you@example.com"]', '[]', '2026-07-04T00:00:00Z', 'processed')`, id)
}

func exec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}
