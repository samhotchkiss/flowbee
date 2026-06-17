// Package webhook is the PUBLIC, internet-reachable webhook listener (DESIGN
// §8.1.3, I-2). It treats every inbound GitHub webhook as a HINT, never authority:
//
//	[1] HMAC-verify X-Hub-Signature-256   (reject unsigned/forged: I-2)
//	[2] dedupe on X-GitHub-Delivery        (durable inbox; replay-safe: I-2)
//	[3] write-ahead to the inbox BEFORE acting (crash-replay: I-2)
//	[4] enqueue a TARGETED refetch of the affected PR through reconcile-IN
//	[5] advance the delivery high-water-mark (gap detection, §8.1.4)
//
// A forged or replayed webhook can therefore at WORST trigger a refetch that reads
// the real (un-approved/un-merged) state — it can never fast-track a state (§8.1.3).
// The verdict was never GitHub's to give (I-9); the webhook is a doorbell.
package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Refetcher triggers a targeted single-PR refetch (satisfied by *reconcile.Reconciler).
// The bool reports whether the PR was bound to a job (and thus reconciled).
type Refetcher interface {
	RefetchHint(ctx context.Context, prNumber int) bool
	// IntakeSweep runs a reconcile sweep so a freshly labeled/opened issue is adopted
	// NOW (event-driven) instead of waiting for the floor poll. Returns whether it ran.
	IntakeSweep(ctx context.Context) bool
}

// Inbox is the durable write-ahead inbox + dedupe (satisfied by *store.Store).
type Inbox interface {
	RecordDelivery(ctx context.Context, deliveryID, event string, prNumber int) (fresh bool, err error)
	MarkDeliveryProcessed(ctx context.Context, deliveryID string) error
}

// Handler is the public webhook HTTP handler.
type Handler struct {
	secret    []byte
	inbox     Inbox
	refetcher Refetcher
}

// New builds the webhook handler. secret is the GitHub App webhook secret used to
// HMAC-verify X-Hub-Signature-256 (I-2). An empty secret REJECTS every request
// (fail closed — the endpoint is internet-reachable).
func New(secret string, inbox Inbox, refetcher Refetcher) *Handler {
	return &Handler{secret: []byte(secret), inbox: inbox, refetcher: refetcher}
}

// ServeHTTP implements the §8.1.3 pipeline. Outcomes:
//   - bad signature / no secret -> 401 (forged/unsigned rejected, I-2)
//   - duplicate delivery id      -> 200 {"deduped":true} (replay-safe, no action)
//   - fresh delivery             -> 200 {"refetched":...} after a TARGETED refetch
//
// Crucially, it NEVER applies the webhook body as state. The only state effect is
// the refetch, which reads the real PR from GitHub under the I-3 guards.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}

	// [1] HMAC-verify (I-2). Fail closed on an empty secret or a bad signature.
	sig := r.Header.Get("X-Hub-Signature-256")
	if len(h.secret) == 0 || !verifySignature(h.secret, body, sig) {
		http.Error(w, "signature verification failed", http.StatusUnauthorized)
		return
	}

	delivery := r.Header.Get("X-GitHub-Delivery")
	event := r.Header.Get("X-GitHub-Event")
	if delivery == "" {
		http.Error(w, "missing X-GitHub-Delivery", http.StatusBadRequest)
		return
	}
	prNumber := parsePRNumber(event, body)

	// [2][3][5] dedupe + write-ahead inbox + advance high-water-mark, atomically.
	fresh, err := h.inbox.RecordDelivery(r.Context(), delivery, event, prNumber)
	if err != nil {
		http.Error(w, "inbox error", http.StatusInternalServerError)
		return
	}
	if !fresh {
		// a replayed (or forged-with-a-seen-id) delivery: dropped, no action.
		writeJSON(w, map[string]any{"deduped": true})
		return
	}

	// [4] TARGETED refetch through reconcile-IN (never a direct state change). The
	// refetch reads the REAL PR state under the I-3 guards. A webhook with no PR
	// target (e.g. a ping) just records and acks.
	reconciled := false
	if prNumber > 0 && h.refetcher != nil {
		reconciled = h.refetcher.RefetchHint(r.Context(), prNumber)
	}
	// an `issues` event whose action could introduce a new intake target (labeled with
	// the opt-in label, or (re)opened) triggers a sweep so the issue is adopted NOW —
	// event-driven intake, not just the floor poll. The sweep is idempotent: it adopts
	// only labeled, not-yet-tracked issues, so a stray edit/close is a harmless no-op.
	if event == "issues" && h.refetcher != nil {
		switch parseIssueAction(body) {
		case "labeled", "opened", "reopened":
			reconciled = h.refetcher.IntakeSweep(r.Context()) || reconciled
		}
	}
	if err := h.inbox.MarkDeliveryProcessed(r.Context(), delivery); err != nil {
		http.Error(w, "inbox error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"refetched": reconciled, "pr": prNumber})
}

// verifySignature checks GitHub's X-Hub-Signature-256 (HMAC-SHA256 over the raw
// body, hex, "sha256=" prefixed) in constant time. I-2.
func verifySignature(secret, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

// Sign computes the X-Hub-Signature-256 header value for a body (test/helper +
// the canonical signing used by any internal caller). Exported so tests can forge
// a CORRECTLY-signed-but-LYING webhook and prove it still cannot fast-track.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// parsePRNumber extracts the affected PR number from a webhook body. The §8.1.3
// subscribed events all carry a PR number in one of these shapes; we treat them
// uniformly as refetch hints. Unknown shapes return 0 (record-only).
// parseIssueAction reads the `action` of an issues webhook (labeled / opened / …).
func parseIssueAction(body []byte) string {
	var p struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	return p.Action
}

func parsePRNumber(event string, body []byte) int {
	var p struct {
		Number      int `json:"number"`
		PullRequest struct {
			Number int `json:"number"`
		} `json:"pull_request"`
		CheckRun struct {
			PullRequests []struct {
				Number int `json:"number"`
			} `json:"pull_requests"`
		} `json:"check_run"`
	}
	if json.Unmarshal(body, &p) != nil {
		return 0
	}
	if p.PullRequest.Number > 0 {
		return p.PullRequest.Number
	}
	if event == "pull_request" && p.Number > 0 {
		return p.Number
	}
	if len(p.CheckRun.PullRequests) > 0 {
		return p.CheckRun.PullRequests[0].Number
	}
	return 0
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}
