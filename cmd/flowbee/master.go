package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// runMaster is the `flowbee master <register|poll|resolve|status>` CLI — the
// supervise-epics skill's runnable surface (epic-lane Phase 6b, plan §1 + §2.2). Unlike
// `flowbee epic`/`seat` (store-direct), the master talks the HTTP API: the resolve state
// machine delivers a reply into a live pane SERVER-side (fenced + ledgered), so the CLI is
// a thin client. It persists the registration + last-leased item_epochs to a small state
// file so `poll`/`resolve` need no --master-id/--epoch/--item-epoch flags (matching the
// skill's documented shape exactly).
func runMaster(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee master <register|poll|resolve|status> ...")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "register":
		return runMasterRegister(rest)
	case "poll":
		return runMasterPoll(rest)
	case "resolve":
		return runMasterResolve(rest)
	case "status":
		return runMasterStatus(rest)
	default:
		return fmt.Errorf("unknown `flowbee master` subcommand %q (want register|poll|resolve|status)", sub)
	}
}

// masterState is the small persisted registration the poll/resolve loop reads back so the
// master (a stateless loop that /clears and re-registers) never tracks ids by hand.
type masterState struct {
	Label    string         `json:"label"`
	MasterID string         `json:"master_id"`
	Epoch    int            `json:"epoch"`
	Leased   map[string]int `json:"leased"` // item id -> item_epoch (from the last poll)
}

func masterStatePath() string {
	if p := os.Getenv("FLOWBEE_MASTER_STATE"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".flowbee", "master.json")
}

func loadMasterState() (masterState, error) {
	var st masterState
	b, err := os.ReadFile(masterStatePath())
	if err != nil {
		return st, fmt.Errorf("no master registration found (run `flowbee master register --label <name>` first): %w", err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, err
	}
	if st.Leased == nil {
		st.Leased = map[string]int{}
	}
	return st, nil
}

func saveMasterState(st masterState) error {
	p := masterStatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func runMasterRegister(args []string) error {
	fs := flag.NewFlagSet("master register", flag.ContinueOnError)
	label := fs.String("label", "", "stable master label (the idempotent registration key)")
	kind := fs.String("kind", "claude", "the master's own agent binary (claude|codex)")
	family := fs.String("model-family", "", "the master's model family (anti-affinity tag; default = kind)")
	box := fs.String("box", "", "the master's box ('' = control-plane box)")
	tmux := fs.String("tmux", "master", "the master's tmux session name (for the pane-idle backstop / push-to-wake)")
	asJSON := fs.Bool("json", false, "print the raw registration JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *label == "" {
		return fmt.Errorf("--label is required")
	}
	mf := *family
	if mf == "" {
		mf = *kind
	}
	body := map[string]any{
		"label": *label, "kind": *kind, "model_family": mf,
		"box": *box, "tmux_name": *tmux,
	}
	var resp struct {
		MasterID           string `json:"master_id"`
		Epoch              int    `json:"epoch"`
		HeartbeatIntervalS int    `json:"heartbeat_interval_s"`
		OpenItems          int    `json:"open_items"`
	}
	raw, err := masterPost("/v1/masters/register", body, &resp)
	if err != nil {
		return err
	}
	if err := saveMasterState(masterState{Label: *label, MasterID: resp.MasterID, Epoch: resp.Epoch, Leased: map[string]int{}}); err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("registered master %q (id=%s, epoch=%d) — %d open item(s), heartbeat every %ds\n",
		*label, resp.MasterID, resp.Epoch, resp.OpenItems, resp.HeartbeatIntervalS)
	return nil
}

func runMasterPoll(args []string) error {
	fs := flag.NewFlagSet("master poll", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw poll JSON (heartbeat + digest + leased items)")
	maxN := fs.Int("max", 5, "max items to lease this poll")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := loadMasterState()
	if err != nil {
		return err
	}
	body := map[string]any{"master_id": st.MasterID, "epoch": st.Epoch, "max": *maxN}
	var resp struct {
		Revoked        bool             `json:"revoked"`
		DigestSeq      int64            `json:"digest_seq"`
		Leased         []map[string]any `json:"leased"`
		Attention      []map[string]any `json:"attention"`
		LeaseExpiresAt string           `json:"lease_expires_at"`
	}
	raw, err := masterPost("/v1/masters/attention/lease", body, &resp)
	if err != nil {
		return err
	}
	if resp.Revoked {
		return fmt.Errorf("registration revoked (a newer master superseded this one) — run `flowbee master register --label %s` again", st.Label)
	}
	// cache the leased items' item_epochs so `resolve <id>` needs no --item-epoch flag.
	st.Leased = map[string]int{}
	for _, it := range resp.Leased {
		id, _ := it["id"].(string)
		ie, _ := it["item_epoch"].(float64)
		if id != "" {
			st.Leased[id] = int(ie)
		}
	}
	_ = saveMasterState(st)
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	if len(resp.Leased) == 0 {
		fmt.Printf("nothing to do (digest_seq=%d, %d open item(s)) — sleep and poll again\n", resp.DigestSeq, len(resp.Attention))
		return nil
	}
	fmt.Printf("leased %d item(s) (digest_seq=%d):\n", len(resp.Leased), resp.DigestSeq)
	for _, it := range resp.Leased {
		fmt.Printf("  %v  %v  (epic %v, prio %v)\n", it["id"], it["kind"], it["epic"], it["priority"])
	}
	return nil
}

func runMasterResolve(args []string) error {
	fs := flag.NewFlagSet("master resolve", flag.ContinueOnError)
	reply := fs.String("reply", "", "steer the session with a verified send (then awaiting_ack)")
	amend := fs.String("amend", "", "authorize a ## Amendments entry (merge-of-main / scope note)")
	dismiss := fs.Bool("dismiss", false, "resolve without a send (a false/cleared item; audited)")
	ack := fs.Bool("ack", false, "acknowledge, benign, no send")
	escalate := fs.Bool("escalate", false, "route to a human (NeedsHuman + optional push)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: flowbee master resolve <id> [--reply|--amend|--dismiss|--ack|--escalate]")
	}
	id := fs.Arg(0)
	action, payload, err := resolveAction(*reply, *amend, *dismiss, *ack, *escalate)
	if err != nil {
		return err
	}
	st, err := loadMasterState()
	if err != nil {
		return err
	}
	itemEpoch, ok := st.Leased[id]
	if !ok {
		return fmt.Errorf("item %q is not in this master's last-leased set — poll first (`flowbee master poll`)", id)
	}
	body := map[string]any{
		"master_id": st.MasterID, "epoch": st.Epoch, "item_epoch": itemEpoch,
		"action": action, "payload": payload,
	}
	if action == "reply" || action == "amend" {
		body["idempotency_key"] = idempotencyKey(st.MasterID, id, itemEpoch, payload)
	}
	var resp map[string]any
	if _, err := masterPost("/v1/masters/attention/"+id+"/resolve", body, &resp); err != nil {
		return err
	}
	// a resolved reply/amend clears the item from the leased cache (it is now awaiting_ack).
	delete(st.Leased, id)
	_ = saveMasterState(st)
	fmt.Printf("resolved %s: %v\n", id, compactJSON(resp))
	return nil
}

// resolveAction validates that exactly one resolve verb was given and returns the server
// action + payload.
func resolveAction(reply, amend string, dismiss, ack, escalate bool) (action, payload string, err error) {
	n := 0
	if reply != "" {
		n, action, payload = n+1, "reply", reply
	}
	if amend != "" {
		n, action, payload = n+1, "amend", amend
	}
	if dismiss {
		n, action = n+1, "dismiss"
	}
	if ack {
		n, action = n+1, "ack"
	}
	if escalate {
		n, action = n+1, "escalate"
	}
	if n != 1 {
		return "", "", fmt.Errorf("give exactly one of --reply/--amend/--dismiss/--ack/--escalate")
	}
	return action, payload, nil
}

func runMasterStatus(args []string) error {
	fs := flag.NewFlagSet("master status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw attention JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, serr := loadMasterState()
	if serr == nil {
		fmt.Printf("registered: %s (id=%s, epoch=%d)\n", st.Label, st.MasterID, st.Epoch)
	} else {
		fmt.Println("not registered on this box (run `flowbee master register --label <name>`)")
	}
	var resp struct {
		DigestSeq int64            `json:"digest_seq"`
		Items     []map[string]any `json:"items"`
	}
	raw, err := masterGet("/v1/masters/attention", &resp)
	if err != nil {
		return err
	}
	if *asJSON {
		fmt.Println(string(raw))
		return nil
	}
	fmt.Printf("open attention (digest_seq=%d): %d item(s)\n", resp.DigestSeq, len(resp.Items))
	for _, it := range resp.Items {
		fmt.Printf("  %v  %v  (epic %v, prio %v, state %v)\n", it["id"], it["kind"], it["epic"], it["priority"], it["state"])
	}
	return nil
}

// idempotencyKey is a STABLE per-intended-send key (plan §1.5): same (master, item,
// item_epoch, payload) → same key, so a retried send is deduped by the delivery_key.
func idempotencyKey(masterID, itemID string, itemEpoch int, payload string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%s", masterID, itemID, itemEpoch, payload)))
	return hex.EncodeToString(h[:16])
}

// ── HTTP plumbing (mirrors work.go's FLOWBEE_URL + FLOWBEE_WORKER_TOKEN posture) ──

func masterPost(path string, body, out any) ([]byte, error) {
	return masterDo(http.MethodPost, path, body, out)
}

func masterGet(path string, out any) ([]byte, error) {
	return masterDo(http.MethodGet, path, nil, out)
}

func masterDo(method, path string, body, out any) ([]byte, error) {
	url := envOr("FLOWBEE_URL", "http://127.0.0.1:7070") + path
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("FLOWBEE_WORKER_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call %s (is `flowbee serve` running at %s?): %w", path, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, bytesTrim(raw))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return raw, fmt.Errorf("decode %s response: %w", path, err)
		}
	}
	return raw, nil
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func bytesTrim(b []byte) string {
	s := string(b)
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
