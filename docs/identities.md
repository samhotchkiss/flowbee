# Identities, lenses, and the flow

Flowbee — not GitHub — is the authority on **who did what**. Every actor in the
pipeline has an *identity* (who to be) and a *lens* (how to think). This is what
makes anti-affinity real: a reviewer can't approve its own build, and a
same-model-family reviewer can't rubber-stamp a shared blind spot.

`flowbee init` scaffolds the whole set, seeded from the `hire` corpus. You commit
it, version it, and edit it to taste.

---

## The files

```
flows/
  flows.yaml                 # roles (provider-agnostic; the §5.6 neutrality lint guards this)
  default.yaml               # the configurable pipeline: which stages run, who staffs them
  identities/
    issue-reviewer.yaml      # one identity per actor
    builder.yaml
    reviewer-correctness.yaml
    reviewer-tests.yaml
    reviewer-security.yaml
  lenses/
    issue-reviewer.md        # the operating prompt for each identity
    builder.md
    reviewer-correctness.md
    reviewer-tests.md
    reviewer-security.md
```

## An identity file

```yaml
id: builder
role: eng_worker
stage: build
source_slug: engineering-generalist
display_name: "Casey Nguyễn"
role_name: "Engineering Generalist"
tagline: "Full-stack generalist who builds complete systems…"
model: "claude-opus-4-6"      # DATA — a recommendation, not a control token
model_tier: "heavy-reasoning"
model_family: "anthropic"     # the anti-affinity axis
lens: lenses/builder.md
```

- **`id`** — referenced by `flows/default.yaml`.
- **`role`** — the Flowbee role this identity defaults for.
- **`model` / `model_tier`** — *data*: which model this identity recommends.
  Provider literals are legitimate here. (Identity files are **not** run through
  the §5.6 provider-neutrality lint — that lint guards `flows/flows.yaml`'s
  control surface only.)
- **`model_family`** — the anti-affinity axis: `anthropic`, `openai`, `google`, …
- **`lens`** — the markdown prompt that *is* this actor's way of thinking.

`flowbee doctor` checks that every identity referenced by the flow exists, has a
matching `id`, and points at a lens file that's present.

## The default flow

`flows/default.yaml` is the **operator** pipeline. The default:

| stage | role | identity | notes |
|-------|------|----------|-------|
| `issue_review` | `issue_reviewer` | `issue-reviewer` | **optional** — drop it by omitting the stage. Amends the issue in place. |
| `build` | `eng_worker` | `builder` | writes the patch |
| `build_review` | `code_reviewer` | fan-out of N reviewers | each a distinct lens + model_family; `decision: all_pass \| majority \| any_veto` |
| `merge` | `merger` | — | runs when build-review hands off |

The build-review fan-out is where anti-affinity bites:

```yaml
build_review:
  role: code_reviewer
  decision: all_pass        # all_pass | majority | any_veto
  reviewers:
    - identity: reviewer-correctness
      lens: correctness
    - identity: reviewer-tests
      lens: tests
    - identity: reviewer-security
      lens: security
```

The independence constraints hold builder ≠ each reviewer and reviewer ≠ reviewer:

```yaml
independence:
  - "eng_worker.identity != code_reviewer.identity"
  - "eng_worker.model_family != code_reviewer.model_family"
  - "code_reviewer.identity != code_reviewer.identity"   # distinct reviewers fan-out
```

The anti-affinity axis is **identity + model_family + fresh context, NOT the
machine** — the same box may build with Codex and then review with Claude.

## Models per stage

The recommended defaults wire straight into the stage identities:

- **issue-review = Sonnet** (`issue-reviewer.yaml`)
- **build = a heavy reasoning model** (`builder.yaml`)
- **build-review = Opus / specialist reviewers** (`reviewer-*.yaml`)

To change who builds vs. who reviews, edit the `model` / `model_family` fields in
the identity files (or add a per-step override — precedence is
`role default < flow < epic < per-job`). Re-run `flowbee doctor` to confirm the
flow still resolves.

## Editing a lens

A lens is plain markdown — the operating identity prose. Edit
`flows/lenses/<id>.md` in place. The seeded files carry a provenance header
noting they came from `tools/seedidentities`; editing in place is expected (the
seeder only overwrites if you re-run it).

## Re-seeding from `hire`

The defaults were generated with:

```bash
go run ./tools/seedidentities -hire ~/dev/russell/public/hire -out flows
```

That's a one-shot developer generator, not part of the engine. `flowbee init`
embeds the already-seeded output, so you do **not** need the `hire` repo on disk
to scaffold a new project.
