package ctxprobe_test

import (
	"path/filepath"
	"testing"

	"github.com/samhotchkiss/flowbee/internal/ctxprobe"
	"github.com/samhotchkiss/flowbee/internal/epicdigest"
)

const cwd = "/work/epic/frob" // slug "-work-epic-frob"

func TestProbeCodex_Normal(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, err := p.ProbeCodex(filepath.Join("testdata", "codex", "home"))
	if err != nil || !ok {
		t.Fatalf("probe: ok=%v err=%v", ok, err)
	}
	if r.UsedTokens != 120000 || r.ContextWindow != 200000 {
		t.Fatalf("reading: %+v", r)
	}
	used, uok := r.UsedPct()
	rem, rok := r.RemainingPct()
	if !uok || !rok || used != 60 || rem != 40 {
		t.Fatalf("pct: used=%v(%v) rem=%v(%v)", used, uok, rem, rok)
	}
	if r.CapturedAt.IsZero() {
		t.Fatalf("expected a captured timestamp")
	}
}

func TestProbeCodex_CompactionReadsPostCompactionTail(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, _ := p.ProbeCodex(filepath.Join("testdata", "codex", "compaction"))
	if !ok {
		t.Fatal("expected a reading")
	}
	// the tail's LAST token_count is the POST-compaction 40000, not the pre-compaction
	// 180000 — an assignment input computed from pre-compaction usage would be worse
	// than none (plan §15.3).
	if r.UsedTokens != 40000 {
		t.Fatalf("expected post-compaction 40000, got %d", r.UsedTokens)
	}
	if rem, _ := r.RemainingPct(); rem != 80 {
		t.Fatalf("expected 80%% remaining post-compaction, got %v", rem)
	}
}

func TestProbeCodex_NoSessionsIsUnknownNotError(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	_, ok, err := p.ProbeCodex(filepath.Join("testdata", "codex", "does-not-exist"))
	if err != nil {
		t.Fatalf("a missing home is unknown, not an error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a missing home")
	}
}

func TestProbeClaude_UnknownWindow(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, err := p.ProbeClaude(filepath.Join("testdata", "claude", "home"), cwd)
	if err != nil || !ok {
		t.Fatalf("probe: ok=%v err=%v", ok, err)
	}
	// occupancy = input + cache_read + cache_creation of the LAST assistant message.
	if r.UsedTokens != 152000 {
		t.Fatalf("expected 152000 occupied, got %d", r.UsedTokens)
	}
	// the on-disk transcript carries NO window — percent must be UNKNOWN, never guessed.
	if r.ContextWindow != 0 {
		t.Fatalf("expected unknown window (0), got %d", r.ContextWindow)
	}
	if _, ok := r.RemainingPct(); ok {
		t.Fatal("RemainingPct must report ok=false when the window is unknown")
	}
}

func TestProbeClaude_WindowedWhenPresentOnDisk(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, _ := p.ProbeClaude(filepath.Join("testdata", "claude", "windowed"), cwd)
	if !ok {
		t.Fatal("expected a reading")
	}
	if r.ContextWindow != 200000 || r.UsedTokens != 150000 {
		t.Fatalf("reading: %+v", r)
	}
	if rem, ok := r.RemainingPct(); !ok || rem != 25 {
		t.Fatalf("expected 25%% remaining when the window IS on disk, got %v ok=%v", rem, ok)
	}
}

func TestProbeClaude_CompactionReadsPostCompactionTail(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, _ := p.ProbeClaude(filepath.Join("testdata", "claude", "compaction"), cwd)
	if !ok {
		t.Fatal("expected a reading")
	}
	// the LAST assistant usage after the summary event is 30000 (10000+19000+1000), not
	// the pre-compaction 180000 — post-compaction accurate.
	if r.UsedTokens != 30000 {
		t.Fatalf("expected post-compaction 30000, got %d", r.UsedTokens)
	}
}

func TestProbeClaude_MissingProjectDirUnknown(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	_, ok, err := p.ProbeClaude(filepath.Join("testdata", "claude", "home"), "/some/other/cwd")
	if err != nil {
		t.Fatalf("a missing project dir is unknown, not an error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a cwd with no transcript dir")
	}
}

func TestProbeGrok_ReadsWindowFromDisk(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	r, ok, err := p.ProbeGrok(filepath.Join("testdata", "grok", "home"), cwd)
	if err != nil || !ok {
		t.Fatalf("probe: ok=%v err=%v", ok, err)
	}
	// the NEWEST session (higher-named uuid) wins: 120000/400000, not the older 999999.
	if r.UsedTokens != 120000 {
		t.Fatalf("expected the newest session's 120000, got %d", r.UsedTokens)
	}
	// grok records the window on disk — it must be READ (400000 here), never the
	// hardcoded 500000 constant.
	if r.ContextWindow != 400000 {
		t.Fatalf("expected the on-disk window 400000 (never a 500000 guess), got %d", r.ContextWindow)
	}
	used, uok := r.UsedPct()
	rem, rok := r.RemainingPct()
	if !uok || !rok || used != 30 || rem != 70 {
		t.Fatalf("pct: used=%v(%v) rem=%v(%v) want 30/70", used, uok, rem, rok)
	}
	if r.Provider != ctxprobe.ProviderGrok {
		t.Fatalf("provider=%q want grok", r.Provider)
	}
}

func TestProbeGrok_MissingCwdDirUnknown(t *testing.T) {
	p := ctxprobe.NewWith(ctxprobe.OSFS{})
	_, ok, err := p.ProbeGrok(filepath.Join("testdata", "grok", "home"), "/some/other/cwd")
	if err != nil {
		t.Fatalf("a missing session dir is unknown, not an error: %v", err)
	}
	if ok {
		t.Fatal("expected ok=false for a cwd with no grok session dir")
	}
}

func TestEncodeCwd(t *testing.T) {
	// live-verified: grok encodes cwd with encodeURIComponent (/ -> %2F).
	if got := ctxprobe.EncodeCwd("/Users/grok1"); got != "%2FUsers%2Fgrok1" {
		t.Fatalf("EncodeCwd(/Users/grok1) = %q, want %%2FUsers%%2Fgrok1", got)
	}
	if got := ctxprobe.EncodeCwd("/work/epic/frob"); got != "%2Fwork%2Fepic%2Ffrob" {
		t.Fatalf("EncodeCwd = %q", got)
	}
	// unreserved runes (-_.~) survive un-encoded; a space and '#' are encoded.
	if got := ctxprobe.EncodeCwd("/a-b_c.d~e f#g"); got != "%2Fa-b_c.d~e%20f%23g" {
		t.Fatalf("EncodeCwd unreserved/encoded mix = %q", got)
	}
}

func TestSlugForCwd(t *testing.T) {
	if got := ctxprobe.SlugForCwd("/Users/sam/dev/flowbee"); got != "-Users-sam-dev-flowbee" {
		t.Fatalf("slug = %q", got)
	}
	if got := ctxprobe.SlugForCwd("/work/epic/frob"); got != "-work-epic-frob" {
		t.Fatalf("slug = %q", got)
	}
}

// TestCompactionJumpCrossCheck ties the two disk readings of a compaction to the pure
// epicdigest helper the ticker uses: the remaining-context RISE across the observations
// classifies as a compaction (not drift).
func TestCompactionJumpCrossCheck(t *testing.T) {
	// pre-compaction 180000/200000 -> 10% remaining; post 40000/200000 -> 80% remaining.
	pre := ctxprobe.Reading{ContextWindow: 200000, UsedTokens: 180000}
	post := ctxprobe.Reading{ContextWindow: 200000, UsedTokens: 40000}
	preRem, _ := pre.RemainingPct()
	postRem, _ := post.RemainingPct()
	if !epicdigest.CompactionJumped(preRem, postRem, epicdigest.DefaultCompactionJumpPoints) {
		t.Fatalf("a 10%%->80%% remaining rise must classify as compaction (pre=%v post=%v)", preRem, postRem)
	}
}
