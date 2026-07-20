package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/samhotchkiss/flowbee/internal/auth"
)

// Broker is a tiny in-process pub/sub for the read-only /v1/events SSE feed. The
// runtime publishes a lifecycle event after each successful state mutation; each
// connected client gets a buffered channel (slow clients drop, never block the
// control plane).
type Broker struct {
	mu      sync.Mutex
	nextID  int
	clients map[int]chan []byte
}

func NewBroker() *Broker {
	return &Broker{clients: make(map[int]chan []byte)}
}

// LifeEvent is one SSE payload describing a job lifecycle transition.
type LifeEvent struct {
	ProjectID string `json:"project_id,omitempty"`
	JobID     string `json:"job_id"`
	State     string `json:"state"`
	Event     string `json:"event"`
	Epoch     int    `json:"lease_epoch"`
	// Global marks a projectless nudge whose payload contains no project-owned
	// data. Exact-project subscribers may receive these shared health/wake hints;
	// an unmarked projectless payload remains portfolio-only and fails closed.
	Global bool `json:"global,omitempty"`
	// DigestSeq rides epic-lane ("epics"-topic) nudges so a constrained consumer (the
	// elgato deck, §15.16b) can dedupe against the digest it last polled WITHOUT a re-poll.
	// Zero (omitted) for non-epic lifecycle events. SSE stays a lossy nudge; poll is truth.
	DigestSeq int64 `json:"digest_seq,omitempty"`
}

func (b *Broker) subscribe() (int, chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan []byte, 64)
	b.clients[id] = ch
	return id, ch
}

func (b *Broker) unsubscribe(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.clients[id]; ok {
		close(ch)
		delete(b.clients, id)
	}
}

// PublishAlarm surfaces a fired scheduler alarm (e.g. no_eligible_worker) live on
// the SSE feed. Satisfies alarm.Publisher so the poller can surface I-6 alarms.
func (b *Broker) PublishAlarm(jobID, kind string) {
	b.Publish(LifeEvent{JobID: jobID, State: "ready", Event: kind})
}

// PublishReconcile surfaces a reconcile-IN outcome (facts_reconciled / superseded
// / reconciled_done / terminal_frozen) live on the SSE feed. Satisfies
// reconcile.Publisher so the sweep/refetch can surface Domain-B drift corrections.
func (b *Broker) PublishReconcile(jobID, event string) {
	b.Publish(LifeEvent{JobID: jobID, Event: event})
}

// PublishLiveness surfaces a liveness outcome (stall_revoked / fast_path_cancel)
// live on the SSE feed (§10). Satisfies alarm.LivenessPublisher so the poller can
// surface a two-rung kill / absolute-cap revoke.
func (b *Broker) PublishLiveness(jobID, event string) {
	b.Publish(LifeEvent{JobID: jobID, Event: event})
}

// Publish broadcasts a lifecycle event to all subscribers (non-blocking).
func (b *Broker) Publish(e LifeEvent) {
	blob, err := json.Marshal(e)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.clients {
		select {
		case ch <- blob:
		default: // slow client: drop rather than block the control plane
		}
	}
}

// eventsHandler streams lifecycle events as text/event-stream.
func (s *Server) eventsHandler(w http.ResponseWriter, r *http.Request) {
	scope, portfolio, ok := s.authorizeLifecycleSSE(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	id, ch := s.broker.subscribe()
	defer s.broker.unsubscribe(id)

	// flush headers so clients (and tests) know the stream is open.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// SSE hygiene (plan §15.16b): a keepalive COMMENT every 20s so a half-open socket on a
	// slept laptop is detected instead of going silently stale. Comment lines (":"-prefixed)
	// are ignored by every SSE client, so this never disturbs a data consumer.
	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case msg, ok := <-ch:
			if !ok {
				return
			}
			msg, ok = s.lifecycleEventForScope(r, msg, scope, portfolio)
			if !ok {
				continue
			}
			// named event: per topic (plan §15.16b) so a client can subscribe by topic. The
			// topic is the event's State bucket ("epics" for epic-lane nudges), defaulting to
			// "lifecycle". A data-only consumer (which reads only "data:" lines) is unaffected.
			topic := topicOf(msg)
			_, _ = w.Write([]byte("event: " + topic + "\n"))
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(msg)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}

// authorizeLifecycleSSE distinguishes an exact project subscription from the
// portfolio stream. Omission is never inferred from a caller's project grants:
// it requires the separately configured `*` portfolio authority.
func (s *Server) authorizeLifecycleSSE(w http.ResponseWriter, r *http.Request) (projectID string, portfolio, ok bool) {
	values, supplied := r.URL.Query()["project_id"]
	if !supplied {
		_, ok = s.requireHumanPortfolio(w, r, auth.HumanProjectRead)
		return "", true, ok
	}
	if len(values) != 1 {
		http.Error(w, "exactly one project_id is required", http.StatusBadRequest)
		return "", false, false
	}
	projectID = strings.TrimSpace(values[0])
	if projectID == "" || projectID == "*" {
		http.Error(w, "an exact project_id is required", http.StatusBadRequest)
		return "", false, false
	}
	_, ok = s.requireHumanProject(w, r, projectID, auth.HumanProjectRead)
	return projectID, false, ok
}

// lifecycleEventForScope is the final no-leak fence. ProjectID on an event is
// an assertion, not authority: job-bearing events are resolved through durable
// job ownership and a mismatched assertion is dropped. Projectless events reach
// an exact-project stream only when explicitly marked Global and carrying no
// project-owned identifier. Portfolio subscribers have explicit cross-project
// authority and receive every nudge unchanged.
func (s *Server) lifecycleEventForScope(r *http.Request, blob []byte, projectID string, portfolio bool) ([]byte, bool) {
	if portfolio {
		return blob, true
	}
	var event LifeEvent
	if json.Unmarshal(blob, &event) != nil {
		return nil, false
	}
	if strings.TrimSpace(event.JobID) != "" {
		owned, err := s.store.GetJob(r.Context(), event.JobID)
		if err != nil || strings.TrimSpace(owned.ProjectID) != projectID ||
			(event.ProjectID != "" && event.ProjectID != owned.ProjectID) {
			return nil, false
		}
		// Older lifecycle publishers carry only job_id. Enrich the emitted nudge
		// with mechanically resolved ownership so clients never have to infer it.
		event.ProjectID = owned.ProjectID
		out, err := json.Marshal(event)
		return out, err == nil
	}
	if event.ProjectID != "" {
		return blob, event.ProjectID == projectID
	}
	if globalLifecycleNudgeSafe(event) {
		return blob, true
	}
	return nil, false
}

// globalLifecycleNudgeSafe is deliberately an allowlist, not merely an empty
// project_id check: Event and State are strings and must not become side
// channels for project, account, session, or terminal identifiers. Global SSE
// carries only generic wake reasons; authorized reads return the actual truth.
func globalLifecycleNudgeSafe(event LifeEvent) bool {
	if !event.Global || event.ProjectID != "" || event.JobID != "" || event.Epoch != 0 {
		return false
	}
	switch event.State {
	case "capacity":
		return event.Event == "account_at_ceiling" && event.DigestSeq == 0
	case "epics":
		return event.Event == "epic_supervision_pass" || event.Event == "capacity_fold"
	default:
		return false
	}
}

// topicOf extracts a small, versioned set of SSE topics. Project events carry
// project_id in their payload so a portfolio client never has to infer scope
// from a job, slug, or terminal identity. Best-effort — an unparseable blob
// defaults to lifecycle (SSE is a lossy nudge; poll is truth).
func topicOf(blob []byte) string {
	var e struct {
		State string `json:"state"`
	}
	if json.Unmarshal(blob, &e) == nil {
		switch e.State {
		case "epics", "projects":
			return e.State
		}
	}
	return "lifecycle"
}
