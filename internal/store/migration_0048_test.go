package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// The 0048 rebuild must retain already-committed action/evidence history while
// changing new Flowbee product messages to a non-session control origin.
func TestMigration0048PreservesLegacyDeliveryEvidenceAndAllowsControlPrincipal(t *testing.T) {
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
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") && entry.Name() < "0048_" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		body, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := st.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply %s: %v", name, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %s: %v", name, err)
		}
	}

	const stamp = "2026-07-19T12:00:00Z"
	seed := []string{
		`INSERT INTO driver_session_bindings
		 (binding_id,project_id,worker_identity,role,binding_epoch,state,host_id,store_id,
		 tmux_server_instance_id,lifecycle_key,target_epoch,profile_id,workspace_root_id,
		 workspace_relative_path,session_id,pane_instance_id,agent_run_id,observed_at)
		 VALUES ('legacy-sender','default','legacy-sender','orchestrator',1,'active','h','s','ts','ls',1,'p','r','w','ss','sp','sr', '` + stamp + `'),
		        ('recipient','default','interactor','interactor',1,'active','h','s','ts','lr',1,'p','r','w','rs','rp','rr','` + stamp + `')`,
		`INSERT INTO driver_observation_events
		 (store_id,event_id,store_seq,cursor,session_seq,transition_id,transition_index,
		 transition_count,host_id,session_id,pane_instance_id,producer_boot_id,kind,
		 observed_at,envelope_sha256,envelope_json)
		 VALUES ('s','event-1',1,'c',1,'t',0,1,'h','rs','rp','boot','phase','` + stamp + `','sha256:e','{}')`,
		`INSERT INTO decision_requests
		 (id,project_id,kind,title,prompt,expected_response_kinds_json,requested_by,route_to,
		 subject_artifact_ref,subject_version,subject_sha256,created_at,updated_at)
		 VALUES ('request','default','question','q','q','["answer"]','interactor','human','artifact',1,'sha256:s','` + stamp + `','` + stamp + `')`,
		`INSERT INTO decision_responses
		 (id,project_id,request_id,request_version,subject_version,subject_sha256,kind,
		 actor_id,idempotency_key,created_at)
		 VALUES ('response','default','request',1,1,'sha256:s','answer','human','once','` + stamp + `')`,
		`INSERT INTO decision_response_actions
		 (id,project_id,request_id,response_id,dedup_key,payload_json,payload_sha256,
		 target_actor_id,sender_binding_id,target_binding_id,grant_id,created_at,updated_at)
		 VALUES ('decision-action','default','request','response','decision-dedup','{}','sha256:d',
		 'interactor','legacy-sender','recipient','grant-d','` + stamp + `','` + stamp + `')`,
		`INSERT INTO decision_response_action_evidence
		 (action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
		 agent_run_id,evidence_kind,payload_sha256,created_at,updated_at)
		 VALUES ('decision-action',0,'s','event-1',1,'rs','rp','rr','provider_user_message_hash','sha256:d','` + stamp + `','` + stamp + `')`,
		`INSERT INTO conversation_threads
		 (id,project_id,conversation_key,interactor_actor_id,focus_ref,creation_idempotency_key,
		 created_at,updated_at)
		 VALUES ('thread','default','primary','interactor','default','thread-once','` + stamp + `','` + stamp + `')`,
		`INSERT INTO conversation_messages
		 (id,project_id,thread_id,thread_seq,role,actor_id,content_text,content_sha256,idempotency_key,created_at)
		 VALUES ('message','default','thread',1,'human','human','hello','sha256:m','message-once','` + stamp + `')`,
		`INSERT INTO conversation_message_actions
		 (id,project_id,thread_id,message_id,dedup_key,payload_text,payload_sha256,
		 target_actor_id,sender_binding_id,target_binding_id,grant_id,created_at,updated_at)
		 VALUES ('conversation-action','default','thread','message','conversation-dedup','hello','sha256:m',
		 'interactor','legacy-sender','recipient','grant-c','` + stamp + `','` + stamp + `')`,
		`INSERT INTO conversation_message_action_evidence
		 (action_id,action_epoch,store_id,event_id,store_seq,session_id,pane_instance_id,
		 agent_run_id,evidence_kind,payload_sha256,created_at,updated_at)
		 VALUES ('conversation-action',0,'s','event-1',1,'rs','rp','rr','provider_user_message_hash','sha256:m','` + stamp + `','` + stamp + `')`,
		`INSERT INTO work_intents
		 (id,project_id,source_message_id,source_message_version,interactor_incarnation_id,title,
		 artifact_ref,intent_version,artifact_sha256,owner_actor_id,submission_idempotency_key,
		 created_at,updated_at)
		 VALUES ('intent','default','source-message',1,'interactor-run','intent','artifact',1,
		 'sha256:i','interactor','intent-once','` + stamp + `','` + stamp + `')`,
		`INSERT INTO work_intent_actions
		 (id,project_id,work_intent_id,intent_version,kind,dedup_key,payload_json,payload_sha256,
		 target_actor_id,target_incarnation,sender_binding_id,grant_id,created_at,updated_at)
		 VALUES ('intent-action','default','intent',1,'deliver_to_orchestrator','intent-dedup','{}',
		 'sha256:i','orchestrator','recipient','legacy-sender','grant-i','` + stamp + `','` + stamp + `')`,
	}
	for _, query := range seed {
		if _, err := st.DB.ExecContext(ctx, query); err != nil {
			t.Fatalf("seed pre-0048: %v\n%s", err, query)
		}
	}
	body, err := os.ReadFile("migrations/0048_driver_control_principal.sql")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"decision_response_action_evidence", "conversation_message_action_evidence"} {
		var count int
		if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d err=%v", table, count, err)
		}
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE conversation_message_actions SET state='fenced'
		WHERE id='conversation-action'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO conversation_message_actions
		(id,project_id,thread_id,message_id,dedup_key,payload_text,payload_sha256,target_actor_id,
		sender_principal_id,sender_binding_id,target_binding_id,grant_id,created_at,updated_at)
		VALUES ('control-action','default','thread','message','control-dedup','hello','sha256:m',
		'interactor','flowbee-control',NULL,'recipient','grant-v24',?,?)`, stamp, stamp); err != nil {
		t.Fatalf("insert v2.4 control-origin action: %v", err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epics
		(id,repo,file_path,state,project_id,slug,admission_key,created_at,updated_at)
		VALUES ('epic-origin','repo','epics/origin.md','running','default','epic-origin','origin-once',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO epic_deliveries
		(epic_id,project_id,state,created_at,updated_at) VALUES ('epic-origin','default','building',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	reject := func(name, query string, args ...any) {
		t.Helper()
		if _, err := st.DB.ExecContext(ctx, query, args...); err == nil {
			t.Fatalf("%s unexpectedly accepted", name)
		}
	}
	reject("empty driver epic action", `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,executor_kind,dedup_key,payload_json,payload_sha256,created_at,updated_at)
		VALUES ('bad-epic-empty','default','epic-origin','review_wake','driver','bad-empty','{}','sha256:b',?,?)`, stamp, stamp)
	reject("mixed driver epic action", `INSERT INTO epic_actions
		(id,project_id,epic_id,kind,executor_kind,dedup_key,payload_json,payload_sha256,
		sender_principal_id,sender_session_id,sender_agent_run_id,created_at,updated_at)
		VALUES ('bad-epic-mixed','default','epic-origin','review_wake','driver','bad-mixed','{}','sha256:b',
		'flowbee-control','session','run',?,?)`, stamp, stamp)
	reject("empty driver grant", `INSERT INTO driver_grants
		(grant_id,project_id,action_id,recipient_session_id,recipient_pane_instance_id,grant_epoch,issued_at,expires_at)
		VALUES ('bad-grant-empty','default','bad', 'recipient','pane',1,?,?)`, stamp, stamp)
	reject("mixed driver grant", `INSERT INTO driver_grants
		(grant_id,project_id,action_id,sender_principal_id,sender_session_id,sender_agent_run_id,
		recipient_session_id,recipient_pane_instance_id,grant_epoch,issued_at,expires_at)
		VALUES ('bad-grant-mixed','default','bad','flowbee-control','session','run','recipient','pane',1,?,?)`, stamp, stamp)
	reject("empty driver receipt", `INSERT INTO driver_receipts
		(delivery_id,action_id,grant_id,grant_epoch,payload_sha256,status,created_at)
		VALUES ('bad-receipt-empty','bad','grant',1,'sha256:b','submitted',?)`, stamp)
	reject("mixed driver receipt", `INSERT INTO driver_receipts
		(delivery_id,action_id,grant_id,grant_epoch,sender_principal_id,sender_session_id,payload_sha256,status,created_at)
		VALUES ('bad-receipt-mixed','bad','grant',1,'flowbee-control','session','sha256:b','submitted',?)`, stamp)
	reject("mixed work-intent origin", `UPDATE work_intent_actions
		SET sender_principal_id='flowbee-control' WHERE id='intent-action'`)
	reject("empty work-intent origin", `UPDATE work_intent_actions
		SET sender_binding_id='' WHERE id='intent-action'`)
	rows, err := st.DB.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid, fkid int64
		_ = rows.Scan(&table, &rowid, &parent, &fkid)
		t.Fatalf("foreign-key violation table=%s rowid=%d parent=%s fk=%d", table, rowid, parent, fkid)
	}
}
