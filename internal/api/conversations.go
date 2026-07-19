package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
	"github.com/samhotchkiss/flowbee/internal/store"
)

const conversationSchemaVersion = "flowbee.conversation/v1"

type conversationThreadView struct {
	ID                      string                      `json:"id"`
	ProjectID               string                      `json:"project_id"`
	ConversationKey         string                      `json:"conversation_key"`
	Title                   string                      `json:"title"`
	InteractorActorID       string                      `json:"interactor_actor_id"`
	InteractorBindingID     string                      `json:"interactor_binding_id"`
	InteractorIncarnationID string                      `json:"interactor_incarnation_id"`
	State                   string                      `json:"state"`
	StateVersion            int                         `json:"state_version"`
	FocusKind               store.ConversationFocusKind `json:"focus_kind"`
	FocusRef                string                      `json:"focus_ref"`
	FocusArtifactSHA256     string                      `json:"focus_artifact_sha256,omitempty"`
	LastMessageSeq          int64                       `json:"last_message_seq"`
	CreatedAt               time.Time                   `json:"created_at"`
	UpdatedAt               time.Time                   `json:"updated_at"`
}

func viewConversationThread(row store.ConversationThread) conversationThreadView {
	return conversationThreadView{
		ID: row.ID, ProjectID: row.ProjectID, ConversationKey: row.ConversationKey, Title: row.Title,
		InteractorActorID: row.InteractorActorID, InteractorBindingID: row.InteractorBindingID,
		InteractorIncarnationID: row.InteractorIncarnationID, State: row.State,
		StateVersion: row.StateVersion, FocusKind: row.FocusKind, FocusRef: row.FocusRef,
		FocusArtifactSHA256: row.FocusArtifactSHA256, LastMessageSeq: row.LastMessageSeq,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

type conversationMessageView struct {
	ID, ProjectID, ThreadID                             string    `json:"-"`
	ThreadSeq                                           int64     `json:"thread_seq"`
	Role, ActorID, AgentIncarnationID, ReplyToMessageID string    `json:"-"`
	ContentText, ContentArtifactRef, ContentSHA256      string    `json:"-"`
	StreamState, IdempotencyKey                         string    `json:"-"`
	DeliveryState                                       string    `json:"-"`
	DeliveryStateVersion                                int       `json:"delivery_state_version"`
	DeliveryActionID, DeliveryReceiptRef, DeliveryError string    `json:"-"`
	CreatedAt, DeliveryUpdatedAt                        time.Time `json:"-"`
}

func (v conversationMessageView) MarshalJSON() ([]byte, error) {
	type wire struct {
		ID                   string    `json:"id"`
		ProjectID            string    `json:"project_id"`
		ThreadID             string    `json:"thread_id"`
		ThreadSeq            int64     `json:"thread_seq"`
		Role                 string    `json:"role"`
		ActorID              string    `json:"actor_id"`
		AgentIncarnationID   string    `json:"agent_incarnation_id,omitempty"`
		ReplyToMessageID     string    `json:"reply_to_message_id,omitempty"`
		ContentText          string    `json:"content_text,omitempty"`
		ContentArtifactRef   string    `json:"content_artifact_ref,omitempty"`
		ContentSHA256        string    `json:"content_sha256"`
		StreamState          string    `json:"stream_state"`
		IdempotencyKey       string    `json:"idempotency_key"`
		DeliveryState        string    `json:"delivery_state"`
		DeliveryStateVersion int       `json:"delivery_state_version"`
		DeliveryActionID     string    `json:"delivery_action_id,omitempty"`
		DeliveryReceiptRef   string    `json:"delivery_receipt_ref,omitempty"`
		DeliveryError        string    `json:"delivery_error,omitempty"`
		CreatedAt            time.Time `json:"created_at"`
		DeliveryUpdatedAt    time.Time `json:"delivery_updated_at"`
	}
	return json.Marshal(wire{
		ID: v.ID, ProjectID: v.ProjectID, ThreadID: v.ThreadID, ThreadSeq: v.ThreadSeq,
		Role: v.Role, ActorID: v.ActorID, AgentIncarnationID: v.AgentIncarnationID,
		ReplyToMessageID: v.ReplyToMessageID, ContentText: v.ContentText,
		ContentArtifactRef: v.ContentArtifactRef, ContentSHA256: v.ContentSHA256,
		StreamState: v.StreamState, IdempotencyKey: v.IdempotencyKey,
		DeliveryState: v.DeliveryState, DeliveryStateVersion: v.DeliveryStateVersion,
		DeliveryActionID: v.DeliveryActionID, DeliveryReceiptRef: v.DeliveryReceiptRef,
		DeliveryError: v.DeliveryError, CreatedAt: v.CreatedAt, DeliveryUpdatedAt: v.DeliveryUpdatedAt,
	})
}

func viewConversationMessage(row store.ConversationMessage) conversationMessageView {
	return conversationMessageView{
		ID: row.ID, ProjectID: row.ProjectID, ThreadID: row.ThreadID, ThreadSeq: row.ThreadSeq,
		Role: row.Role, ActorID: row.ActorID, AgentIncarnationID: row.AgentIncarnationID,
		ReplyToMessageID: row.ReplyToMessageID, ContentText: row.ContentText,
		ContentArtifactRef: row.ContentArtifactRef, ContentSHA256: row.ContentSHA256,
		StreamState: row.StreamState, IdempotencyKey: row.IdempotencyKey,
		DeliveryState: row.DeliveryState, DeliveryStateVersion: row.DeliveryStateVersion,
		DeliveryActionID: row.DeliveryActionID, DeliveryReceiptRef: row.DeliveryReceiptRef,
		DeliveryError: row.DeliveryError, CreatedAt: row.CreatedAt, DeliveryUpdatedAt: row.DeliveryUpdatedAt,
	}
}

func (s *Server) conversationThreadsList(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanConversationRead); !ok {
		return
	}
	rows, err := s.store.ListConversationThreads(r.Context(), projectID)
	if err != nil {
		http.Error(w, "conversation list error", http.StatusInternalServerError)
		return
	}
	views := make([]conversationThreadView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewConversationThread(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": conversationSchemaVersion, "conversations": views})
}

func (s *Server) conversationOne(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanConversationRead); !ok {
		return
	}
	row, err := s.store.GetConversationThread(r.Context(), projectID, r.PathValue("thread_id"))
	if err != nil {
		s.writeConversationError(w, err)
		return
	}
	digest, err := s.store.ConversationDigestSeq(r.Context(), projectID, row.ID)
	if err != nil {
		http.Error(w, "conversation digest error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": conversationSchemaVersion,
		"digest_seq": digest, "conversation": viewConversationThread(row)})
}

type createConversationBody struct {
	ID                      string                      `json:"id"`
	ConversationKey         string                      `json:"conversation_key"`
	Title                   string                      `json:"title"`
	InteractorActorID       string                      `json:"interactor_actor_id"`
	InteractorBindingID     string                      `json:"interactor_binding_id"`
	InteractorIncarnationID string                      `json:"interactor_incarnation_id"`
	FocusKind               store.ConversationFocusKind `json:"focus_kind"`
	FocusRef                string                      `json:"focus_ref"`
	FocusArtifactSHA256     string                      `json:"focus_artifact_sha256"`
}

func (s *Server) conversationCreate(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("project_id")
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanConversationManage); !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	var body createConversationBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid conversation", http.StatusBadRequest)
		return
	}
	// Phase 2 projects never accept a browser-supplied session route. Resolve the
	// one logical Interactor and its current exact Driver incarnation from durable
	// project ownership. The default project retains the Phase-1 compatibility
	// body during rollout.
	if projectID != "default" {
		route, err := s.store.GetProjectActor(r.Context(), projectID, store.DriverInteractorRole)
		if err != nil || route.State != "active" {
			http.Error(w, "project has no active Interactor route", http.StatusConflict)
			return
		}
		binding, err := s.store.ActiveDriverSessionBinding(r.Context(), projectID, route.ActorID, store.DriverInteractorRole)
		if err != nil {
			http.Error(w, "project Interactor has no active Driver binding", http.StatusConflict)
			return
		}
		body.InteractorActorID = route.ActorID
		body.InteractorBindingID = binding.BindingID
		body.InteractorIncarnationID = binding.AgentRunID
	}
	row, err := s.store.CreateConversationThread(r.Context(), store.CreateConversationThreadInput{
		ID: body.ID, ProjectID: projectID, ConversationKey: body.ConversationKey, Title: body.Title,
		InteractorActorID: body.InteractorActorID, InteractorBindingID: body.InteractorBindingID,
		InteractorIncarnationID: body.InteractorIncarnationID, FocusKind: body.FocusKind,
		FocusRef: body.FocusRef, FocusArtifactSHA256: body.FocusArtifactSHA256, IdempotencyKey: key,
	}, s.clock.Now())
	if err != nil {
		s.writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schema_version": conversationSchemaVersion,
		"conversation": viewConversationThread(row)})
}

type conversationFocusBody struct {
	ProjectID            string                      `json:"project_id"`
	ExpectedStateVersion int                         `json:"expected_state_version"`
	FocusKind            store.ConversationFocusKind `json:"focus_kind"`
	FocusRef             string                      `json:"focus_ref"`
	FocusArtifactSHA256  string                      `json:"focus_artifact_sha256"`
}

func (s *Server) conversationFocus(w http.ResponseWriter, r *http.Request) {
	var body conversationFocusBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid conversation focus", http.StatusBadRequest)
		return
	}
	principal, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanConversationManage)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	row, err := s.store.UpdateConversationFocus(r.Context(), store.UpdateConversationFocusInput{
		ProjectID: body.ProjectID, ThreadID: r.PathValue("thread_id"), IdempotencyKey: key,
		ExpectedStateVersion: body.ExpectedStateVersion, FocusKind: body.FocusKind,
		FocusRef: body.FocusRef, FocusArtifactSHA256: body.FocusArtifactSHA256,
	}, principal.Identity, s.clock.Now())
	if err != nil {
		s.writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": conversationSchemaVersion,
		"conversation": viewConversationThread(row)})
}

func (s *Server) conversationMessagesList(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanConversationRead); !ok {
		return
	}
	after, err := boundedNonNegativeQuery(r, "after", 0)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limit, err := boundedNonNegativeQuery(r, "limit", 100)
	if err != nil || limit == 0 {
		http.Error(w, "limit must be a positive integer", http.StatusBadRequest)
		return
	}
	threadID := r.PathValue("thread_id")
	if _, err := s.store.GetConversationThread(r.Context(), projectID, threadID); err != nil {
		s.writeConversationError(w, err)
		return
	}
	rows, err := s.store.ListConversationMessages(r.Context(), projectID, threadID, int64(after), limit)
	if err != nil {
		http.Error(w, "conversation messages error", http.StatusInternalServerError)
		return
	}
	views := make([]conversationMessageView, 0, len(rows))
	next := int64(after)
	for _, row := range rows {
		views = append(views, viewConversationMessage(row))
		next = row.ThreadSeq
	}
	digest, err := s.store.ConversationDigestSeq(r.Context(), projectID, threadID)
	if err != nil {
		http.Error(w, "conversation digest error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": conversationSchemaVersion,
		"digest_seq": digest, "next_after": next, "messages": views})
}

type appendConversationMessageBody struct {
	ProjectID          string `json:"project_id"`
	Role               string `json:"role"`
	AgentIncarnationID string `json:"agent_incarnation_id"`
	ReplyToMessageID   string `json:"reply_to_message_id"`
	ContentText        string `json:"content_text"`
	ContentArtifactRef string `json:"content_artifact_ref"`
	ContentSHA256      string `json:"content_sha256"`
	StreamState        string `json:"stream_state"`
}

func (s *Server) conversationMessageAppend(w http.ResponseWriter, r *http.Request) {
	var body appendConversationMessageBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid conversation message", http.StatusBadRequest)
		return
	}
	principal, ok := s.requireHumanProject(w, r, body.ProjectID, auth.HumanConversationSend)
	if !ok {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	if body.Role == "" {
		body.Role = "human"
	}
	row, err := s.store.AppendConversationMessage(r.Context(), store.AppendConversationMessageInput{
		ID: "message-" + s.minter.New(), ProjectID: body.ProjectID, ThreadID: r.PathValue("thread_id"),
		Role: body.Role, ActorID: principal.Identity, AgentIncarnationID: body.AgentIncarnationID,
		ReplyToMessageID: body.ReplyToMessageID, ContentText: body.ContentText,
		ContentArtifactRef: body.ContentArtifactRef, ContentSHA256: body.ContentSHA256,
		StreamState: body.StreamState, IdempotencyKey: key,
	}, s.clock.Now())
	if err != nil {
		s.writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schema_version": conversationSchemaVersion,
		"message": viewConversationMessage(row)})
}

type conversationDeliveryBody struct {
	ProjectID            string `json:"project_id"`
	ExpectedStateVersion int    `json:"expected_state_version"`
	State                string `json:"state"`
	ActionID             string `json:"action_id"`
	ReceiptRef           string `json:"receipt_ref"`
	LastError            string `json:"last_error"`
}

// conversationMessageDelivery is the replaceable transport-projector seam. It
// records Driver-derived delivery state only; it never sends to tmux or treats a
// receipt as an Interactor/workflow acknowledgement.
func (s *Server) conversationMessageDelivery(w http.ResponseWriter, r *http.Request) {
	var body conversationDeliveryBody
	if err := decodeBoundedJSON(r, &body); err != nil {
		http.Error(w, "invalid conversation delivery", http.StatusBadRequest)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		http.Error(w, "Idempotency-Key is required", http.StatusBadRequest)
		return
	}
	row, err := s.store.UpdateConversationMessageDelivery(r.Context(), store.UpdateConversationDeliveryInput{
		ProjectID: body.ProjectID, ThreadID: r.PathValue("thread_id"), MessageID: r.PathValue("message_id"),
		IdempotencyKey: key, ExpectedStateVersion: body.ExpectedStateVersion, State: body.State,
		ActionID: body.ActionID, ReceiptRef: body.ReceiptRef, LastError: body.LastError,
	}, decisionActor(r), s.clock.Now())
	if err != nil {
		s.writeConversationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema_version": conversationSchemaVersion,
		"message": viewConversationMessage(row)})
}

// conversationEvents streams the persisted event ledger. Last-Event-ID and
// ?after= are durable database cursors, so server restart/reconnect replays every
// committed event in order; SSE remains a wake/read channel, not workflow truth.
func (s *Server) conversationEvents(w http.ResponseWriter, r *http.Request) {
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if _, ok := s.requireHumanProject(w, r, projectID, auth.HumanConversationRead); !ok {
		return
	}
	threadID := r.PathValue("thread_id")
	if _, err := s.store.GetConversationThread(r.Context(), projectID, threadID); err != nil {
		s.writeConversationError(w, err)
		return
	}
	afterText := strings.TrimSpace(r.URL.Query().Get("after"))
	if afterText == "" {
		afterText = strings.TrimSpace(r.Header.Get("Last-Event-ID"))
	}
	var cursor int64
	if afterText != "" {
		value, err := strconv.ParseInt(afterText, 10, 64)
		if err != nil || value < 0 {
			http.Error(w, "conversation cursor must be a non-negative integer", http.StatusBadRequest)
			return
		}
		cursor = value
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	poll := time.NewTicker(250 * time.Millisecond)
	keepalive := time.NewTicker(20 * time.Second)
	defer poll.Stop()
	defer keepalive.Stop()
	for {
		events, err := s.store.ListConversationEvents(r.Context(), projectID, threadID, cursor, 100)
		if err != nil {
			return
		}
		for _, event := range events {
			payload := map[string]any{"schema_version": conversationSchemaVersion, "seq": event.Seq,
				"project_id": event.ProjectID, "thread_id": event.ThreadID, "message_id": event.MessageID,
				"kind": event.Kind, "payload": json.RawMessage(event.PayloadJSON), "created_at": event.CreatedAt}
			blob, _ := json.Marshal(payload)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, event.Kind, blob); err != nil {
				return
			}
			cursor = event.Seq
			flusher.Flush()
		}
		if len(events) > 0 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
		}
	}
}

func boundedNonNegativeQuery(r *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func (s *Server) writeConversationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrConversationNotFound), errors.Is(err, store.ErrConversationMessageNotFound):
		http.Error(w, "conversation not found", http.StatusNotFound)
	case errors.Is(err, store.ErrConversationIdempotencyConflict), errors.Is(err, store.ErrConversationStale):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		if strings.Contains(err.Error(), "conversation") || strings.Contains(err.Error(), "message") ||
			strings.Contains(err.Error(), "interactor") || strings.Contains(err.Error(), "reply") ||
			strings.Contains(err.Error(), "transition") || strings.Contains(err.Error(), "artifact") {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, "conversation error", http.StatusInternalServerError)
	}
}
