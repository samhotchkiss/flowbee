package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func TestHumanLoginTokenIsHashedOneTimeAndExpires(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 19, 18, 0, 0, 0, time.UTC)
	raw := strings.Repeat("raw-secret-", 4)
	if err := st.CreateHumanLoginToken(ctx, raw, "sam", "browser-1", now.Add(5*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := st.DB.QueryRowContext(ctx, `SELECT token_sha256 FROM human_login_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == raw || strings.Contains(stored, raw) || !strings.HasPrefix(stored, "sha256:") {
		t.Fatalf("database retained bearer material: %q", stored)
	}
	login, err := st.ConsumeHumanLoginToken(ctx, raw, now.Add(time.Minute))
	if err != nil || login.Identity != "sam" || login.SessionID != "browser-1" {
		t.Fatalf("consume = %#v, %v", login, err)
	}
	if _, err := st.ConsumeHumanLoginToken(ctx, raw, now.Add(2*time.Minute)); !errors.Is(err, store.ErrHumanLoginUsed) {
		t.Fatalf("replay error = %v, want used", err)
	}

	expired := strings.Repeat("expired-secret-", 3)
	if err := st.CreateHumanLoginToken(ctx, expired, "sam", "browser-2", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeHumanLoginToken(ctx, expired, now.Add(2*time.Minute)); !errors.Is(err, store.ErrHumanLoginExpired) {
		t.Fatalf("expired error = %v", err)
	}
}
