package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/epicspec"
	"github.com/samhotchkiss/flowbee/internal/gitops"
	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/tmuxio"
	"github.com/samhotchkiss/flowbee/internal/ulid"
	"github.com/samhotchkiss/flowbee/internal/watchdog"
)

// runEpic is the `flowbee epic <start|status|abandon>` CLI (epic-lane Phase 2).
// Talks directly to the local control-plane DB and control-plane mirror on disk —
// same posture as `flowbee session`/`flowbee host`: pure local reads/writes for
// status+abandon, and for `start` a series of git-mirror reads + ssh/tmux calls
// that need no serve process running (the mirror is just files on disk; `start`
// provisions/reads it exactly like serve's own mirror-refresh loop does).
func runEpic(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee epic <start|status|abandon> ...")
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
	case "start":
		return runEpicStart(ctx, st, rest)
	case "status":
		return runEpicStatus(ctx, st, rest)
	case "abandon":
		return runEpicAbandon(ctx, st, rest)
	case "digest":
		return runEpicDigest(rest)
	case "plan":
		return runEpicPlan(ctx, st, rest)
	default:
		return fmt.Errorf("unknown `flowbee epic` subcommand %q (want start|status|abandon|digest|plan)", sub)
	}
}

// safeSlugRe gates any epic-derived string (the slug, used unquoted-adjacent in
// several remote-shell command arguments after being embedded in paths like
// "<home>/epics/<slug>") to a conservative safe character set BEFORE it is ever
// used to build a command — defense in depth on top of shQuote (internal/watchdog),
// matching store.validateArgvSafe's posture for goal_sessions box/tmux_name.
var safeSlugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// safeAgentRe gates the launch agent's name (review MAJOR M2). This value can
// come FROM THE EPIC FILE (frontmatter `agent:`) and becomes the tmux session's
// shell-executed start command on the target box — without this gate, a committed
// `agent: "codex; curl …|sh"` is remote code execution on the host at launch time
// (shQuote does not help: the string is EXECUTED by a shell as tmux's session
// command, not merely passed as an inert argument). The charset is a strict
// binary-name allowshape: letters/digits/._- only — no spaces (so no arguments:
// an agent needing flags gets a wrapper script on the box), no separators, no
// path slashes (the agent must be on the box's PATH; a full path would also
// smuggle "/" past review too easily). Mirrors safeSlugRe/validateArgvSafe.
var safeAgentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validateAgent enforces safeAgentRe (see its doc — review M2) as the single seam
// every launch path must pass the resolved agent through before it can become a
// shell-executed tmux start command.
func validateAgent(agent string) error {
	if !safeAgentRe.MatchString(agent) {
		return fmt.Errorf("agent %q has characters outside [A-Za-z0-9._-] — refusing to shell-execute it on the host (an agent needing arguments gets a wrapper script on the box)", agent)
	}
	return nil
}

// deriveSlug extracts the epic id from its file path (author-epic/SKILL.md:
// "epics/YYYY-MM-DD-<slug>.md" -> the filename minus ".md"). The FULL filename
// stem is the slug (including the date prefix) — matches store.EpicRun.ID's doc
// ("slug parsed off the filename").
func deriveSlug(filePath string) (string, error) {
	base := path.Base(filePath) // repo-relative paths are always "/"-separated (git), not OS paths
	slug := strings.TrimSuffix(base, ".md")
	if slug == base || slug == "" {
		return "", fmt.Errorf("epic file %q must be a .md file", filePath)
	}
	if !safeSlugRe.MatchString(slug) {
		return "", fmt.Errorf("epic slug %q (from %q) has characters outside [A-Za-z0-9._-] — unsafe to use in a remote command", slug, filePath)
	}
	return slug, nil
}

func runEpicStart(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("epic start", flag.ContinueOnError)
	hostFlag := fs.String("host", "", "restrict seat selection to this box (an override that must name a registered seat's box; §15.13)")
	tzFlag := fs.String("tz", "", "the host's IANA timezone (default: probe the box via `date`/`timedatectl`, else assume serve-local — mirrors `flowbee session add --tz`)")
	agentFlag := fs.String("agent", "", "coding agent family to launch (overrides the epic file's frontmatter agent:; claude|codex, default codex)")
	planFlag := fs.Bool("plan", false, "dry-run: validate step count + scope-overlap + selected-seat headroom, do NOT launch (§5f/§10c)")
	forceQuota := fs.Bool("force-quota", false, "launch even if no seat has weekly headroom (bypasses the severity/seat-health gate — use sparingly)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: flowbee epic start <repo> <epics/....md> [--host <box>] [--tz <iana>] [--agent claude|codex] [--plan] [--force-quota]")
	}
	repoID, filePath := fs.Arg(0), fs.Arg(1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	repo, err := st.GetRepo(ctx, repoID)
	if err != nil {
		if errors.Is(err, store.ErrRepoNotFound) {
			return fmt.Errorf("no such repo %q (see `flowbee repo list`)", repoID)
		}
		return err
	}

	// read the epic file off the CONTROL-PLANE MIRROR's tracked main — never an ad-hoc
	// clone (§ task brief). controlMirrorFor/ensureRepoMirror/repoTokenURL are the
	// exact same helpers serve.go's 45s mirror-refresh loop uses, reused verbatim so
	// `epic start` never has a second, divergent notion of "where is this repo's mirror".
	mp := controlMirrorFor(repo)
	if mp == "" {
		return fmt.Errorf("no control-plane mirror configured (FLOWBEE_MIRROR_PATH unset) — start `flowbee serve` at least once to provision it")
	}
	ensureRepoMirror(logger, mp, repoTokenURL(repo))
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	mirror := gitops.Open(mp)
	if err := mirror.FetchBranch(branch); err != nil {
		return fmt.Errorf("fetch %s's %s from origin: %w", repoID, branch, err)
	}
	content, ok, err := mirror.ReadFileAtRef("refs/heads/"+branch, filePath)
	if err != nil {
		return fmt.Errorf("read %s at %s: %w", filePath, branch, err)
	}
	if !ok {
		return fmt.Errorf("%s not found on %s's %s (origin/%s) — commit it to main before launching (author-epic/SKILL.md)", filePath, repoID, branch, branch)
	}

	spec, err := epicspec.ParseSpec(content)
	if err != nil {
		return fmt.Errorf("parse %s: %w", filePath, err)
	}
	slug, err := deriveSlug(filePath)
	if err != nil {
		return err
	}
	if _, err := st.GetEpicRun(ctx, slug); err == nil {
		return fmt.Errorf("epic %q is already registered (see `flowbee epic status`)", slug)
	} else if !errors.Is(err, store.ErrEpicRunNotFound) {
		return err
	}

	// ── scope reservation (blast-radius overlap, same repo only) — fast feedback; the
	// authoritative, race-free check runs inside store.AddEpicRun's tx (review m6). ──
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil {
		return err
	}
	for _, e := range active {
		if e.Repo != repoID {
			continue
		}
		if overlaps, ga, gb := epicspec.ScopeOverlap(spec.Scope, e.Scope); overlaps {
			return fmt.Errorf("scope %q overlaps active epic %q's scope %q in repo %q — narrow scope: or wait for it to finish", ga, e.ID, gb, repoID)
		}
	}

	// ── agent FAMILY resolution (the seat registry provisions a session of this family;
	// it also drives cross-family review). Seats are keyed on claude|codex. ──
	family := *agentFlag
	if family == "" {
		family = spec.Agent
	}
	if family == "" {
		family = "codex" // author-epic/SKILL.md's documented default coding agent
	}
	if err := validateAgent(family); err != nil {
		return err
	}

	// ── SEAT SELECTION = launch (plan §15.13): pick a ready seat of the epic's family with
	// weekly headroom, reading account_windows.severity + seat HEALTH (NOT just
	// worker_accounts.usage_pct — a weekly_scoped-only critical is invisible there, per the
	// 0028 capacity-store note). Anti-collocation prefers a seat not already powering an
	// active epic. Refuses on no-seat / no-headroom; fails open on a probe error / stale
	// critical (§4.4/§12.14 — a flaky ssh must not ground the fleet). ──
	seat, gate, serr := epicSelectSeat(ctx, st, family, *hostFlag, active)
	if serr != nil {
		return serr
	}
	// --force-quota bypasses ONLY the headroom/severity refusal — never a hard no-seat (m2):
	// launching with no seat would come up on the box's DEFAULT account with an empty
	// builder_model, breaking the cross-family review handoff.
	if gate.hardNoSeat {
		return fmt.Errorf("launch refused: %s", gate.reason)
	}
	if !*forceQuota && gate.refuse {
		return fmt.Errorf("launch refused: %s (pass --force-quota to override)", gate.reason)
	}

	// ── step-count advisory (§10c: warn > ~12, never refuse) ──
	if n := len(spec.Steps); n > 12 {
		fmt.Printf("⚠ %d steps declared (> ~12) — consider splitting this epic (§10 sizing); launching anyway.\n", n)
	}

	if *planFlag {
		fmt.Printf("plan (dry-run) for epic %q in repo %q:\n", slug, repoID)
		fmt.Printf("  steps declared: %d\n", len(spec.Steps))
		fmt.Printf("  scope-overlap:  OK (disjoint from every active epic)\n")
		if gate.refuse {
			fmt.Printf("  seat/headroom:  REFUSE — %s\n", gate.reason)
		} else {
			fmt.Printf("  seat/headroom:  OK — seat %q (family %s, box %q, account %q)%s\n",
				seat.ID, seat.AgentFamily, dashIfEmpty(seat.Box), dashIfEmpty(seat.AccountKey), gateNote(gate))
		}
		fmt.Println("  (dry-run — nothing launched)")
		return nil
	}

	builderFamily := seat.AgentFamily
	tz := *tzFlag
	if tz != "" {
		if _, lerr := time.LoadLocation(tz); lerr != nil {
			return fmt.Errorf("invalid --tz %q (want an IANA name like America/Denver): %w", tz, lerr)
		}
	}

	// ── register (state=launching) BEFORE the launch ladder, so a crash mid-launch leaves a
	// VISIBLE half-launched row the launching-reaper (supervision ticker) releases — not a
	// silently-stranded host reservation. ──
	now := time.Now()
	tmuxName := "epic-" + slug
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: slug, Repo: repoID, FilePath: filePath, Title: spec.Title, Scope: spec.Scope,
		Host: seat.Box, Branch: "epic/" + slug, TmuxName: tmuxName, Agent: family,
	}, now); err != nil {
		return fmt.Errorf("register epic: %w", err)
	}
	// bind the seat/account/builder_model_family from the RESOLVED seat (not config intent —
	// this drives the completion-triggered cross-family review handoff, plan §11).
	if err := st.SetEpicSeatBinding(ctx, slug, seat.AccountKey, seat.ID, builderFamily, now); err != nil {
		logger.Warn("epic registered but failed to bind its seat/account (review handoff may pick the wrong family)", "epic", slug, "err", err)
	}

	// ── THE LAUNCH LADDER (plan §15.15): drive a LOCAL tmux session through the staged,
	// pane-classified launch machine, composed from tmuxio primitives + the family verb
	// table. The local pane is the attach; a remote seat lives in a remote tmux the ssh
	// line creates. ──
	client := tmuxio.New()
	res, lerr := watchdog.RunLadder(ctx, client, realTmuxClock{}, watchdog.LadderParams{
		Slug:     slug,
		SpecPath: filePath,
		Seat: watchdog.LaunchSeat{
			Box: seat.Box, AgentFamily: seat.AgentFamily, ConfigDir: seat.ConfigDir,
			CodexHome: seat.CodexHome, Account: seat.AccountKey, ExtraEnv: seat.ExtraEnv,
		},
	})
	if lerr != nil {
		raiseLaunchFailed(ctx, st, slug, repoID, "ladder infra error at stage "+string(res.Stage)+": "+lerr.Error(), now)
		_ = st.DeleteEpicRun(ctx, slug)
		return fmt.Errorf("launch ladder infra error at stage %q: %w (epic registration rolled back)", res.Stage, lerr)
	}
	switch res.Outcome {
	case watchdog.LaunchAwaitingAuth:
		// interactive auth at the ssh stage — a HUMAN answers once in the LOCAL pane. Leave
		// the session + the 'launching' row alive; raise a human-facing item. The reaper
		// releases it only if the human never completes it (LaunchStrandAfter).
		raiseLaunchFailed(ctx, st, slug, repoID, "awaiting interactive auth: "+res.Evidence, now)
		fmt.Printf("⏸ epic %q launch is AWAITING INTERACTIVE AUTH in local tmux %q — answer the prompt in the pane, then it will proceed.\n   %s\n", slug, res.Session, res.Evidence)
		return nil
	case watchdog.LaunchFailed:
		raiseLaunchFailed(ctx, st, slug, repoID, "launch ladder failed at stage "+string(res.Stage)+": "+res.Evidence, now)
		_ = st.DeleteEpicRun(ctx, slug) // roll back — the ladder already killed the local session
		return fmt.Errorf("launch ladder failed at stage %q: %s (epic registration rolled back — nothing reserved)", res.Stage, res.Evidence)
	}

	// LaunchVerified: the CLI is up and the pane classified WORKING. Register the goal-
	// session watch (box records the seat box for the §15.15 dual-path fallback) and mark
	// the epic running. A goal-session registration failure must NOT roll back a live agent.
	if err := st.AddGoalSession(ctx, store.GoalSession{
		ID: tmuxName, Box: seat.Box, TmuxName: tmuxName, TZ: tz, Repo: repoID, Note: "epic: " + spec.Title,
	}, now); err != nil {
		logger.Error("epic launched but failed to register its goal-session watch — the watchdog will NOT observe it until this is fixed", "epic", slug, "err", err)
	}
	if err := st.MarkEpicLaunched(ctx, slug, now); err != nil {
		return fmt.Errorf("epic launched but failed to mark it running in the registry: %w", err)
	}
	fmt.Printf("✓ launched epic %q (seat %q, family %s, tmux %q, branch epic/%s) — run `flowbee epic status` to confirm\n",
		slug, seat.ID, builderFamily, res.Session, slug)
	return nil
}

// raiseLaunchFailed upserts a launch_failed attention item (plan §1.3) so a master/operator
// routes the retry/reassign. Best-effort — a failed producer must not mask the launch error.
func raiseLaunchFailed(ctx context.Context, st *store.Store, slug, repo, detail string, now time.Time) {
	_, _, _ = st.UpsertAttentionItem(ctx, store.AttentionItem{
		ID: ulid.New(), Kind: "launch_failed", EpicID: slug, Repo: repo, Priority: 10,
		DedupKey: slug + ":launch_failed", Blocking: true,
		Evidence: map[string]string{"detail": detail},
	}, now)
}

// seatGate is epicSelectSeat's headroom verdict — refuse (no-seat / no-headroom) with a
// reason, or an OK seat possibly carrying a fail-open warning (a probe error / stale
// critical is NOT a hard refusal, §4.4/§12.14). hardNoSeat marks the ONE refusal
// --force-quota must NEVER bypass: there is no ready seat of the family at all, so a forced
// launch would come up on the box's DEFAULT account (wrong-account launch) with an empty
// builder_model that breaks the §5b cross-family review handoff (m2). Only the
// headroom/severity refusal is a "you know better, override it" call.
type seatGate struct {
	refuse     bool
	hardNoSeat bool
	reason     string
	warning    string
}

func gateNote(g seatGate) string {
	if g.warning != "" {
		return " (warning: " + g.warning + ")"
	}
	return ""
}

// epicSelectSeat picks a ready seat of the epic's family with weekly headroom (plan §15.13c
// + §4.3), reading account_windows.severity + seat HEALTH — NOT just worker_accounts.usage_pct
// (a weekly_scoped-only critical is invisible there; the 0028 capacity-store note requires
// this). Anti-collocation prefers a seat whose account is not already powering an active
// epic. hostFilter (if set) restricts to seats on that box. Refuses on no-seat / no-headroom;
// a probe error / stale critical fails OPEN (a flaky ssh must not ground the fleet).
func epicSelectSeat(ctx context.Context, st *store.Store, family, hostFilter string, active []store.EpicRun) (store.Seat, seatGate, error) {
	seats, err := st.ListReadySeats(ctx, family)
	if err != nil {
		return store.Seat{}, seatGate{}, err
	}
	if hostFilter != "" {
		filtered := seats[:0]
		for _, s := range seats {
			if s.Box == hostFilter {
				filtered = append(filtered, s)
			}
		}
		seats = filtered
	}
	if len(seats) == 0 {
		return store.Seat{}, seatGate{refuse: true, hardNoSeat: true,
			reason: fmt.Sprintf("no ready %s seat available (register one with `flowbee seat discover <box>`; a `%s` epic needs a ready `%s` seat)", family, family, family)}, nil
	}
	// accounts already powering an active epic (anti-collocation).
	busy := map[string]bool{}
	for _, e := range active {
		if e.AccountKey != "" {
			busy[e.AccountKey] = true
		}
	}
	var headroom, collocated []store.Seat
	sawCriticalHeadroom := false
	for _, s := range seats {
		// Read account_windows.severity directly (belt-and-suspenders over seat health):
		// a weekly_scoped-only critical excludes the seat even though usage_pct is below
		// ceiling. A probe error / stale critical (CriticalNonStale=false) does NOT exclude
		// — fail open (§12.14).
		if s.AccountKey != "" {
			if aw, ok, gerr := st.GetAccountWindow(ctx, s.AccountKey); gerr == nil && ok && aw.CriticalNonStale() {
				sawCriticalHeadroom = true
				continue // this account is weekly-critical (non-stale) — cannot finish an epic on it
			}
		}
		if s.AccountKey != "" && busy[s.AccountKey] {
			collocated = append(collocated, s)
			continue
		}
		headroom = append(headroom, s)
	}
	pick := func(list []store.Seat) (store.Seat, bool) {
		if len(list) == 0 {
			return store.Seat{}, false
		}
		return list[0], true // ListReadySeats is ordered by id — a deterministic pick
	}
	if seat, ok := pick(headroom); ok {
		return seat, seatGate{}, nil
	}
	if seat, ok := pick(collocated); ok {
		// no non-collocated seat with headroom; accept a collocated one with a warning.
		return seat, seatGate{warning: "chosen seat's account already powers another active epic (weekly budget shared)"}, nil
	}
	if sawCriticalHeadroom {
		return store.Seat{}, seatGate{refuse: true,
			reason: fmt.Sprintf("every ready %s seat's account is weekly-critical (severity=critical) — no weekly headroom to finish an epic", family)}, nil
	}
	return store.Seat{}, seatGate{refuse: true,
		reason: fmt.Sprintf("no ready %s seat with weekly headroom", family)}, nil
}

// realTmuxClock is the production tmuxio.Clock the launch ladder + its tmuxio.Client share
// (tmuxio's own default clock is unexported, and RunLadder needs the SAME clock the client
// was built with — here both use the wall clock).
type realTmuxClock struct{}

func (realTmuxClock) Now() time.Time { return time.Now() }
func (realTmuxClock) Sleep(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// epicQuotaGate implements the design doc's quota gate: "refuse if the launch
// agent's account usage_pct >= 75 in worker_accounts (fresh <=24h reading)". This
// is DELIBERATELY simpler than (and independent of) the dispatch-time rollover
// selection in capacity.go (SelectAccountForModel/AtCeiling): that machinery
// gates a single BUILD claim against the live, current ceiling with no staleness
// exception (dispatch always wants the freshest known truth, stale or not — an
// unreachable box's frozen high usage_pct correctly keeps withholding work from
// it). This gate is a coarser, ONE-TIME check at epic-launch time answering a
// different question — "is it a bad week to start a multi-day commitment?" — where
// the explicit "fresh reading" qualifier in the design doc matters: a stale gauge
// from a box that's simply been quiet must NOT block a launch (mirrors the
// watchdog's own warnUsageCeilings staleness rule).
//
// "the launch agent's account" is resolved as the PRIMARY account for the agent's
// model_family — the lowest preference_rank in the rollover chain (AccountsForModel's
// own ordering) — since the rollover chain exists for FAILOVER during dispatch, not
// for "pick whichever is least busy right now" at launch time; checking the primary
// is the account this epic will actually start consuming first.
//
// Fail-open (blocked=false) when: no accounts are enrolled for this agent at all,
// the primary account has never reported usage, or its last report is >24h stale.
func epicQuotaGate(ctx context.Context, st *store.Store, agent string, now time.Time) (blocked bool, reason string, err error) {
	accts, err := st.AccountsForModel(ctx, agent)
	if err != nil {
		return false, "", err
	}
	if len(accts) == 0 {
		return false, "", nil // no accounts enrolled for this agent: fail-open
	}
	primary := accts[0] // AccountsForModel orders by preference_rank ASC, account_id ASC

	usage, err := st.AllAccountUsage(ctx)
	if err != nil {
		return false, "", err
	}
	for _, a := range usage {
		if a.AccountID != primary.AccountID {
			continue
		}
		if a.UsagePct < usageCeilingWarnPct {
			return false, "", nil
		}
		if a.ReportedAt == "" {
			return false, "", nil // never reported: fail-open, nothing fresh to gate on
		}
		reported, perr := time.Parse(time.RFC3339Nano, a.ReportedAt)
		if perr != nil || now.Sub(reported) > 24*time.Hour {
			return false, "", nil // unparseable or stale: fail-open
		}
		return true, fmt.Sprintf("account %s (%s) is at %d%% usage, reported %s ago",
			a.AccountID, agent, a.UsagePct, now.Sub(reported).Round(time.Minute)), nil
	}
	return false, "", nil // the primary account has no usage row at all: fail-open
}

// runEpicDigest is `flowbee epic digest` (plan §2.1) — the deterministically-compiled
// board (master + all epics + attention) via the HTTP API's GET /v1/epics/digest, so the
// master/operator reads one screen instead of pane-scraping. `--id <slug>` fetches one
// epic's digest (optionally with `--tail` for the bounded UNTRUSTED pane tail).
func runEpicDigest(args []string) error {
	fs := flag.NewFlagSet("epic digest", flag.ContinueOnError)
	id := fs.String("id", "", "one epic's digest (default: the whole board)")
	tail := fs.Bool("tail", false, "with --id, include the bounded UNTRUSTED pane tail")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := "/v1/epics/digest"
	if *id != "" {
		path = "/v1/epics/" + *id + "/digest"
		if *tail {
			path += "?tail=1"
		}
	}
	var raw json.RawMessage
	if _, err := masterGet(path, &raw); err != nil {
		return err
	}
	fmt.Println(string(raw))
	return nil
}

// runEpicPlan is `flowbee epic plan` (plan §4.5 / §15.12f, ADVISORY read-only — the
// dispatcher itself is Phase 9). It surfaces the placement read-model authoring designs
// AROUND (author-epic §10b): every active epic's RESERVED scope globs + subsystem
// adjacency, and the ready-seat capacity per family. Pure local reads, no launch.
func runEpicPlan(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("epic plan", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil {
		return err
	}
	fmt.Println("active epics (reserved scope — author your scope: to NOT overlap; overlap is a hard launch refusal):")
	if len(active) == 0 {
		fmt.Println("  (none)")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  EPIC\tREPO\tSTATE\tSUBSYSTEMS\tSCOPE")
	for _, e := range active {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", e.ID, dashIfEmpty(e.Repo), e.State,
			dashIfEmpty(strings.Join(subsystemsOf(e.Scope), ",")), dashIfEmpty(strings.Join(e.Scope, " ")))
	}
	tw.Flush() //nolint:errcheck

	fmt.Println("\nready seats (a launch of family F needs a ready F seat with weekly headroom):")
	seats, err := st.ListSeats(ctx)
	if err != nil {
		return err
	}
	byFamily := map[string]int{}
	for _, s := range seats {
		if s.Enabled && s.Health == store.SeatReady {
			byFamily[s.AgentFamily]++
		}
	}
	if len(byFamily) == 0 {
		fmt.Println("  (no ready seats — `flowbee seat discover <box>` to register one)")
	}
	for _, fam := range []string{"claude", "codex"} {
		fmt.Printf("  %s: %d ready seat(s)\n", fam, byFamily[fam])
	}
	return nil
}

// subsystemsOf reduces scope globs to their coarse top-level subsystem dirs (the first two
// path segments) — the §15.5a subsystem-adjacency signal a human eyeballs so two epics with
// disjoint globs don't silently build the same adapter.
func subsystemsOf(scope []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range scope {
		parts := strings.SplitN(g, "/", 3)
		key := parts[0]
		if len(parts) >= 2 {
			key = parts[0] + "/" + parts[1]
		}
		key = strings.TrimSuffix(key, "**")
		key = strings.TrimSuffix(key, "/")
		if key != "" && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

func runEpicAbandon(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: flowbee epic abandon <id>")
	}
	id := args[0]
	if err := st.AbandonEpicRun(ctx, id, time.Now()); err != nil {
		if errors.Is(err, store.ErrEpicRunNotFound) {
			return fmt.Errorf("no such epic %q", id)
		}
		return err
	}
	// operator decision, called out explicitly (§ task brief): abandon releases the
	// scope/host reservation and stops the watchdog watching it, but the tmux
	// session itself (and any still-running agent inside it) is left alone.
	fmt.Printf("epic %q abandoned — scope + host reservation released, goal-session watch disabled.\n"+
		"the tmux session was NOT killed; if it's still running, stop it by hand (`tmux kill-session -t epic-%s` on its host).\n", id, id)
	return nil
}

func runEpicStatus(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("epic status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	epics, err := st.ListEpicRuns(ctx)
	if err != nil {
		return err
	}
	sessions, err := st.ListGoalSessions(ctx)
	if err != nil {
		return err
	}
	byID := map[string]store.GoalSession{}
	for _, g := range sessions {
		byID[g.ID] = g
	}
	printEpicStatus(os.Stdout, epics, byID, time.Now())
	return nil
}

// printEpicStatus renders one row per epic (§ task brief point 5): title · repo ·
// host · session-liveness (live-joined from goal_sessions, never persisted onto
// the epics row itself — see UpsertEpicStatus's doc) · step N/M · State · Blockers
// · Updated age.
func printEpicStatus(w io.Writer, epics []store.EpicRun, sessions map[string]store.GoalSession, now time.Time) {
	if len(epics) == 0 {
		fmt.Fprintln(w, "no epics registered (flowbee epic start <repo> <epics/....md>)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tREPO\tHOST\tSESSION\tSTEP\tSTATE\tBLOCKERS\tUPDATED")
	for _, e := range epics {
		liveness := "-"
		if g, ok := sessions[e.TmuxName]; ok {
			liveness = g.State
			if !g.Enabled {
				liveness += " [paused]"
			}
		}
		step := "-"
		if e.StatusStepsTotal > 0 {
			step = fmt.Sprintf("%d/%d", e.StatusCurrentStep, e.StatusStepsTotal)
		}
		updated := epicUpdatedAge(e, now)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.ID, dashIfEmpty(truncate(e.Title, 40)), dashIfEmpty(e.Repo), dashIfEmpty(e.Host),
			liveness, step, e.State, dashIfEmpty(truncate(e.StatusBlockers, 40)), updated)
	}
	tw.Flush() //nolint:errcheck
}

// epicUpdatedAge renders the age of the LAST ## Status ingestion (not epics.updated_at
// generically — status_updated_at is the agent's own claimed timestamp, which is
// what an operator actually wants to know: "how long since the agent last told me
// anything", not "when did our DB row last change for any reason"). Falls back to
// "-" when nothing has been ingested yet (a freshly-launched epic whose branch
// hasn't appeared in the mirror).
func epicUpdatedAge(e store.EpicRun, now time.Time) string {
	if e.StatusUpdatedAt == "" {
		return "-"
	}
	ts, err := time.Parse(time.RFC3339, e.StatusUpdatedAt)
	if err != nil {
		return e.StatusUpdatedAt // unparsed: show the raw agent-written text verbatim
	}
	age := now.Sub(ts)
	if age < 0 {
		age = 0
	}
	return age.Round(time.Minute).String() + " ago"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ── status ingestion (serve.go wires this onto its own ~2-minute ticker) ──

// ingestEpicStatuses is the epic-lane status ingestion tick (§ task brief point 4):
// for every ACTIVE epic, fetch its branch on the repo's control-plane mirror, read
// epics/<file>.md AT THAT BRANCH (never main — main is spec-frozen once triggered,
// per author-epic/SKILL.md's "Spec immutability"), leniently parse its ## Status,
// and fold it into the epics row. One epic's parse/fetch error is logged and
// SKIPPED — it must never blind ingestion to every other epic (§ task brief), which
// is why every per-epic step below is wrapped in its own continue, not a return.
func ingestEpicStatuses(ctx context.Context, logger *slog.Logger, st *store.Store, now time.Time) {
	active, err := st.ListActiveEpicRuns(ctx)
	if err != nil {
		logger.Error("epic status ingestion: list active epics", "err", err)
		return
	}
	if len(active) == 0 {
		return
	}
	repoCache := map[string]store.Repo{}
	for _, e := range active {
		repo, ok := repoCache[e.Repo]
		if !ok {
			r, rerr := st.GetRepo(ctx, e.Repo)
			if rerr != nil {
				logger.Warn("epic status ingestion: unknown repo, skipping", "epic", e.ID, "repo", e.Repo, "err", rerr)
				continue
			}
			repo = r
			repoCache[e.Repo] = repo
		}
		mp := controlMirrorFor(repo)
		if mp == "" {
			continue // no mirror configured at all: nothing to ingest from, not an error
		}
		mirror := gitops.Open(mp)
		branch := e.Branch
		if branch == "" {
			branch = "epic/" + e.ID
		}
		// the epic's branch may not exist yet in the first minute or two after launch
		// (the agent hasn't pushed its first commit) — a fetch failure here is
		// EXPECTED and not logged as an error, only skipped for this pass.
		if ferr := mirror.FetchBranch(branch); ferr != nil {
			continue
		}
		content, found, rerr := mirror.ReadFileAtRef("refs/heads/"+branch, e.FilePath)
		if rerr != nil {
			logger.Warn("epic status ingestion: read epic file", "epic", e.ID, "branch", branch, "err", rerr)
			continue
		}
		if !found {
			continue // branch exists but hasn't carried the epic file forward yet
		}
		statusBody := epicspec.ParseStatusSection(content)
		sb := epicspec.ParseStatus(statusBody)
		if uerr := st.UpsertEpicStatus(ctx, e.ID, sb, now); uerr != nil {
			logger.Warn("epic status ingestion: upsert", "epic", e.ID, "err", uerr)
			continue
		}
	}
}
