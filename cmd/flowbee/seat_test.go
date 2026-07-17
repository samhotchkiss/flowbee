package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/store"
)

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
