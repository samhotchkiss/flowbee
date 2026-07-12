package epicspec

import "testing"

func TestParseStatus_Happy(t *testing.T) {
	body := `Updated: 2026-07-03T12:00:00Z · Current: step 2/5 · State: building
- [x] Step 1 — stand up skeleton (evidence: go test ./foo/... -> ok, 3 passed)
- [ ] Step 2 — implement the fix
Blockers: none
`
	sb := ParseStatus(body)
	if sb.UpdatedRaw != "2026-07-03T12:00:00Z" {
		t.Errorf("updated = %q", sb.UpdatedRaw)
	}
	if sb.CurrentStep != 2 || sb.StepsTotal != 5 {
		t.Errorf("current = %d/%d", sb.CurrentStep, sb.StepsTotal)
	}
	if sb.State != "building" {
		t.Errorf("state = %q", sb.State)
	}
	if len(sb.Checklist) != 2 {
		t.Fatalf("checklist = %d items, want 2", len(sb.Checklist))
	}
	if !sb.Checklist[0].Checked || sb.Checklist[0].Step != 1 {
		t.Errorf("checklist[0] = %+v", sb.Checklist[0])
	}
	if sb.Checklist[0].Evidence != "go test ./foo/... -> ok, 3 passed" {
		t.Errorf("checklist[0] evidence = %q", sb.Checklist[0].Evidence)
	}
	if sb.Checklist[0].Text != "stand up skeleton" {
		t.Errorf("checklist[0] text = %q", sb.Checklist[0].Text)
	}
	if sb.Checklist[1].Checked {
		t.Errorf("checklist[1] should be unchecked")
	}
	if sb.Blockers != "none" {
		t.Errorf("blockers = %q", sb.Blockers)
	}
}

func TestParseStatus_NotStarted(t *testing.T) {
	body := "Updated: 2026-01-01T00:00:00Z · Current: not started · State: pending\n- [ ] Step 1 — do it\n"
	sb := ParseStatus(body)
	if sb.CurrentStep != 0 || sb.StepsTotal != 0 {
		t.Errorf("expected 0/0 for 'not started', got %d/%d", sb.CurrentStep, sb.StepsTotal)
	}
	if sb.State != "pending" {
		t.Errorf("state = %q", sb.State)
	}
}

func TestParseStatus_Blocked(t *testing.T) {
	body := `Updated: 2026-07-03T12:00:00Z · Current: step 3/5 · State: blocked
- [x] Step 1 — a (evidence: ok)
- [x] Step 2 — b (evidence: ok)
- [ ] Step 3 — c
Blockers: needs gh auth on this box, tried gh auth login, got a 403
`
	sb := ParseStatus(body)
	if sb.State != "blocked" {
		t.Errorf("state = %q", sb.State)
	}
	if sb.Blockers != "needs gh auth on this box, tried gh auth login, got a 403" {
		t.Errorf("blockers = %q", sb.Blockers)
	}
}

func TestParseStatus_Garbage(t *testing.T) {
	for _, body := range []string{
		"",
		"total noise, no fields at all\n",
		"Updated Current State no colons",
		"\x00\x01\x02 binary junk",
		"- [?] Step abc — malformed checkbox and step number",
	} {
		// must never panic; a garbage body degrades to (mostly) zero values.
		sb := ParseStatus(body)
		if sb.State != "" && body != "" {
			// not a hard assertion (garbage could coincidentally match), just
			// exercising that this never panics/errors.
			_ = sb
		}
	}
}

func TestParseStatus_MissingBlockers(t *testing.T) {
	body := "Updated: 2026-01-01T00:00:00Z · Current: step 1/1 · State: done\n- [x] Step 1 — done (evidence: ok)\n"
	sb := ParseStatus(body)
	if sb.Blockers != "" {
		t.Errorf("blockers = %q, want empty", sb.Blockers)
	}
	if sb.State != "done" {
		t.Errorf("state = %q", sb.State)
	}
}
