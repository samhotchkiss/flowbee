package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/capacitycollector"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

type fixedCapacityProbe struct {
	result capacitycollector.ProbeResult
	err    error
	seat   capacitycollector.Seat
}

func (p *fixedCapacityProbe) ProbeLive(_ context.Context, seat capacitycollector.Seat) (capacitycollector.ProbeResult, error) {
	p.seat = seat
	return p.result, p.err
}

func TestObserveCapacityIdentityUsesLiveV2AdapterFactsWithoutWritingBinding(t *testing.T) {
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	seat := store.Seat{ID: "|codex|/codex/a", AgentFamily: "codex", CodexHome: "/codex/a",
		ExtraEnv: map[string]string{}}
	probe := &fixedCapacityProbe{result: capacitycollector.ProbeResult{Result: &acctprobe.Result{
		Identity: acctprobe.Identity{Provider: acctprobe.ProviderCodex, AccountKey: "account-1",
			LineageDigest: "sha256-lineage", Verified: true},
		TrustState: acctprobe.TrustVerified, CapturedAt: now, Source: "codex_app_server",
	}}}
	got, err := observeCapacityIdentity(context.Background(), probe, seat, "host-local")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccountKey != "account-1" || got.CredentialLineage != "sha256-lineage" ||
		got.Source != "live_app_server" || got.TrustState != "verified" {
		t.Fatalf("observation=%+v", got)
	}
	if !probe.seat.Local || probe.seat.ConfigHome != seat.CodexHome ||
		probe.seat.ExpectedAccountKey != "" || probe.seat.ExpectedCredentialLineage != "" {
		t.Fatalf("probe seat=%+v", probe.seat)
	}
}

func TestObserveCapacityIdentityFailsClosedForCacheUnverifiedAndRemote(t *testing.T) {
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	seat := store.Seat{ID: "|grok|/grok/a", AgentFamily: "grok", ConfigDir: "/grok/a", ExtraEnv: map[string]string{}}
	probe := &fixedCapacityProbe{result: capacitycollector.ProbeResult{Result: &acctprobe.Result{
		Identity: acctprobe.Identity{Provider: acctprobe.ProviderGrok, AccountKey: "account-1",
			CredentialDigest: "lineage", Verified: false},
		TrustState: acctprobe.TrustVerifiedLocal, CapturedAt: now, Source: "/cache/usage.json",
	}}}
	if _, err := observeCapacityIdentity(context.Background(), probe, seat, "host-local"); err == nil {
		t.Fatal("cache-derived identity was accepted")
	}
	seat.Box = "remote-host"
	if _, err := observeCapacityIdentity(context.Background(), probe, seat, "remote-host"); err == nil {
		t.Fatal("remote seat was accepted through local probe")
	}
}

// ── an in-memory acctprobe.FS so discoverSeats is exercised without real ssh ──

type memFS struct {
	files map[string][]byte
	dirs  map[string][]memEntry
}

type memEntry struct {
	name string
	dir  bool
}

func (e memEntry) Name() string      { return e.name }
func (e memEntry) IsDir() bool       { return e.dir }
func (e memEntry) Type() fs.FileMode { return 0 }
func (e memEntry) Info() (fs.FileInfo, error) {
	return memInfo{name: e.name, dir: e.dir}, nil
}

type memInfo struct {
	name string
	dir  bool
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return 0 }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.dir }
func (i memInfo) Sys() any           { return nil }

func (m memFS) ReadFile(name string) ([]byte, error) {
	if b, ok := m.files[name]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}
func (m memFS) ReadDir(name string) ([]fs.DirEntry, error) {
	e, ok := m.dirs[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	out := make([]fs.DirEntry, len(e))
	for i := range e {
		out[i] = e[i]
	}
	return out, nil
}
func (m memFS) Stat(name string) (fs.FileInfo, error) {
	if b, ok := m.files[name]; ok {
		return memInfo{name: name, size: int64(len(b))}, nil
	}
	if _, ok := m.dirs[name]; ok {
		return memInfo{name: name, dir: true}, nil
	}
	return nil, os.ErrNotExist
}
func (m memFS) Open(name string) (acctprobe.File, error) {
	b, ok := m.files[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return memOpenFile{Reader: bytes.NewReader(b), size: int64(len(b))}, nil
}

type memOpenFile struct {
	*bytes.Reader
	size int64
}

func (f memOpenFile) Close() error               { return nil }
func (f memOpenFile) Stat() (fs.FileInfo, error) { return memInfo{size: f.size}, nil }

func TestDiscoverSeats(t *testing.T) {
	fetched := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	claudeJSON := `{"oauthAccount":{"accountUuid":"uuid-pearl","emailAddress":"pearl@swh.me","organizationName":"Acme"},` +
		`"cachedUsageUtilization":{"fetchedAtMs":` + itoa(fetched.UnixMilli()) + `,"utilization":{` +
		`"five_hour":{"utilization":40,"resets_at":"2026-07-16T18:00:00Z"},` +
		`"seven_day":{"utilization":55,"resets_at":"2026-07-20T00:00:00Z"}}}}`
	m := memFS{
		files: map[string][]byte{
			"/home/ops/.claude-pearl/.claude.json": []byte(claudeJSON),
		},
		dirs: map[string][]memEntry{
			"/home/ops":               {{name: ".claude-pearl", dir: true}, {name: ".codex", dir: true}},
			"/home/ops/.claude-pearl": {{name: ".claude.json"}},
			"/home/ops/.codex":        {}, // present but no auth.json -> no codex identity
		},
	}
	p := acctprobe.NewWith(m, nil, nil, nil, clock.NewFake(fetched))

	proposals := discoverSeats(p, "buncher", "/home/ops")
	if len(proposals) != 1 {
		t.Fatalf("expected 1 claude proposal, got %d: %+v", len(proposals), proposals)
	}
	pr := proposals[0]
	if pr.seat.AgentFamily != "claude" || pr.seat.AccountKey != "uuid-pearl" || pr.seat.ConfigDir != "/home/ops/.claude-pearl" {
		t.Fatalf("unexpected proposal seat: %+v", pr.seat)
	}
	if pr.email != "pearl@swh.me" {
		t.Fatalf("email: %q", pr.email)
	}
	if pr.health != store.SeatReady {
		t.Fatalf("a fresh, headroom account should read ready, got %q", pr.health)
	}
}

func TestClassifySeatHealth(t *testing.T) {
	// probe error -> unreachable
	if h, _ := classifySeatHealth(nil, errors.New("ssh: connection refused")); h != store.SeatUnreachable {
		t.Fatalf("error -> %q, want unreachable", h)
	}
	// a hold with an auth reason -> auth_dead
	authErr := &acctprobe.HoldError{Reason: acctprobe.ReasonTokenRejected}
	if h, _ := classifySeatHealth(nil, authErr); h != store.SeatAuthDead {
		t.Fatalf("token_rejected -> %q, want auth_dead", h)
	}
	// a resolved account with a critical window -> limit_critical
	crit := &acctprobe.Result{
		Identity:   acctprobe.Identity{AccountKey: "a"},
		TrustState: acctprobe.TrustVerifiedLocal,
		Usage:      acctprobe.Usage{Windows: acctprobe.Windows{{Kind: acctprobe.KindWeeklyAll, Percent: 100, Severity: acctprobe.SeverityCritical}}},
	}
	if h, _ := classifySeatHealth(crit, nil); h != store.SeatLimitCritical {
		t.Fatalf("critical -> %q, want limit_critical", h)
	}
	// a resolved, non-critical account -> ready
	ok := &acctprobe.Result{Identity: acctprobe.Identity{AccountKey: "a"}, TrustState: acctprobe.TrustVerifiedLocal,
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{{Kind: acctprobe.KindWeeklyAll, Percent: 30}}}}
	if h, _ := classifySeatHealth(ok, nil); h != store.SeatReady {
		t.Fatalf("healthy -> %q, want ready", h)
	}
	// a held reading with an identity-missing reason -> auth_dead
	held := &acctprobe.Result{TrustState: acctprobe.TrustHeld, Hold: acctprobe.ReasonTokenExpired}
	if h, _ := classifySeatHealth(held, nil); h != store.SeatAuthDead {
		t.Fatalf("held token_expired -> %q, want auth_dead", h)
	}
}

func TestEnvFlagParsing(t *testing.T) {
	e := envFlag{}
	if err := e.Set("FOO=bar"); err != nil || e["FOO"] != "bar" {
		t.Fatalf("set FOO=bar: %v %v", err, e)
	}
	if err := e.Set("noeq"); err == nil {
		t.Fatal("expected an error for a non KEY=VALUE flag")
	}
}

func TestSeatBindCapacityPersistsOperatorExpectation(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	seat := store.Seat{AgentFamily: "codex", CodexHome: "/opt/codex-a"}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	if err := runSeatBindCapacity(ctx, st, []string{
		"--family", "codex", "--codex-home", "/opt/codex-a",
		"--host-id", "control-host", "--account-key", "account-a",
		"--credential-lineage", "sha256:lineage-a", "--reserve-pct", "12.5",
		"--account-max-concurrent", "3",
	}); err != nil {
		t.Fatal(err)
	}
	var host, account, lineage string
	var reserve float64
	var maximum int
	if err := st.DB.QueryRowContext(ctx, `SELECT expected_host_id,expected_account_key,
		expected_credential_lineage,capacity_reserve_pct,account_max_concurrent
		FROM seats WHERE id=?`, seat.ComposeID()).Scan(&host, &account, &lineage, &reserve, &maximum); err != nil {
		t.Fatal(err)
	}
	if host != "control-host" || account != "account-a" || lineage != "sha256:lineage-a" || reserve != 12.5 || maximum != 3 {
		t.Fatalf("binding host=%q account=%q lineage=%q reserve=%v maximum=%d", host, account, lineage, reserve, maximum)
	}
}

func TestSeatBindDriverPersistsOnlyInventoryTargetNotSessionIdentity(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 12, 30, 0, 0, time.UTC)
	seat := store.Seat{Box: "stable-host", AgentFamily: "codex", CodexHome: "/opt/codex-a"}
	if err := st.AddSeat(ctx, seat, now); err != nil {
		t.Fatal(err)
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err := st.DB.ExecContext(ctx, `INSERT INTO driver_instances
		(instance_ref,host_id,store_id,producer_boot_id,tmux_server_domain_id,tmux_server_ownership,state,created_at,updated_at)
		VALUES ('driver-a','stable-host','store-a','boot-a','flowbee','managed_dedicated','live',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := runSeatBindDriver(ctx, st, []string{"--box", "stable-host", "--family", "codex",
		"--codex-home", "/opt/codex-a", "--instance-ref", "driver-a",
		"--tmux-server-domain-id", "flowbee",
		"--tmux-server-instance-id", "server-a", "--profile-id", "codex-builder",
		"--workspace-root-id", "repos", "--workspace-relative-base", "worktrees"}); err != nil {
		t.Fatal(err)
	}
	var instance, domain, server, profile, root, base string
	if err := st.DB.QueryRowContext(ctx, `SELECT instance_ref,tmux_server_domain_id,tmux_server_instance_id,
		profile_id,workspace_root_id,workspace_relative_base FROM builder_driver_targets
		WHERE project_id='default' AND seat_id=?`, seat.ComposeID()).Scan(&instance, &domain, &server,
		&profile, &root, &base); err != nil {
		t.Fatal(err)
	}
	if instance != "driver-a" || domain != "flowbee" || server != "server-a" || profile != "codex-builder" ||
		root != "repos" || base != "worktrees" {
		t.Fatalf("target=%q/%q/%q/%q/%q/%q", instance, domain, server, profile, root, base)
	}
	var sessions int
	_ = st.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM driver_session_bindings`).Scan(&sessions)
	if sessions != 0 {
		t.Fatalf("operator target registration injected %d session identities", sessions)
	}
}

func TestShquoteAndBoxSafety(t *testing.T) {
	if shquote("a'b") != `'a'\''b'` {
		t.Fatalf("shquote: %q", shquote("a'b"))
	}
	if err := assertBoxArgvSafe("-oProxyCommand=x"); err == nil {
		t.Fatal("expected a leading-dash box to be rejected")
	}
	if err := assertBoxArgvSafe("buncher"); err != nil {
		t.Fatalf("a normal box name should pass: %v", err)
	}
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
