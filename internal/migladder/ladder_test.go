package migladder

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleLadder is a minimal well-formed ladder: a couple of ordinary numbers, a
// grandfathered double (0023), and a forward reservation with no file yet (0027).
const sampleLadder = `# Migration number ladder

prose that mentions 0023/0024 and 0027 must NOT be parsed as entries.

<!-- ladder:reserved:begin -->
` + "```text" + `
0001_init
0023_adopted_pr_diff_empty
0023_self_unblock
0026_epics
0027_epic_attention
` + "```" + `
<!-- ladder:reserved:end -->

trailing prose 0099_ignored is also not an entry (it has no owning block).
`

func writeLadder(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "LADDER.md")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeMigrations(t *testing.T, stems ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, s := range stems {
		if err := os.WriteFile(filepath.Join(dir, s+".sql"), []byte("-- x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func ladderWith(stems ...string) string {
	return "# Migration number ladder\n\n" + beginMarker + "\n```text\n" +
		strings.Join(stems, "\n") + "\n```\n" + endMarker + "\n"
}

func baseSetFromDir(t *testing.T, dir string) BaseSet {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	set := NewBaseSet()
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			set[strings.TrimSuffix(entry.Name(), ".sql")] = struct{}{}
		}
	}
	return set
}

func TestParseIgnoresProseAndFences(t *testing.T) {
	l, err := Parse([]byte(sampleLadder))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(l.Entries), 5; got != want {
		t.Fatalf("got %d entries, want %d: %+v", got, want, l.Entries)
	}
	if l.MaxNumber() != 27 {
		t.Fatalf("MaxNumber = %d, want 27", l.MaxNumber())
	}
	if l.numberCounts()[23] != 2 {
		t.Fatalf("expected 0023 recorded twice (grandfathered double), got %d", l.numberCounts()[23])
	}
}

func TestParseRejectsMissingMarkers(t *testing.T) {
	if _, err := Parse([]byte("# no markers here\n0001_init\n")); err == nil {
		t.Fatal("expected error for missing markers")
	}
}

func TestParseRejectsMalformedEntry(t *testing.T) {
	bad := strings.Replace(sampleLadder, "0001_init", "1_init", 1) // not NNNN_
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected error for malformed reserved line")
	}
}

func TestReserveAppendsNextNumber(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	stem, err := Reserve(p, "my_thing")
	if err != nil {
		t.Fatal(err)
	}
	if stem != "0028_my_thing" {
		t.Fatalf("Reserve = %q, want 0028_my_thing", stem)
	}
	// second reservation advances to 0029 and the file still parses (block intact).
	stem2, err := Reserve(p, "other")
	if err != nil {
		t.Fatal(err)
	}
	if stem2 != "0029_other" {
		t.Fatalf("second Reserve = %q, want 0029_other", stem2)
	}
	l, err := ParseFile(p)
	if err != nil {
		t.Fatalf("ladder no longer parses after reserve: %v", err)
	}
	if l.MaxNumber() != 29 {
		t.Fatalf("MaxNumber after two reserves = %d, want 29", l.MaxNumber())
	}
	if !l.stems()["0028_my_thing"] || !l.stems()["0029_other"] {
		t.Fatalf("reserved stems missing after append: %+v", l.Entries)
	}
}

func TestReserveRejectsInvalidSlug(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	for _, bad := range []string{"", "-leading", "Upper", "has space", "sym!bol", "_leading_underscore"} {
		if _, err := Reserve(p, bad); err == nil {
			t.Errorf("Reserve(%q) succeeded; want rejection", bad)
		}
	}
}

func TestCheckCleanTree(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	// files are a strict subset of the ladder (0027 is reserved but has no file yet).
	dir := writeMigrations(t, "0001_init", "0023_adopted_pr_diff_empty", "0023_self_unblock", "0026_epics")
	v, err := Check(dir, p, NewBaseSet("0001_init", "0023_adopted_pr_diff_empty", "0023_self_unblock", "0026_epics"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations, got: %v", v)
	}
}

func TestCheckFailsOnUnreservedNumber(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	dir := writeMigrations(t, "0001_init", "0028_sneaky") // 0028 not in ladder
	v, err := Check(dir, p, NewBaseSet("0001_init"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) == 0 || !strings.Contains(strings.Join(v, "\n"), "0028_sneaky") {
		t.Fatalf("expected a not-registered violation for 0028_sneaky, got: %v", v)
	}
}

// TestCheckRejectsDuplicateNumber is the plan's Phase-4 acceptance for the
// allocator: a migration re-picking a number already taken (here a second 0026
// with a different slug) is rejected — both the not-registered message and the
// explicit duplicate-number message fire.
func TestCheckRejectsDuplicateNumber(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	dir := writeMigrations(t, "0026_epics", "0026_collision") // duplicate 0026, only one sanctioned
	v, err := Check(dir, p, NewBaseSet("0026_epics"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v, "\n")
	if !strings.Contains(joined, "0026_collision") {
		t.Errorf("expected not-registered violation for the colliding file, got: %v", v)
	}
	if !strings.Contains(joined, "number 0026 is used by 2 files") {
		t.Errorf("expected explicit duplicate-number violation, got: %v", v)
	}
}

// TestCheckAllowsGrandfatheredDouble proves the sanctioned historical double
// (two 0023 files, both recorded in the ladder) does NOT trip the duplicate guard.
func TestCheckAllowsGrandfatheredDouble(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	dir := writeMigrations(t, "0023_adopted_pr_diff_empty", "0023_self_unblock")
	v, err := Check(dir, p, NewBaseSet("0023_adopted_pr_diff_empty", "0023_self_unblock"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("grandfathered 0023 double should pass, got: %v", v)
	}
}

func TestCheckFlagsBadFilename(t *testing.T) {
	p := writeLadder(t, sampleLadder)
	dir := writeMigrations(t, "0001_init", "not_a_migration")
	v, err := Check(dir, p, NewBaseSet("0001_init"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(v, "\n"), "naming convention") {
		t.Fatalf("expected a naming-convention violation, got: %v", v)
	}
}

// TestRealLadderMatchesRealMigrations guards the committed tree itself: the seeded
// LADDER.md must register every real migrations/*.sql (this is what CI enforces).
func TestRealLadderMatchesRealMigrations(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "store", "migrations")
	ladder := filepath.Join(dir, "LADDER.md")
	v, err := Check(dir, ladder, baseSetFromDir(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("committed ladder has violations against the committed migrations: %v", v)
	}
}

func TestCheckBaseAwareAllowsGrandfatheredHistoryAndNextBatch(t *testing.T) {
	stems := []string{
		"0001_init",
		"0023_old_a",
		"0023_old_b",
		"0026_previous",
		"0027_first_new",
		"0028_second_new",
		"0031_after_reserved_gap",
	}
	p := writeLadder(t, ladderWith(stems...))
	dir := writeMigrations(t, stems...)
	base := NewBaseSet("0001_init", "0023_old_a", "0023_old_b", "0026_previous")

	v, err := Check(dir, p, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("next candidate batch should pass while preserving base duplicate and reserved gaps: %v", v)
	}
}

func TestCheckBaseAwareRejectsBackfillAtOrBelowBaseMax(t *testing.T) {
	stems := []string{"0001_init", "0025_backfill", "0026_previous"}
	p := writeLadder(t, ladderWith(stems...))
	dir := writeMigrations(t, stems...)

	v, err := Check(dir, p, NewBaseSet("0001_init", "0026_previous"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v, "\n")
	if !strings.Contains(joined, "at or below max(base) 0026") {
		t.Fatalf("expected base-backfill violation, got: %v", v)
	}
}

func TestCheckBaseAwareRequiresCandidateToStartAtNextNumber(t *testing.T) {
	stems := []string{"0001_init", "0026_previous", "0028_skipped_next"}
	p := writeLadder(t, ladderWith(stems...))
	dir := writeMigrations(t, stems...)

	v, err := Check(dir, p, NewBaseSet("0001_init", "0026_previous"))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v, "\n")
	if !strings.Contains(joined, "first new migration is 0028, want exactly 0027") {
		t.Fatalf("expected max(base)+1 violation, got: %v", v)
	}
}

func TestCheckBaseAwareRejectsNewDuplicateEvenWhenLadderRecordsBoth(t *testing.T) {
	stems := []string{"0001_init", "0026_previous", "0027_branch_a", "0027_branch_b"}
	p := writeLadder(t, ladderWith(stems...))
	dir := writeMigrations(t, stems...)

	v, err := Check(dir, p, NewBaseSet("0001_init", "0026_previous"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(v, "\n"), "only duplicates wholly present at the merge base are grandfathered") {
		t.Fatalf("expected new-duplicate violation, got: %v", v)
	}
}

func TestCheckMergeOrderRejectsSecondPRUntilRenumbered(t *testing.T) {
	// branch_a has already merged as 0027. A stale branch that also reserved
	// 0027 must fail after rebase even if it carries both ladder entries.
	stems := []string{"0001_init", "0026_previous", "0027_branch_a", "0027_branch_b"}
	p := writeLadder(t, ladderWith(stems...))
	dir := writeMigrations(t, stems...)
	base := NewBaseSet("0001_init", "0026_previous", "0027_branch_a")

	v, err := Check(dir, p, base)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v, "\n")
	if !strings.Contains(joined, "at or below max(base) 0027") ||
		!strings.Contains(joined, "new migration number 0027 is duplicated") {
		t.Fatalf("expected stale second PR to require rebase+renumber, got: %v", v)
	}
}

func TestCheckBaseAwareRejectsDeletedMigrationHistory(t *testing.T) {
	p := writeLadder(t, ladderWith("0001_init", "0026_previous"))
	dir := writeMigrations(t, "0001_init")

	v, err := Check(dir, p, NewBaseSet("0001_init", "0026_previous"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(v, "\n"), "base migration 0026_previous.sql was deleted or renamed") {
		t.Fatalf("expected immutable-history violation, got: %v", v)
	}
}

func TestCheckRequiresBaseSet(t *testing.T) {
	p := writeLadder(t, ladderWith("0001_init"))
	dir := writeMigrations(t, "0001_init")
	if _, err := Check(dir, p, nil); err == nil || !strings.Contains(err.Error(), "base set is required") {
		t.Fatalf("expected missing-base-set error, got %v", err)
	}
}

// repoRoot walks up from the test's CWD to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			t.Fatal("could not find repo root (go.mod)")
		}
		d = parent
	}
}
