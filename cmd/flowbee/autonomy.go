package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/samhotchkiss/flowbee/internal/config"
	"github.com/samhotchkiss/flowbee/internal/store"
)

// runAutonomy prints, READ-ONLY, what the self-clearing ladder would do against the live
// control-plane DB right now — which stuck jobs the janitor would requeue, which the advisor
// would engage, and how every parked job would eventually exit. It mutates nothing and makes
// no model calls, so it is safe to run against a live serve (SQLite WAL allows the concurrent
// read). Use it to validate the ladder on the real backlog before enabling FLOWBEE_AUTONOMOUS.
//
//	flowbee autonomy            # the preview report
func runAutonomy(args []string) error {
	fs := flag.NewFlagSet("autonomy", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := store.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	now := time.Now()

	staleHB := 3 * cfg.HeartbeatInterval()
	if staleHB <= 0 {
		staleHB = 90 * time.Second
	}
	// same rung constants serve uses.
	const (
		minUnblock = 2
		advisorCap = 3
		maxUnblock = 2
	)
	pv, err := db.AutonomyPreview(ctx, now, staleHB, minUnblock, advisorCap, maxUnblock)
	if err != nil {
		return err
	}

	fmt.Printf("flowbee autonomy preview (read-only; nothing mutated)\n")
	fmt.Printf("fleet: %d live worker(s)\n\n", pv.LiveWorkers)

	fmt.Printf("mechanical janitor would requeue %d stall job(s): %s\n",
		len(pv.MechanicalUnblock), joinOrDash(pv.MechanicalUnblock))
	fmt.Printf("advisor would engage %d job(s):\n", len(pv.AdvisorEngage))
	for _, e := range pv.AdvisorEngage {
		fmt.Printf("  %s  (%s)\n", e.JobID, e.Reason)
	}
	if pv.LiveWorkers == 0 {
		fmt.Printf("\n⚠️  no live workers — the janitor stands down (nothing to requeue onto) until the fleet is up.\n")
	}

	// Full needs_human breakdown with each reason's eventual exit — the "does everything
	// converge?" view. Uses NeedsHumanView so the EFFECTIVE reason is shown (a blank-column
	// bounce-exhaustion is classified as `bounces`, exactly as the ladder now treats it).
	nh, err := db.NeedsHumanView(ctx)
	if err != nil {
		return err
	}
	byReason := map[string]int{}
	for _, r := range nh {
		byReason[r.Reason]++
	}
	fmt.Printf("\nneeds_human parked jobs — by (effective) reason and exit path:\n")
	if len(byReason) == 0 {
		fmt.Printf("  (none)\n")
	}
	for reason, n := range byReason {
		fmt.Printf("  %-22s %3d   → %s\n", reason, n, exitPath(reason))
	}
	fmt.Printf("\nEnable with FLOWBEE_AUTONOMOUS_SHADOW=on (watch the serve log), then FLOWBEE_AUTONOMOUS=on.\n")
	return nil
}

// exitPath describes how a parked reason eventually leaves needs_human under autonomy.
func exitPath(reason string) string {
	switch reason {
	case "stall":
		return "mechanical janitor → advisor → auto-cancel"
	case "bounces", "attempts", "reviewer_rejections":
		return "advisor (guided retry) → auto-cancel if unfixable"
	case "no_eligible_worker":
		return "24h time-backstop auto-cancel (a retry can't fix caps)"
	case "project_out", "pr_closed", "cost", "ci_stalled", "design":
		return "legible park — needs external/human action (never auto-cancelled)"
	default:
		return "legible park"
	}
}

func joinOrDash(ss []string) string {
	if len(ss) == 0 {
		return "-"
	}
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
