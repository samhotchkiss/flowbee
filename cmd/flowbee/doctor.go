package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/samhotchkiss/flowbee/internal/api"
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
	runningOnly := fs.Bool("running", false, "check only the currently running control plane's redacted config")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var rep onboarding.DoctorReport
	if !*runningOnly {
		// honor FLOWBEE_CONFIG so `flowbee doctor` validates the SAME config `flowbee serve`
		// runs — not a stray <cwd>/flowbee.yaml. An explicit --dir (non-default) still wins.
		configPath := ""
		if *dir == "." {
			configPath = envOr("FLOWBEE_CONFIG", "")
		}
		var err error
		rep, err = onboarding.Doctor(context.Background(), onboarding.DoctorOptions{
			Root:       *dir,
			ConfigPath: configPath,
			SkipGitHub: *offline,
		})
		if err != nil {
			return err
		}
		rep.Checks = append(rep.Checks, binarySourceCheck(context.Background()))
	}
	rep.Checks = append(rep.Checks, runningConfigCheck(context.Background()))

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

func binarySourceCheck(ctx context.Context) onboarding.Check {
	prov := currentProvenance(ctx, true)
	detail := fmt.Sprintf("version=%s source_commit=%s tree_dirty=%v behind_origin_main_by=%s",
		prov.Version, orDash(prov.SourceCommit), prov.TreeDirty, behindProvenanceString(prov))
	if prov.Warning != "" {
		return onboarding.Check{Name: "binary-source", Status: onboarding.StatusWarn, Detail: "WARN: " + prov.Warning + "; " + detail}
	}
	return onboarding.Check{Name: "binary-source", Status: onboarding.StatusPass, Detail: detail}
}

func behindProvenanceString(prov provenance) string {
	if !prov.BehindOriginMainKnown {
		return "unknown"
	}
	return fmt.Sprintf("%d", prov.BehindOriginMainBy)
}

func runningConfigCheck(ctx context.Context) onboarding.Check {
	base := strings.TrimRight(envOr("FLOWBEE_URL", "http://127.0.0.1:7070"), "/")
	reqCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/config", nil)
	if err != nil {
		return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn, Detail: err.Error()}
	}
	if tok := envOr("FLOWBEE_WORKER_TOKEN", ""); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn,
			Detail: "no running control plane reached at " + base + " (local config checks only)"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized {
			return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn,
				Detail: "running control plane requires auth for /v1/config; set FLOWBEE_WORKER_TOKEN to report running config"}
		}
		if resp.StatusCode == http.StatusForbidden {
			return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn,
				Detail: "running control plane exposes /v1/config only to loopback callers unless worker auth is configured"}
		}
		return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn,
			Detail: fmt.Sprintf("running control plane at %s returned status %d", base, resp.StatusCode)}
	}
	var cfg api.RunningConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return onboarding.Check{Name: "running-config", Status: onboarding.StatusWarn, Detail: "decode /v1/config: " + err.Error()}
	}
	repos := make([]string, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		label := r.ID
		if label == "" {
			label = r.Owner + "/" + r.Repo
		}
		if r.TokenPresent {
			label += ":token"
		} else {
			label += ":no-token"
		}
		repos = append(repos, label)
	}
	if len(repos) == 0 {
		repos = append(repos, "none")
	}
	st := onboarding.StatusPass
	prefix := ""
	if cfg.SourceWarning != "" {
		st = onboarding.StatusWarn
		prefix = "WARN: " + cfg.SourceWarning + "; "
	}
	return onboarding.Check{Name: "running-config", Status: st,
		Detail: prefix + fmt.Sprintf("version=%s source_commit=%s tree_dirty=%v behind_origin_main_by=%s pid=%d config=%s db=%s private=%s self_merge=%v mirror=%s git_remote=%s token_present=%v webhook_secret=%v worker_auth=%v insecure=%v log_path=%s repos=%s",
			cfg.Version, orDash(cfg.SourceCommit), cfg.TreeDirty, behindString(cfg),
			cfg.PID, orDash(cfg.ConfigPath), cfg.DatabaseURL, cfg.PrivateAddr,
			cfg.AllowSelfMerge, orDash(cfg.MirrorPath), orDash(cfg.GitRemote), cfg.GitHubTokenPresent,
			cfg.WebhookSecretPresent, cfg.WorkerAuthConfigured, cfg.InsecureWorkerAPI,
			orDash(cfg.LogPath), strings.Join(repos, ","))}
}

func behindString(cfg api.RunningConfig) string {
	if !cfg.BehindOriginMainKnown {
		return "unknown"
	}
	return fmt.Sprintf("%d", cfg.BehindOriginMainBy)
}
