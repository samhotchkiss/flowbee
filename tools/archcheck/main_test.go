package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeArchFixture(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRawTmuxBoundaryRejectsNewProductImport(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/store/rogue.go", `package store
import _ "github.com/samhotchkiss/flowbee/internal/tmuxio"
`)
	if got := scanRawTmuxSources(root); got == 0 {
		t.Fatal("new product raw-tmux import passed architecture gate")
	}
}

func TestRawTmuxBoundaryRejectsSplitArgvMutation(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/store/rogue.go", `package store
import "os/exec"
func send() { _ = exec.Command("tmux", "send-keys", "-t", "x", "hello").Run() }
`)
	if got := scanRawTmuxSources(root); got == 0 {
		t.Fatal("exec.Command raw tmux send passed architecture gate")
	}
}

func TestRawTmuxBoundaryRejectsProductSessionOrigin(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/store/rogue.go", `package store
const insert = "INSERT INTO actions(sender_session_id) VALUES (?)"
`)
	if got := scanRawTmuxSources(root); got == 0 {
		t.Fatal("product session-origin materializer passed architecture gate")
	}
}

func TestRawTmuxBoundaryPreservesAuditedLegacyImplementation(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/tmuxio/send.go", `package tmuxio
import "os/exec"
func send() { _ = exec.Command("tmux", "send-keys", "-t", "x", "hello").Run() }
`)
	if got := scanRawTmuxSources(root); got != 0 {
		t.Fatalf("audited legacy implementation rejected with %d violation(s)", got)
	}
}

func TestLegacyTmuxCallSitesRetainDurableV2Fences(t *testing.T) {
	if got := checkLegacyTmuxFenceInvariants("../.."); got != 0 {
		t.Fatalf("current legacy call sites lost %d durable-v2 fence(s)", got)
	}
}

func TestP1NotificationBoundaryRejectsGenericOutboundDrainer(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/alerting/drainer.go", `package alerting
type WebhookSink struct{ URL string }
`)
	if got := checkP1HumanNotificationBoundary(root); got == 0 {
		t.Fatal("generic outbound alert drainer passed architecture gate")
	}
}

func TestP1NotificationBoundaryRejectsDirectControlAlertAcknowledgement(t *testing.T) {
	root := t.TempDir()
	writeArchFixture(t, root, "internal/store/rogue.go", `package store
func AcknowledgeControlAlert() {}
`)
	if got := checkP1HumanNotificationBoundary(root); got == 0 {
		t.Fatal("direct 2xx-style control alert acknowledgement passed architecture gate")
	}
}
