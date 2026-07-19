// Package deadman implements the external Flowbee control-plane watchdog.
//
// It deliberately has no database or tmux dependency. The watchdog observes the
// public health listener, persists its own incident/outbox state, and publishes
// authenticated notifications even when the Flowbee process cannot start.
package deadman

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const StateVersion = 2

var stableProjectIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ValidateProjectID applies the same stable identifier shape used by Flowbee's
// project portfolio. The watchdog must be explicitly scoped; it never infers a
// project from its URL, host, state path, or receiver.
func ValidateProjectID(projectID string) error {
	if !stableProjectIDPattern.MatchString(projectID) {
		return errors.New("watchdog project id must match ^[a-z0-9][a-z0-9-]{0,62}$")
	}
	return nil
}

type Observation struct {
	Healthy              bool     `json:"healthy"`
	Reason               string   `json:"reason,omitempty"`
	Detail               string   `json:"detail,omitempty"`
	HTTPStatus           int      `json:"http_status,omitempty"`
	ReconcilerOverdue    int      `json:"reconciler_overdue,omitempty"`
	ReconcilerOverdueIDs []string `json:"reconciler_overdue_names,omitempty"`
}

type Probe interface {
	Probe(context.Context) (Observation, error)
}

type Publisher interface {
	Publish(context.Context, Notification) error
}

type HeartbeatPublisher interface {
	PublishHeartbeat(context.Context, Heartbeat) error
}

type StateStore interface {
	Load() (State, error)
	Save(State) error
}

type Incident struct {
	ID                   string    `json:"id"`
	Sequence             int64     `json:"sequence"`
	StartedAt            time.Time `json:"started_at"`
	Reason               string    `json:"reason"`
	Detail               string    `json:"detail,omitempty"`
	HTTPStatus           int       `json:"http_status,omitempty"`
	ReconcilerOverdue    int       `json:"reconciler_overdue,omitempty"`
	ReconcilerOverdueIDs []string  `json:"reconciler_overdue_names,omitempty"`
	LastObservedAt       time.Time `json:"last_observed_at"`
}

// Notification is immutable once queued. Delivery-attempt fields are stored in
// State.PendingNotification instead, so retrying an Idempotency-Key never changes
// the request body.
type Notification struct {
	FormatVersion string    `json:"format_version"`
	ID            string    `json:"id"`
	DedupKey      string    `json:"dedup_key"`
	ProjectID     string    `json:"project_id"`
	WatchdogID    string    `json:"watchdog_id"`
	Target        string    `json:"target"`
	Status        string    `json:"status"` // firing or resolved
	Incident      Incident  `json:"incident"`
	ObservedAt    time.Time `json:"observed_at"`
	ResolvedAt    time.Time `json:"resolved_at,omitempty"`
}

type Heartbeat struct {
	FormatVersion string    `json:"format_version"`
	ProjectID     string    `json:"project_id"`
	WatchdogID    string    `json:"watchdog_id"`
	Target        string    `json:"target"`
	Sequence      int64     `json:"sequence"`
	ObservedAt    time.Time `json:"observed_at"`
}

type PendingNotification struct {
	Notification Notification `json:"notification"`
	Attempts     int          `json:"attempts"`
	LastAttempt  time.Time    `json:"last_attempt_at,omitempty"`
	LastError    string       `json:"last_error,omitempty"`
}

type Resolution struct {
	IncidentID string    `json:"incident_id"`
	Sequence   int64     `json:"sequence"`
	ResolvedAt time.Time `json:"resolved_at"`
}

type State struct {
	Version               int                   `json:"version"`
	ProjectID             string                `json:"project_id"`
	WatchdogID            string                `json:"watchdog_id"`
	Target                string                `json:"target"`
	NextIncident          int64                 `json:"next_incident_sequence"`
	Active                *Incident             `json:"active_incident,omitempty"`
	Pending               []PendingNotification `json:"pending_notifications,omitempty"`
	LastResolution        *Resolution           `json:"last_resolution,omitempty"`
	NextHeartbeatSequence int64                 `json:"next_heartbeat_sequence,omitempty"`
	PendingHeartbeat      *Heartbeat            `json:"pending_heartbeat,omitempty"`
	LastCheckAt           time.Time             `json:"last_check_at,omitempty"`
	LastHealthyAt         time.Time             `json:"last_healthy_at,omitempty"`
	LastObservation       Observation           `json:"last_observation"`
}

type Runner struct {
	ProjectID          string
	WatchdogID         string
	Target             string
	Probe              Probe
	Publisher          Publisher
	HeartbeatPublisher HeartbeatPublisher
	Store              StateStore
	Now                func() time.Time
}

type Report struct {
	Observation       Observation
	IncidentStarted   bool
	IncidentResolved  bool
	NotificationsSent int
	NotificationsLeft int
	IncidentID        string
}

// RunOnce performs one probe and drains its durable notification queue. An
// unhealthy target is an observed condition, not an execution error. State is
// committed before any webhook call, making a crash retry the same immutable
// notification under the same idempotency key.
func (r Runner) RunOnce(ctx context.Context) (Report, error) {
	var out Report
	if err := ValidateProjectID(r.ProjectID); err != nil {
		return out, err
	}
	if strings.TrimSpace(r.WatchdogID) == "" || strings.TrimSpace(r.Target) == "" {
		return out, errors.New("watchdog project id, watchdog id, and target are required")
	}
	if r.Probe == nil || r.Publisher == nil || r.Store == nil {
		return out, errors.New("watchdog requires probe, publisher, and state store")
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	state, err := r.Store.Load()
	if err != nil {
		return out, fmt.Errorf("load watchdog state: %w", err)
	}
	if err := bindState(&state, r.ProjectID, r.WatchdogID, r.Target); err != nil {
		return out, err
	}
	var heartbeatErr error
	if r.HeartbeatPublisher != nil {
		if state.PendingHeartbeat == nil {
			state.NextHeartbeatSequence++
			state.PendingHeartbeat = &Heartbeat{FormatVersion: "flowbee.deadman-heartbeat/v1",
				ProjectID: r.ProjectID, WatchdogID: r.WatchdogID, Target: r.Target,
				Sequence: state.NextHeartbeatSequence, ObservedAt: now}
			if err := r.Store.Save(state); err != nil {
				return out, fmt.Errorf("persist watchdog heartbeat: %w", err)
			}
		}
		if err := r.HeartbeatPublisher.PublishHeartbeat(ctx, *state.PendingHeartbeat); err != nil {
			heartbeatErr = fmt.Errorf("publish watchdog heartbeat: %w", err)
		} else {
			state.PendingHeartbeat = nil
			if err := r.Store.Save(state); err != nil {
				return out, fmt.Errorf("persist watchdog heartbeat acknowledgement: %w", err)
			}
		}
	}
	obs, err := r.Probe.Probe(ctx)
	if err != nil {
		return out, fmt.Errorf("probe health endpoint: %w", err)
	}
	if !obs.Healthy && obs.Reason == "" {
		obs.Reason = "control_plane_unhealthy"
	}
	out.Observation = obs
	state.LastCheckAt = now
	state.LastObservation = obs
	if obs.Healthy {
		state.LastHealthyAt = now
		if state.Active != nil {
			incident := *state.Active
			incident.LastObservedAt = now
			state.Pending = append(state.Pending, PendingNotification{Notification: notificationFor(
				r.ProjectID, r.WatchdogID, r.Target, "resolved", incident, now,
			)})
			out.IncidentResolved = true
			out.IncidentID = incident.ID
			state.LastResolution = &Resolution{IncidentID: incident.ID, Sequence: incident.Sequence, ResolvedAt: now}
			state.Active = nil
		}
	} else if state.Active == nil {
		state.NextIncident++
		incident := Incident{
			ID: incidentID(r.ProjectID, r.WatchdogID, r.Target, state.NextIncident), Sequence: state.NextIncident,
			StartedAt: now, Reason: obs.Reason, Detail: obs.Detail, HTTPStatus: obs.HTTPStatus,
			ReconcilerOverdue:    obs.ReconcilerOverdue,
			ReconcilerOverdueIDs: append([]string(nil), obs.ReconcilerOverdueIDs...), LastObservedAt: now,
		}
		state.Active = &incident
		state.Pending = append(state.Pending, PendingNotification{Notification: notificationFor(
			r.ProjectID, r.WatchdogID, r.Target, "firing", incident, now,
		)})
		out.IncidentStarted = true
		out.IncidentID = incident.ID
	} else {
		// Retain the first cause as incident identity, while keeping the local
		// read model current for an operator inspecting the state file.
		state.Active.LastObservedAt = now
		state.Active.Detail = obs.Detail
		state.Active.HTTPStatus = obs.HTTPStatus
		state.Active.ReconcilerOverdue = obs.ReconcilerOverdue
		state.Active.ReconcilerOverdueIDs = append(state.Active.ReconcilerOverdueIDs[:0], obs.ReconcilerOverdueIDs...)
		out.IncidentID = state.Active.ID
	}
	if err := r.Store.Save(state); err != nil {
		return out, fmt.Errorf("persist watchdog observation: %w", err)
	}

	for len(state.Pending) > 0 {
		pending := state.Pending[0]
		err := r.Publisher.Publish(ctx, pending.Notification)
		pending.Attempts++
		pending.LastAttempt = now
		if err != nil {
			pending.LastError = err.Error()
			state.Pending[0] = pending
			if saveErr := r.Store.Save(state); saveErr != nil {
				return out, fmt.Errorf("publish notification: %v (persist retry: %w)", err, saveErr)
			}
			out.NotificationsLeft = len(state.Pending)
			return out, fmt.Errorf("publish notification %s: %w", pending.Notification.ID, err)
		}
		state.Pending = state.Pending[1:]
		if err := r.Store.Save(state); err != nil {
			// The receiver has the same Idempotency-Key. If this save failed, a
			// restart safely retries the identical body rather than inventing an
			// acknowledgement that may not have happened.
			return out, fmt.Errorf("persist notification acknowledgement: %w", err)
		}
		out.NotificationsSent++
	}
	out.NotificationsLeft = len(state.Pending)
	if heartbeatErr != nil {
		return out, heartbeatErr
	}
	return out, nil
}

func bindState(state *State, projectID, watchdogID, target string) error {
	if state.Version == 0 {
		if state.ProjectID != "" || state.WatchdogID != "" || state.Target != "" || state.NextIncident != 0 ||
			state.Active != nil || len(state.Pending) != 0 || state.LastResolution != nil ||
			state.NextHeartbeatSequence != 0 || state.PendingHeartbeat != nil || !state.LastCheckAt.IsZero() {
			return errors.New("unversioned watchdog state contains durable data and cannot be assigned to a project")
		}
		state.Version = StateVersion
		state.ProjectID = projectID
		state.WatchdogID = watchdogID
		state.Target = target
		return nil
	}
	if state.Version != StateVersion {
		return fmt.Errorf("unsupported watchdog state version %d", state.Version)
	}
	if state.ProjectID != projectID || state.WatchdogID != watchdogID || state.Target != target {
		return fmt.Errorf("watchdog state belongs to project=%q id=%q target=%q, not project=%q id=%q target=%q",
			state.ProjectID, state.WatchdogID, state.Target, projectID, watchdogID, target)
	}
	return nil
}

func incidentID(projectID, watchdogID, target string, sequence int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", projectID, watchdogID, target, sequence)))
	return "deadman-" + hex.EncodeToString(sum[:12])
}

func notificationFor(projectID, watchdogID, target, status string, incident Incident, now time.Time) Notification {
	id := incident.ID + ":" + status
	n := Notification{FormatVersion: "flowbee.deadman-alert/v1", ID: id, DedupKey: id,
		ProjectID: projectID, WatchdogID: watchdogID, Target: target, Status: status, Incident: incident, ObservedAt: now}
	if status == "resolved" {
		n.ResolvedAt = now
	}
	return n
}
