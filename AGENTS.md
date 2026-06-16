# AGENTS.md — the Flowbee install runbook (for the agent doing the install)

You are a coding agent. Your human pasted a short prompt that pointed you here.
Your job: install and configure Flowbee on their repo **end-to-end**, ask them
only the handful of decisions that actually need a human, and finish with
`flowbee doctor` reporting green. Everything else, you do.

Work from the human's **main machine**. If they have other machines that should
run agents, configure those over SSH from here — don't make the human touch each
box.

---

## What Flowbee is (so you make good defaults)

Flowbee is a control plane that turns a pool of coding-agent machines into an
engineering org against one GitHub repo. **Workers never touch GitHub** — Flowbee
is the only GitHub caller, and it owns actor identity. The control plane has no
LLM; it's deterministic. The intelligence is in the agents you'll wire up as
workers.

The pipeline per issue: **issue-review → build → build-review ×N → merge.**

---

## Step 1 — install the binary

```bash
go install github.com/samhotchkiss/flowbee/cmd/flowbee@latest
flowbee version
```

If `go install` isn't available, clone and build:

```bash
git clone https://github.com/samhotchkiss/flowbee && cd flowbee
go build -o flowbee ./cmd/flowbee && ./flowbee version
```

## Step 2 — scaffold config into the repo

From the root of the repo Flowbee will manage:

```bash
cd <the repo>
flowbee init
```

This writes `flowbee.yaml` + `flows/` (identities, lenses, the default flow),
prefills `github_owner`/`github_repo` from the `origin` remote, and gitignores
`flowbee.db`. It prints a 3-item checklist — that's your to-do list. `init` is
idempotent; re-running never clobbers an edited file.

**Read the scaffolded `flowbee.yaml` and `flows/default.yaml` now** so you can
answer the human's questions concretely.

## Step 3 — the guided Q&A (ask ONLY these)

Ask the human these, with the defaults below. Don't ask anything else.

1. **Open issues — adopt all, or start fresh?**
   - If *adopting*: triage each open issue. *Ready to build* → it enters the
     pipeline. *Needs more definition* → mark it backlog / `needs_design` (it's
     surfaced for spec'ing first, not auto-built).
   - **Closed-issue history is always backfilled into memory regardless** — that
     seeds the precedent gate (what was tried, what was reverted, why).

2. **Which machines run workers?**
   - Ideally each is SSH-reachable from this main machine, so you can configure
     them all from here: `scp` the binary, set the per-stage agent command, and
     start `flowbee work` on each. No per-box hands-on for the human.

3. **Models per stage?** (recommended defaults — wire into the stage identities
   in `flows/identities/*.yaml`):
   - **issue-review = Sonnet**
   - **build = a strong builder model (e.g. GPT-5.5)**
   - **build-review = Opus**

4. **Autonomous-merge posture?** Default is **Branch B — autonomous merge, no
   human gate** (`allow_self_merge: true`, already set by `init`). The safety net
   is deterministic (content-integrity gate + CI-green-at-head + reconciled,
   SHA-bound verdict). Only flip to `false` if the human explicitly wants a human
   in the merge loop.

Apply their answers by editing `flowbee.yaml` and `flows/` — these are versioned
config; commit them.

## Step 4 — GitHub token

Workers get **no** credentials. Flowbee needs one fine-grained, **repo-scoped
PAT** (read/write on Contents, Pull requests, Issues, Checks; read on Metadata).
Have the human create it, then:

```bash
export FLOWBEE_GITHUB_TOKEN=github_pat_...
```

Put it in the human's shell profile or a secrets manager — never in a committed
file.

## Step 5 — configure each worker (over SSH, from here)

For each machine the human named in Step 3.2:

```bash
scp $(command -v flowbee) <host>:/usr/local/bin/flowbee
ssh <host> 'flowbee work --server http://<main-host>:7070 --agent-cmd "<the per-stage agent command>"'
```

The worker dials *out* to the control plane, leases a job, runs it with whatever
agent it wraps, and reports back. It holds no GitHub creds.

## Step 6 — start it

On the main machine:

```bash
flowbee migrate up      # create the SQLite schema
flowbee serve &         # the control plane (board at http://localhost:7070)
```

## Step 7 — confirm green

```bash
flowbee doctor
```

`doctor` validates config + flow identities + GitHub reachability. A clean run
ends with **`flowbee doctor: green`**. If it isn't green, read the failing check:

- `config` failing → `flowbee.yaml` is malformed or `lease_ttl_s < 3 *
  heartbeat_interval_s`. Fix the file.
- `identities` failing → an identity referenced by `flows/default.yaml` is
  missing, or its lens file is gone. Re-run `flowbee init` to restore, or fix the
  reference.
- `github` failing → the token can't reach GitHub. Check `FLOWBEE_GITHUB_TOKEN`
  and the repo coords. To validate everything else while genuinely offline, run
  `flowbee doctor --offline` (this is a warn, not a failure).

When doctor is green, tell the human: submit an epic and watch the board at
`http://localhost:7070`.

---

## Reference

- [`SETUP.md`](./SETUP.md) — the same steps, human-readable.
- [`docs/config.md`](./docs/config.md) — every config key + env override.
- [`docs/identities.md`](./docs/identities.md) — identities, lenses, models-per-stage.
- [`DESIGN.md`](./DESIGN.md) — the full architecture, if you need to reason about
  why something works the way it does.
