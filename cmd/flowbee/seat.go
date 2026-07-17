package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/clock"
	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runSeat is the `flowbee seat <add|list|probe|discover>` CLI: the SEAT registry (plan
// §15.13, 0028_epic_capacity.sql) — where each account is already logged-in-and-usable
// on a box. Store-direct like `flowbee host` (pure registry CRUD, no serve round-trip).
// Flowbee NEVER logs in; discovery only READS what a human already authenticated, and
// this command NEVER prints a token or secret value (acctprobe's parsers only surface
// non-secret identity + server percentages).
func runSeat(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee seat <add|list|probe|discover> ...")
	}
	sub, rest := args[0], args[1:]

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	switch sub {
	case "add":
		return runSeatAdd(ctx, st, rest)
	case "list", "ls":
		return runSeatList(ctx, st, rest)
	case "probe":
		return runSeatProbe(ctx, st, rest)
	case "discover":
		return runSeatDiscover(ctx, st, rest)
	default:
		return fmt.Errorf("unknown `flowbee seat` subcommand %q (want add|list|probe|discover)", sub)
	}
}

// envFlag collects repeatable --env KEY=VAL flags into a map.
type envFlag map[string]string

func (e envFlag) String() string { return "" }
func (e envFlag) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return fmt.Errorf("--env must be KEY=VALUE, got %q", v)
	}
	e[k] = val
	return nil
}

func runSeatAdd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("seat add", flag.ContinueOnError)
	box := fs.String("box", "", "registered host / ssh destination ('' = control-plane box)")
	family := fs.String("family", "", "agent family: claude|codex (required)")
	configDir := fs.String("config-dir", "", "CLAUDE_CONFIG_DIR (claude seats)")
	codexHome := fs.String("codex-home", "", "CODEX_HOME (codex seats)")
	account := fs.String("account-key", "", "account_windows.account_key (optional; a probe resolves it)")
	env := envFlag{}
	fs.Var(env, "env", "extra launch env KEY=VALUE (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seat := store.Seat{
		Box: *box, AgentFamily: *family, AccountKey: *account,
		ConfigDir: *configDir, CodexHome: *codexHome, ExtraEnv: map[string]string(env),
	}
	if err := st.AddSeat(ctx, seat, time.Now()); err != nil {
		if errors.Is(err, store.ErrSeatExists) {
			return fmt.Errorf("a seat for this box+dir is already registered")
		}
		return err
	}
	fmt.Printf("✓ registered %s seat on box %q (%s)\n", *family, boxLabel(*box), seatDirOf(seat))
	return nil
}

func runSeatList(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("seat list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	seats, err := st.ListSeats(ctx)
	if err != nil {
		return err
	}
	printSeatList(os.Stdout, seats)
	return nil
}

func printSeatList(w io.Writer, seats []store.Seat) {
	if len(seats) == 0 {
		fmt.Fprintln(w, "no seats registered (flowbee seat add --box <b> --family <claude|codex> --config-dir/--codex-home <dir>)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BOX\tFAMILY\tDIR\tACCOUNT\tHEALTH\tPROBED")
	for _, s := range seats {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			boxLabel(s.Box), s.AgentFamily, seatDirOf(s), dashIfEmpty(s.AccountKey),
			s.Health, dashIfEmpty(s.LastProbeAt))
	}
	tw.Flush() //nolint:errcheck
}

// runSeatProbe probes each registered seat's identity + cached usage (local FS for a
// local seat, read-only ssh for a remote seat), folds the reading into account_windows,
// and sets the seat's health. It is a LIGHTWEIGHT reachability + logged-in check; the
// authoritative live headroom is filled by the consolidated capacity ticker (Phase 6b).
func runSeatProbe(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("seat probe", flag.ContinueOnError)
	only := fs.String("box", "", "probe only seats on this box")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seats, err := st.ListSeats(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "BOX\tFAMILY\tDIR\tACCOUNT\tHEALTH\tDETAIL")
	for _, s := range seats {
		if *only != "" && s.Box != *only {
			continue
		}
		res, perr := probeSeatDir(s)
		health, detail := classifySeatHealth(res, perr)
		if res != nil && res.Identity.AccountKey != "" {
			// fold identity + whatever usage the reading carried (respecting trust).
			if uerr := st.UpsertAccountLimits(ctx, *res, now); uerr != nil {
				detail = "fold failed: " + uerr.Error()
			}
			if s.AccountKey == "" {
				_ = st.SetSeatAccountKey(ctx, s.ID, res.Identity.AccountKey, now)
			}
		}
		if uerr := st.UpdateSeatHealth(ctx, s.ID, health, detail, now); uerr != nil {
			return uerr
		}
		acct := dashIfEmpty(s.AccountKey)
		if res != nil && res.Identity.AccountKey != "" {
			acct = res.Identity.AccountKey
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", boxLabel(s.Box), s.AgentFamily, seatDirOf(s), acct, health, dashIfEmpty(detail))
	}
	tw.Flush() //nolint:errcheck
	return nil
}

// runSeatDiscover scans a box's home over READ-ONLY ssh for logged-in agent config
// dirs (claude .claude* dirs + the default ~/.codex), resolves each account's
// identity + cached usage via acctprobe's parsers fed by an ssh-backed FS, and proposes
// seats. --yes registers all proposals (skipping dups) and folds their readings.
func runSeatDiscover(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("seat discover", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "register all proposed seats without prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: flowbee seat discover <box> [--yes]")
	}
	box := fs.Arg(0)
	if err := assertBoxArgvSafe(box); err != nil {
		return err
	}

	rr := &remoteRunner{box: box, timeout: 15 * time.Second}
	home, err := rr.home()
	if err != nil {
		return fmt.Errorf("resolve remote home on %q: %w", box, err)
	}
	prober := acctprobe.NewWith(sshFS{rr: rr}, nil, nil, nil, clock.Real{})

	proposals := discoverSeats(prober, box, home)
	if len(proposals) == 0 {
		fmt.Printf("no logged-in agent config dirs found under %s on %q\n", home, box)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FAMILY\tDIR\tACCOUNT\tEMAIL\tHEALTH")
	for _, pr := range proposals {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", pr.seat.AgentFamily, seatDirOf(pr.seat),
			dashIfEmpty(pr.seat.AccountKey), dashIfEmpty(pr.email), pr.health)
	}
	tw.Flush() //nolint:errcheck

	if !*yes {
		fmt.Printf("\n%d seat(s) proposed. Re-run with --yes to register them.\n", len(proposals))
		return nil
	}
	now := time.Now()
	registered := 0
	for _, pr := range proposals {
		if err := st.AddSeat(ctx, pr.seat, now); err != nil {
			if errors.Is(err, store.ErrSeatExists) {
				continue // already registered — idempotent accept
			}
			return err
		}
		registered++
		if pr.result != nil && pr.result.Identity.AccountKey != "" {
			_ = st.UpsertAccountLimits(ctx, *pr.result, now)
		}
	}
	fmt.Printf("✓ registered %d new seat(s) on %q\n", registered, box)
	return nil
}

// seatProposal is one discovered seat plus its probe reading (for the confirmation view
// and, on --yes, the account_windows fold).
type seatProposal struct {
	seat   store.Seat
	result *acctprobe.Result
	email  string
	health string
}

// discoverSeats probes each claude config dir + the default codex home under a box's
// home and builds seat proposals from the readings that resolved an account identity.
func discoverSeats(p *acctprobe.Prober, box, home string) []seatProposal {
	var out []seatProposal
	if dirs, err := p.DiscoverClaudeDirs(home); err == nil {
		for _, dir := range dirs {
			res, perr := p.ProbeClaudeDir(dir)
			if res == nil || res.Identity.AccountKey == "" {
				continue
			}
			health, _ := classifySeatHealth(res, perr)
			out = append(out, seatProposal{
				seat:   store.Seat{Box: box, AgentFamily: "claude", AccountKey: res.Identity.AccountKey, ConfigDir: dir},
				result: res, email: res.Identity.Email, health: health,
			})
		}
	}
	// the default codex home (CODEX_HOME is env-scoped; discovery checks the convention).
	codexHome := filepath.Join(home, ".codex")
	if res, perr := p.ProbeCodexHome(codexHome); res != nil && res.Identity.AccountKey != "" {
		health, _ := classifySeatHealth(res, perr)
		out = append(out, seatProposal{
			seat:   store.Seat{Box: box, AgentFamily: "codex", AccountKey: res.Identity.AccountKey, CodexHome: codexHome},
			result: res, email: res.Identity.Email, health: health,
		})
	}
	return out
}

// probeSeatDir probes a registered seat's config dir/codex home for identity + cached
// usage, using the local FS for a local seat and read-only ssh for a remote one.
func probeSeatDir(s store.Seat) (*acctprobe.Result, error) {
	var p *acctprobe.Prober
	if s.Box == "" {
		p = acctprobe.New()
	} else {
		p = acctprobe.NewWith(sshFS{rr: &remoteRunner{box: s.Box, timeout: 15 * time.Second}}, nil, nil, nil, clock.Real{})
	}
	if s.AgentFamily == "codex" {
		return p.ProbeCodexHome(s.CodexHome)
	}
	return p.ProbeClaudeDir(s.ConfigDir)
}

// classifySeatHealth maps an acctprobe reading to a seat health (plan §15.13a). A probe
// error → unreachable; a login-gone hold → auth_dead; a non-stale critical window →
// limit_critical; a resolved, logged-in account → ready.
func classifySeatHealth(res *acctprobe.Result, err error) (health, detail string) {
	if err != nil || res == nil {
		var he *acctprobe.HoldError
		if errors.As(err, &he) && isAuthDeadReason(he.Reason) {
			return store.SeatAuthDead, string(he.Reason)
		}
		return store.SeatUnreachable, probeErrText(err)
	}
	if res.TrustState == acctprobe.TrustHeld && isAuthDeadReason(res.Hold) {
		return store.SeatAuthDead, string(res.Hold)
	}
	if res.Identity.AccountKey == "" {
		return store.SeatAuthDead, "no account identity on disk (re-login required)"
	}
	if res.Usage.Windows.Critical() {
		return store.SeatLimitCritical, "server reports a critical window"
	}
	return store.SeatReady, string(res.TrustState)
}

// isAuthDeadReason reports whether a hold reason means the human must re-login (distinct
// from a wait-for-reset limit) — the §12.4 auth-dead axis.
func isAuthDeadReason(r acctprobe.HoldReason) bool {
	switch r {
	case acctprobe.ReasonTokenExpired, acctprobe.ReasonTokenRejected,
		acctprobe.ReasonCredentialsMissing, acctprobe.ReasonIdentityMissing,
		acctprobe.ReasonAppServerAuth:
		return true
	}
	return false
}

func probeErrText(err error) string {
	if err == nil {
		return "unreachable"
	}
	return err.Error()
}

func boxLabel(box string) string {
	if box == "" {
		return "(local)"
	}
	return box
}

func seatDirOf(s store.Seat) string {
	if s.AgentFamily == "codex" {
		return s.CodexHome
	}
	return s.ConfigDir
}

// ── read-only ssh transport for acctprobe's FS abstraction ──
//
// acctprobe's FS is injected over ABSOLUTE paths; this ssh-backed implementation lets
// its parsers run against a remote box without touching the probe logic. Every command
// is a READ-ONLY primitive (cat / ls / test) built from a CLOSED template with the box
// and path shell-quoted and a `--` end-of-options guard; nothing is ever executed from a
// file's contents, and no secret is printed (acctprobe parses only non-secret fields).

type remoteRunner struct {
	box     string
	timeout time.Duration
}

// output runs a read-only inner command on the box over ssh, returning stdout only
// (stderr is dropped so an error message can never corrupt parsed file content).
func (r *remoteRunner) output(inner string) ([]byte, error) {
	cmd := "ssh -o BatchMode=yes -o ConnectTimeout=8 -- " + shquote(r.box) + " " + shquote(inner)
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = io.Discard
	err := c.Run()
	return out.Bytes(), err
}

func (r *remoteRunner) home() (string, error) {
	out, err := r.output("printf %s \"$HOME\"")
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", errors.New("empty $HOME")
	}
	return home, nil
}

// sshFS satisfies acctprobe.FS over read-only ssh.
type sshFS struct{ rr *remoteRunner }

func (f sshFS) ReadFile(name string) ([]byte, error) {
	// test-then-cat so a missing file maps to os.ErrNotExist (acctprobe distinguishes it
	// from a parse error to drive its legacy-file fallback).
	out, err := f.rr.output("test -f " + shquote(name) + " && cat -- " + shquote(name))
	if err != nil {
		return nil, notFound(name)
	}
	return out, nil
}

func (f sshFS) ReadDir(name string) ([]fs.DirEntry, error) {
	out, err := f.rr.output("ls -1Ap -- " + shquote(name) + " 2>/dev/null")
	if err != nil {
		return nil, notFound(name)
	}
	var entries []fs.DirEntry
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		isDir := strings.HasSuffix(line, "/")
		entries = append(entries, sshDirEntry{name: strings.TrimSuffix(line, "/"), dir: isDir})
	}
	return entries, nil
}

func (f sshFS) Stat(name string) (fs.FileInfo, error) {
	out, err := f.rr.output("test -e " + shquote(name) + " && echo Y")
	if err != nil || strings.TrimSpace(string(out)) != "Y" {
		return nil, notFound(name)
	}
	return sshFileInfo{name: filepath.Base(name)}, nil
}

func (f sshFS) Open(name string) (acctprobe.File, error) {
	b, err := f.ReadFile(name)
	if err != nil {
		return nil, err
	}
	return &memFile{Reader: bytes.NewReader(b), size: int64(len(b)), name: filepath.Base(name)}, nil
}

func notFound(name string) error {
	return fmt.Errorf("%s: %w", name, os.ErrNotExist)
}

type sshDirEntry struct {
	name string
	dir  bool
}

func (e sshDirEntry) Name() string { return e.name }
func (e sshDirEntry) IsDir() bool  { return e.dir }
func (e sshDirEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e sshDirEntry) Info() (fs.FileInfo, error) { return sshFileInfo{name: e.name, dir: e.dir}, nil }

type sshFileInfo struct {
	name string
	dir  bool
	size int64
}

func (i sshFileInfo) Name() string { return i.name }
func (i sshFileInfo) Size() int64  { return i.size }
func (i sshFileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir
	}
	return 0
}
func (i sshFileInfo) ModTime() time.Time { return time.Time{} }
func (i sshFileInfo) IsDir() bool        { return i.dir }
func (i sshFileInfo) Sys() any           { return nil }

// memFile is an in-memory acctprobe.File (a whole remote file slurped into memory, then
// exposed as a seekable reader) — the rollout tail's Seek works against the memory copy.
type memFile struct {
	*bytes.Reader
	size int64
	name string
}

func (m *memFile) Close() error               { return nil }
func (m *memFile) Stat() (fs.FileInfo, error) { return sshFileInfo{name: m.name, size: m.size}, nil }

// assertBoxArgvSafe rejects a box that could subvert the ssh argv (a leading '-' read as
// an option, or whitespace/control splitting argv) — the same posture store.AddSeat and
// AddEpicHost apply, applied here for the discover target which is not yet registered.
func assertBoxArgvSafe(box string) error {
	if box == "" {
		return errors.New("discover requires a box (ssh destination)")
	}
	if strings.HasPrefix(box, "-") {
		return fmt.Errorf("box %q must not start with '-'", box)
	}
	for _, r := range box {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("box %q must not contain whitespace or control characters", box)
		}
	}
	return nil
}

// shquote single-quotes s for safe embedding in an `sh -c` string (POSIX total quoting).
func shquote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// (compile-time assertions that the ssh FS satisfies the acctprobe interfaces.)
var (
	_ acctprobe.FS   = sshFS{}
	_ acctprobe.File = (*memFile)(nil)
	_ fs.DirEntry    = sshDirEntry{}
)
