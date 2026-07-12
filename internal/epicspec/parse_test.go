package epicspec

import (
	"strings"
	"testing"
)

const sampleSpec = `---
title: Wire up the frobnicator
scope:
  - internal/frob/**
  - cmd/frob/**.go
host: buncher
agent: codex
---

## Goal

Make the frobnicator frob correctly. Done means ` + "`go test ./internal/frob/...`" + ` is green.

## Constraints / Non-Goals

Do not touch internal/other. Do not upgrade dependencies.

## Steps

1. Stand up a failing test.
   Validate: ` + "`go test ./internal/frob/... -run TestFrob`" + ` fails as expected.

2. Implement the fix.
   Validate: ` + "`go test ./internal/frob/...`" + ` passes.

## Status
Updated: 2026-01-01T00:00:00Z · Current: not started · State: pending
- [ ] Step 1 — stand up a failing test
- [ ] Step 2 — implement the fix
`

func TestParseSpec_Happy(t *testing.T) {
	s, err := ParseSpec(sampleSpec)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if s.Title != "Wire up the frobnicator" {
		t.Errorf("title = %q", s.Title)
	}
	if len(s.Scope) != 2 || s.Scope[0] != "internal/frob/**" || s.Scope[1] != "cmd/frob/**.go" {
		t.Errorf("scope = %v", s.Scope)
	}
	if s.Host != "buncher" || s.Agent != "codex" {
		t.Errorf("host/agent = %q/%q", s.Host, s.Agent)
	}
	if !strings.Contains(s.Goal, "frobnicator frob correctly") {
		t.Errorf("goal = %q", s.Goal)
	}
	if !strings.Contains(s.Constraints, "Do not touch internal/other") {
		t.Errorf("constraints = %q", s.Constraints)
	}
	if len(s.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(s.Steps))
	}
	if s.Steps[0].N != 1 || !strings.Contains(s.Steps[0].Text, "Stand up a failing test") {
		t.Errorf("step 1 = %+v", s.Steps[0])
	}
	if s.Steps[0].Validate != "`go test ./internal/frob/... -run TestFrob` fails as expected." {
		t.Errorf("step 1 validate = %q", s.Steps[0].Validate)
	}
	if s.Steps[1].N != 2 {
		t.Errorf("step 2 N = %d", s.Steps[1].N)
	}
}

func TestParseSpec_BulletedSteps(t *testing.T) {
	content := `---
title: T
scope:
  - foo/**
---
## Steps
- First thing.
  Validate: ` + "`true`" + `
- Second thing.
  Validate: ` + "`true`" + `
`
	s, err := ParseSpec(content)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if len(s.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(s.Steps))
	}
	// bulleted steps are numbered by position.
	if s.Steps[0].N != 1 || s.Steps[1].N != 2 {
		t.Errorf("step numbers = %d, %d", s.Steps[0].N, s.Steps[1].N)
	}
}

func TestParseSpec_MissingTitle(t *testing.T) {
	content := `---
scope:
  - foo/**
---
## Steps
1. Do it.
   Validate: ` + "`true`" + `
`
	if _, err := ParseSpec(content); err == nil {
		t.Fatal("expected error for missing title")
	}
}

func TestParseSpec_MissingScope(t *testing.T) {
	content := `---
title: T
---
## Steps
1. Do it.
   Validate: ` + "`true`" + `
`
	if _, err := ParseSpec(content); err == nil {
		t.Fatal("expected error for missing scope")
	}
}

func TestParseSpec_NoSteps(t *testing.T) {
	content := `---
title: T
scope:
  - foo/**
---
## Goal
Do the thing.
`
	if _, err := ParseSpec(content); err == nil {
		t.Fatal("expected error for missing steps")
	}
}

func TestParseSpec_Garbage(t *testing.T) {
	for _, c := range []string{
		"",
		"not even markdown, just noise \x00\x01",
		"---\nnot: valid: yaml: at: all: [\n---\n## Steps\n1. x\nValidate: y",
		"## Steps\nno frontmatter at all\n1. x\nValidate: y",
	} {
		if _, err := ParseSpec(c); err == nil {
			t.Errorf("ParseSpec(%q): expected an error, got none", c)
		}
	}
}

func TestParseSpec_NoOptionalFields(t *testing.T) {
	content := `---
title: T
scope:
  - foo/**
---
## Steps
1. Do it.
   Validate: ` + "`true`" + `
`
	s, err := ParseSpec(content)
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	if s.Host != "" || s.Agent != "" {
		t.Errorf("expected empty optional fields, got host=%q agent=%q", s.Host, s.Agent)
	}
}
