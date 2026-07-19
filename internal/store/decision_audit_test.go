package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestDecisionAuditExportsExactFenceActorScopeTransitionAndAck(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	request, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "audit-exact", ProjectID: "default", Kind: workintent.DecisionAuthorization,
		Title: "Ship exact build", Prompt: "Approve the displayed immutable build only.",
		ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
		RequestedBy:           "interactor:default", RouteTo: "human:sam",
		SubjectArtifactRef: "artifact://build/exact", SubjectVersion: 7,
		SubjectSHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	response, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: request.ID, RequestVersion: request.RequestVersion,
		SubjectVersion: request.SubjectVersion, SubjectSHA256: request.SubjectSHA256,
		Kind: workintent.ResponseApprove, StructuredValue: json.RawMessage(`{"approved":true}`),
		ActorID: "sam", AuthorizationScope: "project:default", IdempotencyKey: "audit-exact-response",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	ackPayload := `{"decision_id":"audit-exact","response_id":"` + response.ID +
		`","request_version":1,"subject_version":7,"subject_sha256":"` + request.SubjectSHA256 + `"}`
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO control_events
		(project_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
		VALUES ('default','decision_response_acknowledged','pending','acknowledged',1,
		'interactor','interactor:default',?,?)`, ackPayload, now.Add(2*time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListDecisionAudit(ctx, "default", request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Responses) != 1 {
		t.Fatalf("audit rows=%+v", rows)
	}
	got := rows[0].Responses[0]
	if got.RequestSHA256 != request.SubjectSHA256 || got.Response.RequestVersion != 1 ||
		got.Response.SubjectVersion != 7 || got.Response.ActorID != "sam" ||
		got.Response.AuthorizationScope != "project:default" || !got.AuthorityExact {
		t.Fatalf("exact response lost its audit fence: %+v", got)
	}
	if got.ResultingTransition == nil || got.ResultingTransition.Kind != "decision_response_recorded" ||
		got.ResultingTransition.FromState != "open" || got.ResultingTransition.ToState != "approved" {
		t.Fatalf("resulting transition=%+v", got.ResultingTransition)
	}
	if got.AcknowledgementState != "acknowledged" || len(got.AcknowledgementEventRefs) != 1 ||
		got.AcknowledgementEventRefs[0].Kind != "decision_response_acknowledged" || got.Response.ID != response.ID {
		t.Fatalf("ack/response projection=%+v", got)
	}
}

func TestDecisionAuditNeverPresentsChangedOrBroaderArtifactAsAuthorized(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	request, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "audit-fail-closed", ProjectID: "default", Kind: workintent.DecisionAuthorization,
		Title: "Exact exception", Prompt: "Authorize one exact artifact.",
		ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove},
		RequestedBy:           "interactor:default", RouteTo: "human:sam",
		SubjectArtifactRef: "artifact://exception/1", SubjectVersion: 1,
		SubjectSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	created := now.Add(time.Minute).Format(time.RFC3339Nano)
	insert := func(id, hash, scope string) {
		t.Helper()
		_, err := st.DB.ExecContext(ctx, `INSERT INTO decision_responses
			(id,project_id,request_id,request_version,subject_version,subject_sha256,kind,
			 structured_value_json,comment,actor_id,authorization_scope,defer_until,defer_condition,
			 downstream_ack_state,audit_ref,idempotency_key,created_at)
			VALUES (?,'default',?,1,1,?,'approve','{}','','sam',?,'','','pending','',?,?)`,
			id, request.ID, hash, scope, id, created)
		if err != nil {
			t.Fatal(err)
		}
		// A forged transition must also match every immutable response fence;
		// merely naming the response ID is insufficient.
		_, err = st.DB.ExecContext(ctx, `INSERT INTO control_events
			(project_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
			VALUES ('default','decision_response_recorded','open','approved',1,'human','sam',?,?)`,
			`{"decision_id":"audit-fail-closed","response_id":"`+id+`","request_version":1,"subject_version":1,"subject_sha256":"`+hash+`"}`,
			created)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("broader-response", request.SubjectSHA256, "project:*")
	insert("changed-response", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", "project:default")

	rows, err := st.ListDecisionAudit(ctx, "default", request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || len(rows[0].Responses) != 2 {
		t.Fatalf("audit rows=%+v", rows)
	}
	for _, got := range rows[0].Responses {
		if got.AuthorityExact || got.AuthorityFailure == "" {
			t.Fatalf("unsafe historical response appeared authorized: %+v", got)
		}
	}
	if _, err := st.ListDecisionAudit(ctx, "other", request.ID); err != store.ErrDecisionNotFound {
		t.Fatalf("cross-project lookup error=%v want decision not found", err)
	}
}
