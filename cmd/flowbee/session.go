package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runSession is the `flowbee session <add|list|rm|pause|resume-watch>` CLI: the
// operator's registry management for the goal-session watchdog (epic-lane Phase 1).
// It talks DIRECTLY to the local control-plane DB (like `flowbee status`/`board` do),
// not over the HTTP API — the watchdog itself lives inside `flowbee serve` and reads
// the same table, so no network round-trip is needed for pure registry CRUD, and
// this stays usable even against a DB the serve process hasn't picked up a config
// reload for yet.
func runSession(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: flowbee session <add|list|rm|pause|resume-watch> ...")
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
		return runSessionAdd(ctx, st, rest)
	case "list", "ls":
		return runSessionList(ctx, st, rest)
	case "rm", "remove":
		return runSessionRm(ctx, st, rest)
	case "pause":
		return runSessionSetEnabled(ctx, st, rest, "pause", false)
	case "resume-watch":
		return runSessionSetEnabled(ctx, st, rest, "resume-watch", true)
	default:
		return fmt.Errorf("unknown `flowbee session` subcommand %q (want add|list|rm|pause|resume-watch)", sub)
	}
}

func runSessionAdd(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("session add", flag.ContinueOnError)
	tmux := fs.String("tmux", "", "tmux session name on the target box (required)")
	box := fs.String("box", "", "ssh host to reach the tmux session on (default: local — the control-plane box)")
	repo := fs.String("repo", "", "repo this goal session is working on (operator-facing only)")
	note := fs.String("note", "", "free-text note (operator-facing only)")
	// the <id> positional can come before OR after the flags (`flowbee session add
	// <id> --tmux X` per the task brief) — Go's flag package stops parsing at the
	// first non-flag token, so a naive single fs.Parse(args) would treat "--tmux"
	// etc. as extra positionals when id comes first. Re-parse in a loop, peeling
	// off ONE positional at a time, mirroring runRepo's identical trick (repo.go).
	var id string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		if id != "" {
			return fmt.Errorf("unexpected extra argument %q (usage: flowbee session add <id> --tmux <name> [flags])", fs.Arg(0))
		}
		id = fs.Arg(0)
		rest = fs.Args()[1:]
	}
	if id == "" {
		return fmt.Errorf("usage: flowbee session add <id> --tmux <name> [--box <host>] [--repo <r>] [--note <s>]")
	}
	if strings.TrimSpace(*tmux) == "" {
		return fmt.Errorf("--tmux is required (the `tmux -t <name>` target)")
	}

	err := st.AddGoalSession(ctx, store.GoalSession{
		ID: id, Box: *box, TmuxName: *tmux, Repo: *repo, Note: *note,
	}, time.Now())
	if errors.Is(err, store.ErrGoalSessionExists) {
		return fmt.Errorf("session %q is already registered (use `flowbee session rm %s` first to re-register)", id, id)
	}
	if err != nil {
		return err
	}
	where := "local"
	if *box != "" {
		where = *box
	}
	fmt.Printf("✓ registered goal session %q (tmux %q on %s) — watched on the next 2-minute tick\n", id, *tmux, where)
	return nil
}

func runSessionRm(ctx context.Context, st *store.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: flowbee session rm <id>")
	}
	id := args[0]
	if err := st.RemoveGoalSession(ctx, id); err != nil {
		if errors.Is(err, store.ErrGoalSessionNotFound) {
			return fmt.Errorf("no such session %q", id)
		}
		return err
	}
	fmt.Printf("removed goal session %q\n", id)
	return nil
}

func runSessionSetEnabled(ctx context.Context, st *store.Store, args []string, verb string, enabled bool) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: flowbee session %s <id>", verb)
	}
	id := args[0]
	if err := st.SetGoalSessionEnabled(ctx, id, enabled, time.Now()); err != nil {
		if errors.Is(err, store.ErrGoalSessionNotFound) {
			return fmt.Errorf("no such session %q", id)
		}
		return err
	}
	state := "PAUSED"
	if enabled {
		state = "watching again"
	}
	fmt.Printf("goal session %q: %s\n", id, state)
	return nil
}

func runSessionList(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("session list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessions, err := st.ListGoalSessions(ctx)
	if err != nil {
		return err
	}
	printSessionList(os.Stdout, sessions)
	return nil
}

// printSessionList renders the full registry as a table (`flowbee session list`):
// more columns than the compact status-line form (formatSessionLine, shared with
// `flowbee status`'s goal-sessions section) since this view has room for repo/note
// and the paused flag explicitly.
func printSessionList(w io.Writer, sessions []store.GoalSession) {
	if len(sessions) == 0 {
		fmt.Fprintln(w, "no goal sessions registered (flowbee session add <id> --tmux <name> ...)")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tBOX\tTMUX\tREPO\tSTATE\tELAPSED\tDETAIL\tWATCHING")
	for _, g := range sessions {
		box := g.Box
		if box == "" {
			box = "local"
		}
		watching := "yes"
		if !g.Enabled {
			watching = "PAUSED"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			g.ID, box, g.TmuxName, dashIfEmpty(g.Repo), g.State, dashIfEmpty(g.GoalElapsed),
			dashIfEmpty(g.StateDetail), watching)
	}
	tw.Flush() //nolint:errcheck
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// formatSessionLine renders one session as "id · box · state (elapsed) [detail]" —
// the compact form `flowbee status`'s goal-sessions section uses (status.go),
// shared here so both surfaces render identically.
func formatSessionLine(g store.GoalSession) string {
	box := g.Box
	if box == "" {
		box = "local"
	}
	line := fmt.Sprintf("%s · %s · %s", g.ID, box, g.State)
	if g.GoalElapsed != "" {
		line += fmt.Sprintf(" (%s)", g.GoalElapsed)
	}
	if g.StateDetail != "" {
		line += fmt.Sprintf(" [%s]", g.StateDetail)
	}
	if !g.Enabled {
		line += " [paused]"
	}
	return line
}
