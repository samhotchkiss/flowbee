package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samhotchkiss/flowbee/client"
)

// TestNodeCommitMessagePrefersAgentNotes: when the agent writes .flowbee/commit.md,
// that detailed account IS the commit message (the node author's own words). With
// none, a rendered default still carries the conventional subject + the task.
func TestNodeCommitMessage(t *testing.T) {
	ws := t.TempDir()
	mkFlowbee := func(body string) {
		dir := filepath.Join(ws, ".flowbee")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "commit.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := &client.LeaseContext{Task: "# Add a CONTRIBUTING guide\n\nDocument the dev loop."}

	// agent wrote detailed notes -> used verbatim.
	mkFlowbee("Add CONTRIBUTING.md\n\nExplains setup, test, and PR flow for new contributors.")
	got := nodeCommitMessage(ws, "builder-claude", "eng_worker", "job-1", ctx)
	if got != "Add CONTRIBUTING.md\n\nExplains setup, test, and PR flow for new contributors." {
		t.Fatalf("agent commit.md should be used verbatim, got:\n%s", got)
	}

	// no commit.md -> rendered default: conventional subject with identity + task title.
	_ = os.RemoveAll(filepath.Join(ws, ".flowbee"))
	got = nodeCommitMessage(ws, "builder-claude", "eng_worker", "job-1", ctx)
	if !strings.HasPrefix(got, "build(builder-claude): Add a CONTRIBUTING guide") {
		t.Fatalf("default subject wrong:\n%s", got)
	}
	if !strings.Contains(got, "Document the dev loop.") {
		t.Fatalf("default body should carry the task:\n%s", got)
	}

	// conflict_resolver role -> "resolve(...)" verb in the default.
	got = nodeCommitMessage(ws, "resolver-opus", "conflict_resolver", "job-2", ctx)
	if !strings.HasPrefix(got, "resolve(resolver-opus): ") {
		t.Fatalf("resolver verb wrong:\n%s", got)
	}
}
