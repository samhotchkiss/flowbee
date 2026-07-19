package deadman

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/alertingress"
)

type HTTPProbe struct {
	URL    string
	Client *http.Client
}

type healthDriverControl struct {
	Required  *bool  `json:"required"`
	Available *bool  `json:"available"`
	Status    string `json:"status"`
	Gap       string `json:"gap"`
}

func (p HTTPProbe) Probe(ctx context.Context) (Observation, error) {
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return Observation{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Observation{}, ctx.Err()
		}
		return Observation{Reason: "process_unreachable", Detail: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if readErr != nil {
		return Observation{Reason: "invalid_health_response", Detail: readErr.Error(), HTTPStatus: resp.StatusCode}, nil
	}
	var wire struct {
		Status          string               `json:"status"`
		DB              *bool                `json:"db"`
		Overdue         *int                 `json:"reconciler_overdue"`
		OverdueNames    []string             `json:"reconciler_overdue_names"`
		ReconcilerError string               `json:"reconciler_health_error"`
		DriverControl   *healthDriverControl `json:"driver_control"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return Observation{Reason: "invalid_health_response",
			Detail: fmt.Sprintf("HTTP %d returned invalid JSON: %v", resp.StatusCode, err), HTTPStatus: resp.StatusCode}, nil
	}
	overdue := 0
	if wire.Overdue != nil {
		overdue = *wire.Overdue
	}
	obs := Observation{HTTPStatus: resp.StatusCode, ReconcilerOverdue: overdue,
		ReconcilerOverdueIDs: append([]string(nil), wire.OverdueNames...)}
	if overdue > 0 {
		obs.Reason = "reconciler_overdue"
		obs.Detail = fmt.Sprintf("%d reconciler heartbeat(s) overdue", overdue)
		if len(wire.OverdueNames) > 0 {
			obs.Detail += ": " + strings.Join(wire.OverdueNames, ", ")
		}
		return obs, nil
	}
	if overdue < 0 || (overdue == 0 && len(wire.OverdueNames) != 0) {
		obs.Reason = "invalid_health_response"
		obs.Detail = "health endpoint returned an inconsistent reconciler summary"
		return obs, nil
	}
	if controlOnlyDegraded(resp.StatusCode, wire.Status, wire.DB, wire.Overdue,
		wire.ReconcilerError, wire.DriverControl) {
		// GAP-FD-003 is a fail-closed product-route hold, not evidence that the
		// Flowbee process or its healing loops are dead. Treating this one exact,
		// structured shape as alive ensures that a later unreachable process or
		// overdue reconciler starts a fresh dead-man episode instead of being
		// hidden behind a permanent startup degradation.
		obs.Healthy = true
		obs.Detail = "control plane alive; Driver control route held by GAP-FD-003"
		return obs, nil
	}
	if wire.DB == nil || wire.Overdue == nil {
		obs.Reason = "invalid_health_response"
		obs.Detail = "health endpoint omitted db or reconciler_overdue"
		return obs, nil
	}
	if !*wire.DB {
		obs.Reason = "control_plane_unhealthy"
		obs.Detail = "health endpoint reports database unavailable"
		return obs, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || wire.Status != "ok" {
		obs.Reason = "control_plane_unhealthy"
		obs.Detail = fmt.Sprintf("health endpoint returned HTTP %d status=%q", resp.StatusCode, wire.Status)
		if wire.ReconcilerError != "" {
			obs.Detail += ": " + wire.ReconcilerError
		}
		return obs, nil
	}
	obs.Healthy = true
	return obs, nil
}

func controlOnlyDegraded(httpStatus int, status string, db *bool, overdue *int, reconcilerError string, driver *healthDriverControl) bool {
	return httpStatus == http.StatusServiceUnavailable &&
		status == "degraded" &&
		db != nil && *db &&
		overdue != nil && *overdue == 0 &&
		reconcilerError == "" &&
		driver != nil &&
		driver.Required != nil && *driver.Required &&
		driver.Available != nil && !*driver.Available &&
		driver.Status == "route_unavailable" &&
		driver.Gap == "GAP-FD-003"
}

type WebhookPublisher struct {
	URL, Secret, ProjectID string
	Client                 *http.Client
}

type controlAlertEnvelope struct {
	FormatVersion string `json:"format_version"`
	ID            string `json:"id"`
	DedupKey      string `json:"dedup_key"`
	ProjectID     string `json:"project_id"`
	EpicID        string `json:"epic_id,omitempty"`
	Kind          string `json:"kind"`
	Payload       any    `json:"payload"`
}

func (p WebhookPublisher) Publish(ctx context.Context, notification Notification) error {
	if strings.TrimSpace(p.URL) == "" || p.Secret == "" {
		return fmt.Errorf("alert webhook URL and secret are required")
	}
	if err := ValidateProjectID(p.ProjectID); err != nil {
		return err
	}
	if notification.ProjectID != p.ProjectID {
		return fmt.Errorf("dead-man notification belongs to project %q, not publisher project %q",
			notification.ProjectID, p.ProjectID)
	}
	// Use Flowbee's signed ingress contract. The receiver durably projects this
	// exact-project obligation to the Interactor; it is not a human webhook sink.
	body, err := json.Marshal(controlAlertEnvelope{
		FormatVersion: "flowbee.control-alert/v1", ID: notification.ID,
		DedupKey: notification.DedupKey, ProjectID: p.ProjectID,
		Kind: "external_deadman", Payload: notification,
	})
	if err != nil {
		return err
	}
	return p.publishEnvelope(ctx, body, notification.DedupKey)
}

func (p WebhookPublisher) PublishHeartbeat(ctx context.Context, heartbeat Heartbeat) error {
	if strings.TrimSpace(p.URL) == "" || p.Secret == "" {
		return fmt.Errorf("alert webhook URL and secret are required")
	}
	if err := ValidateProjectID(p.ProjectID); err != nil {
		return err
	}
	if heartbeat.ProjectID != p.ProjectID || heartbeat.FormatVersion != alertingress.ExternalWatchdogHeartbeatFormatVersion ||
		heartbeat.WatchdogID == "" || heartbeat.Target == "" || heartbeat.Sequence < 1 || heartbeat.ObservedAt.IsZero() {
		return fmt.Errorf("watchdog heartbeat is incomplete or belongs to the wrong project")
	}
	key := fmt.Sprintf("deadman-heartbeat:%s:%s:%d", heartbeat.ProjectID, heartbeat.WatchdogID, heartbeat.Sequence)
	body, err := json.Marshal(controlAlertEnvelope{
		FormatVersion: alertingress.FormatVersion, ID: key, DedupKey: key,
		ProjectID: p.ProjectID, Kind: alertingress.ExternalWatchdogHeartbeatKind, Payload: heartbeat,
	})
	if err != nil {
		return err
	}
	return p.publishEnvelope(ctx, body, key)
}

func (p WebhookPublisher) publishEnvelope(ctx context.Context, body []byte, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(p.Secret))
	_, _ = mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	client := p.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	noRedirectClient := *client
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("alert webhook status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		return fmt.Errorf("control-alert ingress 2xx did not return application/json acknowledgement")
	}
	ackBody, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil {
		return fmt.Errorf("read control-alert ingress acknowledgement: %w", err)
	}
	hash := sha256.Sum256(body)
	return alertingress.ValidateAcknowledgement(ackBody, resp.Header.Get("X-Flowbee-Signature"),
		p.Secret, key, hex.EncodeToString(hash[:]))
}
