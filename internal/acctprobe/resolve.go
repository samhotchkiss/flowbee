package acctprobe

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Resolution is the account-binding read off a running agent process's environment:
// which provider it is and which config dir / CODEX_HOME (hence which account) it
// will use. Account is set only when FLOWBEE_ACCOUNT pins one explicitly.
type Resolution struct {
	Provider  Provider
	ConfigDir string            // resolved CLAUDE_CONFIG_DIR (claude) or CODEX_HOME (codex)
	Account   string            // FLOWBEE_ACCOUNT value ("provider:email") if set, else ""
	Env       map[string]string // the parsed acctprobe-relevant vars (diagnostic)
}

// ResolvePID reads the environment of process pid and resolves which account it uses.
// Precedence: FLOWBEE_ACCOUNT ("provider:email") wins for the provider; otherwise the
// provider is inferred from CODEX_HOME / CLAUDE_CONFIG_DIR presence (and the command
// name as a tiebreak). The config dir defaults to home/.codex or home/.claude when the
// respective env var is unset. home is used only to expand those defaults.
//
// It reads the env via `ps eww` (injected ExecRunner) — the same non-secret vars a
// human would read; it never returns process secrets.
func (p *Prober) ResolvePID(ctx context.Context, pid int, home string) (*Resolution, error) {
	out, err := p.Exec.Output(ctx, "ps", "eww", "-o", "command=", "-p", fmt.Sprintf("%d", pid))
	if err != nil {
		return nil, fmt.Errorf("acctprobe: read env of pid %d: %w", pid, err)
	}
	cmdline := string(out)
	env := parseProcEnv(cmdline)

	res := &Resolution{Env: map[string]string{}}
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "FLOWBEE_ACCOUNT"} {
		if v, ok := env[k]; ok {
			res.Env[k] = v
		}
	}

	// Provider precedence: FLOWBEE_ACCOUNT prefix wins.
	if fa := env["FLOWBEE_ACCOUNT"]; fa != "" {
		res.Account = fa
		if prov, _, ok := strings.Cut(fa, ":"); ok {
			switch Provider(strings.ToLower(strings.TrimSpace(prov))) {
			case ProviderClaude:
				res.Provider = ProviderClaude
			case ProviderCodex:
				res.Provider = ProviderCodex
			case ProviderGrok:
				res.Provider = ProviderGrok
			}
		}
	}
	if res.Provider == "" {
		res.Provider = inferProvider(env, cmdline)
	}

	switch res.Provider {
	case ProviderCodex:
		res.ConfigDir = firstNonEmpty(env["CODEX_HOME"], filepath.Join(home, ".codex"))
	case ProviderClaude:
		res.ConfigDir = firstNonEmpty(env["CLAUDE_CONFIG_DIR"], filepath.Join(home, ".claude"))
	case ProviderGrok:
		res.ConfigDir = firstNonEmpty(env["GROK_HOME"], filepath.Join(home, ".grok"))
	default:
		// unknown provider but a dir env may still tell us where to look.
		if v := env["CODEX_HOME"]; v != "" {
			res.ConfigDir = v
		} else if v := env["CLAUDE_CONFIG_DIR"]; v != "" {
			res.ConfigDir = v
		} else if v := env["GROK_HOME"]; v != "" {
			res.ConfigDir = v
		}
	}
	return res, nil
}

// inferProvider guesses the provider from env presence, then the command name.
func inferProvider(env map[string]string, cmdline string) Provider {
	if _, ok := env["CODEX_HOME"]; ok {
		return ProviderCodex
	}
	if _, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		return ProviderClaude
	}
	if _, ok := env["GROK_HOME"]; ok {
		return ProviderGrok
	}
	lc := strings.ToLower(cmdline)
	// check codex/grok first: their command tokens are specific, whereas a claude
	// wrapper path could contain neither.
	if strings.Contains(lc, "codex") {
		return ProviderCodex
	}
	if strings.Contains(lc, "grok") {
		return ProviderGrok
	}
	if strings.Contains(lc, "claude") {
		return ProviderClaude
	}
	return ""
}

// parseProcEnv extracts the acctprobe-relevant KEY=VALUE tokens from a `ps eww`
// command+env line. `ps` space-separates the command and each env entry with no clean
// delimiter, so rather than trying to split command from env, we scan for our three
// specific KEY= prefixes (which are vanishingly unlikely to appear inside a command
// arg) and take each value up to the next space. Values with spaces are not supported
// (none of these three ever contain one), matching `ps`'s own ambiguity.
func parseProcEnv(cmdline string) map[string]string {
	env := map[string]string{}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "FLOWBEE_ACCOUNT"} {
		prefix := key + "="
		idx := strings.LastIndex(cmdline, prefix)
		if idx < 0 {
			continue
		}
		rest := cmdline[idx+len(prefix):]
		if sp := strings.IndexAny(rest, " \t\n"); sp >= 0 {
			rest = rest[:sp]
		}
		env[key] = rest
	}
	return env
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
