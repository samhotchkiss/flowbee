package worker

import "testing"

func TestLooksLikePrompt(t *testing.T) {
	fire := []string{
		"Applying changes...\nDo you want to proceed?",
		"Overwrite existing file? [y/n]",
		"Continue? (y/n)",
		"...\nPress Enter to continue",
		"Approval required",
		"Do you want to make this edit to config.go?",
	}
	for _, s := range fire {
		if !looksLikePrompt([]byte(s)) {
			t.Errorf("expected prompt detection for %q", s)
		}
	}
	// must NOT fire: prompt-shaped text that is NOT at the tail (more output followed), and
	// ordinary agent prose — a false fire only costs a clean re-dispatch, but keep it precise.
	noFire := []string{
		"Do you want to proceed? Yes, I proceeded and wrote the file.",
		"The function asks the user: do you want to proceed? Then it reads stdin.",
		"Building the project now, running tests, all green.",
		"",
		"echo 'continue?' >> script.sh\nWrote script.sh successfully.",
	}
	for _, s := range noFire {
		if looksLikePrompt([]byte(s)) {
			t.Errorf("did NOT expect prompt detection for %q", s)
		}
	}
}

// TestPromptDetectorIdleGate: the detector only reports a stable match; the caller fires
// only when the byte count is unchanged across ticks (prompt printed AND output idle).
func TestPromptDetectorIdleGate(t *testing.T) {
	d := &promptDetector{enabled: true}
	d.observe([]byte("working...\n"))
	if _, matched := d.snapshot(); matched {
		t.Fatal("no prompt yet — must not match")
	}
	d.observe([]byte("Do you want to proceed?"))
	b1, matched := d.snapshot()
	if !matched {
		t.Fatal("prompt at tail must match")
	}
	// more output arrives -> byte count moves -> the caller's `b == lastBytes` idle gate fails,
	// so a still-working agent that merely printed the phrase is never cancelled.
	d.observe([]byte("\nactually I kept going and finished."))
	b2, matched2 := d.snapshot()
	if matched2 {
		t.Fatal("output continued past the prompt — must no longer match")
	}
	if b2 == b1 {
		t.Fatal("byte count must advance as output continues")
	}
}

// TestPromptDetectorDisabled: with the kill-switch off, observe is a no-op.
func TestPromptDetectorDisabled(t *testing.T) {
	d := &promptDetector{enabled: false}
	d.observe([]byte("Do you want to proceed?"))
	if _, matched := d.snapshot(); matched {
		t.Fatal("disabled detector must never match")
	}
}
