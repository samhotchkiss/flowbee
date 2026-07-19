// Command laddercheck enforces the migration-number ladder (epic-lane plan
// §12.6): every internal/store/migrations/*.sql must carry a number registered in
// LADDER.md, and no number may be duplicated on disk beyond a sanctioned
// grandfathered double. It FAILS a PR that introduces an unreserved or colliding
// migration number, backfills at/below the merge-base maximum, or does not start
// its new sequence at max(base)+1 — the number-space analogue of archcheck's
// boundary gate, closing the self-inflicted 0023/0024 collision hole before
// parallel epic builders start.
//
// CI runs this alongside archcheck and providerlint; `make laddercheck` runs it
// locally. Usage: laddercheck [migrationsDir [ladderPath]] — defaults to the
// repo-relative locations when run from the repo root.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/samhotchkiss/flowbee/internal/migladder"
)

func main() {
	migrationsDir := migladder.DefaultMigrationsDir()
	ladderPath := migladder.DefaultLadderPath()
	if len(os.Args) > 1 {
		migrationsDir = os.Args[1]
	}
	if len(os.Args) > 2 {
		ladderPath = os.Args[2]
	}

	baseSet, mergeBase, err := migrationBaseSet(migrationsDir)
	if err != nil {
		fmt.Printf("laddercheck: resolve origin/main merge base: %v\n", err)
		os.Exit(1)
	}

	violations, err := migladder.Check(migrationsDir, ladderPath, baseSet)
	if err != nil {
		fmt.Printf("laddercheck: %v\n", err)
		os.Exit(1)
	}
	if len(violations) > 0 {
		for _, v := range violations {
			fmt.Printf("VIOLATION: %s\n", v)
		}
		fmt.Printf("laddercheck: %d migration-ladder violation(s)\n", len(violations))
		os.Exit(1)
	}
	shortBase := mergeBase
	if len(shortBase) > 12 {
		shortBase = shortBase[:12]
	}
	fmt.Printf("laddercheck: migration ladder clean against base %s\n", shortBase)
}

// migrationBaseSet resolves the exact merge base required by the forward-only
// ladder gate and reads migration names from that tree, not from the working
// tree. It deliberately fails closed when origin/main or history is unavailable;
// CI checks out full history so a shallow/default fallback can never silently
// weaken merge-order enforcement.
func migrationBaseSet(migrationsDir string) (migladder.BaseSet, string, error) {
	repoRoot, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, "", err
	}
	repoRoot = strings.TrimSpace(repoRoot)

	absMigrations, err := filepath.Abs(migrationsDir)
	if err != nil {
		return nil, "", fmt.Errorf("absolute migrations path: %w", err)
	}
	relMigrations, err := filepath.Rel(repoRoot, absMigrations)
	if err != nil {
		return nil, "", fmt.Errorf("migrations path relative to repository: %w", err)
	}
	if relMigrations == ".." || strings.HasPrefix(relMigrations, ".."+string(filepath.Separator)) {
		return nil, "", fmt.Errorf("migrations directory %s is outside repository %s", absMigrations, repoRoot)
	}

	mergeBase, err := gitOutput(repoRoot, "merge-base", "origin/main", "HEAD")
	if err != nil {
		return nil, "", err
	}
	mergeBase = strings.TrimSpace(mergeBase)
	listing, err := gitOutput(repoRoot, "ls-tree", "-r", "--name-only", mergeBase, "--", filepath.ToSlash(relMigrations))
	if err != nil {
		return nil, "", err
	}

	baseSet := migladder.NewBaseSet()
	for _, path := range strings.Split(strings.TrimSpace(listing), "\n") {
		if path == "" || !strings.HasSuffix(path, ".sql") {
			continue
		}
		stem := strings.TrimSuffix(filepath.Base(filepath.FromSlash(path)), ".sql")
		baseSet[stem] = struct{}{}
	}
	return baseSet, mergeBase, nil
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(out))
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return string(out), nil
}
