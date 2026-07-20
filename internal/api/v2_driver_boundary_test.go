package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
	"github.com/samhotchkiss/flowbee/internal/ulid"
)

type countingPaneDeliverer struct{ calls int }

func (d *countingPaneDeliverer) Deliver(context.Context, string, string, string) (string, string, error) {
	d.calls++
	return "strong", "unexpected legacy send", nil
}

func TestV2MasterReplyNeverCallsLegacyPaneDeliverer(t *testing.T) {
	st := testutil.NewStore(t)
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{DisableLegacyPaneActuation: true}, "test")
	deliverer := &countingPaneDeliverer{}
	srv.SetPaneDeliverer(deliverer)

	for _, action := range []string{"reply", "amend"} {
		t.Run(action, func(t *testing.T) {
			body, _ := json.Marshal(resolveRequest{
				Action: action, Payload: "ship it", IdempotencyKey: action + "-1",
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/masters/attention/att-1/resolve", bytes.NewReader(body))
			rec := httptest.NewRecorder()
			srv.PrivateHandler().ServeHTTP(rec, req)

			if rec.Code != http.StatusGone {
				t.Fatalf("status = %d body=%q, want 410 v2 boundary fence", rec.Code, rec.Body.String())
			}
		})
	}
	if deliverer.calls != 0 {
		t.Fatalf("legacy pane deliverer called %d times under v2, want zero", deliverer.calls)
	}
}

func TestV2PaneTailDoesNotCaptureRawTmux(t *testing.T) {
	st := testutil.NewStore(t)
	srv := New(st, clock.Real{}, ulid.NewMinter(nil), Config{DisableLegacyPaneActuation: true}, "test")

	// No such tmux session exists. A legacy capture would return an error; the v2
	// path returns no tail because observations must come from Driver's archive.
	tail, err := srv.paneTail(context.Background(), store.EpicRun{TmuxName: "must-not-be-captured"})
	if err != nil || tail != "" {
		t.Fatalf("v2 paneTail = (%q, %v), want empty/no-error without raw capture", tail, err)
	}
}
