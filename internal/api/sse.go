package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
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
	JobID string `json:"job_id"`
	State string `json:"state"`
	Event string `json:"event"`
	Epoch int    `json:"lease_epoch"`
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

// topicOf extracts the SSE event topic from a published LifeEvent blob: the "epics" bucket
// for epic-lane supervision nudges, else "lifecycle". Best-effort — an unparseable blob
// defaults to lifecycle (SSE is a lossy nudge; poll is truth).
func topicOf(blob []byte) string {
	var e struct {
		State string `json:"state"`
	}
	if json.Unmarshal(blob, &e) == nil && e.State == "epics" {
		return "epics"
	}
	return "lifecycle"
}
