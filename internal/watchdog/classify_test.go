package watchdog

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyBlocked_UsageLimitDailySample(t *testing.T) {
	// the exact daily-variant phrasing given in the task brief.
	scrollback := "You've hit your usage limit for the gpt-5.6 model, try again at 10:47 AM"
	now := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)

	got := classifyBlocked(scrollback, now)
	if got.Kind != blockUsageLimit {
		t.Fatalf("kind = %v, want blockUsageLimit", got.Kind)
	}
	if got.Weekly {
		t.Fatalf("weekly = true, want false (same-day clock time)")
	}
	want := time.Date(2026, 7, 3, 10, 47, 0, 0, time.UTC)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", got.ResetAt, want)
	}
}

func TestClassifyBlocked_UsageLimitDailyAlreadyPassedRollsToTomorrow(t *testing.T) {
	scrollback := "You've hit your usage limit, try again at 10:47 AM"
	now := time.Date(2026, 7, 3, 11, 0, 0, 0, time.UTC) // 10:47 AM already passed today

	got := classifyBlocked(scrollback, now)
	want := time.Date(2026, 7, 4, 10, 47, 0, 0, time.UTC)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v (tomorrow)", got.ResetAt, want)
	}
}

// TestClassifyBlocked_TimezoneResolution pins the review-MAJOR-#1 contract: the
// message's "10:47 AM" is BOX-local wall clock, so `now` must arrive already In()
// the box's location and the returned deadline is the box-local instant. The
// west-of-serve case is the dangerous one: serve-zone resolution would compute a
// deadline 7h too EARLY, and the watcher would resume into a still-live cap.
func TestClassifyBlocked_TimezoneResolution(t *testing.T) {
	scrollback := "You've hit your usage limit, try again at 10:47 AM"
	la, err := time.LoadLocation("America/Los_Angeles") // UTC-7 in July (PDT)
	if err != nil {
		t.Skipf("no tzdata available: %v", err)
	}

	// Box WEST of serve: serve-now is 15:00 UTC == 08:00 PDT, so box-local 10:47 AM
	// is still ahead today. The correct deadline is 10:47 PDT == 17:47 UTC; the OLD
	// serve-zone bug would have produced 10:47 UTC — 7h early, mid-cap.
	serveNow := time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC)
	got := classifyBlocked(scrollback, serveNow.In(la))
	want := time.Date(2026, 7, 3, 10, 47, 0, 0, la)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("west-of-serve resetAt = %v, want %v (10:47 box-local == 17:47 UTC)", got.ResetAt, want.UTC())
	}
	if got.ResetAt.Equal(time.Date(2026, 7, 3, 10, 47, 0, 0, time.UTC)) {
		t.Fatalf("resetAt resolved in serve's zone — the exact too-early bug this fixes")
	}

	// same box, but 10:47 AM box-local has ALREADY passed (serve-now 18:30 UTC ==
	// 11:30 PDT) → tomorrow 10:47 box-local, still in the box's zone.
	got = classifyBlocked(scrollback, time.Date(2026, 7, 3, 18, 30, 0, 0, time.UTC).In(la))
	want = time.Date(2026, 7, 4, 10, 47, 0, 0, la)
	if !got.ResetAt.Equal(want) {
		t.Fatalf("box-local already-passed resetAt = %v, want %v (tomorrow box-local)", got.ResetAt, want.UTC())
	}

	// weekly variant also resolves the weekday in the BOX's zone: at 01:00 UTC
	// Friday it is still THURSDAY evening in LA, so "try again Monday" is the LA
	// Monday midnight — not the UTC one.
	weekly := "You've hit your weekly usage limit, try again Monday"
	utcFri := time.Date(2026, 7, 3, 1, 0, 0, 0, time.UTC) // Fri 01:00 UTC == Thu 18:00 PDT
	got = classifyBlocked(weekly, utcFri.In(la))
	want = time.Date(2026, 7, 6, 0, 0, 0, 0, la) // Monday 00:00 box-local
	if !got.Weekly || !got.ResetAt.Equal(want) {
		t.Fatalf("weekly box-local resetAt = %v weekly=%v, want %v", got.ResetAt, got.Weekly, want.UTC())
	}
}

func TestClassifyBlocked_UsageLimitWeeklyVariant(t *testing.T) {
	// exact wording not given in the task brief — best-effort heuristic per the
	// code comment in classify.go. now is a Thursday.
	scrollback := "You've hit your weekly usage limit, try again Monday"
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) // Thursday
	if now.Weekday() != time.Thursday {
		t.Fatalf("test setup bug: expected Thursday, got %v", now.Weekday())
	}

	got := classifyBlocked(scrollback, now)
	if got.Kind != blockUsageLimit || !got.Weekly {
		t.Fatalf("got %+v, want usage-limit weekly", got)
	}
	want := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC) // next Monday
	if !got.ResetAt.Equal(want) {
		t.Fatalf("resetAt = %v, want %v", got.ResetAt, want)
	}
}

func TestClassifyBlocked_UsageLimitUnparseableClauseFallsBackConservatively(t *testing.T) {
	scrollback := "You've hit your usage limit, try again once quota renews"
	now := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)

	got := classifyBlocked(scrollback, now)
	if got.Kind != blockUsageLimit {
		t.Fatalf("kind = %v, want blockUsageLimit (must not silently fall through to auto-resume)", got.Kind)
	}
	if !got.ResetAt.After(now) {
		t.Fatalf("resetAt %v should be a future cool-down, not zero/past", got.ResetAt)
	}
}

func TestClassifyBlocked_InfraKeywords(t *testing.T) {
	cases := []string{
		"gh auth login required — you are not logged into any GitHub hosts",
		"fatal: could not read from remote repository",
		"write error: No space left on device",
	}
	now := time.Now()
	for _, sb := range cases {
		t.Run(sb, func(t *testing.T) {
			got := classifyBlocked(sb, now)
			if got.Kind != blockInfra {
				t.Fatalf("kind = %v, want blockInfra for %q", got.Kind, sb)
			}
		})
	}
}

func TestClassifyBlocked_OtherwiseAutoResumes(t *testing.T) {
	scrollback := "some unrelated scrollback, nothing special happened here\ncodex is ready"
	got := classifyBlocked(scrollback, time.Now())
	if got.Kind != blockAutoResume {
		t.Fatalf("kind = %v, want blockAutoResume", got.Kind)
	}
}

func TestClassifyBlocked_EmptyScrollbackAutoResumes(t *testing.T) {
	got := classifyBlocked("", time.Now())
	if got.Kind != blockAutoResume {
		t.Fatalf("kind = %v, want blockAutoResume on empty/unavailable scrollback", got.Kind)
	}
}

func TestClassifyBlocked_InfraTakesPrecedenceOverUsageLimit(t *testing.T) {
	// both signals present: infra must win — retrying /goal resume against a
	// broken environment is never safe, even if usage-limit text also appears.
	scrollback := "usage limit notice earlier in scrollback... gh auth expired, please re-login"
	got := classifyBlocked(scrollback, time.Now())
	if got.Kind != blockInfra {
		t.Fatalf("kind = %v, want blockInfra to take precedence", got.Kind)
	}
}

func TestClassifyBlocked_InfraKeywordsCaseInsensitive(t *testing.T) {
	got := classifyBlocked(strings.ToUpper("gh auth required"), time.Now())
	if got.Kind != blockInfra {
		t.Fatalf("kind = %v, want blockInfra (case-insensitive match)", got.Kind)
	}
}

// TestClassifyBlocked_InfraScopedToScrollbackTail: an infra phrase merely DISCUSSED
// high up in the transcript (>15 non-empty lines above the bottom) must NOT strand
// the session as needs_operator — the reason a session is blocked NOW lives at the
// bottom of the pane. The same phrase within the tail still classifies as infra.
func TestClassifyBlocked_InfraScopedToScrollbackTail(t *testing.T) {
	// 20 filler lines push the discussion line beyond the 15-line tail window.
	filler := strings.Repeat("just some ordinary transcript output line\n", 20)
	discussed := "earlier we talked about running gh auth login on the other box\n" + filler
	got := classifyBlocked(discussed, time.Now())
	if got.Kind == blockInfra {
		t.Fatalf("an infra phrase outside the tail window must not classify as infra")
	}

	recent := filler + "fatal: gh auth login required\n"
	got = classifyBlocked(recent, time.Now())
	if got.Kind != blockInfra {
		t.Fatalf("kind = %v, want blockInfra for a tail-window match", got.Kind)
	}
}
