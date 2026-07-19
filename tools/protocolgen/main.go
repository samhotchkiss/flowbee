// Command protocolgen materializes the bounded actor/recovery documentation from
// the normative Flowbee v2 protocol. Run with --check in CI.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	actorprotocol "github.com/samhotchkiss/flowbee/protocol/flowbee/v2"
)

func main() {
	check := flag.Bool("check", false, "fail when checked-in generated files differ")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	contract, err := actorprotocol.Load()
	if err != nil {
		fatal(err)
	}
	files, err := actorprotocol.GeneratedFiles(contract)
	if err != nil {
		fatal(err)
	}
	for _, relative := range actorprotocol.SortedGeneratedPaths(files) {
		path := filepath.Join(*root, relative)
		want := files[relative]
		if *check {
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				fatal(fmt.Errorf("generated file differs: %s", relative))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "protocolgen:", err)
	os.Exit(1)
}
