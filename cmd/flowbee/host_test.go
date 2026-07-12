package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// TestRunHostAddListRm exercises `flowbee host add/list/rm` end to end against a
// real (temp-file) DB, mirroring session_test.go's pattern.
func TestRunHostAddListRm(t *testing.T) {
	newSessionTestDB(t) // reuses session_test.go's FLOWBEE_DATABASE_URL/CONFIG fixture

	if err := runHost([]string{"add", "buncher", "--note", "big box"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := runHost([]string{"add", "buncher"}); err == nil {
		t.Fatalf("expected an error re-adding an existing host")
	}
	if err := runHost([]string{"add", "imac"}); err != nil {
		t.Fatalf("add second: %v", err)
	}
	if err := runHost([]string{"rm", "imac"}); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if err := runHost([]string{"rm", "imac"}); err == nil {
		t.Fatalf("expected an error removing a nonexistent host")
	}
	if err := runHost([]string{"list"}); err != nil {
		t.Fatalf("list: %v", err)
	}
}

func TestPrintHostList(t *testing.T) {
	var buf bytes.Buffer
	printHostList(&buf, nil, nil)
	if !strings.Contains(buf.String(), "no epic hosts registered") {
		t.Fatalf("expected empty-registry message, got:\n%s", buf.String())
	}

	buf.Reset()
	printHostList(&buf, []store.EpicHost{
		{Name: "buncher", Note: "big box", Enabled: true},
		{Name: "imac", Enabled: false},
	}, map[string]string{"buncher": "2026-07-03-frobnicator"})
	out := buf.String()
	for _, want := range []string{"buncher", "big box", "2026-07-03-frobnicator", "imac", "no"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}
