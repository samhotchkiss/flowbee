package verbs

import (
	"errors"
	"strings"
	"testing"
)

func TestForFamily(t *testing.T) {
	for _, ok := range []string{"codex", "CODEX", " claude ", "Claude"} {
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
}

func TestStaticVerbs(t *testing.T) {
	for _, fam := range []string{"codex", "claude"} {
		v, _ := For(fam)
		if got := v.NudgeEnter(); got.Key != "Enter" || got.Text != "" {
			t.Fatalf("%s NudgeEnter = %+v", fam, got)
		}
		if got := v.EscapeModal(); got.Key != "Escape" || got.Text != "" {
			t.Fatalf("%s EscapeModal = %+v", fam, got)
		}
		clr, err := v.ClearContext()
		if err != nil || clr.Text != "/clear" || !clr.SubmitEnter {
			t.Fatalf("%s ClearContext = %+v err=%v", fam, clr, err)
		}
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
	// codex resolves the identical plain-text ping.
	cx, _ := For("codex")
	if got2, _ := cx.NotifyMaster(4, "scope_violation"); got2.Text != want {
		t.Fatalf("codex NotifyMaster diverged: %q", got2.Text)
	}
	// an unknown top-kind is REJECTED — the template must never carry free text.
	for _, bad := range []string{"", "bogus", "goal_paused", "; rm -rf /"} {
		if _, err := v.NotifyMaster(1, bad); !errors.Is(err, ErrInvalidKind) {
			t.Fatalf("NotifyMaster(%q) = %v, want ErrInvalidKind", bad, err)
		}
	}
}
