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
