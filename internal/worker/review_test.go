package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/samhotchkiss/flowbee/client"
)

func TestIsReviewRole(t *testing.T) {
	for _, r := range []string{"spec_author", "spec_reviewer", "code_reviewer"} {
		if !IsReviewRole(r) {
			t.Fatalf("%s should be a review role", r)
		}
	}
	for _, r := range []string{"eng_worker", "conflict_resolver", ""} {
		if IsReviewRole(r) {
			t.Fatalf("%s should NOT be a review role", r)
		}
	}
}

func TestReadVerdict(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "verdict.json")
	if err := os.WriteFile(p, []byte(`{"decision":"approved","disposition":"self_merge","meets_style":true,"notes":"lgtm"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	v, err := readVerdict(p)
	if err != nil {
		t.Fatal(err)
	}
	if v.Decision != "approved" || v.Disposition != "self_merge" || !v.MeetsStyle {
		t.Fatalf("parsed wrong: %+v", v)
	}
	if _, err := readVerdict(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteReviewDiffArtifactWritesNonemptyDiff(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".flowbee", "diff.patch")
	const diff = "diff --git a/x b/x\n+new\n"
	if err := writeReviewDiffArtifact(p, &client.LeaseContext{Diff: diff}); err != nil {
		t.Fatalf("write diff artifact: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read diff artifact: %v", err)
	}
	if string(b) != diff {
		t.Fatalf("diff artifact=%q, want exact lease diff", string(b))
	}
}

func TestWriteReviewDiffArtifactWritesExplicitEmptyDiff(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".flowbee", "diff.patch")
	if err := writeReviewDiffArtifact(p, &client.LeaseContext{DiffEmpty: true}); err != nil {
		t.Fatalf("write empty diff artifact: %v", err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("explicit empty diff should still materialize diff.patch: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("empty diff artifact has %d bytes", len(b))
	}
}

func TestWriteReviewDiffArtifactLeavesLegacyMissingDiffAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, ".flowbee", "diff.patch")
	if err := writeReviewDiffArtifact(p, &client.LeaseContext{}); err != nil {
		t.Fatalf("legacy missing diff write: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("legacy missing diff should remain absent, stat err=%v", err)
	}
}

func TestRenderReviewBriefCodeReviewer(t *testing.T) {
	c := &client.LeaseContext{Identity: "senior_code_reviewer", Lens: "correctness", Task: "Add CHANGELOG", Diff: "MY_UNIQUE_DIFF_BODY"}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	// the diff is now embedded INLINE in the brief (not just referenced by file path) so the
	// agent always sees the change and cannot review blind.
	for _, want := range []string{"code_reviewer", "senior_code_reviewer", "full unified diff", "MY_UNIQUE_DIFF_BODY", "$FLOWBEE_VERDICT_FILE", "approved|changes_requested", "Add CHANGELOG"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("code_reviewer brief missing %q\n%s", want, brief)
		}
	}
	// an empty diff falls back to the $FLOWBEE_DIFF_FILE reference
	if fb := renderReviewBrief("job-1", "code_reviewer", &client.LeaseContext{Diff: ""}); !strings.Contains(fb, "$FLOWBEE_DIFF_FILE") {
		t.Fatalf("empty-diff brief should fall back to $FLOWBEE_DIFF_FILE\n%s", fb)
	}
}

// TestRenderReviewBriefCapsOversizedDiff is the regression for russ #3388's chronic
// needs_human bounce: a big diff embedded inline makes the rendered brief the single shell
// argv passed to the review agent (`codex exec "$(cat "$FLOWBEE_TASK_FILE")"`), and Linux's
// MAX_ARG_STRLEN caps any ONE argv string at ~128KiB regardless of the much larger total
// ARG_MAX — confirmed live: a real 269,705-byte review brief for PR #3396 (a large doc-sweep
// diff) made every single reviewer's `codex exec` invocation fail at exec() with "Argument
// list too long" (exit 126), night after night, before the agent ever ran. A diff at/above
// maxTotalBriefBytes must fall back to the $FLOWBEE_DIFF_FILE reference (with an explicit
// "you must read this file" instruction) instead of being embedded, so the review can still
// launch; a diff comfortably under the cap stays inline (the original spurious-bounce fix).
func TestRenderReviewBriefCapsOversizedDiff(t *testing.T) {
	small := strings.Repeat("x", 100)
	fb := renderReviewBrief("job-1", "code_reviewer", &client.LeaseContext{Diff: small})
	if !strings.Contains(fb, small) {
		t.Fatalf("a small diff (%d bytes, well under the cap) must stay inline\n%s", len(small), fb)
	}

	huge := strings.Repeat("x", maxTotalBriefBytes+1)
	fb = renderReviewBrief("job-1", "code_reviewer", &client.LeaseContext{Diff: huge})
	if strings.Contains(fb, huge) {
		t.Fatalf("a diff over maxTotalBriefBytes (%d) must NOT be embedded inline — it would "+
			"blow the OS exec argv-string limit and fail every review attempt", len(huge))
	}
	if !strings.Contains(fb, "$FLOWBEE_DIFF_FILE") || !strings.Contains(fb, "MUST open and read") {
		t.Fatalf("an oversized diff must fall back to a forceful $FLOWBEE_DIFF_FILE read instruction\n%s", fb)
	}
}

// TestRenderReviewBriefNonEpicPRUnaffected proves the epic-lane Phase 3 brief
// injection is a complete no-op when the lease context carries no epic criteria
// (the overwhelmingly common, non-epic-PR review) — byte-identical to the brief a
// pre-Phase-3 build would have rendered from the same non-epic fields.
func TestRenderReviewBriefNonEpicPRUnaffected(t *testing.T) {
	withEpic := &client.LeaseContext{Identity: "r", Task: "t", Diff: "d"}
	withoutEpicFields := &client.LeaseContext{Identity: "r", Task: "t", Diff: "d"}
	got := renderReviewBrief("job-1", "code_reviewer", withEpic)
	want := renderReviewBrief("job-1", "code_reviewer", withoutEpicFields)
	if got != want {
		t.Fatalf("a lease context with no EpicCriteria must render byte-identically:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(got, "Epic Contract") {
		t.Fatalf("brief should not mention the Epic Contract section at all when EpicCriteria is empty\n%s", got)
	}
}

// TestRenderReviewBriefInjectsEpicCriteria: a code_reviewer job carrying epic
// criteria gets the structured "Epic Contract" section, including the claimed status
// checklist, when it comfortably fits the size cap.
func TestRenderReviewBriefInjectsEpicCriteria(t *testing.T) {
	c := &client.LeaseContext{
		Identity: "r", Task: "t", Diff: "d",
		EpicCriteria:  "**Goal:**\n\nShip the thing.\n\n1. step one\n   Validate: go test ./...\n",
		EpicChecklist: "State: done\n\n- [x] Step 1 — step one (evidence: go test passed)\n",
	}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	for _, want := range []string{
		"Epic Contract", "Ship the thing.", "step one", "Validate: go test ./...",
		"Claimed status", "State: done", "[x] Step 1", "go test passed",
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("epic brief missing %q\n%s", want, brief)
		}
	}
}

// TestRenderReviewBriefTruncatesEpicChecklistNotCriteria: when the FULL epic section
// (fixed criteria + checklist) would blow the total brief cap, the FIXED
// Goal/Constraints/Steps criteria stays intact and only the CHECKLIST is truncated,
// with an explicit note — the brief never silently drops the epic's own contract, and
// never exceeds maxTotalBriefBytes (the argv-limit guard every brief must respect).
func TestRenderReviewBriefTruncatesEpicChecklistNotCriteria(t *testing.T) {
	criteria := "**Goal:**\n\nShip the thing UNIQUE_GOAL_MARKER.\n\n"
	giantChecklist := strings.Repeat("- [x] Step N — done (evidence: blah blah blah)\n", 10000) // huge
	c := &client.LeaseContext{
		Identity: "r", Task: "t", Diff: "d",
		EpicCriteria:  criteria,
		EpicChecklist: giantChecklist,
	}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	if len(brief) > maxTotalBriefBytes {
		t.Fatalf("rendered brief (%d bytes) must never exceed maxTotalBriefBytes (%d) — argv limit", len(brief), maxTotalBriefBytes)
	}
	if !strings.Contains(brief, "UNIQUE_GOAL_MARKER") {
		t.Fatalf("the FIXED criteria (Goal/Constraints/Steps) must survive truncation intact\n%s", brief[:2000])
	}
	if !strings.Contains(brief, "TRUNCATED") {
		t.Fatalf("an oversized checklist must be truncated WITH an explicit note, not silently cut\n%s", brief[len(brief)-2000:])
	}
	if strings.Contains(brief, giantChecklist) {
		t.Fatal("the full giant checklist must NOT be embedded whole")
	}
}

// TestRenderReviewBriefTruncatesGiantEpicCriteria (review F5): the criteria section
// is only "fixed" per epic, not fixed in SIZE — a pathological Steps list can alone
// exceed the total cap, so the criteria must ALSO truncate (with a note) rather than
// being written unconditionally past the argv limit.
func TestRenderReviewBriefTruncatesGiantEpicCriteria(t *testing.T) {
	giantCriteria := "UNIQUE_CRITERIA_HEAD_MARKER\n" +
		strings.Repeat("999. do a pathological amount of step text here\n   Validate: go test ./...\n", 8000)
	c := &client.LeaseContext{
		Identity: "r", Task: "t", Diff: "d",
		EpicCriteria:  giantCriteria,
		EpicChecklist: "State: done\n- [x] Step 1 — x (evidence: y)\n",
	}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	if len(brief) > maxTotalBriefBytes {
		t.Fatalf("rendered brief (%d bytes) must never exceed maxTotalBriefBytes (%d) even for a giant criteria", len(brief), maxTotalBriefBytes)
	}
	if !strings.Contains(brief, "UNIQUE_CRITERIA_HEAD_MARKER") {
		t.Fatal("the criteria's head must survive (truncated from the tail, not dropped)")
	}
	if !strings.Contains(brief, "criteria TRUNCATED") {
		t.Fatal("a truncated criteria must carry an explicit note")
	}
	if !strings.Contains(brief, "Claimed status") {
		t.Fatal("the claimed-status section header must survive even when the criteria was truncated")
	}
}

// TestRenderReviewBriefEpicTruncationIsRuneSafe (review F5): a byte-count cut can
// land mid-rune (the epic checklist format itself uses multi-byte em dashes), and a
// naive s[:budget] slice would leave invalid UTF-8 in the rendered brief.
func TestRenderReviewBriefEpicTruncationIsRuneSafe(t *testing.T) {
	// worst case: the truncatable content is ALL multi-byte runes, so any misaligned
	// byte cut is guaranteed to split one.
	c := &client.LeaseContext{
		Identity: "r", Task: "t", Diff: "d",
		EpicCriteria:  strings.Repeat("—", 200_000),
		EpicChecklist: strings.Repeat("—", 200_000),
	}
	brief := renderReviewBrief("job-1", "code_reviewer", c)
	if len(brief) > maxTotalBriefBytes {
		t.Fatalf("rendered brief (%d bytes) exceeds the cap", len(brief))
	}
	if !utf8.ValidString(brief) {
		t.Fatal("truncation split a multi-byte rune: the rendered brief is not valid UTF-8")
	}
}

// TestRenderReviewBriefCapsOnTotalBudgetNotDiffAlone is the regression for the escaped case
// that TestRenderReviewBriefCapsOversizedDiff's diff-only accounting missed live: job
// 01KWMSKDKAV3WC9QZ4Q20B0N8E had diff_bytes=96559 (comfortably under a 100KiB diff-only cap)
// but task_bytes=20934 + spec_bytes=20934 + ac_bytes=662 pushed the TOTAL rendered brief past
// the ~128KiB exec argv wall anyway — "Argument list too long" on every reviewer across all
// three fleet boxes despite the diff being "capped". The inline decision must budget against
// the brief rendered SO FAR (task+spec+acceptance-criteria+boilerplate), not the diff in
// isolation, so oversized surrounding text correctly eats into the diff's inline allowance.
func TestRenderReviewBriefCapsOnTotalBudgetNotDiffAlone(t *testing.T) {
	bigText := strings.Repeat("y", 30*1024)
	diff := strings.Repeat("x", 90*1024) // under maxTotalBriefBytes alone, but not combined
	c := &client.LeaseContext{Task: bigText, Spec: bigText, AcceptanceCriteria: bigText, Diff: diff}
	fb := renderReviewBrief("job-1", "code_reviewer", c)
	if strings.Contains(fb, diff) {
		t.Fatalf("a diff that fits alone but blows the TOTAL brief budget (task+spec+ac=%d, diff=%d) "+
			"must NOT be embedded inline", 3*len(bigText), len(diff))
	}
	if !strings.Contains(fb, "$FLOWBEE_DIFF_FILE") || !strings.Contains(fb, "MUST open and read") {
		t.Fatalf("must fall back to the forceful $FLOWBEE_DIFF_FILE read instruction\n(brief omitted for size)")
	}
}

func TestRenderReviewBriefSpecReviewer(t *testing.T) {
	c := &client.LeaseContext{Identity: "engineering_manager", Spec: "# Feature\n\nbody"}
	brief := renderReviewBrief("job-2", "spec_reviewer", c)
	for _, want := range []string{"issue-reviewer", "signed_off|amended|needs_design", "# Feature", "$FLOWBEE_VERDICT_FILE"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("spec_reviewer brief missing %q\n%s", want, brief)
		}
	}
}

// TestRenderReviewBriefSpecAuthor pins the imperative file-write instruction: the
// first live spec_author runs re-claimed because a generic `claude -p` agent emitted
// the spec to stdout instead of writing $FLOWBEE_SPEC_FILE. The brief must tell it to
// WRITE the file (with a concrete shell example) and not just print — and explicitly
// warn that a run writing nothing is discarded.
func TestRenderReviewBriefSpecAuthor(t *testing.T) {
	c := &client.LeaseContext{Identity: "product_speccer", Task: "Add a board command", AcceptanceCriteria: "prints a table"}
	brief := renderReviewBrief("job-3", "spec_author", c)
	for _, want := range []string{"$FLOWBEE_SPEC_FILE", "Add a board command", "prints a table"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("spec_author brief missing %q\n%s", want, brief)
		}
	}
	// it must be imperative about writing the file, not printing.
	if !strings.Contains(brief, "REQUIRED") || !strings.Contains(brief, "Do NOT just print") {
		t.Fatalf("spec_author brief must force a file write, got:\n%s", brief)
	}
}
