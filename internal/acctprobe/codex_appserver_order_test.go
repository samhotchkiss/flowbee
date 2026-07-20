package acctprobe

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExecAppServerWaitsForInitializeResponseBeforeReads(t *testing.T) {
	// The helper refuses any second input line before it emits initialize's
	// response. This reproduces a strict/cold app-server that rejects the former
	// optimistic initialize+reads batch.
	helper := filepath.Join(t.TempDir(), "strict-app-server")
	script := `#!/bin/bash
IFS= read -r initialize || exit 20
[[ "$initialize" == *'"method":"initialize"'* ]] || exit 21
if IFS= read -r -t 0.15 early; then
  exit 22
fi
printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"serverInfo":{"name":"fixture"}}}'
IFS= read -r initialized || exit 23
[[ "$initialized" == *'"method":"initialized"'* ]] || exit 24
IFS= read -r limits || exit 25
[[ "$limits" == *'"method":"account/rateLimits/read"'* ]] || exit 26
IFS= read -r account || exit 27
[[ "$account" == *'"method":"account/read"'* ]] || exit 28
printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"rateLimitsByLimitId":{}}}'
printf '%s\n' '{"jsonrpc":"2.0","id":3,"result":{"account":{"email":"fixture@example.com"}}}'
`
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	client := &execAppServer{binary: helper, timeout: 2 * time.Second}
	if _, err := client.Read(context.Background(), "/tmp/codex-home"); err != nil {
		t.Fatalf("strict initialize ordering failed: %v", err)
	}
}
