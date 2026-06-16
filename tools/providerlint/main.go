// Command providerlint enforces DESIGN §5.6: no provider literal (codex, opus,
// claude, gpt, …) may appear in a control position in the flow/role config. The
// ONLY allowlisted positions are a `model_family:*` capability tag and a
// `lens.prompt_ref` path. A literal anywhere else (a role/stage key, a `when:`
// predicate, an independence term, a requires tag) FAILS the build — neutrality is
// an enforced invariant, not an aspiration. CI runs this alongside archcheck.
//
// Usage: providerlint [flows.yaml ...]  (defaults to flows/flows.yaml)
package main

import (
	"fmt"
	"os"

	"github.com/samhotchkiss/flowbee/internal/flow"
)

func main() {
	paths := os.Args[1:]
	if len(paths) == 0 {
		paths = []string{"flows/flows.yaml"}
	}
	violations := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("providerlint: cannot read %s: %v\n", path, err)
			violations++
			continue
		}
		var c flow.Config
		// Decode without the fail-fast Parse so we can report ALL violations.
		if perr := flow.Unmarshal(data, &c); perr != nil {
			fmt.Printf("providerlint: parse %s: %v\n", path, perr)
			violations++
			continue
		}
		errs := c.LintNeutrality()
		for _, e := range errs {
			fmt.Printf("VIOLATION (%s): %s\n", path, e)
			violations++
		}
	}
	if violations > 0 {
		fmt.Printf("providerlint: %d provider-neutrality violation(s) (§5.6)\n", violations)
		os.Exit(1)
	}
	fmt.Println("providerlint: provider-neutrality clean")
}
