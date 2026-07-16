// Package acctprobe resolves, for a running agent process (or a config
// directory), WHICH account the agent is using and that account's REAL
// usage-limit state, reading the data both agent CLIs already have available:
// live from the provider (the source of truth) with the on-disk caches as a
// fallback tier. It feeds Flowbee's dashboard (per epic: account + remaining
// session/weekly limits) and folds into the F6 scheduler (internal/capacity).
//
// Two tiers, because a cache is not a truth:
//   - LIVE is authoritative. Claude: GET api.anthropic.com/api/oauth/usage with
//     the account's own OAuth token (the same call the Claude Code UI makes).
//     Codex: the `codex app-server` JSON-RPC (account/rateLimits/read +
//     account/read) — the on-disk `rate_limits` telemetry is display-only and
//     carries known upstream stamping bugs (openai/codex#16323, #14880), so it
//     is NEVER scheduled on.
//   - LOCAL cache is a cheap fallback that only refreshes while THAT config-dir's
//     CLI is running — a dormant account (the prime candidate for new work) goes
//     stale. Every local reading is stamped with its capture time so callers can
//     age it; the scheduler must treat it as at best VerifiedLocal/Stale.
//
// FAIL-CLOSED — every probe yields a Result whose TrustState says how much the
// reading can be trusted (Verified / VerifiedLocal / Stale / DisplayOnly / Held),
// and a Held result carries a typed HoldReason (token_expired, token_rejected,
// throttled, apikey_no_subscription, unrecognized_payload, duplicate_identity, …)
// so a caller can react precisely. Unknown is always distinguishable from zero: an
// absent window is absent, never synthesized as 0%. Percent-of-limit is the only
// currency — this package never models absolute token budgets.
//
// SECURITY — this package touches OAuth tokens (it must, to make the live call)
// but NEVER logs, returns, or embeds a token, JWT, or credential value in any
// exported type. Tokens live only in transient locals used to build a request
// Authorization header and to compute a one-way digest; the raw secret is never
// stored. The only credential-derived values that leave the package are digests:
// CredentialDigest / LineageDigest (sha256, truncated) for TOCTOU-safe identity
// binding. Config files are parsed against an explicit allow-list of non-secret
// fields (see claudeConfig / codexAuth) so a stray secret has no field to land in.
//
// REMOTE-READY — filesystem, exec, HTTP, the app-server client, and the clock are
// all injected interfaces (defaults wrap os/net); the same probes can later run
// over ssh against a remote box without touching the probe logic.
//
// NOTICE — the live-read semantics, per-config-dir Keychain namespacing, org-header
// trust-on-first-use binding, Codex window bucketing (by real windowDurationMins),
// the API-key exclusion, and the trust/hold vocabulary are closely ported from
// headroom (https://github.com/domanski-ai/headroom, MIT, © 2026 Paul Domanski).
package acctprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/samhotchkiss/flowbee/internal/capacity"
	"github.com/samhotchkiss/flowbee/internal/clock"
)

// Provider names the agent CLI an account belongs to; it is the model_family the
// capacity scheduler keys slots and accounts on.
type Provider string

const (
	ProviderClaude Provider = "claude"
	ProviderCodex  Provider = "codex"
)

// TrustState says how far a Result's reading can be trusted. Routable() is the
// gate the scheduler uses; the dashboard shows every state.
type TrustState string

const (
	// TrustVerified: a LIVE, identity-bound reading from the provider.
	TrustVerified TrustState = "verified"
	// TrustVerifiedLocal: a fresh on-disk cache reading whose identity was bound
	// from local metadata only (no network verification).
	TrustVerifiedLocal TrustState = "verified_local"
	// TrustStale: a reading present but older than the freshness bound — shown,
	// not routed.
	TrustStale TrustState = "stale"
	// TrustDisplayOnly: Codex on-disk session telemetry — visible on the
	// dashboard, NEVER capacity-routable (display-only per upstream bugs).
	TrustDisplayOnly TrustState = "display_only"
	// TrustHeld: no trustworthy reading; see Result.Hold for the typed reason.
	TrustHeld TrustState = "held"
)

// Routable reports whether the scheduler may dispatch against this trust state.
func (t TrustState) Routable() bool {
	return t == TrustVerified || t == TrustVerifiedLocal
}

// HoldReason is the closed, typed vocabulary for WHY a probe held a seat, so a
// caller can react to each precisely (re-login vs. wait-out-a-429 vs. exclude).
type HoldReason string

const (
	ReasonTokenExpired         HoldReason = "token_expired"          // cached token past expiry (we never refresh)
	ReasonTokenRejected        HoldReason = "token_rejected"         // provider 401/403 — expired or revoked
	ReasonThrottled            HoldReason = "throttled"              // provider 429 on the usage meter itself
	ReasonCredentialsMissing   HoldReason = "credentials_missing"    // login present but token unreadable
	ReasonIdentityMissing      HoldReason = "identity_missing"       // no usable account identity on disk
	ReasonOrgUnverifiable      HoldReason = "org_unverifiable"       // usage response carried no org header to bind
	ReasonOrgChanged           HoldReason = "org_changed"            // usage org != the pinned (trust-on-first-use) org
	ReasonUnrecognizedPayload  HoldReason = "unrecognized_payload"   // response parsed but had no usable window
	ReasonApikeyNoSubscription HoldReason = "apikey_no_subscription" // Codex API-key seat: no subscription windows exist
	ReasonAppServerUnavailable HoldReason = "app_server_unavailable" // codex app-server could not be reached (old CLI)
	ReasonAppServerAuth        HoldReason = "app_server_auth"        // app-server rejected auth (re-login required)
	ReasonAppServerProtocol    HoldReason = "app_server_protocol"    // app-server malformed/protocol error
	ReasonDuplicateIdentity    HoldReason = "duplicate_identity"     // two config dirs resolve to one account
)

// HoldError is the typed error every probe returns when it cannot produce a
// trustworthy reading. RetryAt is set for ReasonThrottled (provider retry window);
// Err wraps the underlying cause for logs but is NEVER a secret.
type HoldError struct {
	Reason  HoldReason
	RetryAt time.Time
	Err     error
}

func (e *HoldError) Error() string {
	if e.Err != nil {
		return "acctprobe: held (" + string(e.Reason) + "): " + e.Err.Error()
	}
	return "acctprobe: held (" + string(e.Reason) + ")"
}

func (e *HoldError) Unwrap() error { return e.Err }

func held(reason HoldReason, err error) *HoldError { return &HoldError{Reason: reason, Err: err} }

// Identity is the stable, non-secret answer to "which account is this?". Fingerprint
// binds the account (sha256 of org uuid for Claude, account_id for Codex), so folds
// are stable across config-dir moves. The *Digest fields bind to the actual token
// lineage to close credential-swap TOCTOUs — they are digests, never the secret.
type Identity struct {
	Provider         Provider
	AccountKey       string // durable account id (claude accountUuid / codex account_id)
	Fingerprint      string // sha256(orgUuid|account_id)[:16]; "" when the id was missing
	Email            string // claude oauthAccount.emailAddress / codex account.email (live)
	Org              string // claude oauthAccount.organizationName
	OrgKey           string // claude oauthAccount.organizationUuid
	ConfigDir        string // resolved config dir (claude) or CODEX_HOME (codex)
	Tier             string // claude organizationRateLimitTier / codex planType
	SeatTier         string // claude oauthAccount.seatTier (may be empty/null on disk)
	AuthMode         string // codex auth.json auth_mode ("chatgpt"/"apikey"); empty for claude
	Model            string // codex config.toml model (e.g. "gpt-5.6-sol"); empty for claude
	Verified         bool   // true only when identity was network-verified (live)
	CredentialDigest string // sha256(accessToken)[:16] — swap detection; never the token
	LineageDigest    string // codex sha256(refreshToken)[:16] — fresh-login detection; never the token
}

// WindowKind classifies a usage window across both providers onto one closed
// vocabulary so the dashboard and scheduler never special-case per provider.
type WindowKind string

const (
	// KindSession is the short rolling window: Claude five_hour / a 300-minute
	// Codex window (the pre-2026-07 5h limit).
	KindSession WindowKind = "session"
	// KindWeeklyAll is the account-wide weekly window: Claude seven_day / a
	// 10080-minute Codex window.
	KindWeeklyAll WindowKind = "weekly_all"
	// KindWeeklyScoped is a per-model weekly sub-limit (Claude weekly_scoped /
	// a Codex model-scoped bucket), keyed by Scope.
	KindWeeklyScoped WindowKind = "weekly_scoped"
)

// Severity mirrors the server's own severity flag. Critical means the server is
// already treating the account as at/near its hard limit.
type Severity string

const (
	SeverityNormal   Severity = "normal"
	SeverityCritical Severity = "critical"
)

// LimitWindow is one usage window as the server reported it. Percent is the REAL
// server percentage (0..100). ResetsAt is zero when unknown. An absent window is
// simply not present in Windows — it is never synthesized as 0%.
type LimitWindow struct {
	Kind          WindowKind
	Percent       float64   // server-reported utilization percent, 0..100
	Severity      Severity  // server severity flag (derived for codex)
	ResetsAt      time.Time // window reset instant; zero if unknown
	Scope         string    // model display name for scoped windows; "" otherwise
	WindowMinutes int       // real window duration in minutes (300, 10080, …); 0 if unknown
	Active        bool      // server is_active flag (claude); true for live codex windows
}

// Windows is the set of limit windows for one account, with helpers that fold them
// to the single numbers the dashboard and scheduler want. A returned ok=false means
// UNKNOWN (no such window was reported) — never a real 0%.
type Windows []LimitWindow

// SessionPct returns the highest session-window percent and whether any existed.
func (w Windows) SessionPct() (float64, bool) { return w.maxOf(KindSession) }

// WeeklyPct returns the highest account-wide weekly percent and whether any existed.
func (w Windows) WeeklyPct() (float64, bool) { return w.maxOf(KindWeeklyAll) }

func (w Windows) maxOf(k WindowKind) (float64, bool) {
	best, ok := 0.0, false
	for _, win := range w {
		if win.Kind != k {
			continue
		}
		if !ok || win.Percent > best {
			best, ok = win.Percent, true
		}
	}
	return best, ok
}

// MaxPct returns the single most-constraining percent across ALL windows — the
// number that actually gates dispatch — and whether any window existed.
func (w Windows) MaxPct() (float64, bool) {
	best, ok := 0.0, false
	for _, win := range w {
		if !ok || win.Percent > best {
			best, ok = win.Percent, true
		}
	}
	return best, ok
}

// Critical reports whether the server flagged (or we derived) ANY window critical.
func (w Windows) Critical() bool {
	for _, win := range w {
		if win.Severity == SeverityCritical {
			return true
		}
	}
	return false
}

// Usage is an account's usage-limit state at a point in time. Windows is the
// authoritative per-window list; the rest are non-secret provider extras.
type Usage struct {
	Windows     Windows
	RateLimited bool // server signalled a hard limit was hit (codex reached-type; or a window at 100%)

	// Codex extras (zero for claude).
	TotalTokens   int64  // newest session info.total_token_usage.total_tokens (local telemetry only)
	ContextWindow int64  // info.model_context_window (local telemetry only)
	CreditBalance string // rate_limits.credits.balance (server sends a string)
	PlanType      string // plan_type / planType

	// Claude extras (zero for codex).
	ExtraUsageEnabled bool    // utilization.extra_usage.is_enabled
	SpendPct          float64 // utilization.spend.percent
	SpendEnabled      bool    // utilization.spend.enabled
}

// Result is the full answer for one account: identity, usage, and how much to
// trust it. CapturedAt is when THIS reading was captured (the provider's own
// timestamp when it gives one, else our observation time) — callers age-bound
// carry-forward against it. Hold is set (and TrustState==TrustHeld) when no
// trustworthy reading was produced.
type Result struct {
	Identity   Identity
	Usage      Usage
	TrustState TrustState
	Hold       HoldReason // "" unless TrustState==TrustHeld
	RetryAt    time.Time  // provider retry window for a throttled hold; else zero
	CapturedAt time.Time  // when this reading was captured; zero when unknown
	Source     string     // where the reading came from ("anthropic_usage_api", cache path, …)
	// UsageOrgFingerprint is the fingerprint of the org the LIVE Claude usage
	// response was bound to (from the anthropic-organization-id header). It can
	// legitimately differ from Identity.Fingerprint (the login's default org) on
	// multi-org accounts, so callers pin it trust-on-first-use and hold the slot if
	// it later CHANGES. Empty for local/codex readings.
	UsageOrgFingerprint string
}

// Routable is a convenience over TrustState for the scheduler.
func (r Result) Routable() bool { return r.TrustState.Routable() }

// Staleness reports how old this reading is relative to a supplied instant. The
// instant is passed IN (never read from a clock here) so the result is a pure
// function of its inputs, matching the deterministic-core spirit of the codebase.
// A zero CapturedAt yields ok=false.
func (r Result) Staleness(now time.Time) (time.Duration, bool) {
	if r.CapturedAt.IsZero() {
		return 0, false
	}
	return now.Sub(r.CapturedAt), true
}

// UsageReport adapts a probe into the capacity scheduler's per-account observation
// for modelFamily. UsagePct is the single most-constraining window percent, rounded
// UP (a fractional 0.4% must not read as 0/idle to a ceiling gate). RateLimited
// carries the server's hard-limit signal so the scheduler pins the account exactly
// as a 429 would. AccountID is the durable AccountKey. NOTE: a caller should only
// fold a Routable() result — a Held/DisplayOnly reading must not drive dispatch.
func (r Result) UsageReport(modelFamily string) capacity.UsageReport {
	pct := 0
	if maxPct, ok := r.Usage.Windows.MaxPct(); ok {
		pct = int(math.Ceil(maxPct))
	}
	return capacity.UsageReport{
		AccountID:   r.Identity.AccountKey,
		ModelFamily: modelFamily,
		UsagePct:    pct,
		RateLimited: r.Usage.RateLimited,
	}
}

// digest16 returns hex(sha256(secret))[:16] and true, or ("",false) for an empty
// secret. It never returns or logs the secret. Used for the *Digest fields.
func digest16(secret string) (string, bool) {
	if secret == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])[:16], true
}

// fingerprint returns hex(sha256(id))[:16] for a non-empty account/org id, or ""
// (a missing id must never mint a valid-looking fingerprint that could collide).
func fingerprint(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])[:16]
}

// ── injected dependencies ──

// FS is the read-only filesystem access acctprobe needs, over ABSOLUTE paths, so
// the local default (OSFS) and a future ssh/sftp implementation are interchangeable.
type FS interface {
	ReadFile(name string) ([]byte, error)
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error)
	Open(name string) (File, error)
}

// File is an open file that supports tail reads (Seek) so large Codex rollout logs
// can be scanned from the end without slurping them whole.
type File interface {
	io.ReadSeekCloser
	Stat() (fs.FileInfo, error)
}

// ExecRunner runs a short, argument-vector command and returns its stdout (only —
// stderr is dropped so an env fragment can never leak into an error string). It is
// how this package runs `ps` and macOS `security`; injecting it keeps them mockable.
type ExecRunner interface {
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
}

// HTTPDoer performs the live usage HTTP request. The default forbids redirects so a
// bearer token can never be forwarded to a redirect target's origin.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// AppServerResult is the raw JSON of the two Codex app-server reads: the
// account/rateLimits/read result (id 2) and the account/read result (id 3).
type AppServerResult struct {
	RateLimits []byte // JSON of result (may hold rateLimitsByLimitId or rateLimits)
	Account    []byte // JSON of result.account
}

// AppServerClient performs the `codex app-server` JSON-RPC handshake against a
// CODEX_HOME and returns the two reads. Injected so tests fake it (no subprocess)
// and a remote mode can drive it over ssh. A *HoldError classifies auth/protocol/
// unavailable failures for the caller.
type AppServerClient interface {
	Read(ctx context.Context, codexHome string) (AppServerResult, error)
}

// OSFS is the local filesystem, reading absolute paths straight through os.*.
type OSFS struct{}

func (OSFS) ReadFile(name string) ([]byte, error)       { return os.ReadFile(name) }
func (OSFS) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }
func (OSFS) Stat(name string) (fs.FileInfo, error)      { return os.Stat(name) }
func (OSFS) Open(name string) (File, error) {
	f, err := os.Open(name) //nolint:gosec // absolute paths are caller-supplied config dirs, read-only
	if err != nil {
		return nil, err
	}
	return f, nil
}

// osExec is the local ExecRunner (stdout only).
type osExec struct{}

func (osExec) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// Prober carries the injected dependencies and exposes every entry point. Build the
// default (local) prober with New(); inject fakes with NewWith for tests or remote.
type Prober struct {
	FS        FS
	Exec      ExecRunner
	HTTP      HTTPDoer
	AppServer AppServerClient
	Clock     clock.Clock
	// StaleAfter bounds how old a LOCAL cache reading may be before it is downgraded
	// from VerifiedLocal to Stale. Zero uses DefaultStaleAfter.
	StaleAfter time.Duration
}

// DefaultStaleAfter is the freshness bound for local cache readings (matches
// headroom's 30-minute observation window).
const DefaultStaleAfter = 30 * time.Minute

// New returns a Prober wired to the local filesystem, os/exec, a redirect-refusing
// 30s HTTP client, the real codex app-server, and the real clock.
func New() *Prober {
	return &Prober{
		FS:        OSFS{},
		Exec:      osExec{},
		HTTP:      newNoRedirectClient(30 * time.Second),
		AppServer: newExecAppServer(osExec{}),
		Clock:     clock.Real{},
	}
}

// NewWith builds a Prober with explicit dependencies; any nil is defaulted to its
// local implementation. For tests and future remote wiring.
func NewWith(filesystem FS, runner ExecRunner, httpDoer HTTPDoer, appServer AppServerClient, clk clock.Clock) *Prober {
	p := &Prober{FS: filesystem, Exec: runner, HTTP: httpDoer, AppServer: appServer, Clock: clk}
	if p.FS == nil {
		p.FS = OSFS{}
	}
	if p.Exec == nil {
		p.Exec = osExec{}
	}
	if p.HTTP == nil {
		p.HTTP = newNoRedirectClient(30 * time.Second)
	}
	if p.AppServer == nil {
		p.AppServer = newExecAppServer(p.Exec)
	}
	if p.Clock == nil {
		p.Clock = clock.Real{}
	}
	return p
}

func (p *Prober) staleAfter() time.Duration {
	if p.StaleAfter > 0 {
		return p.StaleAfter
	}
	return DefaultStaleAfter
}

// newNoRedirectClient builds an *http.Client that never follows a redirect (a
// redirect would forward the Authorization bearer to the target origin).
func newNoRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}
