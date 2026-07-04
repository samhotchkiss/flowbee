package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

func TestMailTraceEndpointRequiresAuthAndReturnsTrace(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/flowbee.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := store.MigrateUp(ctx, st.DB); err != nil {
		t.Fatal(err)
	}
	createMailTraceTables(t, st)
	execSQL(t, st, `INSERT INTO email_message (id, subject, from_address, to_addresses, cc_addresses, received_at, processing_status)
		VALUES ('m-api','API subject','sender@example.com','["you@example.com"]','[]','2026-07-04T00:00:00Z','processed')`)
	execSQL(t, st, `INSERT INTO mail_item_score (message_id, stage1_band, stage2_prompt_key, details, rank_rationale)
		VALUES ('m-api','needs_llm','light_personal','{"known_contact":true}','ranked')`)
	execSQL(t, st, `INSERT INTO model_invocation (id, message_id, stage, model, status, created_at)
		VALUES ('i-api','m-api','mail_comprehension_light','haiku','succeeded','2026-07-04T00:00:00Z')`)
	execSQL(t, st, `INSERT INTO model_invocation_payload (model_invocation_id, request_text, response_text)
		VALUES ('i-api','PROMPT','RESPONSE')`)

	authn := auth.NewBearer([]byte("server-secret"), []string{"superadmin", "builder"}, false)
	srv := api.New(st, clock.NewFake(time.Unix(1000, 0)), ulid.NewMinter(nil), api.Config{Authenticator: authn}, "v")
	h := srv.PrivateHandler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/mail/messages/m-api/trace", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trace status=%d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/mail/messages/m-api/trace", nil)
	req.Header.Set("Authorization", "Bearer "+authn.Mint("builder"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("builder trace status=%d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/admin/mail/messages/m-api/trace", nil)
	req.Header.Set("Authorization", "Bearer "+authn.Mint("superadmin"))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{`"message_id":"m-api"`, `"request_text":"PROMPT"`, `"stage2_prompt_key":"light_personal"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("trace response missing %s: %s", want, body)
		}
	}
}

func createMailTraceTables(t *testing.T, st *store.Store) {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE email_message (id TEXT PRIMARY KEY, subject TEXT, from_address TEXT, to_addresses TEXT, cc_addresses TEXT, received_at TEXT, processing_status TEXT)`,
		`CREATE TABLE mail_item_score (message_id TEXT PRIMARY KEY, stage1_band TEXT, stage2_prompt_key TEXT, details TEXT, rank_rationale TEXT)`,
		`CREATE TABLE email_message_comprehension (message_id TEXT PRIMARY KEY, prompt_version TEXT, content_class TEXT, scores TEXT, summary TEXT, key_points TEXT, quick_reply TEXT, open_loop TEXT, escalate BOOLEAN, escalate_reason TEXT, parsed_json TEXT)`,
		`CREATE TABLE email_message_comprehension_heavy (message_id TEXT PRIMARY KEY, draft TEXT, options TEXT, belief_delta TEXT, push_verdict TEXT, context_bundle_manifest TEXT, escalation_reason TEXT, output_json TEXT)`,
		`CREATE TABLE model_invocation (id TEXT PRIMARY KEY, message_id TEXT, stage TEXT, request_id TEXT, provider TEXT, model TEXT, model_version TEXT, started_at TEXT, completed_at TEXT, latency_ms INTEGER, cost_amount REAL, cost_currency TEXT, status TEXT, error TEXT, created_at TEXT)`,
		`CREATE TABLE model_invocation_payload (model_invocation_id TEXT PRIMARY KEY, request_text TEXT, response_text TEXT)`,
	} {
		execSQL(t, st, q)
	}
}

func execSQL(t *testing.T, st *store.Store, q string) {
	t.Helper()
	if _, err := st.DB.Exec(q); err != nil {
		t.Fatalf("exec %s: %v", q, err)
	}
}
