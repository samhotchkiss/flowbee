package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/ulid"
)

const (
	maxConversationKey     = 120
	maxConversationTitle   = 240
	maxConversationContent = 100_000
	maxConversationPage    = 200
)

var (
	ErrConversationNotFound            = errors.New("conversation not found")
	ErrConversationMessageNotFound     = errors.New("conversation message not found")
	ErrConversationIdempotencyConflict = errors.New("conversation idempotency key reused with different content")
	ErrConversationStale               = errors.New("conversation state version is stale")
)

type ConversationFocusKind string

const (
	ConversationFocusProject  ConversationFocusKind = "project"
	ConversationFocusEpic     ConversationFocusKind = "epic"
	ConversationFocusArtifact ConversationFocusKind = "artifact"
	ConversationFocusDecision ConversationFocusKind = "decision"
)

type ConversationThread struct {
	ID, ProjectID, ConversationKey, Title                           string
	InteractorActorID, InteractorBindingID, InteractorIncarnationID string
	State                                                           string
	StateVersion                                                    int
	FocusKind                                                       ConversationFocusKind
	FocusRef, FocusArtifactSHA256                                   string
	LastMessageSeq                                                  int64
	CreatedAt, UpdatedAt                                            time.Time
}

type CreateConversationThreadInput struct {
	ID, ProjectID, ConversationKey, Title                           string
	InteractorActorID, InteractorBindingID, InteractorIncarnationID string
	FocusKind                                                       ConversationFocusKind
	FocusRef, FocusArtifactSHA256                                   string
	IdempotencyKey                                                  string
}

type ConversationMessage struct {
	ID, ProjectID, ThreadID                             string
	ThreadSeq                                           int64
	Role, ActorID, AgentIncarnationID, ReplyToMessageID string
	ContentText, ContentArtifactRef, ContentSHA256      string
	StreamState, IdempotencyKey                         string
	DeliveryState                                       string
	DeliveryStateVersion                                int
	DeliveryActionID, DeliveryReceiptRef, DeliveryError string
	CreatedAt, DeliveryUpdatedAt                        time.Time
}

type AppendConversationMessageInput struct {
	ID, ProjectID, ThreadID                             string
	Role, ActorID, AgentIncarnationID, ReplyToMessageID string
	ContentText, ContentArtifactRef, ContentSHA256      string
	StreamState, IdempotencyKey                         string
}

type ConversationEvent struct {
	Seq                            int64
	ProjectID, ThreadID, MessageID string
	Kind, PayloadJSON              string
	CreatedAt                      time.Time
}

type UpdateConversationFocusInput struct {
	ProjectID, ThreadID, IdempotencyKey string
	ExpectedStateVersion                int
	FocusKind                           ConversationFocusKind
	FocusRef, FocusArtifactSHA256       string
}

type UpdateConversationDeliveryInput struct {
	ProjectID, ThreadID, MessageID, IdempotencyKey string
	ExpectedStateVersion                           int
	State, ActionID, ReceiptRef, LastError         string
}

func (s *Store) CreateConversationThread(ctx context.Context, in CreateConversationThreadInput, now time.Time) (ConversationThread, error) {
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.ConversationKey = strings.TrimSpace(in.ConversationKey)
	in.Title = strings.TrimSpace(in.Title)
	in.InteractorActorID = strings.TrimSpace(in.InteractorActorID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ID == "" {
		in.ID = "conversation-" + ulid.New()
	}
	if in.ProjectID == "" || in.ConversationKey == "" || in.InteractorActorID == "" ||
		in.InteractorBindingID == "" || in.InteractorIncarnationID == "" || in.IdempotencyKey == "" {
		return ConversationThread{}, errors.New("conversation project, key, exact interactor route, and idempotency key are required")
	}
	if len(in.ConversationKey) > maxConversationKey || len(in.Title) > maxConversationTitle {
		return ConversationThread{}, errors.New("conversation key or title exceeds its bound")
	}
	if in.FocusKind == "" {
		in.FocusKind = ConversationFocusProject
	}
	if in.FocusKind == ConversationFocusProject && in.FocusRef == "" {
		in.FocusRef = in.ProjectID
	}
	nowText := now.UTC().Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_threads
			WHERE project_id=? AND (conversation_key=? OR creation_idempotency_key=?)
			ORDER BY CASE WHEN conversation_key=? THEN 0 ELSE 1 END LIMIT 1`,
			in.ProjectID, in.ConversationKey, in.IdempotencyKey, in.ConversationKey).Scan(&existingID)
		if err == nil {
			existing, getErr := getConversationThreadTx(ctx, tx, in.ProjectID, existingID)
			if getErr != nil {
				return getErr
			}
			if conversationThreadMatchesCreate(existing, in) {
				in.ID = existing.ID
				return nil
			}
			return ErrConversationIdempotencyConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err := validateConversationFocusTx(ctx, tx, in.ProjectID, in.FocusKind, in.FocusRef, in.FocusArtifactSHA256); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_threads
			(id,project_id,conversation_key,title,interactor_actor_id,interactor_binding_id,
			 interactor_incarnation_id,state,state_version,focus_kind,focus_ref,
			 focus_artifact_sha256,last_message_seq,creation_idempotency_key,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,'active',1,?,?,?,0,?,?,?)`, in.ID, in.ProjectID,
			in.ConversationKey, in.Title, in.InteractorActorID, in.InteractorBindingID,
			in.InteractorIncarnationID, in.FocusKind, in.FocusRef,
			strings.ToLower(in.FocusArtifactSHA256), in.IdempotencyKey, nowText, nowText)
		if err != nil {
			return err
		}
		payload := mustConversationJSON(map[string]any{"thread_id": in.ID, "conversation_key": in.ConversationKey,
			"focus_kind": in.FocusKind, "focus_ref": in.FocusRef})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,kind,payload_json,created_at) VALUES (?,?, 'thread_created',?,?)`,
			in.ProjectID, in.ID, payload, nowText); err != nil {
			return err
		}
		return appendConversationControlEventTx(ctx, tx, in.ProjectID, "conversation_thread_created", "", "active", 1,
			in.InteractorActorID, payload, now)
	})
	if err != nil {
		return ConversationThread{}, err
	}
	return s.GetConversationThread(ctx, in.ProjectID, in.ID)
}

func conversationThreadMatchesCreate(got ConversationThread, in CreateConversationThreadInput) bool {
	return got.ProjectID == in.ProjectID && got.ConversationKey == in.ConversationKey &&
		got.Title == in.Title && got.InteractorActorID == in.InteractorActorID &&
		got.InteractorBindingID == in.InteractorBindingID &&
		got.InteractorIncarnationID == in.InteractorIncarnationID && got.FocusKind == in.FocusKind &&
		got.FocusRef == in.FocusRef && got.FocusArtifactSHA256 == strings.ToLower(in.FocusArtifactSHA256)
}

func (s *Store) GetConversationThread(ctx context.Context, projectID, id string) (ConversationThread, error) {
	row := s.DB.QueryRowContext(ctx, conversationThreadSelect+` WHERE project_id=? AND id=?`, projectID, id)
	return scanConversationThread(row)
}

func (s *Store) ListConversationThreads(ctx context.Context, projectID string) ([]ConversationThread, error) {
	rows, err := s.DB.QueryContext(ctx, conversationThreadSelect+`
		WHERE project_id=? ORDER BY CASE state WHEN 'active' THEN 0 ELSE 1 END,updated_at DESC,id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ConversationThread, 0)
	for rows.Next() {
		item, err := scanConversationThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateConversationFocus(ctx context.Context, in UpdateConversationFocusInput, actorID string, now time.Time) (ConversationThread, error) {
	if in.ProjectID == "" || in.ThreadID == "" || in.IdempotencyKey == "" || in.ExpectedStateVersion < 1 {
		return ConversationThread{}, errors.New("conversation focus project, thread, expected version, and idempotency key are required")
	}
	requestHash := conversationCommandHash(struct {
		Version int                   `json:"expected_state_version"`
		Kind    ConversationFocusKind `json:"focus_kind"`
		Ref     string                `json:"focus_ref"`
		SHA     string                `json:"focus_artifact_sha256"`
	}{in.ExpectedStateVersion, in.FocusKind, in.FocusRef, strings.ToLower(in.FocusArtifactSHA256)})
	err := s.tx(ctx, func(tx *sql.Tx) error {
		replayed, err := conversationCommandReplayTx(ctx, tx, in.ProjectID, in.ThreadID, in.IdempotencyKey, "focus", requestHash)
		if err != nil || replayed {
			return err
		}
		thread, err := getConversationThreadTx(ctx, tx, in.ProjectID, in.ThreadID)
		if err != nil {
			return err
		}
		if thread.StateVersion != in.ExpectedStateVersion || thread.State != "active" {
			return ErrConversationStale
		}
		if err := validateConversationFocusTx(ctx, tx, in.ProjectID, in.FocusKind, in.FocusRef, in.FocusArtifactSHA256); err != nil {
			return err
		}
		nextVersion := thread.StateVersion + 1
		nowText := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE conversation_threads SET focus_kind=?,focus_ref=?,
			focus_artifact_sha256=?,state_version=?,updated_at=? WHERE project_id=? AND id=? AND state_version=? AND state='active'`,
			in.FocusKind, in.FocusRef, strings.ToLower(in.FocusArtifactSHA256), nextVersion, nowText,
			in.ProjectID, in.ThreadID, in.ExpectedStateVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConversationStale
		}
		payload := mustConversationJSON(map[string]any{"thread_id": in.ThreadID, "focus_kind": in.FocusKind,
			"focus_ref": in.FocusRef, "focus_artifact_sha256": strings.ToLower(in.FocusArtifactSHA256),
			"state_version": nextVersion})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,kind,payload_json,created_at) VALUES (?,?,'focus_changed',?,?)`,
			in.ProjectID, in.ThreadID, payload, nowText); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_commands
			(project_id,thread_id,idempotency_key,kind,request_sha256,result_ref,result_version,created_at)
			VALUES (?,?,?,?,?,?,?,?)`, in.ProjectID, in.ThreadID, in.IdempotencyKey, "focus", requestHash,
			in.ThreadID, nextVersion, nowText); err != nil {
			return err
		}
		return appendConversationControlEventTx(ctx, tx, in.ProjectID, "conversation_focus_changed",
			string(thread.FocusKind), string(in.FocusKind), nextVersion, actorID, payload, now)
	})
	if err != nil {
		return ConversationThread{}, err
	}
	return s.GetConversationThread(ctx, in.ProjectID, in.ThreadID)
}

func (s *Store) AppendConversationMessage(ctx context.Context, in AppendConversationMessageInput, now time.Time) (ConversationMessage, error) {
	in.ProjectID = strings.TrimSpace(in.ProjectID)
	in.ThreadID = strings.TrimSpace(in.ThreadID)
	in.Role = strings.TrimSpace(in.Role)
	in.ActorID = strings.TrimSpace(in.ActorID)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	if in.ID == "" {
		in.ID = "message-" + ulid.New()
	}
	if in.ProjectID == "" || in.ThreadID == "" || in.ActorID == "" || in.IdempotencyKey == "" {
		return ConversationMessage{}, errors.New("conversation message project, thread, actor, and idempotency key are required")
	}
	if in.Role != "human" && in.Role != "interactor" && in.Role != "system" {
		return ConversationMessage{}, errors.New("conversation message role is invalid")
	}
	if in.Role == "interactor" && in.AgentIncarnationID == "" {
		return ConversationMessage{}, errors.New("interactor messages require an exact agent incarnation")
	}
	if in.ContentText == "" && in.ContentArtifactRef == "" {
		return ConversationMessage{}, errors.New("conversation message content or artifact is required")
	}
	if len(in.ContentText) > maxConversationContent {
		return ConversationMessage{}, errors.New("conversation message content exceeds its bound; use an immutable artifact")
	}
	if in.StreamState == "" {
		in.StreamState = "complete"
	}
	if in.StreamState != "complete" && in.StreamState != "streaming" {
		return ConversationMessage{}, errors.New("conversation message stream state is invalid")
	}
	if in.ContentText != "" {
		digest := sha256.Sum256([]byte(in.ContentText))
		computed := "sha256:" + hex.EncodeToString(digest[:])
		if in.ContentSHA256 != "" && !strings.EqualFold(in.ContentSHA256, computed) {
			return ConversationMessage{}, errors.New("conversation message content hash does not match exact UTF-8 content")
		}
		in.ContentSHA256 = computed
	} else if !validSHA256(in.ContentSHA256) {
		return ConversationMessage{}, errors.New("artifact-only conversation message requires a sha256")
	}
	in.ContentSHA256 = strings.ToLower(in.ContentSHA256)
	nowText := now.UTC().Format(rfc3339)
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM conversation_messages
			WHERE project_id=? AND thread_id=? AND idempotency_key=?`, in.ProjectID, in.ThreadID, in.IdempotencyKey).Scan(&existingID)
		if err == nil {
			existing, getErr := getConversationMessageTx(ctx, tx, in.ProjectID, in.ThreadID, existingID)
			if getErr != nil {
				return getErr
			}
			if conversationMessageMatchesAppend(existing, in) {
				in.ID = existing.ID
				return nil
			}
			return ErrConversationIdempotencyConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		thread, err := getConversationThreadTx(ctx, tx, in.ProjectID, in.ThreadID)
		if err != nil {
			return err
		}
		if thread.State != "active" {
			return ErrConversationStale
		}
		if in.Role == "interactor" && (thread.InteractorActorID != in.ActorID ||
			thread.InteractorIncarnationID != "" && thread.InteractorIncarnationID != in.AgentIncarnationID) {
			return errors.New("interactor message does not match the thread actor incarnation")
		}
		if in.ReplyToMessageID != "" {
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_messages
				WHERE id=? AND project_id=? AND thread_id=?)`, in.ReplyToMessageID, in.ProjectID, in.ThreadID).Scan(&exists); err != nil {
				return err
			}
			if exists != 1 {
				return errors.New("reply target is not in this conversation")
			}
		}
		seq := thread.LastMessageSeq + 1
		_, err = tx.ExecContext(ctx, `INSERT INTO conversation_messages
			(id,project_id,thread_id,thread_seq,role,actor_id,agent_incarnation_id,reply_to_message_id,
			 content_text,content_artifact_ref,content_sha256,stream_state,idempotency_key,created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.ID, in.ProjectID, in.ThreadID, seq, in.Role,
			in.ActorID, in.AgentIncarnationID, nullableText(in.ReplyToMessageID), in.ContentText,
			in.ContentArtifactRef, in.ContentSHA256, in.StreamState, in.IdempotencyKey, nowText)
		if err != nil {
			return err
		}
		deliveryState := "not_required"
		if in.Role == "human" {
			deliveryState = "pending"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_message_deliveries
			(message_id,project_id,thread_id,state,state_version,updated_at) VALUES (?,?,?,?,1,?)`,
			in.ID, in.ProjectID, in.ThreadID, deliveryState, nowText); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE conversation_threads SET last_message_seq=?,updated_at=?
			WHERE project_id=? AND id=? AND last_message_seq=?`, seq, nowText, in.ProjectID, in.ThreadID, thread.LastMessageSeq)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConversationStale
		}
		payload := mustConversationJSON(map[string]any{"thread_id": in.ThreadID, "message_id": in.ID,
			"thread_seq": seq, "role": in.Role, "content_sha256": in.ContentSHA256,
			"stream_state": in.StreamState, "delivery_state": deliveryState})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,message_id,kind,payload_json,created_at)
			VALUES (?,?,?,'message_appended',?,?)`, in.ProjectID, in.ThreadID, in.ID, payload, nowText); err != nil {
			return err
		}
		return appendConversationControlEventTx(ctx, tx, in.ProjectID, "conversation_message_appended", "", deliveryState,
			int(seq), in.ActorID, payload, now)
	})
	if err != nil {
		return ConversationMessage{}, err
	}
	return s.GetConversationMessage(ctx, in.ProjectID, in.ThreadID, in.ID)
}

func conversationMessageMatchesAppend(got ConversationMessage, in AppendConversationMessageInput) bool {
	return got.ProjectID == in.ProjectID && got.ThreadID == in.ThreadID && got.Role == in.Role &&
		got.ActorID == in.ActorID && got.AgentIncarnationID == in.AgentIncarnationID &&
		got.ReplyToMessageID == in.ReplyToMessageID && got.ContentText == in.ContentText &&
		got.ContentArtifactRef == in.ContentArtifactRef && got.ContentSHA256 == in.ContentSHA256 &&
		got.StreamState == in.StreamState
}

func (s *Store) GetConversationMessage(ctx context.Context, projectID, threadID, id string) (ConversationMessage, error) {
	return scanConversationMessage(s.DB.QueryRowContext(ctx, conversationMessageSelect+`
		WHERE m.project_id=? AND m.thread_id=? AND m.id=?`, projectID, threadID, id))
}

func (s *Store) ListConversationMessages(ctx context.Context, projectID, threadID string, afterSeq int64, limit int) ([]ConversationMessage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxConversationPage {
		limit = maxConversationPage
	}
	rows, err := s.DB.QueryContext(ctx, conversationMessageSelect+`
		WHERE m.project_id=? AND m.thread_id=? AND m.thread_seq>? ORDER BY m.thread_seq LIMIT ?`,
		projectID, threadID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ConversationMessage, 0)
	for rows.Next() {
		item, err := scanConversationMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) UpdateConversationMessageDelivery(ctx context.Context, in UpdateConversationDeliveryInput, actorID string, now time.Time) (ConversationMessage, error) {
	if in.ProjectID == "" || in.ThreadID == "" || in.MessageID == "" || in.IdempotencyKey == "" || in.ExpectedStateVersion < 1 {
		return ConversationMessage{}, errors.New("conversation delivery identity, expected version, and idempotency key are required")
	}
	requestHash := conversationCommandHash(struct {
		Version int    `json:"expected_state_version"`
		State   string `json:"state"`
		Action  string `json:"action_id"`
		Receipt string `json:"receipt_ref"`
		Error   string `json:"last_error"`
	}{in.ExpectedStateVersion, in.State, in.ActionID, in.ReceiptRef, in.LastError})
	err := s.tx(ctx, func(tx *sql.Tx) error {
		replayed, err := conversationCommandReplayTx(ctx, tx, in.ProjectID, in.ThreadID, in.IdempotencyKey, "delivery", requestHash)
		if err != nil || replayed {
			return err
		}
		message, err := getConversationMessageTx(ctx, tx, in.ProjectID, in.ThreadID, in.MessageID)
		if err != nil {
			return err
		}
		if message.DeliveryStateVersion != in.ExpectedStateVersion {
			return ErrConversationStale
		}
		if !validConversationDeliveryTransition(message.DeliveryState, in.State) {
			return fmt.Errorf("invalid conversation delivery transition %s -> %s", message.DeliveryState, in.State)
		}
		nextVersion := message.DeliveryStateVersion + 1
		nowText := now.UTC().Format(rfc3339)
		res, err := tx.ExecContext(ctx, `UPDATE conversation_message_deliveries SET state=?,state_version=?,
			action_id=?,receipt_ref=?,last_error=?,updated_at=? WHERE message_id=? AND project_id=?
			AND thread_id=? AND state_version=?`, in.State, nextVersion, in.ActionID, in.ReceiptRef,
			in.LastError, nowText, in.MessageID, in.ProjectID, in.ThreadID, in.ExpectedStateVersion)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrConversationStale
		}
		payload := mustConversationJSON(map[string]any{"thread_id": in.ThreadID, "message_id": in.MessageID,
			"from": message.DeliveryState, "to": in.State, "state_version": nextVersion,
			"action_id": in.ActionID, "receipt_ref": in.ReceiptRef, "last_error": in.LastError})
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_events
			(project_id,thread_id,message_id,kind,payload_json,created_at)
			VALUES (?,?,?,'delivery_changed',?,?)`, in.ProjectID, in.ThreadID, in.MessageID, payload, nowText); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversation_commands
			(project_id,thread_id,idempotency_key,kind,request_sha256,result_ref,result_version,created_at)
			VALUES (?,?,?,?,?,?,?,?)`, in.ProjectID, in.ThreadID, in.IdempotencyKey, "delivery", requestHash,
			in.MessageID, nextVersion, nowText); err != nil {
			return err
		}
		return appendConversationControlEventTx(ctx, tx, in.ProjectID, "conversation_delivery_changed",
			message.DeliveryState, in.State, nextVersion, actorID, payload, now)
	})
	if err != nil {
		return ConversationMessage{}, err
	}
	return s.GetConversationMessage(ctx, in.ProjectID, in.ThreadID, in.MessageID)
}

func validConversationDeliveryTransition(from, to string) bool {
	allowed := map[string]map[string]bool{
		"pending":   {"routing": true, "failed": true, "fenced": true},
		"routing":   {"submitted": true, "uncertain": true, "failed": true, "fenced": true},
		"submitted": {"acknowledged": true, "uncertain": true, "failed": true, "fenced": true},
		"uncertain": {"submitted": true, "acknowledged": true, "failed": true, "fenced": true},
		"failed":    {"routing": true, "fenced": true},
	}
	return allowed[from][to]
}

func (s *Store) ListConversationEvents(ctx context.Context, projectID, threadID string, after int64, limit int) ([]ConversationEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxConversationPage {
		limit = maxConversationPage
	}
	rows, err := s.DB.QueryContext(ctx, `SELECT seq,project_id,thread_id,message_id,kind,payload_json,created_at
		FROM conversation_events WHERE project_id=? AND thread_id=? AND seq>? ORDER BY seq LIMIT ?`,
		projectID, threadID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ConversationEvent, 0)
	for rows.Next() {
		var item ConversationEvent
		var created string
		if err := rows.Scan(&item.Seq, &item.ProjectID, &item.ThreadID, &item.MessageID, &item.Kind, &item.PayloadJSON, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = parseOptionalTime(created)
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *Store) ConversationDigestSeq(ctx context.Context, projectID, threadID string) (int64, error) {
	var seq int64
	err := s.DB.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM conversation_events
		WHERE project_id=? AND thread_id=?`, projectID, threadID).Scan(&seq)
	return seq, err
}

func validateConversationFocusTx(ctx context.Context, tx *sql.Tx, projectID string, kind ConversationFocusKind, ref, artifactSHA string) error {
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id=?)`, projectID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return fmt.Errorf("project %q does not exist", projectID)
	}
	switch kind {
	case ConversationFocusProject:
		if ref != projectID {
			return errors.New("project conversation focus must reference its own project")
		}
	case ConversationFocusEpic:
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM epics WHERE id=? AND project_id=?)`, ref, projectID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return errors.New("conversation epic focus does not belong to project")
		}
	case ConversationFocusDecision:
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM decision_requests WHERE id=? AND project_id=?)`, ref, projectID).Scan(&exists); err != nil {
			return err
		}
		if exists != 1 {
			return errors.New("conversation decision focus does not belong to project")
		}
	case ConversationFocusArtifact:
		if ref == "" || !validSHA256(artifactSHA) {
			return errors.New("conversation artifact focus requires an immutable reference and sha256")
		}
	default:
		return errors.New("conversation focus kind is invalid")
	}
	return nil
}

func conversationCommandReplayTx(ctx context.Context, tx *sql.Tx, projectID, threadID, key, kind, hash string) (bool, error) {
	var oldKind, oldHash string
	err := tx.QueryRowContext(ctx, `SELECT kind,request_sha256 FROM conversation_commands
		WHERE project_id=? AND thread_id=? AND idempotency_key=?`, projectID, threadID, key).Scan(&oldKind, &oldHash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if oldKind != kind || oldHash != hash {
		return false, ErrConversationIdempotencyConflict
	}
	return true, nil
}

func conversationCommandHash(value any) string {
	blob, _ := json.Marshal(value)
	sum := sha256.Sum256(blob)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustConversationJSON(value any) string {
	blob, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func appendConversationControlEventTx(ctx context.Context, tx *sql.Tx, projectID, kind, from, to string, version int, actorID, payload string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO control_events
		(project_id,kind,from_state,to_state,state_version,actor_kind,actor_id,payload_json,created_at)
		VALUES (?,?,?,?,?,'conversation',?,?,?)`, projectID, kind, from, to, version, actorID,
		payload, now.UTC().Format(rfc3339))
	return err
}

const conversationThreadSelect = `SELECT id,project_id,conversation_key,title,interactor_actor_id,
	interactor_binding_id,interactor_incarnation_id,state,state_version,focus_kind,focus_ref,
	focus_artifact_sha256,last_message_seq,created_at,updated_at FROM conversation_threads`

func getConversationThreadTx(ctx context.Context, tx *sql.Tx, projectID, id string) (ConversationThread, error) {
	return scanConversationThread(tx.QueryRowContext(ctx, conversationThreadSelect+` WHERE project_id=? AND id=?`, projectID, id))
}

func scanConversationThread(row decisionRowScanner) (ConversationThread, error) {
	var out ConversationThread
	var created, updated string
	err := row.Scan(&out.ID, &out.ProjectID, &out.ConversationKey, &out.Title, &out.InteractorActorID,
		&out.InteractorBindingID, &out.InteractorIncarnationID, &out.State, &out.StateVersion,
		&out.FocusKind, &out.FocusRef, &out.FocusArtifactSHA256, &out.LastMessageSeq, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationThread{}, ErrConversationNotFound
	}
	if err != nil {
		return ConversationThread{}, err
	}
	out.CreatedAt, out.UpdatedAt = parseOptionalTime(created), parseOptionalTime(updated)
	return out, nil
}

const conversationMessageSelect = `SELECT m.id,m.project_id,m.thread_id,m.thread_seq,m.role,m.actor_id,
	m.agent_incarnation_id,COALESCE(m.reply_to_message_id,''),m.content_text,m.content_artifact_ref,
	m.content_sha256,m.stream_state,m.idempotency_key,d.state,d.state_version,d.action_id,d.receipt_ref,
	d.last_error,m.created_at,d.updated_at FROM conversation_messages m
	JOIN conversation_message_deliveries d ON d.message_id=m.id`

func getConversationMessageTx(ctx context.Context, tx *sql.Tx, projectID, threadID, id string) (ConversationMessage, error) {
	return scanConversationMessage(tx.QueryRowContext(ctx, conversationMessageSelect+`
		WHERE m.project_id=? AND m.thread_id=? AND m.id=?`, projectID, threadID, id))
}

func scanConversationMessage(row decisionRowScanner) (ConversationMessage, error) {
	var out ConversationMessage
	var created, deliveryUpdated string
	err := row.Scan(&out.ID, &out.ProjectID, &out.ThreadID, &out.ThreadSeq, &out.Role, &out.ActorID,
		&out.AgentIncarnationID, &out.ReplyToMessageID, &out.ContentText, &out.ContentArtifactRef,
		&out.ContentSHA256, &out.StreamState, &out.IdempotencyKey, &out.DeliveryState,
		&out.DeliveryStateVersion, &out.DeliveryActionID, &out.DeliveryReceiptRef, &out.DeliveryError,
		&created, &deliveryUpdated)
	if errors.Is(err, sql.ErrNoRows) {
		return ConversationMessage{}, ErrConversationMessageNotFound
	}
	if err != nil {
		return ConversationMessage{}, err
	}
	out.CreatedAt, out.DeliveryUpdatedAt = parseOptionalTime(created), parseOptionalTime(deliveryUpdated)
	return out, nil
}
