// Package workintent is the deterministic Phase-1 domain core for typed human
// decisions and automatic work-intent promotion. It performs no I/O and reads no
// clock; Store reconcilers persist its transitions and durable actions atomically.
package workintent

import (
	"errors"
	"fmt"
	"time"
)

type State string

const (
	StateCaptured             State = "captured"
	StateDefining             State = "defining"
	StateAwaitingDecision     State = "awaiting_decision"
	StateReadyForOrchestrator State = "ready_for_orchestrator"
	StateOrchestrating        State = "orchestrating"
	StateSubmitting           State = "submitting"
	StateAdmitted             State = "admitted"
	StateCancelled            State = "cancelled"
	StateSuperseded           State = "superseded"
)

func (s State) Terminal() bool {
	return s == StateAdmitted || s == StateCancelled || s == StateSuperseded
}

type DecisionKind string

const (
	DecisionQuestion      DecisionKind = "question"
	DecisionPlanReview    DecisionKind = "plan_review"
	DecisionDesignReview  DecisionKind = "design_review"
	DecisionAuthorization DecisionKind = "authorization"
	DecisionException     DecisionKind = "exception"
)

type RequestState string

const (
	RequestOpen             RequestState = "open"
	RequestViewed           RequestState = "viewed"
	RequestAnswered         RequestState = "answered"
	RequestApproved         RequestState = "approved"
	RequestChangesRequested RequestState = "changes_requested"
	RequestDeferred         RequestState = "deferred"
	RequestSuperseded       RequestState = "superseded"
	RequestCancelled        RequestState = "cancelled"
)

func (s RequestState) Current() bool { return s == RequestOpen || s == RequestViewed }

type ResponseKind string

const (
	ResponseAnswer         ResponseKind = "answer"
	ResponseApprove        ResponseKind = "approve"
	ResponseRequestChanges ResponseKind = "request_changes"
	ResponseDefer          ResponseKind = "defer"
	ResponseDeny           ResponseKind = "deny"
)

type DecisionRequest struct {
	ID, ProjectID        string
	Kind                 DecisionKind
	State                RequestState
	RequestVersion       int
	SubjectVersion       int
	SubjectSHA256        string
	ExpectedResponseKind []ResponseKind
}

type DecisionResponse struct {
	RequestID       string
	RequestVersion  int
	SubjectVersion  int
	SubjectSHA256   string
	Kind            ResponseKind
	IdempotencyKey  string
	ActorID         string
	Authorization   string
	StructuredValue string
	DeferUntil      time.Time
	DeferCondition  string
}

var (
	ErrStaleSubject       = errors.New("decision subject changed")
	ErrRequestNotCurrent  = errors.New("decision request is not current")
	ErrResponseNotAllowed = errors.New("decision response kind is not allowed")
)

// ValidateResponse is the artifact/version fence at the dashboard write boundary.
// A browser retry reuses IdempotencyKey; persistence owns deduplication.
func ValidateResponse(req DecisionRequest, response DecisionResponse) error {
	if req.ID == "" || req.ProjectID == "" || req.RequestVersion < 1 || req.SubjectVersion < 1 || req.SubjectSHA256 == "" {
		return errors.New("incomplete decision request")
	}
	if response.RequestID != req.ID || response.RequestVersion != req.RequestVersion ||
		response.SubjectVersion != req.SubjectVersion || response.SubjectSHA256 != req.SubjectSHA256 {
		return ErrStaleSubject
	}
	if !req.State.Current() {
		return ErrRequestNotCurrent
	}
	if response.IdempotencyKey == "" || response.ActorID == "" {
		return errors.New("response identity and idempotency key are required")
	}
	allowed := false
	for _, kind := range req.ExpectedResponseKind {
		if response.Kind == kind {
			allowed = true
			break
		}
	}
	if !allowed {
		return ErrResponseNotAllowed
	}
	if response.Kind == ResponseDefer && response.DeferUntil.IsZero() && response.DeferCondition == "" {
		return errors.New("defer response requires a due time or durable condition")
	}
	if (req.Kind == DecisionAuthorization || req.Kind == DecisionException) &&
		(response.Kind == ResponseApprove || response.Kind == ResponseAnswer) && response.Authorization == "" {
		return errors.New("authorization response requires an exact scope")
	}
	return nil
}

func ResultingRequestState(kind ResponseKind) RequestState {
	switch kind {
	case ResponseAnswer:
		return RequestAnswered
	case ResponseApprove:
		return RequestApproved
	case ResponseRequestChanges, ResponseDeny:
		return RequestChangesRequested
	case ResponseDefer:
		return RequestDeferred
	default:
		return ""
	}
}

type Gate struct {
	DecisionID     string
	SubjectVersion int
	SubjectSHA256  string
	Resolved       bool
	Accepted       bool
}

type Intent struct {
	ID, ProjectID            string
	Version, StateVersion    int
	ArtifactSHA256           string
	State                    State
	DefinitionComplete       bool
	Gates                    []Gate
	OrchestratorRegistration string
	OrchestratorActionID     string
	AdmittedEpicID           string
}

type Transition struct {
	From, To State
	Action   string
	Reason   string
}

const ActionEnsureOrchestratorDelivery = "ensure_orchestrator_delivery"

// Advance computes one legal automatic promotion step. In particular, satisfying
// definition and typed gates is the go-signal: no human "send to Flowbee" state or
// transition exists.
func Advance(intent Intent) (Transition, error) {
	if intent.ID == "" || intent.ProjectID == "" || intent.Version < 1 || intent.StateVersion < 1 || intent.ArtifactSHA256 == "" {
		return Transition{}, errors.New("incomplete work intent")
	}
	if intent.State.Terminal() {
		return Transition{From: intent.State, To: intent.State}, nil
	}
	switch intent.State {
	case StateCaptured:
		return Transition{From: intent.State, To: StateDefining, Reason: "capture acknowledged"}, nil
	case StateDefining, StateAwaitingDecision:
		if !intent.DefinitionComplete {
			return Transition{From: intent.State, To: StateDefining, Reason: "definition incomplete"}, nil
		}
		for _, gate := range intent.Gates {
			if gate.DecisionID == "" || gate.SubjectVersion != intent.Version || gate.SubjectSHA256 != intent.ArtifactSHA256 {
				return Transition{From: intent.State, To: StateAwaitingDecision, Reason: "current artifact lacks a version-bound decision"}, nil
			}
			if !gate.Resolved || !gate.Accepted {
				return Transition{From: intent.State, To: StateAwaitingDecision, Reason: "typed decision unresolved"}, nil
			}
		}
		if intent.OrchestratorRegistration == "" {
			return Transition{From: intent.State, To: StateReadyForOrchestrator, Reason: "paired orchestrator route absent"}, nil
		}
		return Transition{From: intent.State, To: StateReadyForOrchestrator, Action: ActionEnsureOrchestratorDelivery, Reason: "definition and typed gates satisfied"}, nil
	case StateReadyForOrchestrator:
		if intent.OrchestratorRegistration == "" {
			return Transition{From: intent.State, To: intent.State, Reason: "paired orchestrator route absent"}, nil
		}
		return Transition{From: intent.State, To: intent.State, Action: ActionEnsureOrchestratorDelivery, Reason: "delivery obligation must exist"}, nil
	case StateOrchestrating:
		return Transition{From: intent.State, To: intent.State, Reason: "await versioned epic contract"}, nil
	case StateSubmitting:
		if intent.AdmittedEpicID != "" {
			return Transition{From: intent.State, To: StateAdmitted, Reason: "admission acknowledged"}, nil
		}
		return Transition{From: intent.State, To: intent.State, Reason: "verify admission by original idempotency key"}, nil
	default:
		return Transition{}, fmt.Errorf("unknown work intent state %q", intent.State)
	}
}

func AdmissionKey(intent Intent) (string, error) {
	if intent.ProjectID == "" || intent.ID == "" || intent.Version < 1 {
		return "", errors.New("incomplete work intent admission identity")
	}
	return fmt.Sprintf("work-intent:%s:%s:v%d", intent.ProjectID, intent.ID, intent.Version), nil
}
