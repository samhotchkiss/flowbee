package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionJSON(t *testing.T) {
	t.Setenv("FLOWBEE_SKIP_ORIGIN_FETCH", "1")
	out := captureStdout(t, func() {
		if err := runVersion([]string{"--json"}); err != nil {
			t.Fatalf("runVersion --json: %v", err)
		}
	})

	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("--json output did not parse as JSON: %v\noutput: %q", err, out)
	}
	if v.Version == "" {
		t.Fatalf("--json output has empty version field; output: %q", out)
	}
}

func TestVersionPlain(t *testing.T) {
	t.Setenv("FLOWBEE_SKIP_ORIGIN_FETCH", "1")
	out := captureStdout(t, func() {
		if err := runVersion(nil); err != nil {
			t.Fatalf("runVersion: %v", err)
		}
	})

	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "flowbee ") {
		t.Fatalf("plain version output should start with 'flowbee ', got: %q", trimmed)
	}
}
