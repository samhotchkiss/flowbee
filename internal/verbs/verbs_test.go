package verbs

import (
	"errors"
	"strings"
	"testing"
)

func TestForFamily(t *testing.T) {
	for _, ok := range []string{"codex", "CODEX", " claude ", "Claude", "grok", "GROK", " Grok "} {
		if _, err := For(ok); err != nil {
			t.Fatalf("For(%q) unexpected error: %v", ok, err)
		}
	}
	if _, err := For("gemini"); !errors.Is(err, ErrUnknownFamily) {
		t.Fatalf("For(gemini) = %v, want ErrUnknownFamily", err)
	}
}

func TestResume(t *testing.T) {
	cx, _ := For("codex")
	got, err := cx.Resume()
	if err != nil {
		t.Fatalf("codex Resume err: %v", err)
	}
	if got.Text != "/goal resume" || !got.SubmitEnter {
		t.Fatalf("codex Resume = %+v", got)
	}
	cl, _ := For("claude")
	if _, err := cl.Resume(); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("claude Resume = %v, want ErrUnsupported (no in-pane resume verb)", err)
	}
	// grok: /goal resume is SUPPORTED (grok has the /goal builtin), like codex — NOT
	// ErrUnsupported like claude.
	gk, _ := For("grok")
	got, err = gk.Resume()
	if err != nil {
		t.Fatalf("grok Resume err: %v (grok has /goal resume — must be supported)", err)
	}
	if got.Text != "/goal resume" || !got.SubmitEnter {
		t.Fatalf("grok Resume = %+v, want /goal resume", got)
	}
}

func TestLaunch(t *testing.T) {
	cx, _ := For("codex")
	got, err := cx.Launch("epics/2026-07-16-frob.md", "2026-07-16-frob")
	if err != nil {
		t.Fatalf("codex Launch err: %v", err)
	}
	want := "/goal execute the epic at epics/2026-07-16-frob.md per epics/INSTRUCTIONS.md. Work on branch epic/2026-07-16-frob."
	if got.Text != want || !got.SubmitEnter {
		t.Fatalf("codex Launch = %q", got.Text)
	}
	cl, _ := For("claude")
	got, err = cl.Launch("epics/2026-07-16-frob.md", "2026-07-16-frob")
	if err != nil {
		t.Fatalf("claude Launch err: %v", err)
	}
	// claude: the same instruction WITHOUT the Codex /goal builtin prefix (plain prompt).
	if strings.HasPrefix(got.Text, "/goal") {
		t.Fatalf("claude Launch must not carry the codex /goal prefix: %q", got.Text)
	}
	if !strings.Contains(got.Text, "execute the epic at epics/2026-07-16-frob.md") ||
		!strings.Contains(got.Text, "branch epic/2026-07-16-frob") {
		t.Fatalf("claude Launch shape wrong: %q", got.Text)
	}
	// grok mirrors codex: the /goal builtin shape (grok has /goal), identical to codex.
	gk, _ := For("grok")
	got, err = gk.Launch("epics/2026-07-16-frob.md", "2026-07-16-frob")
	if err != nil {
		t.Fatalf("grok Launch err: %v", err)
	}
	if got.Text != want || !got.SubmitEnter {
		t.Fatalf("grok Launch = %q, want the codex /goal shape %q", got.Text, want)
	}
}

func TestStaticVerbs(t *testing.T) {
	// NudgeEnter and ClearContext are agnostic across ALL families; EscapeModal is Escape
	// for codex/claude but Ctrl+U for grok (checked separately below).
	for _, fam := range []string{"codex", "claude", "grok"} {
		v, _ := For(fam)
		if got := v.NudgeEnter(); got.Key != "Enter" || got.Text != "" {
			t.Fatalf("%s NudgeEnter = %+v", fam, got)
		}
		clr, err := v.ClearContext()
		if err != nil || clr.Text != "/clear" || !clr.SubmitEnter {
			t.Fatalf("%s ClearContext = %+v err=%v", fam, clr, err)
		}
	}
	for _, fam := range []string{"codex", "claude"} {
		v, _ := For(fam)
		if got := v.EscapeModal(); got.Key != "Escape" || got.Text != "" {
			t.Fatalf("%s EscapeModal = %+v", fam, got)
		}
	}
	// grok's EscapeModal must NOT be Escape (a no-op in grok) and must NOT be Ctrl+C
	// (destructive — cancels the turn / exits). It is Ctrl+U (clear-input), a safe dismiss.
	gk, _ := For("grok")
	esc := gk.EscapeModal()
	if esc.Key == "Escape" {
		t.Fatalf("grok EscapeModal must not be Escape (Esc is a no-op in grok): %+v", esc)
	}
	if esc.Key == "C-c" || esc.Key == "C-C" {
		t.Fatalf("grok EscapeModal must not be Ctrl+C (destructive): %+v", esc)
	}
	if esc.Key != "C-u" || esc.Text != "" {
		t.Fatalf("grok EscapeModal = %+v, want Key=C-u (clear-input dismiss)", esc)
	}
}

func TestNotifyMaster(t *testing.T) {
	v, _ := For("claude")
	got, err := v.NotifyMaster(4, "scope_violation")
	if err != nil {
		t.Fatalf("NotifyMaster err: %v", err)
	}
	want := "flowbee: 4 attention items pending (top: scope_violation). Run: flowbee master poll"
	if got.Text != want || !got.SubmitEnter {
		t.Fatalf("NotifyMaster = %q", got.Text)
	}
	// codex and grok resolve the identical plain-text ping (family-agnostic).
	cx, _ := For("codex")
	if got2, _ := cx.NotifyMaster(4, "scope_violation"); got2.Text != want {
		t.Fatalf("codex NotifyMaster diverged: %q", got2.Text)
	}
	gk, _ := For("grok")
	if got3, _ := gk.NotifyMaster(4, "scope_violation"); got3.Text != want {
		t.Fatalf("grok NotifyMaster diverged: %q", got3.Text)
	}
	// an unknown top-kind is REJECTED — the template must never carry free text.
	for _, bad := range []string{"", "bogus", "goal_paused", "; rm -rf /"} {
		if _, err := v.NotifyMaster(1, bad); !errors.Is(err, ErrInvalidKind) {
			t.Fatalf("NotifyMaster(%q) = %v, want ErrInvalidKind", bad, err)
		}
	}
}
