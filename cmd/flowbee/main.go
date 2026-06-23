// Command flowbee is the single static binary that runs as either the control
// plane (`flowbee serve`) or a worker client (`flowbee work|lease|submit`).
// DESIGN: "two binaries / one artifact". M0 implements serve + migrate; the
// worker subcommands are stubbed until M1.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/buildinfo"
)

// version is overridden at build time via -ldflags "-X main.version=<sha>".
var version = "dev"

// buildVersion returns the running binary's identity. With ldflags it's the injected
// version; otherwise it reads the git revision Go embeds via `go build` (so a plain
// `go build` still tells you EXACTLY which commit is running — the answer to "is this
// the rebuilt binary or a stale one?", which bit the first multi-repo run).
func buildVersion() string {
	return buildinfo.Current(version).Version
}

func runVersion(args []string) error {
	info := buildinfo.Current(version)
	origin := buildinfo.CheckOriginMain(context.Background(), ".", info, fetchOriginMain())
	for _, a := range args {
		if a == "--json" {
			out, _ := json.Marshal(map[string]any{
				"version":               info.Version,
				"source_commit":         info.SourceCommit,
				"tree_dirty":            info.TreeDirty,
				"behind_origin_main_by": behindPtr(origin),
				"origin_main_warning":   origin.Warning,
				"origin_main_error":     origin.Err,
			})
			fmt.Println(string(out))
			return nil
		}
	}
	fmt.Printf("flowbee %s\n", info.Version)
	fmt.Printf("source_commit=%s tree_dirty=%v behind_origin_main_by=%s\n",
		orDash(info.SourceCommit), info.TreeDirty, behindString(origin))
	if origin.Warning != "" {
		fmt.Println(origin.Warning)
	} else if origin.Err != "" {
		fmt.Println("WARN: could not compare binary source against origin/main: " + origin.Err)
	}
	return nil
}

func behindPtr(st buildinfo.OriginStatus) *int {
	if !st.Checked {
		return nil
	}
	return &st.BehindBy
}

func behindString(st buildinfo.OriginStatus) string {
	if !st.Checked {
		return "unknown"
	}
	return fmt.Sprintf("%d", st.BehindBy)
}

func fetchOriginMain() bool {
	return os.Getenv("FLOWBEE_SKIP_ORIGIN_FETCH") == ""
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "init":
		err = runInit(args)
	case "doctor":
		err = runDoctor(args)
	case "board":
		err = runBoard(args)
	case "status":
		err = runStatus(args)
	case "reservations":
		err = runReservations(args)
	case "spec":
		err = runSpec(args)
	case "repo":
		err = runRepo(args)
	case "serve":
		err = runServe(args)
	case "up":
		err = runUp(args)
	case "fleet":
		err = runFleet(args)
	case "migrate":
		err = runMigrate(args)
	case "seed":
		err = runSeed(args)
	case "token":
		err = runToken(args)
	case "work":
		err = runWork(args)
	case "lease":
		err = runLease(args)
	case "submit":
		err = runSubmit(args)
	case "requeue":
		err = runRequeue(args)
	case "cancel":
		err = runCancel(args)
	case "card":
		err = runCard(args)
	case "retry-outbox":
		err = runRetryOutbox(args)
	case "outbox":
		err = runOutbox(args)
	case "backup":
		err = runBackup(args)
	case "restore":
		err = runRestore(args)
	case "pause":
		err = runPause(args)
	case "resume":
		err = runResume(args)
	case "build":
		err = runBuild(args)
	case "version", "-v", "--version":
		err = runVersion(args)
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "flowbee %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: flowbee <init|doctor|board|status|spec|repo|card|up|fleet|serve|token|migrate|work|lease|submit|requeue|cancel|retry-outbox|backup|restore|pause|resume|seed|build|version>")
}
