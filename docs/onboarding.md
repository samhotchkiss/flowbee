# Onboarding a repo to Flowbee

This is the end-to-end checklist for pointing Flowbee at a **new repository** — the operator
steps on the control plane, the setup steps on the repo, and how to start queuing work. For the
config-key reference see [`config.md`](config.md); for day-to-day running see
[`operating.md`](operating.md).

## The mental model (read this first)

The build/review agents do **not** "integrate with" Flowbee or read its docs. It is the other
way around: **Flowbee runs the agents and hands each one a complete, self-contained brief at
runtime** — the task, the spec, the acceptance criteria, the diff to review, and exactly where
to write its result. Flowbee owns every git mechanic: branching, commits, opening the PR,
running CI, routing review, and merging (plus base_sha refresh after each merge). The agent
just produces a diff (builder) or a verdict (reviewer).

So a repo needs **no Flowbee-specific files**. The only documentation the agents read from the
repo is the repo's own **`AGENTS.md`** (build/test commands + conventions) — picked up
automatically because the agent runs inside a checkout of the repo.

## 1. Register the repo (control plane)

One command on the control-plane host, instead of hand-editing `flowbee.yaml`:

```sh
flowbee repo add <owner>/<repo> --id <short-name> --allow-own-source-merge --reviewers 1
```

- `--id` is a short stable handle that scopes the repo's jobs (defaults to the repo name).
- `--allow-own-source-merge` lets Flowbee auto-merge **this** repo's own `internal/`/`cmd/`
  code (those are the repo's paths, not Flowbee's). **Omit it** to keep every merge at the
  human gate. (Only has effect when global self-merge is on — see [`operating.md`](operating.md) §8.)
- `--archive` opts into the `docs/history/<id>.md` build-provenance archive on each merge.
- `--reviewers N` sets a per-repo consensus-panel size (0 = inherit the global default).

`flowbee repo add` validates the entry, refuses a duplicate `id`/`owner/repo`, preserves the
file's comments + formatting, and re-checks that the result still loads before writing. **Then
restart the control plane** — it reads config at startup (graceful `kill -TERM` the running
`flowbee serve`, then relaunch). The worker fleet is repo-agnostic; it picks the new repo up
with no per-worker change.

## 2. Set up the repo (one-time)

Needs `gh` authenticated to the repo:

- **CI on pull requests — required.** Flowbee will not merge until CI is green on the PR.
  Ensure a workflow runs the build *and* tests on `pull_request` (a clearly-named job, e.g.
  `build-test`). Add a minimal `.github/workflows/ci.yml` if there isn't one.
- **Labels:**
  ```sh
  gh label create "flowbee:build" --description "Hand this issue to Flowbee to build" --color fbca04 --force
  gh label create "flowbee:adopt" --description "Flowbee: adopt into the spec-review flow" --color 7057ff --force
  ```
- **`AGENTS.md` at the repo root** — the exact build command, test command, lint/format
  commands, and any must-follow conventions. This is the only place the build agents learn how
  to build *this* codebase; keep it accurate and concise.
- The control-plane GitHub token (`FLOWBEE_GITHUB_TOKEN`, or a per-repo `token_env`) needs
  read/write to the repo (Contents, Issues, Pull Requests, merge). A fine-grained PAT scoped to
  the repo suffices — no org/admin perms.

## 3. Queue work — two front doors

- **GitHub issue:** add the `flowbee:build` label to any issue. The issue body *is* the build
  brief; structure it so the implementer and reviewer both get a clear spec + done-when:
  ```
  <one-paragraph description of the task>

  ## Spec
  <design / context the implementer needs>

  ## Acceptance Criteria
  - <testable done-when bullet>
  ```
  **Priority (optional):** any issue defaults to priority **5**. To rank it, add a
  `flowbee:p<N>` label — `flowbee:p1` = drop-everything urgent … `flowbee:p10` = nice-to-have
  whenever there's time (**lower = more urgent**). The scheduler runs lower numbers first, and
  aging keeps anything from starving. (When `main` is red, file the *fix* as `flowbee:p1` so it
  jumps ahead of feature work.)
- **CLI:** `flowbee spec "add rate limiting to /login" --repo <short-name>` runs the full
  spec-author → issue-review → build flow. `POST /v1/specs` and `/v1/epics` take a `priority`
  field (1–10, default 5) the same way.

## 4. Watch + recover

- `flowbee doctor` — validate config/token/CI before you start.
- `flowbee status` — one-glance health (warns loudly if the fleet ever wedges).
- `flowbee board` / `flowbee card <job-id>` — all jobs / one job's full timeline.
- `flowbee requeue --state needs_human --repo <short-name>` — re-arm any jobs that bounced out
  to the human-attention park (skips PRs a human deliberately closed).

## 5. Flowbee v2 project and fleet inventory

V2 separates a configured GitHub repository from a product project and its exact
Interactor/Orchestrator routes. Perform inventory mutations while `flowbee serve` is
stopped; every command below takes the control-plane OS writer lock and refuses to
race a live process. Status is read-only and remains available while serve runs.

```sh
flowbee project add --id russ --name Russ
flowbee project attach-repo --project-id russ --repo-id russ
flowbee project bind-actor --project-id russ --role interactor --actor-id russ-claude
flowbee project bind-actor --project-id russ --role orchestrator --actor-id russ-orchestrator
```

An actor launched through Driver lifecycle is bound with its lifecycle target. An
already-running Interactor can instead be adopted only by its complete Driver-owned
stable tuple. Obtain those IDs from Driver; a tmux name, raw `%N`, CWD, PID, provider,
or timestamp is never accepted as authority.

```sh
# Driver-managed actor
flowbee project bind-session --project-id russ --role orchestrator \
  --worker-identity russ-orchestrator --lifecycle-key orchestrator:russ \
  --target-epoch 1 --profile-id grok-orchestrator \
  --workspace-root-id projects --workspace-relative-path russ

# Existing observed Interactor
flowbee project bind-session --project-id russ --role interactor \
  --worker-identity russ-claude \
  --host-id <host_id> --store-id <store_id> --session-id <session_id> \
  --pane-instance-id <pane_instance_id> --agent-run-id <agent_run_id> \
  --tmux-server-instance-id <tmux_server_instance_id>
```

The command re-proves the tuple against current Driver metadata and a current session
snapshot before writing it. Every routed send to an observed-only actor repeats that
exact non-mutating proof before projecting a grant; a pane/store/agent-run replacement
fences the old binding.

For each local seat, discover the v2 identity through the exact production live
provider adapter. This command is read-only, never uses a cache, never prints a token,
and prints the explicit `bind-capacity` command containing only the non-secret account
and credential-lineage digests:

```sh
flowbee seat capacity-probe --family codex --codex-home ~/.codex \
  --host-id <driver-host-id>
# stop serve, review the result, then run the printed bind-capacity command

flowbee seat bind-driver --project-id russ --family codex --codex-home ~/.codex \
  --instance-ref local-driver --tmux-server-instance-id <server-id> \
  --profile-id codex-builder --workspace-root-id projects \
  --workspace-relative-base russ

# Bind each reviewer incarnation to its exact capacity seat. The seat id is
# printed by `flowbee seat list`; it must already have a live capacity identity.
flowbee project bind-session --project-id russ --role code_reviewer \
  --worker-identity reviewer-russ --seat-id '<host>|grok|<grok-home>' \
  --host-id <host_id> --store-id <store_id> --session-id <session_id> \
  --pane-instance-id <pane_instance_id> --agent-run-id <agent_run_id> \
  --tmux-server-instance-id <tmux_server_instance_id>
```

Reviewer onboarding fails closed unless that exact seat is enabled, capacity-bound,
on the same Driver host, and has the same provider family as the observed reviewer.
Interactor and Orchestrator bindings are control actors and must not carry `--seat-id`.

Remote seats remain fail-closed until an authenticated remote capacity collector is
enrolled; `capacity-probe` deliberately does not substitute SSH or a one-off protocol.

Finally, remove `FLOWBEE_INSECURE`, configure `FLOWBEE_WORKER_AUTH_SECRET`, set
`FLOWBEE_AUTH_LOOPBACK_BYPASS=false`, enroll the capacity collector and every reviewer
identity (with its credential-bound family) in `FLOWBEE_ENROLLED_IDENTITIES`, authorize
each scheduling identity's exact capabilities in `FLOWBEE_WORKER_ATTESTATIONS_JSON`,
and mint/distribute a distinct worker token. The collector remains authenticated but
gets an empty capability list:

```sh
export FLOWBEE_ENROLLED_IDENTITIES='reviewer-russ:grok,capacity-local'
export FLOWBEE_WORKER_ATTESTATIONS_JSON='{"reviewer-russ":["role:code_reviewer"],"capacity-local":[]}'
FLOWBEE_WORKER_AUTH_SECRET='<same owner secret as serve>' \
  flowbee token --identity <enrolled-worker-id>
```

`flowbee project status --project-id russ` reports both `configured` (durable static
inventory complete) and `live_ready` (a current complete capacity generation has at
least one routable seat), plus exact actor binding, reviewer, builder-target, capacity,
and worker-auth holds. Do not enable production dispatch until both the project and
worker-auth sections are green. Status proves each reviewer binding's exact capacity
seat is configured/routable and compares the running writer's durable auth-posture
heartbeat with the CLI's effective config; shell-only exports cannot false-green it.

## Appendix — a prompt to hand a setup agent

Paste this into an agent that has access to the repo (and ideally the control-plane host):

> You're connecting **this repository** to **Flowbee**, an orchestrator that builds, reviews,
> and merges labelled GitHub issues with AI agents. You don't handle any git mechanics — this
> is setup only.
>
> 1. **Register the repo** (control-plane host): `flowbee repo add <owner>/<repo> --id <short-name>
>    --allow-own-source-merge --reviewers 1` (drop `--allow-own-source-merge` to keep a human
>    merge gate; add `--archive` for provenance). Then **restart the control plane**. If you
>    can't run `flowbee` there, print the command for the operator.
> 2. **Repo setup** (`gh` authenticated): ensure CI runs build+tests on `pull_request`; create
>    the `flowbee:build` and `flowbee:adopt` labels; ensure `AGENTS.md` at the root has the
>    exact build + test + lint commands and any conventions.
> 3. **Report back**: the repo's `id`, that registration + CI + both labels + `AGENTS.md` are in
>    place, and the build/test commands you found.
