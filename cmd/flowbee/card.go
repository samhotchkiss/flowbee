package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/history"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runCard prints a single job's full §F history card — status, attempts, verdicts,
// lessons, and the institutional timeline — folded from the event ledger. The same
// curated view the archive writes to docs/history, but for ANY job (stuck, cancelled,
// in-flight, or done), so an operator can answer "why is this job here / how did it
// get built" without reading the DB or the GitHub archive. Local + read-only (mirrors
// `board`/`status`/`doctor`): opens the control-plane DB, no writes or RPCs.
func runCard(args []string) error {
	jsonDefault, args, err := cardJSONFlag(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("card", flag.ContinueOnError)
	jsonOut := fs.Bool("json", jsonDefault, "print the card as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: flowbee card <job-id> [--json]")
	}
	jobID := fs.Arg(0)

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

	card, err := st.HistoryCardForJob(ctx, jobID)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("no initialized flowbee database at %q — start the control plane first, or point FLOWBEE_CONFIG / database_url at the live DB (standard location: ~/.flowbee/flowbee.db)", cfg.DatabaseURL)
		}
		return err
	}
	// a job with no events folds to an empty card (no id): treat as not-found so a
	// mistyped/truncated id gets actionable guidance, not a blank render.
	if card.JobID == "" {
		return fmt.Errorf("no such job %q (check the FULL job id, not a truncated one — `flowbee board` lists them)", jobID)
	}
	return printCard(os.Stdout, card, *jsonOut)
}

func cardJSONFlag(args []string) (bool, []string, error) {
	jsonOut := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		switch {
		case arg == "--json":
			jsonOut = true
		case strings.HasPrefix(arg, "--json="):
			v, err := strconv.ParseBool(strings.TrimPrefix(arg, "--json="))
			if err != nil {
				return false, nil, fmt.Errorf("invalid --json value %q", strings.TrimPrefix(arg, "--json="))
			}
			jsonOut = v
		default:
			filtered = append(filtered, arg)
		}
	}
	return jsonOut, filtered, nil
}

func printCard(w io.Writer, card history.Card, jsonOut bool) error {
	if !jsonOut {
		_, err := fmt.Fprint(w, history.Render(card))
		return err
	}
	enc := json.NewEncoder(w)
	return enc.Encode(cardJSON(card))
}

type cardJSONDoc struct {
	ID       string             `json:"id"`
	State    string             `json:"state"`
	Role     string             `json:"role"`
	Kind     string             `json:"kind"`
	Flow     string             `json:"flow"`
	PRNumber int                `json:"pr_number"`
	BaseSHA  string             `json:"base_sha"`
	HeadSHA  string             `json:"head_sha"`
	Attempts int                `json:"attempts"`
	Bounces  int                `json:"bounces"`
	Timeline []cardJSONTimeline `json:"timeline"`
}

type cardJSONTimeline struct {
	Seq  int       `json:"seq"`
	Kind string    `json:"kind"`
	At   time.Time `json:"at,omitempty"`
	Note string    `json:"note"`
}

func cardJSON(card history.Card) cardJSONDoc {
	timeline := make([]cardJSONTimeline, 0, len(card.Timeline))
	for _, e := range card.Timeline {
		timeline = append(timeline, cardJSONTimeline{
			Seq: e.Seq, Kind: string(e.Kind), At: e.At, Note: e.Note,
		})
	}
	return cardJSONDoc{
		ID:       card.JobID,
		State:    string(card.Status),
		Role:     string(card.Role),
		Kind:     string(card.Kind),
		Flow:     card.Flow,
		PRNumber: card.PRNumber,
		BaseSHA:  card.BaseSHA,
		HeadSHA:  card.HeadSHA,
		Attempts: card.Attempts,
		Bounces:  card.Bounces,
		Timeline: timeline,
	}
}
