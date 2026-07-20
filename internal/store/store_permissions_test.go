package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
)

func TestOpenHardensDatabaseAndSQLiteSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "flowbee.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB.Exec(`CREATE TABLE permission_probe (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("stat %s: %v", candidate, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode=%04o want 0600", candidate, got)
		}
	}
}
