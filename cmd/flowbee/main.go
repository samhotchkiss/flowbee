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
	"runtime/debug"
)

// version is overridden at build time via -ldflags "-X main.version=<sha>".
var version = "dev"

// buildVersion returns the running binary's identity. With ldflags it's the injected
// version; otherwise it reads the git revision Go embeds via `go build` (so a plain
// `go build` still tells you EXACTLY which commit is running — the answer to "is this
// the rebuilt binary or a stale one?", which bit the first multi-repo run).
func buildVersion() string {
	if version != "dev" && version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		var rev, mod string
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				mod = s.Value
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if mod == "true" {
				rev += "+dirty"
			}
			return "dev-" + rev
		}
	}
	return version
}

func runVersion(args []string) error {
	prov := currentProvenance(context.Background(), true)
	for _, a := range args {
		if a == "--json" {
			out, _ := json.Marshal(prov)
			fmt.Println(string(out))
			return nil
		}
	}
	fmt.Printf("flowbee %s\n", prov.Version)
	if prov.SourceCommit != "" {
		fmt.Printf("source_commit=%s tree_dirty=%v", prov.SourceCommit, prov.TreeDirty)
		if prov.BehindOriginMainKnown {
			fmt.Printf(" behind_origin_main_by=%d", prov.BehindOriginMainBy)
		}
		fmt.Println()
	}
	if prov.Warning != "" {
		fmt.Println("WARN: " + prov.Warning)
	}
	return nil
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
	case "list":
		err = runList(args)
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
	case "adopt":
		err = runAdopt(args)
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
	case "build":
		err = runBuild(args)
	case "restore":
		err = runRestore(args)
	case "pause":
		err = runPause(args)
	case "resume":
		err = runResume(args)
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
	fmt.Fprintln(os.Stderr, "usage: flowbee <init|doctor|board|list|status|spec|repo|card|up|fleet|serve|token|migrate|work|lease|submit|requeue|adopt|cancel|retry-outbox|backup|build|restore|pause|resume|seed|version>")
}
