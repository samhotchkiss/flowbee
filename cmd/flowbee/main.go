// Command flowbee is the single static binary that runs as either the control
// plane (`flowbee serve`) or a worker client (`flowbee work|lease|submit`).
// DESIGN: "two binaries / one artifact". M0 implements serve + migrate; the
// worker subcommands are stubbed until M1.
package main

import (
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
	case "version", "-v", "--version":
		fmt.Printf("flowbee %s\n", buildVersion())
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
	fmt.Fprintln(os.Stderr, "usage: flowbee <init|doctor|board|up|fleet|serve|token|migrate|work|lease|submit|requeue|seed|version>")
}
