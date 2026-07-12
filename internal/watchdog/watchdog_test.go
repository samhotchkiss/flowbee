package watchdog

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/samhotchkiss/flowbee/internal/store"
)

// ── fakes ──

// fakeRunner scripts responses by exact command string and records every command
// it was asked to run, in order — the assertion surface for "the ONLY input ever
// sent is /goal resume and bare Enter" (§ safety).
type fakeRunner struct {
	responses map[string]string
	errs      map[string]error
	calls     []string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{responses: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeRunner) Run(_ context.Context, cmd string) (string, error) {
	f.calls = append(f.calls, cmd)
	if err, ok := f.errs[cmd]; ok {
		return "", err
	}
	return f.responses[cmd], nil
}

// fakeSessions is an in-memory SessionStore, mirroring the real store's semantics
// closely enough to drive the watcher state machine end-to-end without a DB.
type fakeSessions struct {
	rows map[string]*store.GoalSession
}

func newFakeSessions(rows ...store.GoalSession) *fakeSessions {
	f := &fakeSessions{rows: map[string]*store.GoalSession{}}
	for i := range rows {
		r := rows[i]
		f.rows[r.ID] = &r
	}
	return f
}

func (f *fakeSessions) ListEnabledGoalSessions(_ context.Context) ([]store.GoalSession, error) {
	var out []store.GoalSession
	for _, r := range f.rows {
		if r.Enabled {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeSessions) UpsertObservation(_ context.Context, id, hash, state, elapsed string, now time.Time) error {
	r, ok := f.rows[id]
	if !ok {
		return store.ErrGoalSessionNotFound
	}
	if hash != r.LastPaneHash {
		r.LastChangeAt = now.Format(time.RFC3339Nano)
	}
	r.LastPaneHash = hash
	r.State = state
	r.GoalElapsed = elapsed
	r.ConsecutiveFailures = 0
	r.LastCheckedAt = now.Format(time.RFC3339Nano)
	return nil
}

func (f *fakeSessions) RecordCaptureFailure(_ context.Context, id string, now time.Time) (int, error) {
	r, ok := f.rows[id]
	if !ok {
		return 0, store.ErrGoalSessionNotFound
	}
	r.ConsecutiveFailures++
	if r.ConsecutiveFailures >= 3 {
		r.State = string(StateUnreachable)
	}
	return r.ConsecutiveFailures, nil
}

func (f *fakeSessions) SetBlockedUntil(_ context.Context, id string, until time.Time, detail string, now time.Time) error {
	r, ok := f.rows[id]
	if !ok {
		return store.ErrGoalSessionNotFound
	}
	r.BlockedUntil = until.Format(time.RFC3339Nano)
	r.StateDetail = detail
	return nil
}

func (f *fakeSessions) SetNeedsOperator(_ context.Context, id, detail string, now time.Time) error {
	r, ok := f.rows[id]
	if !ok {
		return store.ErrGoalSessionNotFound
	}
	r.StateDetail = "needs_operator: " + detail
	return nil
}

func (f *fakeSessions) ClearBlock(_ context.Context, id string, now time.Time) error {
	r, ok := f.rows[id]
	if !ok {
		return store.ErrGoalSessionNotFound
	}
	r.StateDetail, r.BlockedUntil, r.ResumeAttempts, r.ResumeWindowStart = "", "", 0, ""
	return nil
}

func (f *fakeSessions) RecordResumeAttempt(_ context.Context, id string, now time.Time) (int, bool, error) {
	r, ok := f.rows[id]
	if !ok {
		return 0, false, store.ErrGoalSessionNotFound
	}
	fresh := r.ResumeWindowStart == ""
	if !fresh {
		ws, err := time.Parse(time.RFC3339Nano, r.ResumeWindowStart)
		fresh = err == nil && now.Sub(ws) >= time.Hour
	}
	if fresh {
		r.ResumeAttempts = 1
		r.ResumeWindowStart = now.Format(time.RFC3339Nano)
		return 1, true, nil
	}
	if r.ResumeAttempts >= 3 {
		return r.ResumeAttempts, false, nil
	}
	r.ResumeAttempts++
	return r.ResumeAttempts, true, nil
}

type fakeAccounts struct {
	rows []store.AccountUsageRow
	err  error
}

func (f fakeAccounts) AllAccountUsage(_ context.Context) ([]store.AccountUsageRow, error) {
	return f.rows, f.err
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(discardWriter{}, nil)) }

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// ── tests ──

func TestWatcher_BlockedAutoResumeSentAndVerified(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	blockedPane := "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
	runner.responses[capturePaneCmd("", "goal-s1")] = blockedPane
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "nothing special here, ready when you are"
	// after sending resume, the verification re-capture shows the TUI moved on
	// (i.e. the command was submitted, no swallowed-Enter retry needed).
	runner.responses[sendResumeCmd("", "goal-s1")] = ""

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), now)

	if got := sessions.rows["s1"].State; got != string(StateBlocked) {
		t.Fatalf("state = %q, want blocked", got)
	}
	if sessions.rows["s1"].ResumeAttempts != 1 {
		t.Fatalf("resume_attempts = %d, want 1", sessions.rows["s1"].ResumeAttempts)
	}
	if !containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
		t.Fatalf("expected /goal resume to be sent, calls: %v", runner.calls)
	}
	// verify: after sending, the pane was re-captured to check submission. The
	// re-captured pane still shows the recognized "Goal blocked (/goal resume)"
	// status line (a redrawn, KNOWN status shape, not raw unsubmitted input), so no
	// bare-Enter retry should follow — that's the last call in the pass.
	if runner.calls[len(runner.calls)-1] != capturePaneCmd("", "goal-s1") {
		t.Fatalf("expected a verify re-capture as the last call, got %v", runner.calls)
	}
	if containsCall(runner.calls, sendEnterCmd("", "goal-s1")) {
		t.Fatalf("must not retry bare Enter when the re-capture shows a recognized status line, calls: %v", runner.calls)
	}
}

func TestWatcher_SwallowedEnterRetriesBareEnter(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	blockedPane := "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
	unsubmittedPane := "some prior lines\n/goal resume"
	runner.responses[capturePaneCmd("", "goal-s1")] = blockedPane
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "ready"
	runner.responses[sendResumeCmd("", "goal-s1")] = ""

	// The FIRST capture (initial pass) sees the blocked status line; the SECOND
	// capture (post-send verification) must see the swallowed-Enter symptom. Since
	// our fake keys purely by command string, script a call-order-aware variant by
	// swapping the response after the first capture.
	callCount := 0
	scripted := &scriptedRunner{
		fallback: runner,
		onCapturePane: func() string {
			callCount++
			if callCount == 1 {
				return blockedPane
			}
			return unsubmittedPane
		},
		captureCmd: capturePaneCmd("", "goal-s1"),
	}

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: scripted, Logger: testLogger()}
	w.Pass(context.Background(), now)

	if !containsCall(scripted.calls, sendEnterCmd("", "goal-s1")) {
		t.Fatalf("expected a bare Enter retry after seeing the unsubmitted command, calls: %v", scripted.calls)
	}
	// the ONLY keys ever sent must be exactly /goal resume + Enter.
	for _, c := range scripted.calls {
		if c == sendResumeCmd("", "goal-s1") || c == sendEnterCmd("", "goal-s1") ||
			c == capturePaneCmd("", "goal-s1") || c == captureScrollbackCmd("", "goal-s1") {
			continue
		}
		t.Fatalf("unexpected command sent: %q", c)
	}
}

// scriptedRunner lets onCapturePane vary its answer per call (order-sensitive),
// falling back to a fakeRunner for everything else — used only for the
// swallowed-Enter test, where the SAME command (capture-pane) must return
// different text on its two different calls within one pass.
type scriptedRunner struct {
	fallback      *fakeRunner
	onCapturePane func() string
	captureCmd    string
	calls         []string
}

func (s *scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if cmd == s.captureCmd {
		return s.onCapturePane(), nil
	}
	return s.fallback.Run(ctx, cmd)
}

func TestWatcher_UsageLimitBlockSetsBlockedUntilAndDoesNotResume(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	now := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)

	blockedPane := "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
	runner.responses[capturePaneCmd("", "goal-s1")] = blockedPane
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "You've hit your usage limit, try again at 10:47 AM"

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), now)

	if containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
		t.Fatalf("must NOT auto-resume while blocked_until is in the future, calls: %v", runner.calls)
	}
	row := sessions.rows["s1"]
	if row.BlockedUntil == "" || row.StateDetail != "usage_limit" {
		t.Fatalf("unexpected row: %+v", row)
	}

	// a SECOND pass, still before the reset time, must not re-classify or resend —
	// it should short-circuit on the still-future blocked_until.
	runner.calls = nil
	w.Pass(context.Background(), now.Add(10*time.Minute))
	if containsCall(runner.calls, captureScrollbackCmd("", "goal-s1")) {
		t.Fatalf("should not re-capture scrollback while still within blocked_until, calls: %v", runner.calls)
	}

	// a THIRD pass, past the reset time, is free to classify + resume again.
	runner.calls = nil
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "ready to go"
	w.Pass(context.Background(), now.Add(2*time.Hour))
	if !containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
		t.Fatalf("expected a resume attempt once past blocked_until, calls: %v", runner.calls)
	}
}

func TestWatcher_InfraBlockNeedsOperatorNeverResumes(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	blockedPane := "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
	runner.responses[capturePaneCmd("", "goal-s1")] = blockedPane
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "gh auth login required to push"

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), now)

	if containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
		t.Fatalf("must NEVER auto-resume an infra block, calls: %v", runner.calls)
	}
	if containsCall(runner.calls, sendEnterCmd("", "goal-s1")) {
		t.Fatalf("must never send bare Enter for an infra block either, calls: %v", runner.calls)
	}
	row := sessions.rows["s1"]
	if row.StateDetail == "" || row.StateDetail == "usage_limit" {
		t.Fatalf("expected needs_operator detail, got %q", row.StateDetail)
	}
}

func TestWatcher_ThreeStrikeResumeRateLimit(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	blockedPane := "  gpt-5.6-sol medium · ~/dev/russ                                     Goal blocked (/goal resume)"
	runner.responses[capturePaneCmd("", "goal-s1")] = blockedPane
	runner.responses[captureScrollbackCmd("", "goal-s1")] = "ready"
	runner.responses[sendResumeCmd("", "goal-s1")] = ""

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}

	// three attempts within the hour succeed (send /goal resume each time);
	// the 4th is refused and escalates to needs_operator with no keys sent.
	for i := 0; i < 3; i++ {
		runner.calls = nil
		w.Pass(context.Background(), base.Add(time.Duration(i)*time.Minute))
		if !containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
			t.Fatalf("attempt %d: expected /goal resume sent, calls: %v", i+1, runner.calls)
		}
	}
	runner.calls = nil
	w.Pass(context.Background(), base.Add(10*time.Minute))
	if containsCall(runner.calls, sendResumeCmd("", "goal-s1")) {
		t.Fatalf("4th attempt within the hour must be refused, calls: %v", runner.calls)
	}
	row := sessions.rows["s1"]
	if row.StateDetail == "" {
		t.Fatalf("expected needs_operator after the 3-strike cap, got %+v", row)
	}
}

func TestWatcher_UnreachableAfterThreeConsecutiveCaptureFailures(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	runner.errs[capturePaneCmd("", "goal-s1")] = errors.New("ssh: connection refused")
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	for i := 1; i <= 2; i++ {
		w.Pass(context.Background(), now.Add(time.Duration(i)*time.Minute))
		if got := sessions.rows["s1"].State; got == string(StateUnreachable) {
			t.Fatalf("flipped to unreachable too early at pass %d", i)
		}
	}
	w.Pass(context.Background(), now.Add(3*time.Minute))
	if got := sessions.rows["s1"].State; got != string(StateUnreachable) {
		t.Fatalf("state = %q, want unreachable after 3 consecutive capture failures", got)
	}
	// no keys of any kind were ever sent to an unreachable box.
	for _, c := range runner.calls {
		if c == sendResumeCmd("", "goal-s1") || c == sendEnterCmd("", "goal-s1") {
			t.Fatalf("unreachable session must never receive keys, calls: %v", runner.calls)
		}
	}
}

func TestWatcher_UnknownStateNeverActedOn(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	runner.responses[capturePaneCmd("", "goal-s1")] = "totally unparseable garbage\nnothing here"
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), now)

	if got := sessions.rows["s1"].State; got != string(StateUnknown) {
		t.Fatalf("state = %q, want unknown", got)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("unknown state must never trigger scrollback capture or keys, calls: %v", runner.calls)
	}
}

func TestWatcher_AchievedIsRecordedButNotActedOn(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: true}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	runner.responses[capturePaneCmd("", "goal-s1")] =
		"  gpt-5.6-sol medium · ~/dev/russ                                     Goal achieved (1h 52m)"
	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), now)

	row := sessions.rows["s1"]
	if row.State != string(StateAchieved) || row.GoalElapsed != "1h 52m" {
		t.Fatalf("unexpected row: %+v", row)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("achieved must not trigger any further keys/captures, calls: %v", runner.calls)
	}
}

func TestWatcher_DisabledSessionNeverTouched(t *testing.T) {
	sess := store.GoalSession{ID: "s1", TmuxName: "goal-s1", Enabled: false}
	sessions := newFakeSessions(sess)
	runner := newFakeRunner()
	w := &Watcher{Sessions: sessions, Accounts: fakeAccounts{}, Runner: runner, Logger: testLogger()}
	w.Pass(context.Background(), time.Now())
	if len(runner.calls) != 0 {
		t.Fatalf("a disabled (paused) session must never be captured, calls: %v", runner.calls)
	}
}

func TestWatcher_UsageCeilingWarningThrottledHourly(t *testing.T) {
	sessions := newFakeSessions()
	runner := newFakeRunner()
	accounts := fakeAccounts{rows: []store.AccountUsageRow{
		{AccountID: "acct-a", ModelFamily: "codex", UsagePct: 80, CeilingPct: 90},
	}}
	w := &Watcher{Sessions: sessions, Accounts: accounts, Runner: runner, Logger: testLogger()}

	now := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	w.Pass(context.Background(), now) // first tick: warns, records lastCeilingWarnAt
	if w.lastCeilingWarnAt.IsZero() {
		t.Fatalf("expected lastCeilingWarnAt to be set after a hot-account pass")
	}
	firstWarnAt := w.lastCeilingWarnAt

	w.Pass(context.Background(), now.Add(2*time.Minute)) // next tick: throttled, no update
	if !w.lastCeilingWarnAt.Equal(firstWarnAt) {
		t.Fatalf("ceiling warning fired again within the hour")
	}

	w.Pass(context.Background(), now.Add(90*time.Minute)) // past the hour: fires again
	if w.lastCeilingWarnAt.Equal(firstWarnAt) {
		t.Fatalf("ceiling warning did not re-fire after an hour")
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
