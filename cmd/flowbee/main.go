// Command flowbee is the single static binary that runs as either the control
// plane (`flowbee serve`) or a worker client (`flowbee work|lease|submit`).
// DESIGN: "two binaries / one artifact". M0 implements serve + migrate; the
// worker subcommands are stubbed until M1.
package main

import (
	"fmt"
	"os"
)

// version is overridden at build time via -ldflags "-X main.version=<sha>".
var version = "dev"

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
		fmt.Printf("flowbee %s\n", version)
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
	fmt.Fprintln(os.Stderr, "usage: flowbee <init|doctor|up|fleet|serve|token|migrate|work|lease|submit|requeue|seed|version>")
}
