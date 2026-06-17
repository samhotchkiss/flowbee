package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/testutil"
)

// TestAdoptIssueReadoptsAfterCancel: a CANCELLED job (abandoned work) must not block an
// issue from being re-adopted on a re-label — but a live or merged job still does. This
// fixes the dead end where a buggy-epic run's cancelled job permanently blocked intake.
func TestAdoptIssueReadoptsAfterCancel(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Unix(1000, 0)

	id1, err := st.AdoptIssueAsBuild(ctx, "flowbee", 47, "glossary", "body", "base", now)
	if err != nil || id1 == "" {
		t.Fatalf("first adopt: id=%q err=%v", id1, err)
	}
	// a live job tracks the issue -> a re-adopt is a no-op (no duplicate).
	if id2, _ := st.AdoptIssueAsBuild(ctx, "flowbee", 47, "glossary", "body", "base", now); id2 != "" {
		t.Fatalf("re-adopt while the job is live must be a no-op, got %q", id2)
	}
	// cancel it (abandoned) -> the issue is re-adoptable.
	if _, err := st.DB.ExecContext(ctx, `UPDATE jobs SET state='cancelled' WHERE id=?`, id1); err != nil {
		t.Fatal(err)
	}
	id3, err := st.AdoptIssueAsBuild(ctx, "flowbee", 47, "glossary", "body", "base", now)
	if err != nil || id3 == "" || id3 == id1 {
		t.Fatalf("re-adopt after cancel must create a NEW job, got id=%q err=%v", id3, err)
	}
}
