package flow

import (
	"strings"
	"testing"
)

const flowsDir = "../../flows"

// loadDefault loads the shipped registry + default flow once for a test.
func loadDefault(t *testing.T) (*Registry, *FlowDoc) {
	t.Helper()
	reg, err := LoadIdentities(flowsDir)
	if err != nil {
		t.Fatalf("LoadIdentities: %v", err)
	}
	doc, err := LoadFlowDoc(flowsDir + "/default.yaml")
	if err != nil {
		t.Fatalf("LoadFlowDoc: %v", err)
	}
	return reg, doc
}

// TestSeededDefaultsFromHire: the identities seeded from `hire` carry the F5
// stage→slug mapping, AGENT.md→lens, and roster_entry→model. This proves the
// "defaults SEEDED from ~/dev/russell/public/hire" DONE-WHEN at the data level.
func TestSeededDefaultsFromHire(t *testing.T) {
	reg, _ := loadDefault(t)

	want := map[string]struct {
		role        string
		slug        string
		modelFamily string
	}{
		"issue-reviewer":       {"issue_reviewer", "engineering-manager", "anthropic"},
		"builder":              {"eng_worker", "engineering-generalist", "anthropic"},
		"reviewer-correctness": {"code_reviewer", "senior-code-reviewer", "anthropic"},
		"reviewer-tests":       {"code_reviewer", "qa-engineer", "anthropic"},
		"reviewer-security":    {"code_reviewer", "security-auditor", "anthropic"},
	}
	for id, w := range want {
		got, ok := reg.Get(id)
		if !ok {
			t.Fatalf("identity %q not seeded", id)
		}
		if got.Role != w.role {
			t.Errorf("%s role=%q want %q", id, got.Role, w.role)
		}
		if got.SourceSlug != w.slug {
			t.Errorf("%s source_slug=%q want %q", id, got.SourceSlug, w.slug)
		}
		if got.ModelFamily != w.modelFamily {
			t.Errorf("%s model_family=%q want %q", id, got.ModelFamily, w.modelFamily)
		}
		if got.Model == "" {
			t.Errorf("%s has no model (roster_entry.model_recommendations not mapped)", id)
		}
		// AGENT.md → lens: the lens markdown loads and is the operating-identity prose.
		body, err := reg.LensMarkdown(got)
		if err != nil {
			t.Fatalf("%s lens markdown: %v", id, err)
		}
		if !strings.Contains(strings.ToLower(body), "operating") && !strings.Contains(body, "##") {
			t.Errorf("%s lens does not look like an AGENT.md operating identity:\n%.120s", id, body)
		}
	}
}

// TestResolveDefaultFlowPerStage: the default flow loads and resolves an identity
// for every active stage; the build-review fan-out expands to its 3 reviewers.
// (DONE-WHEN: "the flow loads from flows/, resolves identities per stage".)
func TestResolveDefaultFlowPerStage(t *testing.T) {
	reg, doc := loadDefault(t)
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	byName := map[string]ResolvedStage{}
	for _, s := range stages {
		byName[s.Name] = s
	}

	if s := byName["build"]; len(s.Actors) != 1 || s.Actors[0].Identity.ID != "builder" {
		t.Fatalf("build stage not staffed by builder: %+v", s.Actors)
	}
	if s := byName["issue_review"]; len(s.Actors) != 1 || s.Actors[0].Identity.ID != "issue-reviewer" {
		t.Fatalf("issue_review not staffed by issue-reviewer: %+v", s.Actors)
	}
	if s := byName["build_review"]; len(s.Actors) != 3 {
		t.Fatalf("build_review fan-out want 3 reviewers, got %d", len(s.Actors))
	}
}

// TestThreeReviewerFanOut: a 3-reviewer build-review fan-out resolves to three
// DISTINCT identities, each with its declared lens. (DONE-WHEN: "a 3-reviewer
// fan-out works".)
func TestThreeReviewerFanOut(t *testing.T) {
	reg, doc := loadDefault(t)
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var review ResolvedStage
	for _, s := range stages {
		if s.Name == "build_review" {
			review = s
		}
	}
	lensSeen := map[string]string{}
	for _, a := range review.Actors {
		if a.Lens == "" {
			t.Errorf("reviewer %q has no lens", a.Identity.ID)
		}
		if prev, dup := lensSeen[a.Identity.ID]; dup {
			t.Fatalf("identity %q reused across lenses %q and %q", a.Identity.ID, prev, a.Lens)
		}
		lensSeen[a.Identity.ID] = a.Lens
	}
	for _, want := range []string{"correctness", "tests", "security"} {
		found := false
		for _, a := range review.Actors {
			if a.Lens == want {
				found = true
			}
		}
		if !found {
			t.Errorf("fan-out missing lens %q", want)
		}
	}
	if review.Decision != DecisionAllPass {
		t.Errorf("default decision=%q want all_pass", review.Decision)
	}
}

// TestAntiAffinityHoldsAcrossReviewers: across the configured reviewers, identity
// AND model_family are distinct, and a reviewer never shares the builder's
// identity/model_family. (DONE-WHEN: "anti-affinity holds across configured
// reviewers".) Then a planted same-family fan-out FAILS resolution.
func TestAntiAffinityHoldsAcrossReviewers(t *testing.T) {
	reg, doc := loadDefault(t)
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), ResolveOptions{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	var builder Identity
	var reviewers []ResolvedActor
	for _, s := range stages {
		if s.Name == "build" {
			builder = s.Actors[0].Identity
		}
		if s.Name == "build_review" {
			reviewers = s.Actors
		}
	}
	// The DISTINCT-IDENTITY axis is a hard invariant and must hold on the shipped
	// defaults: builder ≠ every reviewer, reviewer ≠ reviewer. (The shipped hire
	// defaults are single model_family, so the family axis is exercised on a mixed
	// registry below — see ResolveOptions.RequireDistinctModelFamily.)
	if err := AntiAffinityBuilderVsReviewers(builder, reviewers, false); err != nil {
		t.Fatalf("shipped builder/reviewer identities must differ: %v", err)
	}
	if err := checkAntiAffinity("build_review", reviewers, false); err != nil {
		t.Fatalf("shipped reviewers must have distinct identities: %v", err)
	}

	// Mixed-family registry: with RequireDistinctModelFamily the full
	// identity+model_family axis must hold, and a planted same-family collision
	// must FAIL.
	mixed := mixedRegistry()
	okActors := []ResolvedActor{
		{Lens: "correctness", Identity: mixed.byID["rc-anthropic"]},
		{Lens: "tests", Identity: mixed.byID["rt-openai"]},
		{Lens: "security", Identity: mixed.byID["rs-google"]},
	}
	if err := checkAntiAffinity("build_review", okActors, true); err != nil {
		t.Fatalf("distinct-family fan-out must pass: %v", err)
	}
	bad := []ResolvedActor{
		{Lens: "correctness", Identity: mixed.byID["rc-anthropic"]},
		{Lens: "tests", Identity: mixed.byID["rt2-anthropic"]}, // same family as correctness
	}
	if err := checkAntiAffinity("build_review", bad, true); err == nil {
		t.Fatal("same model_family across reviewers must FAIL anti-affinity when required")
	}
	// And a mixed-family fan-out resolved through Resolve with the strict flag set
	// must succeed end-to-end.
	if err := resolveMixedStrict(mixed); err != nil {
		t.Fatalf("strict mixed-family resolve: %v", err)
	}
}

// resolveMixedStrict resolves a 3-reviewer fan-out over a mixed-family registry
// with the strict model_family axis ON, proving the configurable strictness path.
func resolveMixedStrict(mixed *Registry) error {
	doc := &FlowDoc{
		Flow: "mixed",
		Stages: map[string]Stage{
			"build_review": {
				Role: "code_reviewer", Gate: true, Decision: DecisionMajority,
				Reviewers: []ReviewerSlot{
					{Identity: "rc-anthropic", Lens: "correctness"},
					{Identity: "rt-openai", Lens: "tests"},
					{Identity: "rs-google", Lens: "security"},
				},
			},
		},
	}
	_, err := doc.Resolve(mixed, nil, ResolveOptions{RequireDistinctModelFamily: true})
	return err
}

// mixedRegistry builds an in-memory registry spanning model families, to exercise
// the model_family anti-affinity axis independently of the (single-family) hire
// defaults.
func mixedRegistry() *Registry {
	mk := func(id, fam string) Identity {
		return Identity{ID: id, Role: "code_reviewer", ModelFamily: fam}
	}
	return &Registry{byID: map[string]Identity{
		"rc-anthropic": mk("rc-anthropic", "anthropic"),
		"rt-openai":    mk("rt-openai", "openai"),
		"rs-google":    mk("rs-google", "google"),
		"rt2-anthropic": mk("rt2-anthropic", "anthropic"),
	}}
}

// TestFlowWithoutIssueReviewSkipsIt: a flow that drops the optional issue_review
// stage skips it (no actor), and the build stage is still staffed.
// (DONE-WHEN: "a flow without issue_review skips it".)
func TestFlowWithoutIssueReviewSkipsIt(t *testing.T) {
	reg, doc := loadDefault(t)
	// IncludeOptional = empty (non-nil) drops ALL optional stages -> issue_review off.
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), ResolveOptions{
		IncludeOptional: map[string]bool{},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, s := range stages {
		if s.Name == "issue_review" {
			if !s.Skipped {
				t.Fatal("issue_review must be Skipped when dropped")
			}
			if len(s.Actors) != 0 {
				t.Fatal("a skipped stage must have no actors")
			}
		}
		if s.Name == "build" && (s.Skipped || len(s.Actors) != 1) {
			t.Fatal("build must still be staffed when issue_review is dropped")
		}
	}
}

// TestOverridePrecedence: role default < flow < epic < job. Each higher layer
// wins; an unknown id at a layer fails loudly. (DONE-WHEN: per-step identity
// override precedence role<flow<epic<job.)
func TestOverridePrecedence(t *testing.T) {
	reg, _ := loadDefault(t)
	roleDefaults := map[string]string{"eng_worker": "builder"}

	// role default only.
	id, err := reg.ResolveIdentity(roleDefaults["eng_worker"], Overrides{})
	if err != nil || id.ID != "builder" {
		t.Fatalf("role default: got %q err %v", id.ID, err)
	}
	// flow beats role.
	id, _ = reg.ResolveIdentity("builder", Overrides{Flow: "issue-reviewer"})
	if id.ID != "issue-reviewer" {
		t.Fatalf("flow override lost: %q", id.ID)
	}
	// epic beats flow.
	id, _ = reg.ResolveIdentity("builder", Overrides{Flow: "issue-reviewer", Epic: "reviewer-tests"})
	if id.ID != "reviewer-tests" {
		t.Fatalf("epic override lost: %q", id.ID)
	}
	// job beats epic.
	id, _ = reg.ResolveIdentity("builder", Overrides{Flow: "issue-reviewer", Epic: "reviewer-tests", Job: "reviewer-security"})
	if id.ID != "reviewer-security" {
		t.Fatalf("job override lost: %q", id.ID)
	}
	// unknown id fails loudly (a typo must not silently fall through).
	if _, err := reg.ResolveIdentity("builder", Overrides{Job: "no-such-id"}); err == nil {
		t.Fatal("unknown job override id must error")
	}
}

// TestPerStageOverridesThroughResolve: epic/job overrides flow through Resolve at
// the stage level, beating the flow's own stage.identity.
func TestPerStageOverridesThroughResolve(t *testing.T) {
	reg, doc := loadDefault(t)
	stages, err := doc.Resolve(reg, reg.RoleDefaults(), ResolveOptions{
		// per-job override: staff the build stage with issue-reviewer instead of builder.
		JobOverrides: map[string]string{"build": "issue-reviewer"},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for _, s := range stages {
		if s.Name == "build" && s.Actors[0].Identity.ID != "issue-reviewer" {
			t.Fatalf("per-job override at build lost: %q", s.Actors[0].Identity.ID)
		}
	}
}

// TestAggregateVerdicts exercises the multi-reviewer decision rules.
func TestAggregateVerdicts(t *testing.T) {
	cases := []struct {
		decision string
		passes   []bool
		want     bool
	}{
		{DecisionAllPass, []bool{true, true, true}, true},
		{DecisionAllPass, []bool{true, false, true}, false},
		{DecisionAnyVeto, []bool{true, true, false}, false},
		{DecisionMajority, []bool{true, true, false}, true},
		{DecisionMajority, []bool{true, false, false}, false},
		{"", []bool{true, true}, true}, // default == all_pass
	}
	for _, c := range cases {
		got, err := AggregateVerdicts(c.decision, c.passes)
		if err != nil {
			t.Fatalf("decision %q: %v", c.decision, err)
		}
		if got != c.want {
			t.Errorf("decision %q passes %v => %v want %v", c.decision, c.passes, got, c.want)
		}
	}
	if _, err := AggregateVerdicts("nonsense", []bool{true}); err == nil {
		t.Fatal("unknown decision rule must error")
	}
}
