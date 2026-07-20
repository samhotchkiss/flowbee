package store_test

import (
	"context"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestMigration0059CreatesDurableRepoAdmissionHolds(t *testing.T) {
	ctx := context.Background()
	st := testutil.NewStore(t)
	var sqlText string
	if err := st.DB.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type='table' AND name='repo_admission_holds'`).Scan(&sqlText); err != nil {
		t.Fatal(err)
	}
	if sqlText == "" {
		t.Fatal("repo_admission_holds schema is empty")
	}
	// Compile the public resolver against a migrated store as a migration-level
	// proof; an unmapped repository must be typed rather than silently defaulted.
	if _, err := st.ResolveRepoAdmissionProject(ctx, "missing"); err == nil {
		t.Fatal("unmapped repository resolved without error")
	} else if _, ok := err.(*store.RepoAdmissionRoutingError); !ok {
		t.Fatalf("untyped routing error %T: %v", err, err)
	}
}
