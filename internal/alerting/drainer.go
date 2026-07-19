package alerting

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

type AlertStore interface {
	ReclaimExpiredControlAlerts(context.Context, time.Time) (int64, error)
	ClaimNextControlAlert(context.Context, string, time.Time, time.Duration) (store.ControlAlert, bool, error)
	AcknowledgeControlAlert(context.Context, string, string, int, time.Time) error
	RetryControlAlert(context.Context, string, string, int, string, time.Time, time.Time) error
	DeadLetterControlAlert(context.Context, string, string, int, string, time.Time) error
}

type Sink interface {
	Publish(context.Context, store.ControlAlert) error
}

type WebhookSink struct {
	URL, Secret string
	Client      *http.Client
}

func (s WebhookSink) Publish(ctx context.Context, alert store.ControlAlert) error {
	if s.URL == "" || s.Secret == "" {
		return errors.New("alert webhook URL and secret are required")
	}
	body, err := json.Marshal(map[string]any{
		"format_version": "flowbee.control-alert/v1",
		"id":             alert.ID,
		"dedup_key":      alert.DedupKey,
		"project_id":     alert.ProjectID,
		"epic_id":        alert.EpicID,
		"kind":           alert.Kind,
		"payload":        json.RawMessage(alert.Payload),
		"attempt":        alert.Attempts,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	mac := hmac.New(sha256.New, []byte(s.Secret))
	_, _ = mac.Write(body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", alert.DedupKey)
	req.Header.Set("X-Flowbee-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("alert webhook status %d: %s", resp.StatusCode, string(msg))
	}
	return nil
}

type Drainer struct {
	Store        AlertStore
	Sink         Sink
	Owner        string
	ClaimTTL     time.Duration
	MaximumTries int
	Batch        int
}

type Report struct{ Reclaimed, Published, Retried, DeadLettered int }

func (d Drainer) Tick(ctx context.Context, now time.Time) (Report, error) {
	var out Report
	if d.Store == nil || d.Sink == nil || d.Owner == "" {
		return out, errors.New("alert drainer requires store, sink, and owner")
	}
	if d.ClaimTTL <= 0 {
		d.ClaimTTL = time.Minute
	}
	if d.MaximumTries <= 0 {
		d.MaximumTries = 5
	}
	if d.Batch <= 0 {
		d.Batch = 50
	}
	reclaimed, err := d.Store.ReclaimExpiredControlAlerts(ctx, now)
	if err != nil {
		return out, err
	}
	out.Reclaimed = int(reclaimed)
	for i := 0; i < d.Batch; i++ {
		a, ok, err := d.Store.ClaimNextControlAlert(ctx, d.Owner, now, d.ClaimTTL)
		if err != nil {
			return out, err
		}
		if !ok {
			break
		}
		if err := d.Sink.Publish(ctx, a); err == nil {
			if err := d.Store.AcknowledgeControlAlert(ctx, a.ID, d.Owner, a.Epoch, now); err != nil {
				return out, err
			}
			out.Published++
			continue
		} else if a.Attempts >= d.MaximumTries {
			if derr := d.Store.DeadLetterControlAlert(ctx, a.ID, d.Owner, a.Epoch, err.Error(), now); derr != nil {
				return out, derr
			}
			out.DeadLettered++
			continue
		} else {
			backoff := time.Minute << min(a.Attempts-1, 3)
			if rerr := d.Store.RetryControlAlert(ctx, a.ID, d.Owner, a.Epoch, err.Error(), now.Add(backoff), now); rerr != nil {
				return out, rerr
			}
			out.Retried++
		}
	}
	return out, nil
}
