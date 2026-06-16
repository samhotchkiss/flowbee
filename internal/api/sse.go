package api

import (
	"encoding/json"
	"net/http"
	"sync"
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

	for {
		select {
		case <-r.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(msg)
			_, _ = w.Write([]byte("\n\n"))
			flusher.Flush()
		}
	}
}
