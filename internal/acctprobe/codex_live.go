package acctprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ── live app-server payload shapes ──

type codexRateLimitsResult struct {
	RateLimitsByLimitID map[string]codexLiveBucket `json:"rateLimitsByLimitId"`
	RateLimits          *codexLiveBucket           `json:"rateLimits"`
}

type codexLiveBucket struct {
	LimitName string        `json:"limitName"`
	Primary   *codexLiveWin `json:"primary"`
	Secondary *codexLiveWin `json:"secondary"`
}

type codexLiveWin struct {
	UsedPercent        *float64        `json:"usedPercent"` // pointer: absent ≠ 0%
	ResetsAt           json.RawMessage `json:"resetsAt"`    // epoch int OR ISO string
	WindowDurationMins int             `json:"windowDurationMins"`
}

type codexAccountResult struct {
	Account struct {
		Email    string `json:"email"`
		PlanType string `json:"planType"`
	} `json:"account"`
}

// ProbeCodexLive reads a Codex home's REAL usage LIVE via the `codex app-server`
// JSON-RPC handshake (account/rateLimits/read + account/read), identity-bound to the
// home's login. This is the authoritative tier; on success TrustState is Verified.
//
// An API-key seat has no subscription windows to route — that is a typed EXCLUSION
// (ReasonApikeyNoSubscription), not a failure. App-server auth/protocol errors hold
// with distinct reasons and NEVER fall back to display-only telemetry; only a genuine
// unavailable app-server (old CLI) is a fallback case (see ProbeCodex).
func (p *Prober) ProbeCodexLive(ctx context.Context, dir string) (*Result, error) {
	id, err := p.codexIdentity(dir)
	if err != nil {
		return nil, err
	}
	if id.AuthMode == "apikey" {
		return nil, held(ReasonApikeyNoSubscription, errors.New("codex api-key seat has no subscription windows"))
	}

	read, err := p.AppServer.Read(ctx, dir)
	if err != nil {
		return nil, err // already a *HoldError from the client
	}

	var rl codexRateLimitsResult
	if err := json.Unmarshal(read.RateLimits, &rl); err != nil {
		return nil, held(ReasonAppServerProtocol, fmt.Errorf("parse codex rate limits: %w", err))
	}
	main := rl.RateLimits
	if b, ok := rl.RateLimitsByLimitID["codex"]; ok {
		bb := b
		main = &bb
	}
	scoped := map[string]codexLiveBucket{}
	for lid, b := range rl.RateLimitsByLimitID {
		if lid != "codex" {
			scoped[lid] = b
		}
	}
	windows, err := codexLiveWindows(main, scoped)
	if err != nil {
		return nil, err
	}

	var acct codexAccountResult
	_ = json.Unmarshal(read.Account, &acct) // best-effort; identity already bound locally
	if acct.Account.Email != "" {
		id.Email = acct.Account.Email
	}
	if acct.Account.PlanType != "" {
		id.Tier = acct.Account.PlanType
	}
	id.Verified = true

	usage := Usage{Windows: windows, PlanType: acct.Account.PlanType}
	if maxPct, ok := windows.MaxPct(); ok && maxPct >= 100 {
		usage.RateLimited = true
	}
	return &Result{
		Identity:   id,
		Usage:      usage,
		TrustState: TrustVerified,
		CapturedAt: p.Clock.Now().UTC(),
		Source:     "codex_app_server",
	}, nil
}

// ProbeCodex is the tiered read: LIVE app-server first (authoritative, routable),
// falling back to DISPLAY-ONLY on-disk telemetry ONLY when the app-server is genuinely
// unavailable (an older Codex CLI without it). Every other live failure — auth
// rejection, protocol error, API-key exclusion, unrecognized capacity — holds with its
// typed reason and never degrades to non-live telemetry.
func (p *Prober) ProbeCodex(ctx context.Context, dir string) (*Result, error) {
	res, err := p.ProbeCodexLive(ctx, dir)
	if err == nil {
		return res, nil
	}
	var hold *HoldError
	if errors.As(err, &hold) && hold.Reason == ReasonAppServerUnavailable {
		if disp, derr := p.ProbeCodexHome(dir); derr == nil {
			return disp, nil
		}
	}
	return nil, err
}

// codexLiveWindows builds usage windows from the app-server rate-limits payload,
// bucketing each by its REAL windowDurationMins (never by primary/secondary position —
// the server reorders and OMITS non-constraining windows). It requires the weekly
// window (7d): OpenAI lifted the 5h limit in 2026-07, so a missing 5h is tolerated but
// an absent weekly means we have no routable capacity signal and the seat is held.
// Model-scoped buckets become weekly scoped rows.
func codexLiveWindows(main *codexLiveBucket, scoped map[string]codexLiveBucket) (Windows, error) {
	var ws Windows
	seen := map[WindowKind]bool{}
	if main != nil {
		for _, w := range []*codexLiveWin{main.Primary, main.Secondary} {
			if w == nil || w.UsedPercent == nil {
				continue
			}
			lw, ok := bucketWindow(w.WindowDurationMins, *w.UsedPercent, parseResetsAt(w.ResetsAt))
			if ok && !seen[lw.Kind] {
				ws = append(ws, lw)
				seen[lw.Kind] = true
			}
		}
	}
	if !seen[KindWeeklyAll] {
		return nil, held(ReasonUnrecognizedPayload, errors.New("codex app-server returned no weekly capacity window"))
	}
	for _, b := range scoped {
		name := strings.TrimSpace(b.LimitName)
		if name == "" {
			continue
		}
		// Show only the trailing model codename ("Spark" from "GPT-5.3-Codex-Spark").
		label := name
		if i := strings.LastIndex(name, "-"); i >= 0 && i+1 < len(name) {
			label = name[i+1:]
		}
		for _, w := range []*codexLiveWin{b.Primary, b.Secondary} {
			if w == nil || w.UsedPercent == nil || w.WindowDurationMins != 10080 {
				continue
			}
			if *w.UsedPercent < 0 || *w.UsedPercent > 100 {
				continue
			}
			sev := SeverityNormal
			if *w.UsedPercent >= 100 {
				sev = SeverityCritical
			}
			ws = append(ws, LimitWindow{
				Kind: KindWeeklyScoped, Percent: *w.UsedPercent, Severity: sev,
				ResetsAt: parseResetsAt(w.ResetsAt), Scope: label,
				WindowMinutes: 10080, Active: true,
			})
			break
		}
	}
	return ws, nil
}

// parseResetsAt accepts either a numeric unix-seconds epoch or an ISO-8601 string.
func parseResetsAt(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return unixMaybe(n)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return parseRFC3339(str)
	}
	return time.Time{}
}

// ── real app-server client ──

// appServer failure classification markers, ported from headroom: an explicit auth
// rejection or protocol error must never degrade into routable local telemetry.
var (
	codexAuthMarkers = []string{
		"token_invalidated", "refresh token", "invalid_grant", "unauthorized",
		"401", "login required", "not logged in", "re-login", "login again",
	}
	codexThrottleMarkers = []string{
		"429", "too many requests", "overload", "throttl",
		"temporarily unavailable", "503", "retry later",
	}
)

// codexAuthOverrideVars are env vars that would silently redirect the app-server off
// the CODEX_HOME login (to an API key or alternate identity); scrubbed before spawn.
var codexAuthOverrideVars = map[string]bool{
	"OPENAI_API_KEY": true, "OPENAI_BASE_URL": true,
	"CODEX_API_KEY": true, "CODEX_AGENT_IDENTITY": true,
}

// execAppServer is the real AppServerClient: it spawns `codex app-server` and drives
// the JSON-RPC handshake over stdio.
type execAppServer struct {
	binary  string
	timeout time.Duration
}

func newExecAppServer(_ ExecRunner) *execAppServer {
	return &execAppServer{binary: "codex", timeout: 25 * time.Second}
}

// Read performs initialize → initialized → account/rateLimits/read (id 2) →
// account/read (id 3), returning the two `result` objects. It classifies failures
// into typed HoldErrors: spawn/no-response ⇒ app_server_unavailable (the only
// fallback case), an RPC error ⇒ auth/throttle/protocol.
func (a *execAppServer) Read(ctx context.Context, codexHome string) (AppServerResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, a.binary, "app-server")
	cmd.Env = scrubbedCodexEnv(codexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	if err := cmd.Start(); err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	responses := make(chan map[string]json.RawMessage, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			var msg map[string]json.RawMessage
			if json.Unmarshal(scanner.Bytes(), &msg) != nil {
				continue
			}
			if _, ok := msg["id"]; ok {
				responses <- msg
			}
		}
		close(responses)
	}()

	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = stdin.Write(append(b, '\n'))
		return err
	}

	if err := send(rpc(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "flowbee-acctprobe", "version": "1"}})); err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	if err := send(map[string]any{"jsonrpc": "2.0", "method": "initialized", "params": map[string]any{}}); err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	if err := send(rpc(2, "account/rateLimits/read", map[string]any{})); err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}
	if err := send(rpc(3, "account/read", map[string]any{})); err != nil {
		return AppServerResult{}, held(ReasonAppServerUnavailable, err)
	}

	byID := map[int]map[string]json.RawMessage{}
	for {
		_, has2 := byID[2]
		_, has3 := byID[3]
		if has2 && has3 {
			return finishAppServer(byID)
		}
		select {
		case <-runCtx.Done():
			return AppServerResult{}, held(ReasonAppServerUnavailable, errors.New("codex app-server timeout"))
		case msg, ok := <-responses:
			if !ok {
				return AppServerResult{}, held(ReasonAppServerUnavailable, errors.New("codex app-server closed before both reads"))
			}
			if id, err := rpcID(msg["id"]); err == nil {
				byID[id] = msg
			}
		}
	}
}

func finishAppServer(byID map[int]map[string]json.RawMessage) (AppServerResult, error) {
	m2, ok2 := byID[2]
	m3, ok3 := byID[3]
	if !ok2 || !ok3 {
		return AppServerResult{}, held(ReasonAppServerUnavailable, errors.New("codex app-server incomplete response"))
	}
	for _, m := range []map[string]json.RawMessage{m2, m3} {
		if raw, ok := m["error"]; ok && len(raw) > 0 && string(raw) != "null" {
			return AppServerResult{}, held(classifyAppServerError(raw), fmt.Errorf("codex app-server error: %s", raw))
		}
	}
	return AppServerResult{RateLimits: resultOf(m2), Account: resultOf(m3)}, nil
}

func resultOf(m map[string]json.RawMessage) []byte {
	if r, ok := m["result"]; ok {
		return r
	}
	return []byte("{}")
}

func rpc(id int, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func rpcID(raw json.RawMessage) (int, error) {
	var id int
	if err := json.Unmarshal(raw, &id); err != nil {
		return 0, err
	}
	return id, nil
}

// classifyAppServerError maps an RPC error object to a distinct hold reason.
func classifyAppServerError(raw json.RawMessage) HoldReason {
	text := strings.ToLower(string(raw))
	for _, m := range codexAuthMarkers {
		if strings.Contains(text, m) {
			return ReasonAppServerAuth
		}
	}
	for _, m := range codexThrottleMarkers {
		if strings.Contains(text, m) {
			return ReasonThrottled
		}
	}
	return ReasonAppServerProtocol
}

// scrubbedCodexEnv returns the process env with auth-override vars removed and
// CODEX_HOME pinned to the slot, so the app-server reads the intended login.
func scrubbedCodexEnv(codexHome string) []string {
	src := os.Environ()
	out := make([]string, 0, len(src)+1)
	for _, kv := range src {
		if k, _, ok := strings.Cut(kv, "="); ok && codexAuthOverrideVars[k] {
			continue
		}
		if strings.HasPrefix(kv, "CODEX_HOME=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "CODEX_HOME="+codexHome)
}
