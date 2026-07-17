package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/acctprobe"
	"github.com/samhotchkiss/flowbee/internal/epicdigest"
	"github.com/samhotchkiss/flowbee/internal/testutil"
)

func claudeResult(key string, session, weekly float64, sev acctprobe.Severity, trust acctprobe.TrustState, at time.Time) acctprobe.Result {
	return acctprobe.Result{
		Identity: acctprobe.Identity{Provider: acctprobe.ProviderClaude, AccountKey: key, Email: "pearl@swh.me"},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindSession, Percent: session, Severity: acctprobe.SeverityNormal, WindowMinutes: 300},
			{Kind: acctprobe.KindWeeklyAll, Percent: weekly, Severity: sev, WindowMinutes: 10080},
		}},
		TrustState: trust,
		CapturedAt: at,
	}
}

func TestUpsertAccountLimits_RoutableWritesAndSyncsWorkerAccounts(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	res := claudeResult("acc-1", 40, 72, acctprobe.SeverityNormal, acctprobe.TrustVerified, now)
	if err := st.UpsertAccountLimits(ctx, res, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	aw, ok, err := st.GetAccountWindow(ctx, "acc-1")
	if err != nil || !ok {
		t.Fatalf("get window: ok=%v err=%v", ok, err)
	}
	if aw.SessionPct != 40 || aw.WeeklyPct != 72 {
		t.Fatalf("percentages: %+v", aw)
	}
	if aw.Severity != "normal" || aw.ProbeStale {
		t.Fatalf("severity/stale: %+v", aw)
	}
	if !aw.Routable() {
		t.Fatalf("verified reading should be routable: %+v", aw)
	}
	if aw.Provider != "claude" || aw.Email != "pearl@swh.me" || aw.FetchedAtMs != now.UnixMilli() {
		t.Fatalf("identity/fetched: %+v", aw)
	}

	// worker_accounts.usage_pct kept in sync as max(session,weekly)=72 (§4.2).
	accts, err := st.AccountsForModel(ctx, "claude")
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	if len(accts) != 1 || accts[0].AccountID != "acc-1" || accts[0].UsagePct != 72 {
		t.Fatalf("worker_accounts sync: %+v", accts)
	}
}

func TestUpsertAccountLimits_NonRoutableNeverOverwritesPercentages(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// A good verified reading lands 40/72 normal.
	if err := st.UpsertAccountLimits(ctx, claudeResult("acc-1", 40, 72, acctprobe.SeverityNormal, acctprobe.TrustVerified, now), now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Later, a STALE reading that (hypothetically) carries CRITICAL windows arrives over
	// a flaky link. It must NOT overwrite the verified 40/72 nor flip severity — only
	// trust_state/probe_stale update (§12.14 stale suppression at the store layer).
	stale := claudeResult("acc-1", 99, 99, acctprobe.SeverityCritical, acctprobe.TrustStale, now.Add(time.Minute))
	if err := st.UpsertAccountLimits(ctx, stale, now.Add(time.Minute)); err != nil {
		t.Fatalf("stale upsert: %v", err)
	}
	aw, _, err := st.GetAccountWindow(ctx, "acc-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if aw.SessionPct != 40 || aw.WeeklyPct != 72 || aw.Severity != "normal" {
		t.Fatalf("stale reading overwrote verified percentages: %+v", aw)
	}
	if !aw.ProbeStale || aw.TrustState != string(acctprobe.TrustStale) {
		t.Fatalf("expected stale flag set: %+v", aw)
	}
	if aw.CriticalNonStale() {
		t.Fatalf("a stale critical must be suppressed (CriticalNonStale=false)")
	}
}

func TestUpsertAccountLimits_NonRoutableDoesNotClearRateLimitPin(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	// Verified reading with a hard rate-limit pins worker_accounts.
	pinned := claudeResult("acc-1", 100, 100, acctprobe.SeverityCritical, acctprobe.TrustVerified, now)
	pinned.Usage.RateLimited = true
	if err := st.UpsertAccountLimits(ctx, pinned, now); err != nil {
		t.Fatalf("pin: %v", err)
	}
	accts, _ := st.AccountsForModel(ctx, "claude")
	if len(accts) != 1 || !accts[0].RateLimited {
		t.Fatalf("expected rate_limited pin: %+v", accts)
	}
	// A non-routable reading must not touch worker_accounts (would clear the 429 pin).
	if err := st.UpsertAccountLimits(ctx, claudeResult("acc-1", 5, 5, acctprobe.SeverityNormal, acctprobe.TrustStale, now.Add(time.Minute)), now.Add(time.Minute)); err != nil {
		t.Fatalf("stale: %v", err)
	}
	accts, _ = st.AccountsForModel(ctx, "claude")
	if !accts[0].RateLimited {
		t.Fatalf("a non-routable reading cleared the 429 pin: %+v", accts)
	}
}

func TestUpsertAccountLimits_HeldMissingKeyRejected(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	res := acctprobe.Result{Identity: acctprobe.Identity{Provider: acctprobe.ProviderClaude}, TrustState: acctprobe.TrustHeld}
	if err := st.UpsertAccountLimits(ctx, res, time.Now()); err == nil {
		t.Fatal("expected an error for a result with no account key")
	}
}

func TestUpsertAccountLimits_WindowsPassthroughVerbatim(t *testing.T) {
	st := testutil.NewStore(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)

	reset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	res := acctprobe.Result{
		Identity: acctprobe.Identity{Provider: acctprobe.ProviderClaude, AccountKey: "acc-1", Email: "pearl@swh.me"},
		Usage: acctprobe.Usage{Windows: acctprobe.Windows{
			{Kind: acctprobe.KindSession, Percent: 40, Severity: acctprobe.SeverityNormal, WindowMinutes: 300},
			{Kind: acctprobe.KindWeeklyAll, Percent: 72, Severity: acctprobe.SeverityNormal, WindowMinutes: 10080, ResetsAt: reset},
			{Kind: acctprobe.KindWeeklyScoped, Percent: 88, Severity: acctprobe.SeverityCritical, Scope: "Fable", WindowMinutes: 10080},
		}},
		TrustState: acctprobe.TrustVerified,
		CapturedAt: now,
	}
	if err := st.UpsertAccountLimits(ctx, res, now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	aw, ok, err := st.GetAccountWindow(ctx, "acc-1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	// the full windows[] is carried verbatim, including the scoped ring's percent+scope.
	if len(aw.Windows) != 3 {
		t.Fatalf("expected 3 windows, got %d: %+v", len(aw.Windows), aw.Windows)
	}
	var scoped *epicdigest.Window
	for i := range aw.Windows {
		if aw.Windows[i].Kind == "weekly_scoped" {
			scoped = &aw.Windows[i]
		}
	}
	if scoped == nil || scoped.Scope != "Fable" || scoped.Percent != 88 || scoped.Severity != "critical" {
		t.Fatalf("scoped window not carried verbatim: %+v", aw.Windows)
	}
	// account is overall critical (the scoped window is critical).
	if aw.Severity != "critical" {
		t.Fatalf("expected overall critical severity, got %q", aw.Severity)
	}
}

func TestListAccountWindows_EmptyIsSliceNotNil(t *testing.T) {
	st := testutil.NewStore(t)
	got, err := st.ListAccountWindows(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got == nil {
		t.Fatal("ListAccountWindows must return a non-nil empty slice ([] never null)")
	}
	if len(got) != 0 {
		t.Fatalf("expected empty, got %d", len(got))
	}
}
