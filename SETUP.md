# Setting up Flowbee

This is the human-readable setup guide. If you'd rather have your own coding
agent do the whole thing, hand it [`AGENTS.md`](./AGENTS.md) — it's the same
steps written as an agent-followable runbook.

Flowbee is one static binary. There is no Postgres, no Docker, no cloud service.
You bring a GitHub repo, a token, and one or more machines that run coding agents.

---

## 1. Install the binary

```bash
go install github.com/samhotchkiss/flowbee/cmd/flowbee@latest
flowbee version
```

Or build from a clone:

```bash
git clone https://github.com/samhotchkiss/flowbee && cd flowbee
go build -o flowbee ./cmd/flowbee
```

## 2. Scaffold config into your repo

From the root of the repo you want Flowbee to manage:

```bash
cd my-project
flowbee init
```

`flowbee init`:

- writes **`flowbee.yaml`** (the control-plane config), prefilling
  `github_owner` / `github_repo` from your `origin` remote;
- scaffolds **`flows/`** — `default.yaml` (the configurable pipeline),
  `flows.yaml` (roles), `identities/*.yaml` (who staffs each stage), and
  `lenses/*.md` (each identity's operating prompt);
- adds **`flowbee.db`** (+ its WAL/SHM sidecars) to `.gitignore` — runtime state
  is local, not committed;
- prints a **3-item checklist** of what to do next.

`init` is **idempotent**: re-running it never clobbers a file you've edited; it
reports each existing file as `kept` and only writes what's missing.

> **Commit `flowbee.yaml` and `flows/`.** They are versioned config — your
> pipeline, your identities, your models-per-stage. The database is not.

## 3. Give Flowbee a GitHub token

Workers hold **no** GitHub credentials — Flowbee is the only thing that talks to
GitHub. A single, fine-grained, **repo-scoped Personal Access Token** is enough
for one operator (reconcile-first means the 5k/hr budget is never the limit). A
GitHub App is only needed for org-scale / OSS distribution.

```bash
export FLOWBEE_GITHUB_TOKEN=github_pat_...
```

Give the PAT read/write on **Contents, Pull requests, Issues, Checks**, and read
on **Metadata**, for the target repo.

## 4. Confirm it's healthy

```bash
flowbee doctor
```

`doctor` validates that:

- **config** — `flowbee.yaml` parses and passes its invariants
  (e.g. `lease_ttl_s >= 3 * heartbeat_interval_s`);
- **repo coords** — `github_owner` / `github_repo` are set;
- **flow + identities** — `flows/default.yaml` parses and every identity it
  references exists, with its lens file;
- **github** — the token reaches GitHub (skip this offline with
  `flowbee doctor --offline`).

A clean run ends with `flowbee doctor: green`. Warnings (e.g. "token unset,
skipping reachability") do **not** break green — they're the offline path.

## 5. Run it

```bash
flowbee migrate up          # create the SQLite schema
flowbee serve &             # the control plane
flowbee work  &             # a worker — or /loop a Claude session as one
```

Open the board at **http://localhost:7070**, submit an epic, and watch the line
run: issue-review → build → build-review ×N → merge.

---

## Configuration reference

- **[docs/config.md](./docs/config.md)** — every `flowbee.yaml` key and every
  `FLOWBEE_*` environment override.
- **[docs/identities.md](./docs/identities.md)** — how identities, lenses, and
  models-per-stage work, and how to change who builds vs. who reviews.

## Autonomous merge

The shipped default is **autonomous merge** (`allow_self_merge: true`): an
approved, content-clean, CI-green-at-head job is merged by Flowbee itself, with
no human gate. The safety net is deterministic — a content-integrity gate, CI
green at the integrated head, and a reconciled, SHA-bound verdict. To keep a
human in the loop instead, set `allow_self_merge: false` (or
`FLOWBEE_ALLOW_SELF_MERGE=false`).

## Multiple repos / multiple workers

- More repos: see the `repos:` block in `flowbee.yaml` (one control plane, a
  shared worker fleet, a global scheduler). See [docs/config.md](./docs/config.md).
- More workers: run `flowbee work` on each machine. Ideally they're SSH-reachable
  from your main box so an install agent can configure them all from one place
  (see [`AGENTS.md`](./AGENTS.md)).
