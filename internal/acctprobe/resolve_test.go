package acctprobe

import (
	"context"
	"testing"
)

func resolverWith(line string) *Prober {
	return NewWith(OSFS{}, fakeExec{fn: func(context.Context, string, ...string) ([]byte, error) {
		return []byte(line), nil
	}}, nil, nil, fakeClock())
}

func TestResolvePID(t *testing.T) {
	const home = "/home/u"
	cases := []struct {
		name     string
		cmdline  string
		wantProv Provider
		wantDir  string
		wantAcct string
	}{
		{
			name:     "FLOWBEE_ACCOUNT wins over the command name",
			cmdline:  "node /x/codex resume CLAUDE_CONFIG_DIR=/foo FLOWBEE_ACCOUNT=claude:me@example.com",
			wantProv: ProviderClaude,
			wantDir:  "/foo",
			wantAcct: "claude:me@example.com",
		},
		{
			name:     "CLAUDE_CONFIG_DIR for a claude process",
			cmdline:  "claude --dangerously-skip-permissions CLAUDE_CONFIG_DIR=/bar",
			wantProv: ProviderClaude,
			wantDir:  "/bar",
		},
		{
			name:     "CODEX_HOME for a codex process",
			cmdline:  "node /x/codex resume CODEX_HOME=/baz",
			wantProv: ProviderCodex,
			wantDir:  "/baz",
		},
		{
			name:     "claude default dir when env unset",
			cmdline:  "/usr/local/bin/claude -p hello",
			wantProv: ProviderClaude,
			wantDir:  "/home/u/.claude",
		},
		{
			name:     "codex default home when env unset",
			cmdline:  "/x/codex exec 'do a thing'",
			wantProv: ProviderCodex,
			wantDir:  "/home/u/.codex",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := resolverWith(c.cmdline).ResolvePID(context.Background(), 1234, home)
			if err != nil {
				t.Fatal(err)
			}
			if res.Provider != c.wantProv {
				t.Errorf("provider=%q want %q", res.Provider, c.wantProv)
			}
			if res.ConfigDir != c.wantDir {
				t.Errorf("configDir=%q want %q", res.ConfigDir, c.wantDir)
			}
			if res.Account != c.wantAcct {
				t.Errorf("account=%q want %q", res.Account, c.wantAcct)
			}
		})
	}
}

func TestParseProcEnv(t *testing.T) {
	env := parseProcEnv("node codex resume CODEX_HOME=/a FLOWBEE_ACCOUNT=codex:x@y.z EXTRA=1")
	if env["CODEX_HOME"] != "/a" {
		t.Errorf("CODEX_HOME=%q", env["CODEX_HOME"])
	}
	if env["FLOWBEE_ACCOUNT"] != "codex:x@y.z" {
		t.Errorf("FLOWBEE_ACCOUNT=%q", env["FLOWBEE_ACCOUNT"])
	}
	if _, ok := env["CLAUDE_CONFIG_DIR"]; ok {
		t.Error("CLAUDE_CONFIG_DIR must be absent when not in the line")
	}
}
