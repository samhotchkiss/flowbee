package workintent_test

import (
	"errors"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/workintent"
)

func TestChangedArtifactCannotConsumeOldApproval(t *testing.T) {
	req := workintent.DecisionRequest{
		ID: "decision-1", ProjectID: "project-a", Kind: workintent.DecisionPlanReview,
		State: workintent.RequestOpen, RequestVersion: 2, SubjectVersion: 4,
		SubjectSHA256: "sha256:new", ExpectedResponseKind: []workintent.ResponseKind{workintent.ResponseApprove},
	}
	response := workintent.DecisionResponse{
		RequestID: req.ID, RequestVersion: req.RequestVersion, SubjectVersion: 3,
		SubjectSHA256: "sha256:old", Kind: workintent.ResponseApprove,
		IdempotencyKey: "response-1", ActorID: "sam",
	}
	if err := workintent.ValidateResponse(req, response); !errors.Is(err, workintent.ErrStaleSubject) {
		t.Fatalf("stale approval accepted: %v", err)
	}
}

func TestDeferRequiresDurableWakeCondition(t *testing.T) {
	req := workintent.DecisionRequest{
		ID: "decision-1", ProjectID: "project-a", Kind: workintent.DecisionQuestion,
		State: workintent.RequestViewed, RequestVersion: 1, SubjectVersion: 1,
		SubjectSHA256: "sha256:subject", ExpectedResponseKind: []workintent.ResponseKind{workintent.ResponseDefer},
	}
	response := workintent.DecisionResponse{
		RequestID: req.ID, RequestVersion: 1, SubjectVersion: 1,
		SubjectSHA256: req.SubjectSHA256, Kind: workintent.ResponseDefer,
		IdempotencyKey: "response-1", ActorID: "sam",
	}
	if err := workintent.ValidateResponse(req, response); err == nil {
		t.Fatal("unbounded defer accepted")
	}
	response.DeferUntil = time.Unix(1000, 0)
	if err := workintent.ValidateResponse(req, response); err != nil {
		t.Fatalf("bounded defer rejected: %v", err)
	}
}

func TestSatisfiedIntentAutomaticallyRoutesWithoutSecondHumanGo(t *testing.T) {
	intent := workintent.Intent{
		ID: "intent-1", ProjectID: "project-a", Version: 3, StateVersion: 7,
		ArtifactSHA256: "sha256:current", State: workintent.StateAwaitingDecision,
		DefinitionComplete: true, OrchestratorRegistration: "orchestrator-project-a",
		Gates: []workintent.Gate{{DecisionID: "design-1", SubjectVersion: 3,
			SubjectSHA256: "sha256:current", Resolved: true, Accepted: true}},
	}
	got, err := workintent.Advance(intent)
	if err != nil {
		t.Fatal(err)
	}
	if got.To != workintent.StateReadyForOrchestrator || got.Action != workintent.ActionEnsureOrchestratorDelivery {
		t.Fatalf("ready intent did not auto-route: %+v", got)
	}
	key, err := workintent.AdmissionKey(intent)
	if err != nil || key != "work-intent:project-a:intent-1:v3" {
		t.Fatalf("admission key=%q err=%v", key, err)
	}
}

func TestIntentGateMustBindCurrentVersionAndHash(t *testing.T) {
	intent := workintent.Intent{
		ID: "intent-1", ProjectID: "project-a", Version: 4, StateVersion: 8,
		ArtifactSHA256: "sha256:new", State: workintent.StateDefining,
		DefinitionComplete: true, OrchestratorRegistration: "orch-a",
		Gates: []workintent.Gate{{DecisionID: "plan", SubjectVersion: 3,
			SubjectSHA256: "sha256:old", Resolved: true, Accepted: true}},
	}
	got, err := workintent.Advance(intent)
	if err != nil {
		t.Fatal(err)
	}
	if got.To != workintent.StateAwaitingDecision || got.Action != "" {
		t.Fatalf("stale gate advanced intent: %+v", got)
	}
}
