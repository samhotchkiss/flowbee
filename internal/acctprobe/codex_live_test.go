package acctprobe

import (
	"context"
	"errors"
	"testing"
)

func win(pct float64, mins int) *codexLiveWin {
	p := pct
	return &codexLiveWin{UsedPercent: &p, WindowDurationMins: mins}
}

// TestCodexLiveWindowsBucketing mirrors headroom's CodexWindowMapping: windows are
// keyed off windowDurationMins, never primary/secondary position; an absent window is
// omitted (never 0%); a payload without a weekly window holds the seat.
func TestCodexLiveWindowsBucketing(t *testing.T) {
	t.Run("standard primary+secondary", func(t *testing.T) {
		ws, err := codexLiveWindows(&codexLiveBucket{Primary: win(12, 300), Secondary: win(88, 10080)}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s, ok := ws.SessionPct(); !ok || s != 12 {
			t.Errorf("session=%v ok=%v want 12", s, ok)
		}
		if wk, ok := ws.WeeklyPct(); !ok || wk != 88 {
			t.Errorf("weekly=%v ok=%v want 88", wk, ok)
		}
	})

	t.Run("5h lifted omits session", func(t *testing.T) {
		ws, err := codexLiveWindows(&codexLiveBucket{Primary: win(16, 10080), Secondary: nil}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if wk, ok := ws.WeeklyPct(); !ok || wk != 16 {
			t.Errorf("weekly=%v ok=%v want 16", wk, ok)
		}
		if _, ok := ws.SessionPct(); ok {
			t.Error("an absent 5h must be OMITTED, never synthesized as 0%")
		}
	})

	t.Run("lone 5h without weekly holds", func(t *testing.T) {
		_, err := codexLiveWindows(&codexLiveBucket{Primary: win(40, 300)}, nil)
		assertHold(t, err, ReasonUnrecognizedPayload)
	})

	t.Run("scoped bucket becomes scoped weekly row", func(t *testing.T) {
		scoped := map[string]codexLiveBucket{
			"codex_bengalfox": {LimitName: "GPT-5.3-Codex-Spark", Primary: win(3, 10080)},
		}
		ws, err := codexLiveWindows(&codexLiveBucket{Primary: win(7, 10080)}, scoped)
		if err != nil {
			t.Fatal(err)
		}
		var spark *LimitWindow
		for i := range ws {
			if ws[i].Kind == KindWeeklyScoped {
				spark = &ws[i]
			}
		}
		if spark == nil || spark.Scope != "Spark" || spark.Percent != 3 {
			t.Errorf("scoped window=%+v want Spark 3%% (trailing codename)", spark)
		}
	})

	t.Run("scoped without weekly is skipped", func(t *testing.T) {
		scoped := map[string]codexLiveBucket{"x": {LimitName: "Weird", Primary: nil, Secondary: nil}}
		ws, err := codexLiveWindows(&codexLiveBucket{Primary: win(7, 10080)}, scoped)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range ws {
			if w.Kind == KindWeeklyScoped {
				t.Error("a scoped bucket with no usable weekly window must be dropped")
			}
		}
	})

	t.Run("empty payload holds", func(t *testing.T) {
		_, err := codexLiveWindows(&codexLiveBucket{}, nil)
		assertHold(t, err, ReasonUnrecognizedPayload)
	})

	t.Run("unrecognized durations hold", func(t *testing.T) {
		_, err := codexLiveWindows(&codexLiveBucket{Primary: win(10, 60)}, nil)
		assertHold(t, err, ReasonUnrecognizedPayload)
	})
}

func TestProbeCodexLiveVerified(t *testing.T) {
	rl := `{"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":12,"windowDurationMins":300},"secondary":{"usedPercent":88,"windowDurationMins":10080}}}}`
	acct := `{"account":{"email":"codexlive@example.com","planType":"pro"}}`
	p := NewWith(OSFS{}, nil, nil, fakeAppServer{result: AppServerResult{RateLimits: []byte(rl), Account: []byte(acct)}}, fakeClock())
	res, err := p.ProbeCodexLive(context.Background(), td("codex", "home_chatgpt"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustVerified || !res.Identity.Verified {
		t.Errorf("trust=%v verified=%v", res.TrustState, res.Identity.Verified)
	}
	if res.Identity.Email != "codexlive@example.com" {
		t.Errorf("live email should override local: %q", res.Identity.Email)
	}
	if res.Identity.Tier != "pro" {
		t.Errorf("tier=%q want pro", res.Identity.Tier)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 88 {
		t.Errorf("weekly=%v ok=%v want 88", wk, ok)
	}
	if res.Source != "codex_app_server" {
		t.Errorf("source=%q", res.Source)
	}
}

func TestProbeCodexLiveApikeyExcluded(t *testing.T) {
	// an API-key seat is a typed EXCLUSION and the app-server is never consulted.
	p := NewWith(OSFS{}, nil, nil, fakeAppServer{err: errors.New("app-server must not be called for apikey")}, fakeClock())
	_, err := p.ProbeCodexLive(context.Background(), td("codex", "home_apikey"))
	assertHold(t, err, ReasonApikeyNoSubscription)
}

func TestProbeCodexLiveAuthRejectionHolds(t *testing.T) {
	p := NewWith(OSFS{}, nil, nil, fakeAppServer{err: held(ReasonAppServerAuth, errors.New("token_invalidated"))}, fakeClock())
	_, err := p.ProbeCodexLive(context.Background(), td("codex", "home_chatgpt"))
	assertHold(t, err, ReasonAppServerAuth)
	// and the tiered ProbeCodex must NOT fall back to display-only on an auth error.
	_, err = p.ProbeCodex(context.Background(), td("codex", "home_chatgpt"))
	assertHold(t, err, ReasonAppServerAuth)
}

func TestProbeCodexFallsBackToDisplayOnlyWhenUnavailable(t *testing.T) {
	p := NewWith(OSFS{}, nil, nil, fakeAppServer{err: held(ReasonAppServerUnavailable, errors.New("old cli"))}, fakeClock())
	res, err := p.ProbeCodex(context.Background(), td("codex", "home_chatgpt"))
	if err != nil {
		t.Fatal(err)
	}
	if res.TrustState != TrustDisplayOnly {
		t.Errorf("trust=%v want display_only fallback", res.TrustState)
	}
	if wk, ok := res.Usage.Windows.WeeklyPct(); !ok || wk != 39 {
		t.Errorf("fallback telemetry weekly=%v ok=%v want 39", wk, ok)
	}
}

func TestClassifyAppServerError(t *testing.T) {
	cases := []struct {
		raw  string
		want HoldReason
	}{
		{`{"message":"token_invalidated"}`, ReasonAppServerAuth},
		{`{"message":"401 unauthorized"}`, ReasonAppServerAuth},
		{`{"message":"429 too many requests"}`, ReasonThrottled},
		{`{"message":"service overload"}`, ReasonThrottled},
		{`{"message":"weird malformed thing"}`, ReasonAppServerProtocol},
	}
	for _, c := range cases {
		if got := classifyAppServerError([]byte(c.raw)); got != c.want {
			t.Errorf("classify(%s)=%s want %s", c.raw, got, c.want)
		}
	}
}

func assertHold(t *testing.T, err error, want HoldReason) {
	t.Helper()
	var hold *HoldError
	if !errors.As(err, &hold) {
		t.Fatalf("err=%v is not a *HoldError", err)
	}
	if hold.Reason != want {
		t.Fatalf("hold reason=%s want %s", hold.Reason, want)
	}
}
