package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"

	"github.com/samhotchkiss/flowbee/internal/onboarding"
)

// runDoctor validates the scaffolded repo (F13): config parses + is valid, the
// flow files reference identities that exist with their lenses, and GitHub is
// reachable (or skipped with --offline). Exits non-zero (returns an error) iff a
// check failed; warnings (offline/no-token) are reported but stay green.
func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dir := fs.String("dir", ".", "repo root to validate")
	offline := fs.Bool("offline", false, "skip the GitHub reachability check")
	quiet := fs.Bool("quiet", false, "suppress per-check lines; print only the summary")
	jsonOut := fs.Bool("json", false, "emit check results as a JSON array (name/status/detail per check)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// honor FLOWBEE_CONFIG so `flowbee doctor` validates the SAME config `flowbee serve`
	// runs — not a stray <cwd>/flowbee.yaml. An explicit --dir (non-default) still wins.
	configPath := ""
	if *dir == "." {
		configPath = envOr("FLOWBEE_CONFIG", "")
	}
	rep, err := onboarding.Doctor(context.Background(), onboarding.DoctorOptions{
		Root:       *dir,
		ConfigPath: configPath,
		SkipGitHub: *offline,
	})
	if err != nil {
		return err
	}

	if *jsonOut {
		type jsonCheck struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		}
		out := make([]jsonCheck, len(rep.Checks))
		for i, c := range rep.Checks {
			out[i] = jsonCheck{Name: c.Name, Status: string(c.Status), Detail: c.Detail}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		fmt.Println(string(b))
		if !rep.Green() {
			return fmt.Errorf("doctor found failing checks")
		}
		return nil
	}

	if !*quiet {
		for _, c := range rep.Checks {
			mark := "ok  "
			switch c.Status {
			case onboarding.StatusWarn:
				mark = "warn"
			case onboarding.StatusFail:
				mark = "FAIL"
			}
			fmt.Printf("  [%s] %-13s %s\n", mark, c.Name, c.Detail)
		}
	}

	if rep.Green() {
		fmt.Println("\nflowbee doctor: green")
		return nil
	}
	if *quiet {
		return fmt.Errorf("flowbee doctor: FAIL")
	}
	return fmt.Errorf("doctor found failing checks (see above)")
}
