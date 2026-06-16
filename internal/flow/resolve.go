package flow

import (
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// FlowDoc is a single configurable-flow document (flows/default.yaml): one named
// flow whose stages are the operator's pipeline. Distinct from flows.yaml's
// `flows:` map; this is the F5 operator surface (optional stages, multi-reviewer
// fan-out). RoleDefaults maps a role name → the default identity id for that
// role (the bottom of the override precedence), populated from the identity
// registry when absent.
type FlowDoc struct {
	Flow         string           `yaml:"flow"`
	Stages       map[string]Stage `yaml:"stages"`
	Independence []string         `yaml:"independence"`
}

// LoadFlowDoc reads a single-flow document from disk. It does NOT run the §5.6
// neutrality lint: a configurable flow references identity ids (data), and the
// neutrality lint guards flows/flows.yaml's control surface, not this file.
func LoadFlowDoc(path string) (*FlowDoc, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read flow doc %s: %w", path, err)
	}
	var d FlowDoc
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse flow doc %s: %w", path, err)
	}
	if len(d.Stages) == 0 {
		return nil, fmt.Errorf("flow doc %s has no stages", path)
	}
	return &d, nil
}

// ResolvedActor is one resolved staffing slot in a resolved stage: the identity
// the worker is fenced to act as, plus the review lens it applies (empty for
// non-review stages). This is the value that goes into the lease (F1): a worker
// can never choose its own identity.
type ResolvedActor struct {
	Stage    string
	Role     string
	Lens     string // review lens axis (correctness|tests|security|""), per slot
	Identity Identity
}

// ResolvedStage is a stage after identity resolution + fan-out expansion. A
// single-actor stage has one Actor; a multi-reviewer fan-out has N. Skipped marks
// an optional stage the operator's flow dropped (it has no actors).
type ResolvedStage struct {
	Name     string
	Role     string
	Gate     bool
	Decision string // multi-reviewer aggregation rule (DecisionAllPass default)
	Actors   []ResolvedActor
	Skipped  bool
	// System marks a stage Flowbee performs itself (e.g. `merge`): it has a role
	// but no agent identity to resolve (no reviewer slots, no identity reference at
	// any layer, and no role default). It is NOT staffed by a worker, so it carries
	// no actor — distinct from a Skipped optional stage.
	System bool
}

// ResolveOptions feeds the per-flow override layers into resolution. EpicID/JobID
// layers come in as identity-id overrides keyed by stage name (epic steering and
// per-job override surfaces). A stage with no entry uses the flow/role default.
type ResolveOptions struct {
	// EpicOverrides[stage] = identity id (epic-level override).
	EpicOverrides map[string]string
	// JobOverrides[stage] = identity id (per-job override; most specific).
	JobOverrides map[string]string
	// IncludeOptional lists optional stages to KEEP. An optional stage absent from
	// this set is dropped (Skipped). A non-nil-but-empty set drops all optional
	// stages; a nil set keeps every optional stage present in the doc (default-on).
	IncludeOptional map[string]bool

	// RequireDistinctModelFamily makes the model_family anti-affinity axis a HARD
	// resolution failure (reviewers must each be a distinct model_family). It is
	// OFF by default because the shipped hire defaults are single-family (hire
	// recommends the same best model per role); the DISTINCT-IDENTITY axis is
	// always enforced. An operator who staffs cross-family reviewers can turn this
	// on to make family diversity a guaranteed property (the §B "same box may build
	// w/ Codex then review w/ Claude" intent).
	RequireDistinctModelFamily bool
}

// Resolve materializes a flow doc into resolved stages against the identity
// registry, applying the F5 override precedence (role < flow < epic < job) per
// stage and expanding multi-reviewer fan-outs. roleDefaults maps role → default
// identity id (the bottom layer). It enforces anti-affinity across a fan-out's
// reviewers (distinct identity AND distinct model_family) and returns an error on
// a violation or an unknown identity id.
func (d *FlowDoc) Resolve(reg *Registry, roleDefaults map[string]string, opt ResolveOptions) ([]ResolvedStage, error) {
	names := make([]string, 0, len(d.Stages))
	for n := range d.Stages {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]ResolvedStage, 0, len(names))
	for _, name := range names {
		st := d.Stages[name]

		// Optional-stage drop: keep only if IncludeOptional permits (nil => keep all).
		if st.Optional && opt.IncludeOptional != nil && !opt.IncludeOptional[name] {
			out = append(out, ResolvedStage{Name: name, Role: st.Role, Gate: st.Gate, Skipped: true})
			continue
		}

		rs := ResolvedStage{Name: name, Role: st.Role, Gate: st.Gate, Decision: decisionOrDefault(st.Decision)}

		ov := func(flowLevel string) Overrides {
			return Overrides{
				Flow: flowLevel,
				Epic: opt.EpicOverrides[name],
				Job:  opt.JobOverrides[name],
			}
		}

		if len(st.Reviewers) > 0 {
			// Multi-reviewer fan-out: one resolved actor per reviewer slot.
			for _, slot := range st.Reviewers {
				id, err := reg.ResolveIdentity(roleDefaults[st.Role], ov(slot.Identity))
				if err != nil {
					return nil, fmt.Errorf("stage %q reviewer %q: %w", name, slot.Identity, err)
				}
				rs.Actors = append(rs.Actors, ResolvedActor{
					Stage: name, Role: st.Role, Lens: slot.Lens, Identity: id,
				})
			}
			if err := checkAntiAffinity(name, rs.Actors, opt.RequireDistinctModelFamily); err != nil {
				return nil, err
			}
		} else {
			// Single-actor stage. A stage with no identity reference at ANY layer and
			// no role default is a SYSTEM stage Flowbee performs itself (e.g. merge):
			// resolve to no actor rather than erroring. An identity referenced but
			// UNKNOWN still fails loudly (a typo must not be mistaken for a system stage).
			flowID := st.Identity
			if !anyOverride(flowID, opt.EpicOverrides[name], opt.JobOverrides[name], roleDefaults[st.Role]) {
				rs.System = true
				out = append(out, rs)
				continue
			}
			id, err := reg.ResolveIdentity(roleDefaults[st.Role], ov(flowID))
			if err != nil {
				return nil, fmt.Errorf("stage %q: %w", name, err)
			}
			rs.Actors = append(rs.Actors, ResolvedActor{
				Stage: name, Role: st.Role, Lens: reviewLensOf(reg, id), Identity: id,
			})
		}
		out = append(out, rs)
	}
	return out, nil
}

// reviewLensOf returns the identity's declared review lens (so a single-reviewer
// review stage still carries its lens into the lease).
func reviewLensOf(_ *Registry, id Identity) string { return id.ReviewLens }

// anyOverride reports whether any precedence layer supplies an identity id.
func anyOverride(layers ...string) bool {
	for _, l := range layers {
		if l != "" {
			return true
		}
	}
	return false
}

func decisionOrDefault(d string) string {
	switch d {
	case DecisionAllPass, DecisionMajority, DecisionAnyVeto:
		return d
	case "":
		return DecisionAllPass
	default:
		return d // unknown decisions are surfaced by AggregateVerdicts, not silently coerced
	}
}

// checkAntiAffinity enforces the anti-affinity axis across a fan-out's reviewers.
// The DISTINCT-IDENTITY axis is ALWAYS a hard fail (no identity may staff two
// lenses of the same fan-out). The DISTINCT-model_family axis is enforced only
// when requireDistinctFamily is set (the shipped hire defaults are single-family;
// see ResolveOptions.RequireDistinctModelFamily). The machine is NOT part of the
// axis (M4): the same box may staff two reviewers as long as identity (and,
// when required, model_family) differ.
func checkAntiAffinity(stage string, actors []ResolvedActor, requireDistinctFamily bool) error {
	seenID := map[string]string{}     // identity id -> first lens that used it
	seenFamily := map[string]string{} // model_family -> first lens that used it
	for _, a := range actors {
		if prev, dup := seenID[a.Identity.ID]; dup {
			return fmt.Errorf("anti-affinity (stage %q): identity %q staffs both lens %q and lens %q",
				stage, a.Identity.ID, prev, a.Lens)
		}
		seenID[a.Identity.ID] = a.Lens
		if requireDistinctFamily && a.Identity.ModelFamily != "" {
			if prev, dup := seenFamily[a.Identity.ModelFamily]; dup {
				return fmt.Errorf("anti-affinity (stage %q): model_family %q shared by lens %q and lens %q",
					stage, a.Identity.ModelFamily, prev, a.Lens)
			}
			seenFamily[a.Identity.ModelFamily] = a.Lens
		}
	}
	return nil
}

// AntiAffinityBuilderVsReviewers checks the cross-stage axis: no reviewer may
// share the builder's identity (always), nor — when requireDistinctFamily is set
// — the builder's model_family (M4 / §6.3.1). Returns an error on the first
// violation.
func AntiAffinityBuilderVsReviewers(builder Identity, reviewers []ResolvedActor, requireDistinctFamily bool) error {
	for _, r := range reviewers {
		if r.Identity.ID == builder.ID {
			return fmt.Errorf("anti-affinity: reviewer %q shares the builder identity %q", r.Lens, builder.ID)
		}
		if requireDistinctFamily && builder.ModelFamily != "" && r.Identity.ModelFamily == builder.ModelFamily {
			return fmt.Errorf("anti-affinity: reviewer %q shares the builder model_family %q", r.Lens, builder.ModelFamily)
		}
	}
	return nil
}

// AggregateVerdicts applies a stage's Decision rule to the per-reviewer pass
// booleans and returns the stage verdict. Unknown decision strings are an error
// (fail loud). majority is a STRICT majority (>half). any_veto fails on any
// single fail (identical to all_pass for boolean verdicts, but named for the
// operator's intent). An empty input is a vacuous pass for all_pass/any_veto and
// a fail for majority (no majority of zero).
func AggregateVerdicts(decision string, passes []bool) (bool, error) {
	pass, total := 0, len(passes)
	for _, p := range passes {
		if p {
			pass++
		}
	}
	switch decisionOrDefault(decision) {
	case DecisionAllPass, DecisionAnyVeto:
		return pass == total, nil
	case DecisionMajority:
		return pass*2 > total, nil
	default:
		return false, fmt.Errorf("unknown decision rule %q (want all_pass|majority|any_veto)", decision)
	}
}
