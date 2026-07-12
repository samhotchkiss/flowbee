package main

import (
	"context"
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
	default:
		return fmt.Errorf("unknown `flowbee epic` subcommand %q (want start|status|abandon)", sub)
	}
}

// safeSlugRe gates any epic-derived string (the slug, used unquoted-adjacent in
// several remote-shell command arguments after being embedded in paths like
// "<home>/epics/<slug>") to a conservative safe character set BEFORE it is ever
// used to build a command — defense in depth on top of shQuote (internal/watchdog),
// matching store.validateArgvSafe's posture for goal_sessions box/tmux_name.
var safeSlugRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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
	hostFlag := fs.String("host", "", "box to launch the epic on (overrides the epic file's frontmatter host:; must be `flowbee host add`-ed first)")
	tzFlag := fs.String("tz", "", "the host's IANA timezone (default: probe the box via `date`/`timedatectl`, else assume serve-local — mirrors `flowbee session add --tz`)")
	agentFlag := fs.String("agent", "", "coding agent to launch (overrides the epic file's frontmatter agent:; default codex)")
	forceQuota := fs.Bool("force-quota", false, "launch even if the agent's account usage is >=75% (fresh reading) — see the quota-gate WHY comment")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return fmt.Errorf("usage: flowbee epic start <repo> <epics/....md> [--host <box>] [--tz <iana>] [--agent <name>] [--force-quota]")
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

	// ── host resolution + occupancy (one-box-one-epic) ──
	host := *hostFlag
	if host == "" {
		host = spec.Host
	}
	if host == "" {
		return fmt.Errorf("no host specified (--host, or the epic file's frontmatter host:) — see `flowbee host list` for registered boxes")
	}
	hostRow, err := st.GetEpicHost(ctx, host)
	if err != nil {
		if errors.Is(err, store.ErrEpicHostNotFound) {
			return fmt.Errorf("host %q is not registered (run `flowbee host add %s` first)", host, host)
		}
		return err
	}
	if !hostRow.Enabled {
		return fmt.Errorf("host %q is disabled", host)
	}
	if held, occupied, err := st.HostActiveEpic(ctx, host); err != nil {
		return err
	} else if occupied {
		return fmt.Errorf("host %q already holds active epic %q — one-box-one-epic (see `flowbee epic status`)", host, held.ID)
	}

	// ── scope reservation (blast-radius overlap, same repo only) ──
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

	// ── quota gate ──
	agent := *agentFlag
	if agent == "" {
		agent = spec.Agent
	}
	if agent == "" {
		agent = "codex" // author-epic/SKILL.md's documented default coding agent
	}
	if !*forceQuota {
		blocked, reason, qerr := epicQuotaGate(ctx, st, agent, time.Now())
		if qerr != nil {
			return qerr
		}
		if blocked {
			return fmt.Errorf("quota gate: %s — retry later, or pass --force-quota to launch anyway", reason)
		}
	}

	// ── preflight on the host (reuses the watchdog Runner abstraction) ──
	runner := watchdog.NewShellRunner()
	homeOut, herr := runner.Run(ctx, watchdog.HomeDirCmd(host))
	if herr != nil {
		return fmt.Errorf("resolve home directory on %q: %w", host, herr)
	}
	home := strings.TrimSpace(homeOut)
	if home == "" {
		return fmt.Errorf("could not resolve a home directory on host %q (is it reachable? `gh` and `tmux` must both be installed there)", host)
	}
	checkoutPath := home + "/epics/" + repoID // the documented ~/epics/<repo> convention, as a resolved literal path

	tz := *tzFlag
	if tz != "" {
		if _, lerr := time.LoadLocation(tz); lerr != nil {
			return fmt.Errorf("invalid --tz %q (want an IANA name like America/Denver): %w", tz, lerr)
		}
	} else if tzOut, terr := runner.Run(ctx, watchdog.TimezoneCmd(host)); terr == nil {
		if probe := strings.TrimSpace(tzOut); probe != "" {
			if _, lerr := time.LoadLocation(probe); lerr == nil {
				tz = probe
			}
			// an unresolvable probe result (e.g. a bare "MST" abbreviation from the
			// date +%Z fallback) leaves tz="" — the documented "assume serve-local"
			// default, same as AddGoalSession's own --tz-omitted behavior.
		}
	}

	pre, err := watchdog.Preflight(ctx, runner, watchdog.PreflightParams{
		Box: host, CheckoutPath: checkoutPath, OwnerRepo: repo.Owner + "/" + repo.Repo,
	})
	if err != nil {
		return fmt.Errorf("preflight on %q: %w", host, err)
	}
	if !pre.GhAuthOK {
		return fmt.Errorf("preflight failed: `gh auth status` is not authenticated on host %q — run `gh auth login` there first (the epic will need it to open its final PR)", host)
	}
	const minFreeKB = 10 * 1024 * 1024 // 10G, per the design doc's disk gate
	if pre.DiskFreeKB < minFreeKB {
		return fmt.Errorf("preflight failed: host %q has only %.1fG free at %s (need >=10G)", host, float64(pre.DiskFreeKB)/(1024*1024), checkoutPath)
	}
	if pre.ClonedFresh {
		logger.Info("cloned a fresh checkout", "host", host, "path", checkoutPath, "repo", repo.Owner+"/"+repo.Repo)
	}

	// ── register (state=launching) BEFORE the tmux launch, so a crash here leaves a
	// visible half-launched row rather than nothing — see AddEpicRun's doc. ──
	now := time.Now()
	tmuxName := "epic-" + slug
	if err := st.AddEpicRun(ctx, store.EpicRun{
		ID: slug, Repo: repoID, FilePath: filePath, Title: spec.Title, Scope: spec.Scope,
		Host: host, Branch: "epic/" + slug, TmuxName: tmuxName, Agent: agent,
	}, now); err != nil {
		return fmt.Errorf("register epic: %w", err)
	}

	// ── launch: tmux new-session + send the goal, reusing the watchdog's exact
	// double-Enter submit-verify mechanics. ──
	goal := fmt.Sprintf("/goal execute the epic at %s per epics/INSTRUCTIONS.md. Work on branch epic/%s.", filePath, slug)
	verified, launchErr := watchdog.LaunchEpicSession(ctx, runner, watchdog.LaunchParams{
		Box: host, TmuxName: tmuxName, Dir: checkoutPath, StartCmd: agent, Goal: goal,
		SettleDelay: 800 * time.Millisecond,
	})
	if launchErr != nil {
		_ = st.DeleteEpicRun(ctx, slug) // roll back the registration — see AddEpicRun's doc
		return fmt.Errorf("launch failed on %q: %w (epic registration rolled back — nothing is reserved)", host, launchErr)
	}
	if !verified {
		logger.Warn("could not verify the goal was submitted (pane capture failed after send) — check the tmux session by hand", "epic", slug, "host", host, "tmux", tmuxName)
	}

	// the tmux session IS now running the agent regardless of what happens below —
	// a goal_sessions registration failure must NOT roll back the launch (that would
	// abandon a live agent with no record of it). It's logged loudly instead.
	if err := st.AddGoalSession(ctx, store.GoalSession{
		ID: tmuxName, Box: host, TmuxName: tmuxName, TZ: tz, Repo: repoID, Note: "epic: " + spec.Title,
	}, now); err != nil {
		logger.Error("epic launched but failed to register its goal-session watch — the watchdog will NOT observe it until this is fixed", "epic", slug, "err", err)
	}
	if err := st.MarkEpicLaunched(ctx, slug, now); err != nil {
		return fmt.Errorf("epic launched but failed to mark it running in the registry: %w", err)
	}

	fmt.Printf("✓ launched epic %q on %q (tmux %q, branch epic/%s) — run `flowbee epic status` to confirm\n", slug, host, tmuxName, slug)
	return nil
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
