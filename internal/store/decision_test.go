package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestDecisionResponseRejectsStaleSubjectAndDeduplicatesBrowserRetry(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	subjectHash := decisionHash("plan-v2")
	req, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "plan-review", ProjectID: "default", Kind: workintent.DecisionPlanReview,
		Title: "Review launch plan", Prompt: "Approve the exact attached plan?",
		Options: json.RawMessage(`[{"id":"approve"}]`), ResponseSchema: json.RawMessage(`{"type":"object"}`),
		ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove, workintent.ResponseRequestChanges},
		Priority:              2, RequestedBy: "orchestrator:default", RouteTo: "human:sam",
		SubjectArtifactRef: "artifact://plans/plan-v2", SubjectVersion: 2, SubjectSHA256: subjectHash,
		EvidenceRefs: json.RawMessage(`[{"kind":"diff","ref":"artifact://diff/2"}]`),
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDecisionRequestViewed(ctx, "default", req.ID, 1, "human:sam", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkDecisionRequestViewed(ctx, "default", req.ID, 1, "human:sam", now.Add(45*time.Second)); err != nil {
		t.Fatalf("view retry should be idempotent: %v", err)
	}

	input := store.DecisionResponseInput{RequestID: req.ID, RequestVersion: req.RequestVersion,
		SubjectVersion: req.SubjectVersion, SubjectSHA256: decisionHash("plan-v1"),
		Kind: workintent.ResponseApprove, StructuredValue: json.RawMessage(`{"approved":true}`),
		Comment: "ship it", ActorID: "human:sam", IdempotencyKey: "browser-submit-1"}
	if _, err := st.RespondDecision(ctx, "default", input, now.Add(time.Minute)); !errors.Is(err, workintent.ErrStaleSubject) {
		t.Fatalf("stale artifact response err=%v, want ErrStaleSubject", err)
	}
	var responses int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_responses WHERE request_id=?`, req.ID).Scan(&responses); err != nil || responses != 0 {
		t.Fatalf("stale response persisted: count=%d err=%v", responses, err)
	}

	input.SubjectSHA256 = subjectHash
	first, err := st.RespondDecision(ctx, "default", input, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := st.RespondDecision(ctx, "default", input, now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != retry.ID || !first.CreatedAt.Equal(retry.CreatedAt) {
		t.Fatalf("retry returned a different response: first=%+v retry=%+v", first, retry)
	}
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_responses WHERE request_id=?`, req.ID).Scan(&responses); err != nil || responses != 1 {
		t.Fatalf("browser retry count=%d err=%v, want 1", responses, err)
	}
	var state, currentResponse string
	if err := st.DB.QueryRowContext(ctx, `SELECT state,current_response_id FROM decision_requests WHERE id=?`, req.ID).Scan(&state, &currentResponse); err != nil {
		t.Fatal(err)
	}
	if state != "approved" || currentResponse != first.ID {
		t.Fatalf("request projection state=%s response=%s", state, currentResponse)
	}
	var responseEvents int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_events
		WHERE project_id='default' AND kind='decision_response_recorded'
		AND json_extract(payload_json,'$.decision_id')=?`, req.ID).Scan(&responseEvents); err != nil || responseEvents != 1 {
		t.Fatalf("response events=%d err=%v, want 1", responseEvents, err)
	}
	var viewedEvents int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM control_events
		WHERE project_id='default' AND kind='decision_request_viewed'
		AND json_extract(payload_json,'$.decision_id')=?`, req.ID).Scan(&viewedEvents); err != nil || viewedEvents != 1 {
		t.Fatalf("viewed events=%d err=%v, want 1", viewedEvents, err)
	}

	input.Comment = "changed after retry"
	if _, err := st.RespondDecision(ctx, "default", input, now.Add(4*time.Minute)); !errors.Is(err, store.ErrDecisionIdempotencyConflict) {
		t.Fatalf("changed body with reused key err=%v, want idempotency conflict", err)
	}
}

func TestDeferredDecisionReopenFencesOldBrowserVersion(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 16, 0, 0, 0, time.UTC)
	hash := decisionHash("design-v4")
	req, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
		ID: "design-review", ProjectID: "default", Kind: workintent.DecisionDesignReview,
		Title: "Review design", Prompt: "Approve or defer this design.",
		ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseApprove, workintent.ResponseDefer},
		RequestedBy:           "interactor:default", RouteTo: "human:sam",
		SubjectArtifactRef: "artifact://design/v4", SubjectVersion: 4, SubjectSHA256: hash,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	deferUntil := now.Add(time.Hour)
	deferred, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: req.ID, RequestVersion: 1, SubjectVersion: 4, SubjectSHA256: hash,
		Kind: workintent.ResponseDefer, ActorID: "human:sam", IdempotencyKey: "defer-1",
		DeferUntil: deferUntil, Comment: "come back after the review",
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if deferred.Kind != workintent.ResponseDefer {
		t.Fatalf("response kind=%s", deferred.Kind)
	}
	if err := st.ReopenDeferredDecision(ctx, "default", req.ID, 1, false, "reconciler:decisions", now.Add(30*time.Minute)); !errors.Is(err, store.ErrDecisionDeferralActive) {
		t.Fatalf("early reopen err=%v, want active deferral", err)
	}
	if err := st.ReopenDeferredDecision(ctx, "default", req.ID, 1, false, "reconciler:decisions", deferUntil); err != nil {
		t.Fatal(err)
	}
	reopened, err := st.GetDecisionRequest(ctx, "default", req.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.State != workintent.RequestOpen || reopened.RequestVersion != 2 || reopened.CurrentResponseID != "" {
		t.Fatalf("reopened request=%+v", reopened)
	}

	oldTab := store.DecisionResponseInput{RequestID: req.ID, RequestVersion: 1,
		SubjectVersion: 4, SubjectSHA256: hash, Kind: workintent.ResponseApprove,
		ActorID: "human:sam", IdempotencyKey: "old-tab-approval"}
	if _, err := st.RespondDecision(ctx, "default", oldTab, deferUntil.Add(time.Minute)); !errors.Is(err, workintent.ErrStaleSubject) {
		t.Fatalf("old browser response err=%v, want stale subject/version", err)
	}
	oldTab.RequestVersion = 2
	oldTab.IdempotencyKey = "current-approval"
	if _, err := st.RespondDecision(ctx, "default", oldTab, deferUntil.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_responses WHERE request_id=?`, req.ID).Scan(&count); err != nil || count != 2 {
		t.Fatalf("response ledger count=%d err=%v, want defer+approval", count, err)
	}
}

func TestDecisionSupersessionCancellationProjectScopeAndAppendOnlyLedger(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 17, 0, 0, 0, time.UTC)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO projects(id,name,state,created_at,updated_at)
		VALUES ('other','Other','active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	makeRequest := func(id, project string) store.DecisionRequest {
		t.Helper()
		r, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
			ID: id, ProjectID: project, Kind: workintent.DecisionQuestion,
			Title: "Question", Prompt: "Choose the durable answer.",
			ExpectedResponseKinds: []workintent.ResponseKind{workintent.ResponseAnswer},
			RequestedBy:           "orchestrator:" + project, RouteTo: "human:sam",
			SubjectArtifactRef: "artifact://question/" + id, SubjectVersion: 1,
			SubjectSHA256: decisionHash(project + ":" + id),
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	old := makeRequest("old", "default")
	replacement := makeRequest("replacement", "default")
	other := makeRequest("other-replacement", "other")
	if _, err := st.GetDecisionRequest(ctx, "other", old.ID); !errors.Is(err, store.ErrDecisionNotFound) {
		t.Fatalf("cross-project read err=%v, want not found", err)
	}
	if err := st.SupersedeDecisionRequest(ctx, "default", old.ID, 1, other.ID, "human:sam", "wrong project", now.Add(time.Minute)); !errors.Is(err, store.ErrDecisionNotFound) {
		t.Fatalf("cross-project replacement err=%v, want not found", err)
	}
	if err := st.SupersedeDecisionRequest(ctx, "default", old.ID, 1, replacement.ID, "human:sam", "new artifact", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	old, err := st.GetDecisionRequest(ctx, "default", old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != workintent.RequestSuperseded || old.SupersededBy != replacement.ID {
		t.Fatalf("superseded request=%+v", old)
	}
	if err := st.CancelDecisionRequest(ctx, "default", replacement.ID, 1, "human:sam", "no longer relevant", now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}

	respondable := makeRequest("respondable", "default")
	resp, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: respondable.ID, RequestVersion: 1, SubjectVersion: 1,
		SubjectSHA256: respondable.SubjectSHA256, Kind: workintent.ResponseAnswer,
		StructuredValue: json.RawMessage(`{"answer":"yes"}`), ActorID: "human:sam",
		IdempotencyKey: "immutable-response",
	}, now.Add(4*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.ExecContext(ctx, `UPDATE decision_responses SET comment='tampered' WHERE id=?`, resp.ID); err == nil {
		t.Fatal("append-only response allowed UPDATE")
	}
	if _, err := st.DB.ExecContext(ctx, `DELETE FROM decision_responses WHERE id=?`, resp.ID); err == nil {
		t.Fatal("append-only response allowed DELETE")
	}
	var stillThere int
	if err := st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM decision_responses WHERE id=?`, resp.ID).Scan(&stillThere); err != nil || stillThere != 1 {
		t.Fatalf("response disappeared after rejected mutation: count=%d err=%v", stillThere, err)
	}
}

func TestDecisionInboxProjectionKeepsAllCurrentAndBoundsResolvedTrail(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 19, 0, 0, 0, time.UTC)
	if err := st.AddEpicRun(ctx, store.EpicRun{ID: "inbox-epic", Repo: "russ", Branch: "epic/inbox",
		TmuxName: "epic-inbox"}, 1, now); err != nil {
		t.Fatal(err)
	}
	create := func(id string, epicID string, kind workintent.DecisionKind, responses []workintent.ResponseKind) store.DecisionRequest {
		t.Helper()
		row, err := st.CreateDecisionRequest(ctx, store.CreateDecisionRequestInput{
			ID: id, ProjectID: "default", EpicID: epicID, Kind: kind, Title: "Decision " + id,
			Prompt: "Record the exact typed response.", ExpectedResponseKinds: responses,
			RequestedBy: "interactor:default", RouteTo: "human:sam",
			SubjectArtifactRef: "artifact://" + id, SubjectVersion: 1, SubjectSHA256: decisionHash(id),
		}, now)
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	blocking := create("blocking-current", "inbox-epic", workintent.DecisionPlanReview,
		[]workintent.ResponseKind{workintent.ResponseApprove})
	if err := st.MarkDecisionRequestViewed(ctx, "default", blocking.ID, blocking.RequestVersion,
		"human:sam", now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	create("second-current", "", workintent.DecisionQuestion, []workintent.ResponseKind{workintent.ResponseAnswer})
	older := create("older-resolved", "", workintent.DecisionQuestion, []workintent.ResponseKind{workintent.ResponseAnswer})
	if _, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: older.ID, RequestVersion: 1, SubjectVersion: 1, SubjectSHA256: older.SubjectSHA256,
		Kind: workintent.ResponseAnswer, StructuredValue: json.RawMessage(`"old"`), ActorID: "human:sam",
		IdempotencyKey: "older-response",
	}, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	newer := create("newer-resolved", "", workintent.DecisionQuestion, []workintent.ResponseKind{workintent.ResponseAnswer})
	response, err := st.RespondDecision(ctx, "default", store.DecisionResponseInput{
		RequestID: newer.ID, RequestVersion: 1, SubjectVersion: 1, SubjectSHA256: newer.SubjectSHA256,
		Kind: workintent.ResponseAnswer, StructuredValue: json.RawMessage(`"new"`), ActorID: "human:sam",
		IdempotencyKey: "newer-response",
	}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListDecisionInboxAllProjects(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("inbox rows=%d, want every 2 current rows plus 1 recent terminal: %+v", len(rows), rows)
	}
	byID := map[string]store.DecisionInboxRow{}
	for _, row := range rows {
		byID[row.Request.ID] = row
	}
	if !byID[blocking.ID].Blocking {
		t.Fatalf("epic-bound decision did not project blocking=true: %+v", byID[blocking.ID])
	}
	if byID[blocking.ID].ViewedAt.IsZero() {
		t.Fatalf("durable viewed event was not projected: %+v", byID[blocking.ID])
	}
	if _, ok := byID[older.ID]; ok {
		t.Fatalf("resolved history limit retained older row: %+v", rows)
	}
	got, ok := byID[newer.ID]
	if !ok || got.ResponseKind != workintent.ResponseAnswer || got.ResponseActorID != "human:sam" ||
		got.DownstreamAckState != "pending" || got.Request.CurrentResponseID != response.ID {
		t.Fatalf("current response acknowledgement projection=%+v", got)
	}
}

func decisionHash(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}
