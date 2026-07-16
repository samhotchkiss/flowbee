// Package migladder implements the migration-number ladder (epic-lane plan
// §12.6): a reserved-number allocator + collision check that closes the
// self-inflicted double-migration hole (the fleet already collided on 0023 and
// 0024 because concurrent branches silently picked the same NUMBER — the runtime
// keys migrations on full FILENAME so both applied, but the number space is a
// shared resource with no arbiter).
//
// The ladder file (internal/store/migrations/LADDER.md) is the single source of
// truth for which numbers are taken. `flowbee migration reserve <slug>` appends
// the next free number under a file lock so parallel builders serialize on the
// allocation; tools/laddercheck (run in CI, like archcheck/providerlint) fails a
// PR whose migrations/*.sql introduces a number absent from the ladder or an
// unsanctioned duplicate.
//
// This package is pure over its inputs (a ladder path + a migrations dir); it
// takes no clock and no randomness, matching the deterministic-core posture.
package migladder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Markers delimit the machine-managed reserved-number block inside LADDER.md.
// Everything outside them is prose the allocator never touches; everything
// inside (that matches entryRe) is an authoritative reservation.
const (
	beginMarker = "<!-- ladder:reserved:begin -->"
	endMarker   = "<!-- ladder:reserved:end -->"
)

// entryRe matches one reserved entry line: the exact migration filename stem
// (NNNN_slug, no .sql). A prose line — even one mentioning "0023/0024" — does not
// match (it requires the WHOLE trimmed line to be a stem), so parsing is robust
// to documentation living in the same file.
var entryRe = regexp.MustCompile(`^([0-9]{4})_([a-z0-9][a-z0-9_]*)$`)

// slugRe gates a slug a caller asks to reserve: lowercase alnum + underscore,
// leading alnum. Keeps a reserved filename shell/glob-safe and consistent with
// every existing migration name.
var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_]*$`)

// Entry is one reserved migration number.
type Entry struct {
	Number int    // the NNNN
	Slug   string // the part after NNNN_
	Stem   string // NNNN_slug (the .sql filename without extension)
}

// Ladder is the parsed reserved-number block.
type Ladder struct {
	Entries []Entry
}

// Parse extracts the reserved entries from LADDER.md content. It requires the
// begin/end markers to be present and well ordered, and rejects a line inside
// the block that is neither blank, a code fence, nor a valid entry — so a
// typo'd reservation fails loud rather than silently dropping a number.
func Parse(content []byte) (*Ladder, error) {
	text := string(content)
	bi := strings.Index(text, beginMarker)
	if bi < 0 {
		return nil, fmt.Errorf("ladder: missing %q marker", beginMarker)
	}
	ei := strings.Index(text, endMarker)
	if ei < 0 {
		return nil, fmt.Errorf("ladder: missing %q marker", endMarker)
	}
	if ei < bi {
		return nil, fmt.Errorf("ladder: %q appears before %q", endMarker, beginMarker)
	}
	block := text[bi+len(beginMarker) : ei]

	var entries []Entry
	sc := bufio.NewScanner(strings.NewReader(block))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "```") {
			continue // blank or a code-fence line wrapping the block for readability
		}
		m := entryRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("ladder: unparseable reserved line %q (want NNNN_slug)", line)
		}
		n, _ := strconv.Atoi(m[1])
		entries = append(entries, Entry{Number: n, Slug: m[2], Stem: line})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("ladder: scan reserved block: %w", err)
	}
	return &Ladder{Entries: entries}, nil
}

// ParseFile reads and parses a ladder file.
func ParseFile(path string) (*Ladder, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// MaxNumber is the highest reserved number (0 for an empty ladder).
func (l *Ladder) MaxNumber() int {
	max := 0
	for _, e := range l.Entries {
		if e.Number > max {
			max = e.Number
		}
	}
	return max
}

// stems returns the set of reserved filename stems.
func (l *Ladder) stems() map[string]bool {
	s := make(map[string]bool, len(l.Entries))
	for _, e := range l.Entries {
		s[e.Stem] = true
	}
	return s
}

// numberCounts returns how many reserved entries share each number (>1 marks a
// number the ladder sanctions as a grandfathered double, e.g. the historical
// 0023/0024).
func (l *Ladder) numberCounts() map[int]int {
	c := make(map[int]int, len(l.Entries))
	for _, e := range l.Entries {
		c[e.Number]++
	}
	return c
}

// Check compares the migrations directory against the ladder and returns a
// (possibly empty) list of human-readable violations. It fails a migration whose
// number is absent from the ladder or whose number is duplicated on disk beyond
// what the ladder records as a sanctioned double. Reservations with no file yet
// (a number reserved ahead of writing the .sql) are NOT violations — the check
// is files ⊆ ladder, not the reverse.
func Check(migrationsDir, ladderPath string) ([]string, error) {
	l, err := ParseFile(ladderPath)
	if err != nil {
		return nil, err
	}
	// ladder-internal sanity: every reserved stem must itself be well-formed
	// (Parse already enforces this) — nothing more to check on the ladder alone.

	dirEntries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	stems := l.stems()
	ladderNumCount := l.numberCounts()

	var fileStems []string
	fileNumCount := map[int]int{}
	var violations []string
	for _, de := range dirEntries {
		name := de.Name()
		if de.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		stem := strings.TrimSuffix(name, ".sql")
		m := entryRe.FindStringSubmatch(stem)
		if m == nil {
			violations = append(violations, fmt.Sprintf(
				"migration %q does not match the NNNN_slug naming convention", name))
			continue
		}
		n, _ := strconv.Atoi(m[1])
		fileStems = append(fileStems, stem)
		fileNumCount[n]++
		if !stems[stem] {
			violations = append(violations, fmt.Sprintf(
				"migration %s is not registered in LADDER.md — reserve it with `flowbee migration reserve %s`",
				name, m[2]))
		}
	}

	// duplicate-number guard: a number used by >=2 files is allowed ONLY if the
	// ladder records at least that many entries for it (the grandfathered doubles).
	// A new duplicate — a builder re-picking a taken number — is caught here with a
	// number-specific message even though the absent-check above also fires.
	dupNums := make([]int, 0)
	for n, c := range fileNumCount {
		if c >= 2 && ladderNumCount[n] < c {
			dupNums = append(dupNums, n)
		}
	}
	sort.Ints(dupNums)
	for _, n := range dupNums {
		violations = append(violations, fmt.Sprintf(
			"migration number %04d is used by %d files but LADDER.md sanctions only %d — a new number collision (never renumber applied migrations; reserve a fresh number)",
			n, fileNumCount[n], ladderNumCount[n]))
	}

	sort.Strings(violations)
	return violations, nil
}

// Reserve atomically appends the next free number for slug to the ladder file and
// returns the reserved filename stem (NNNN_slug). It holds an exclusive advisory
// lock (flock) on the ladder file across read→compute→write so two concurrent
// `flowbee migration reserve` invocations serialize — the whole point of the
// ladder (parallel builders must not both grab the same number). The write is
// performed in place while the lock is held.
func Reserve(ladderPath, slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if !slugRe.MatchString(slug) {
		return "", fmt.Errorf("invalid slug %q: want lowercase alnum + underscore, leading alnum (e.g. epic_attention)", slug)
	}

	f, err := os.OpenFile(ladderPath, os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("open ladder: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock ladder: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	content, err := os.ReadFile(ladderPath)
	if err != nil {
		return "", fmt.Errorf("read ladder: %w", err)
	}
	l, err := Parse(content)
	if err != nil {
		return "", err
	}
	stem := fmt.Sprintf("%04d_%s", l.MaxNumber()+1, slug)
	if l.stems()[stem] {
		return "", fmt.Errorf("ladder already reserves %s", stem)
	}

	updated, err := insertEntry(content, stem)
	if err != nil {
		return "", err
	}
	if err := f.Truncate(0); err != nil {
		return "", fmt.Errorf("truncate ladder: %w", err)
	}
	if _, err := f.WriteAt(updated, 0); err != nil {
		return "", fmt.Errorf("write ladder: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync ladder: %w", err)
	}
	return stem, nil
}

// insertEntry places a new stem line immediately after the last existing entry
// line inside the reserved block (which keeps it before any closing code fence),
// preserving the surrounding prose and markers byte-for-byte.
func insertEntry(content []byte, stem string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	begin, end := -1, -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == beginMarker {
			begin = i
		}
		if t == endMarker {
			end = i
			break
		}
	}
	if begin < 0 || end < 0 || end < begin {
		return nil, fmt.Errorf("ladder: reserved block markers missing or out of order")
	}
	// last entry-matching line within (begin, end)
	insertAt := begin + 1
	for i := begin + 1; i < end; i++ {
		if entryRe.MatchString(strings.TrimSpace(lines[i])) {
			insertAt = i + 1
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:insertAt]...)
	out = append(out, stem)
	out = append(out, lines[insertAt:]...)
	return []byte(strings.Join(out, "\n")), nil
}

// DefaultMigrationsDir / DefaultLadderPath are the repo-relative locations, used
// by the CLI and the checker when run from the repo root (the archcheck/
// providerlint convention).
func DefaultMigrationsDir() string { return filepath.Join("internal", "store", "migrations") }
func DefaultLadderPath() string    { return filepath.Join(DefaultMigrationsDir(), "LADDER.md") }
