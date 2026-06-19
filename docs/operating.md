# Operating Flowbee

A start-to-finish runbook: stand up a control plane, bring up a worker fleet, feed it
work, and watch a change go from idea to merged PR — with real agents at every step of the pipeline.

Flowbee is **reconcile-first**: GitHub is the source of truth, the control plane is the
only component that calls the GitHub API and the only one that merges, and workers only
ever commit + push their own branches. If a process dies, the next reconcile re-derives
the world from GitHub + the ledger — there is no hidden in-memory state to lose.

---

## 1. Prerequisites

- **Go 1.22+** to build (`go build -o bin/flowbee ./cmd/flowbee`).
- A **GitHub token** (`FLOWBEE_GITHUB_TOKEN`) with `repo` scope for each managed repo.
- An **agent CLI** on every worker box — by default `claude` (authenticated, on `PATH`).
  Verify with `claude --version`; use `codex` instead when running `--agent codex`.
  The fleet smoke-tests the selected agent before starting.
- **SSH push access** from each worker box to the managed repos if you run SSH remotes
  (`FLOWBEE_GIT_REMOTE=ssh`); otherwise an HTTPS token URL.

---

## Preflight checks (`flowbee doctor`)

Run `flowbee doctor` before starting `flowbee serve` to validate your configuration:

- **Config parsing** — confirms `flowbee.yaml` is valid YAML and passes schema validation.
- **Repo coordinates** — verifies the owner/repo reference resolves on GitHub.
- **Flow file identities** — checks that every identity referenced in the flow file exists.
- **Lens coverage** — ensures each identity has a lens configured.

Pass `--offline` to skip the GitHub reachability check when running in an air-gapped or offline environment.

---

## 2. The control plane (`flowbee serve`)

The control plane runs the scheduler, the per-repo reconcile-IN / project-OUT loops, the
lease API the fleet talks to, and the watchdogs. It owns the single-file SQLite DB.

```sh
FLOWBEE_CONFIG=~/.flowbee/flowbee.yaml \
FLOWBEE_GITHUB_TOKEN=ghp_… \
FLOWBEE_MIRROR_PATH=~/.flowbee/cp-mirror.git \
FLOWBEE_ALLOW_SELF_MERGE=1 \
FLOWBEE_GIT_REMOTE=ssh \
flowbee serve
```

Key environment:

| Variable | Purpose |
|---|---|
| `FLOWBEE_CONFIG` | path to `flowbee.yaml` (repos, lease TTL, tuning) |
| `FLOWBEE_GITHUB_TOKEN` | the API token (sole GitHub caller) |
| `FLOWBEE_MIRROR_PATH` | control-plane bare mirror (history archive + intake base_sha) |
| `FLOWBEE_ALLOW_SELF_MERGE` | `1` lets the gate merge autonomously once a verdict binds |
| `FLOWBEE_GIT_REMOTE` | `ssh` ships `git@github.com:…` remotes to SSH-only worker boxes |
| `FLOWBEE_RECONCILE_INTERVAL_S` | reconcile cadence (default 45s) |
| `FLOWBEE_WORKER_AUTH_SECRET` | shared secret the fleet authenticates with |
| `FLOWBEE_INSECURE` | dev only: disable mTLS/auth on the private API |

Run it as a managed service so it survives reboots and restarts cleanly (the control
plane is the most critical component — don't leave it under bare `nohup`):

```sh
flowbee serve --systemd > /tmp/flowbee-serve.unit   # prints an env file + systemd unit
# install the printed ~/.flowbee/serve.env (fill in FLOWBEE_GITHUB_TOKEN) + the unit, then:
sudo systemctl daemon-reload && sudo systemctl enable --now flowbee-serve
journalctl -u flowbee-serve -f   # the startup line shows the build SHA
```

`flowbee.yaml` lists the repos one board serves (see [`deploy/multi-repo.md`](deploy/multi-repo.md)):

```yaml
lease_ttl_s: 1200          # absolute lease cap — must exceed your longest agent run
repos:
  - id: flowbee
    owner: samhotchkiss
    repo: flowbee
  - id: russ
    owner: samhotchkiss
    repo: russ
```

> **lease_ttl_s** is the un-gameable absolute cap on a lease (§6.7). Set it comfortably
> above your slowest build — a 1200s default covers a 4–8 min agent build with margin. A
> value below the real build time gets leases revoked mid-build.

---

## 3. The fleet (`flowbee fleet`)

One command on each worker box brings up every role — build workers, a code reviewer, a
conflict resolver, a spec author, and a spec reviewer:

```sh
FLOWBEE_REPO_URL=git@github.com:samhotchkiss/flowbee.git \
flowbee fleet --url http://<control-plane-host>:7070 --builders 3
```

What it does:

- Smoke-tests the agent CLI first (fail loud, not mid-job).
- Spawns `--builders` N parallel **eng_worker** build workers (each gets its own worktree
  off a shared per-repo bare mirror under `~/.flowbee/mirrors`).
- Spawns one **code_reviewer**, one **conflict_resolver** (rebases + resolves a PR that
  conflicts after a sibling merged, so conflicts resolve autonomously instead of
  escalating), one **spec_author**, one **spec_reviewer**.
- Workers commit + push with the box's own key; the control plane never gets their creds.
- Reports per-job token + cost usage so the control plane can meter spend.
- Resolves merge conflicts automatically via a dedicated conflict resolver.
- Streams each worker's logs to journald for live tailing.
- Backs the job ledger up to object storage on a schedule.
- Rotates each worker's auth token before it expires.

Flags: `--builders N`, `--mirror DIR`, `--agent claude|codex` (default `claude`;
also `FLOWBEE_FLEET_AGENT`), `--agent-cmd` (review/author roles),
`--build-agent-cmd` (build role — writes files), `--no-smoke`, `--systemd` (print
a managed-service unit + env file and exit).

`fleet` and `up` also accept `--model-label`, which sets the model label shown on
§F history cards. Each card node records the model that performed that node's work,
for example `Lease claimed by feller-builder-2 (codex)`. With `--agent codex`, the
label is `codex`; for Claude agents, it is the Claude model family unless overridden.

Run it as a service so it survives reboots:

```sh
flowbee fleet --url http://<host>:7070 --builders 3 --systemd > flowbee-fleet.service
# install the printed unit + env file, then: systemctl --user enable --now flowbee-fleet
```

### Review-model independence

Flowbee runs genuinely different models for builder and reviewer roles so that reviews are
uncorrelated with the code that produced them. Build workers and spec-author workers default
to Sonnet (`claude --model sonnet`), while the code reviewer, conflict resolver, and spec
reviewer default to Opus (`claude --model opus`). Because the reviewer never shares the
builder's model, it cannot share the same blind spots — a systematic mistake the builder
would miss is more likely to be caught.

Set `required_reviewers` above 1 to require an all-must-pass consensus panel: N distinct
reviewer identities must approve at the current head before the verdict mints. Each
approval below N re-arms the job for the next distinct reviewer; the Nth approval mints the
verdict. Any `changes_requested` at any point bounces the whole job to rebuild, and a new
build resets the round. Configure it globally with `FLOWBEE_REQUIRED_REVIEWERS` or
top-level `required_reviewers:` in `flowbee.yaml`, or per repo with `required_reviewers:`
in that repo's registry entry, which overrides the global value. The default is 1,
preserving the single-reviewer gate. Under `--agent codex`, panel reviewers use the same
model but distinct identities; anti-affinity is per identity, not per model family, so an
N>1 panel is satisfiable on a single-backend fleet.

Operators can override the model per role via the `--agent-cmd` flag (reviewer, conflict
resolver, and spec-reviewer roles) and `--build-agent-cmd` flag (build and spec-author
roles), or the equivalent environment variables `FLOWBEE_AGENT_CMD` and
`FLOWBEE_BUILD_AGENT_CMD`. For example, to point all review roles at a custom wrapper:

```sh
flowbee fleet --url http://<host>:7070 --builders 3 --agent-cmd "claude --model opus-custom"
```

With `--agent claude`, Flowbee keeps genuine cross-model review: Sonnet builds while Opus
reviews and resolves.

### Running on Codex

Install the `codex` CLI on every worker box, authenticate it with `codex login`
(ChatGPT-authenticated), and verify it with `codex --version`. The fleet smoke-tests the
selected agent at startup, just as it does for Claude.

Enable Codex with `flowbee fleet --agent codex` or `FLOWBEE_FLEET_AGENT=codex`. Explicit
per-role overrides still win: `--agent-cmd` for review/author roles and `--build-agent-cmd`
for build/spec-author roles.

With `--agent codex`, every role runs `codex exec` on one Codex model. Roles differ by task
context, not model, so the fleet spends Codex quota instead of the Claude weekly limit. This
trades the §5.5 cross-model review diversity for cost; distinct `model_family` anti-affinity
tags still keep a build off its own review/resolve.

Use `flowbee status` to confirm the live fleet backend, for example
`fleet: 14 live, 0 stale workers (codex:14)`. Use `flowbee card <job-id>` to inspect the
model recorded for each node. Switch back with `--agent claude` (the default), which restores
Sonnet builds and Opus reviews/resolves for cross-model review.

---

## 4. Feeding it work

There are two entry points, by design:

**A labeled GitHub issue → straight to build.** The issue body already *is* the spec, so
intake adopts it as a build cut from current `main`. Add the `flowbee:build` label to any
issue; the next reconcile sweep adopts it (no command needed). The issue body is parsed
into task / spec / acceptance-criteria sections.

**An idea → the full spec flow.** When you start from a vague idea rather than a written
issue, ingest it so a **spec author** drafts the spec first. The first-class CLI for this
front door is `flowbee spec`:

```sh
flowbee spec "add request timeouts to the HTTP client" \
  --repo flowbee --title "HTTP timeouts" --acceptance "context deadline is honored"
```

Only the task description is required (`--repo` defaults to the primary registered repo).
It prints the seeded job id; watch it with `flowbee board` / `flowbee card <id>`. The same
endpoint is reachable directly for scripted ingest:

```sh
curl -s -X POST http://<host>:7070/v1/specs \
  -H 'Content-Type: application/json' \
  -d '{"title":"…","task":"…","acceptance":"…","repo":"flowbee","priority":5}'
```

Either path creates a `spec_authoring` job that flows: **spec_authoring → spec_review → ready →
building → review_pending → code_review → mergeable → merging → done** (see
[`pipeline.md`](pipeline.md)). Every stage is run by a real agent via the worker harness.

---

## 5. Watching it run

- **Local status:** `flowbee status` is a read-only, no-network snapshot: per-repo
  job-state counts, `awaiting human` totals (`merge_handoff`, `needs_human`), and
  the fleet line. The fleet line breaks down live workers by backend, for example
  `fleet: 14 live, 0 stale workers (codex:14)`, so you can confirm at a glance
  which model family a `--agent codex` fleet is running.
- **Liveness:** `GET /v1/fleet-health` → `{live_workers, stale_workers, waiting_jobs,
  stranded}`. `stranded: true` (work waiting, no live worker) is the loud "is the fleet
  up?" signal.
- **Operator queues** (the human-in-the-loop lanes, each on the private API `:7070`):
  `GET /v1/merge-handoff` lists approved PRs awaiting a human merge (with `allow_self_merge`
  off, this is your whole merge queue); `GET /v1/needs-human` lists escalated jobs, each
  tagged with the trigger (attempts/bounces/reviewer_rejections/cost/stall/ci_stalled/project_out); `GET /v1/needs-input` lists design
  forks awaiting an answer. Inspect a job's full story first with **`flowbee card <job-id>`**
  (its verdicts, lessons, timeline, and per-node model labels such as
  `Lease claimed by feller-builder-2 (codex)`, folded from the ledger), then act:
  **`flowbee requeue <job-id>`** re-arms it for a fresh attempt, or **`flowbee cancel
  <job-id>`** terminally dismisses a dead end so it leaves the triage view (both take
  `--force` for an actively-leased job; the matching POST endpoints are
  `/v1/jobs/{job}/requeue` and `/v1/jobs/{job}/cancel`).
- **Metrics:** `GET /metrics` on the health listener (`:7001`, same unauthenticated port as
  `/healthz`) emits Prometheus text format — point a scrape at it. Series: `flowbee_jobs{repo,state}`
  (job counts; a missing state means zero — alert on `flowbee_jobs{state="needs_human"} > 0`),
  `flowbee_fleet_workers{status="live"|"stale"}`, `flowbee_fleet_waiting_jobs`,
  `flowbee_cost_micro_usd_total` (cumulative metered spend), `flowbee_jobs_over_budget`, and
  `flowbee_github_last_success_age_seconds` (seconds since the last successful GitHub reconcile
  sweep — grows without bound when the control plane can't reach GitHub: an **expired/revoked
  token**, exhausted rate limit, or connectivity loss; `/healthz` carries the error in
  `github_last_error`), `flowbee_db_size_bytes` (the SQLite file size — the ledger
  `job_events` is append-only, so this grows with throughput; see Durability below), and
  `flowbee_outbox_abandoned{action}` (dead-lettered GitHub writes that never took effect —
  critical ones also escalate to `needs_human`, but cosmetic ones are otherwise silent, so
  alert on any growth; each is also logged with a `dead-lettered GitHub write` WARN naming the
  action + job, and **`flowbee retry-outbox <job-id>`** re-arms a job's abandoned actions once
  you've fixed the cause). The pages that matter: a wedged `needs_human` job,
  `flowbee_fleet_workers{status="live"} == 0` with waiting jobs, `over_budget` climbing,
  `flowbee_outbox_abandoned` growing, or `flowbee_github_last_success_age_seconds` past a few
  minutes (all progress has silently stalled).
  Example minimal `prometheus.yml` scrape config:

  ```yaml
  scrape_configs:
    - job_name: flowbee
      scrape_interval: 30s
      static_configs:
        - targets: ['localhost:7001']
  ```
- **The board:** the control plane logs each state transition, so `journalctl`/stdout is
  the live board. Each transition is also queryable from the single-file SQLite DB.
- **The git trail** is the durable record: each issue lives on `flowbee/issue-N`; every
  node commits to it (builder + reviser author real commits; the reviewer lands an empty
  `review(<id>): APPROVED|CHANGES REQUESTED` commit), and the merge is a merge commit so
  `git log --first-parent main` reads as a clean history of merged work.

### Per-Project Board Marks

When a board serves multiple repositories, each project is automatically assigned two
distinct visual marks that appear on its cards — no operator configuration required.

**Project emoji.** Each project (repository) receives a unique emoji. The emoji appears on
every board card belonging to that project, making it easy to identify which repo a card
comes from at a glance.

**Colored left-border stripe.** Each project is also assigned a distinct color. Board cards
display a colored left-border stripe in that project's color, letting operators visually
group and distinguish cards from different projects without reading the repo name. Like the
emoji, the color is assigned automatically.

**Timer urgency is on the timer chip, not the stripe.** Per-card urgency indicators (overdue,
due-soon, etc.) appear on the timer chip. The left-border stripe reflects only project
identity — its color does not change based on timer state.

---

## 6. Durability & backup

Flowbee's state lives in **two places**, and they recover differently:

- **GitHub** is ground truth for the facts it owns (PR exists, CI status, merged). These
  re-derive on the next reconcile — nothing to back up.
- **`flowbee.db`** holds the **ledger** — the Domain-A source of truth (the job graph,
  every verdict, the full lineage). The jobs table is a fold of it. **This is the thing
  to back up:** lose it and you lose orchestration state that GitHub can't reconstruct.

The DB runs in WAL mode, which is **litestream-friendly** — but Flowbee does NOT run
litestream for you. For any real deployment, run it as a sidecar so the DB streams
continuously to object storage:

```yaml
# /etc/litestream.yml
dbs:
  - path: /home/sam/.flowbee/flowbee.db
    replicas:
      - url: s3://my-bucket/flowbee-db    # or gcs://, abs://, file:// for a local disk
```

```sh
# run it alongside the control plane (its own systemd unit, or `litestream replicate`)
sudo systemctl enable --now litestream
# disaster recovery: restore before starting flowbee serve
litestream restore -o /home/sam/.flowbee/flowbee.db s3://my-bucket/flowbee-db
```

No object store handy? **`flowbee backup`** is the turnkey on-disk floor: it takes a
consistent snapshot (safe to run while `serve` is live — WAL), **integrity-checks it**,
and prunes to the most recent N:

```bash
flowbee backup                 # snapshot -> ~/.flowbee/backups/, keep last 7
flowbee backup --dir /mnt/ext/flowbee-backups --keep 30   # an external disk is better than the same one
# schedule it (cron/launchd) for an ongoing floor:
# 0 * * * *  flowbee backup --dir /mnt/ext/flowbee-backups
```

To recover from a snapshot, **`flowbee restore`** does it safely — it verifies the
snapshot first (integrity + ledger rows), safety-backs-up the current DB (so the restore
is itself reversible), and replaces atomically:

```bash
flowbee serve   # ← STOP serve first (a restore under a running server is unsupported)
flowbee restore --latest --force         # restore the newest snapshot in the backup dir
flowbee restore ~/.flowbee/backups/flowbee-20260619-011338.402.db --force   # or an explicit one
```

`--force` is required (the confirmation gate). The restore is internally consistent
because the jobs table is a pure fold of the append-only ledger.

It's the equivalent of `sqlite3 .backup` but it knows the DB path, verifies the copy, and
rotates — a backup you can't restore is no backup. **It is still a *floor*:** same-disk
snapshots don't survive disk loss, so litestream's continuous WAL replication to object
storage is the production answer (point-in-time recovery, seconds of data loss vs. a day).
The ledger is append-only, so a restore is always internally consistent — replay folds the
jobs table back exactly.

**On growth:** `job_events` is the append-only ledger (the source of truth), so the DB grows
with throughput — watch `flowbee_db_size_bytes`. SQLite handles multi-GB comfortably and it is
litestream-replicated, so this is a slow scaling line (≈hundreds of MB/year at a steady cadence),
not a near-term limit. The `jobs` table (one row per job, the final projection + verdict +
counters), the GitHub `audit_log`, and the git history are the durable record of *what happened*;
the per-job event timeline is the fine-grained detail. A retention policy that archives + prunes
old **terminal** jobs' events is a deliberate, opt-in future feature — terminal jobs are never
re-folded, but pruning the source of truth is not something to default-on. Each merged job also
adds one `flowbee: archive history for <id>` commit to the integration branch, containing the
`docs/history` card and regenerated TOC together, and a re-drain is idempotent.

### Crash recovery

When the control plane restarts, Flowbee recovers without operator intervention. GitHub is
the source of truth: the board sweep and targeted per-job refetches run on startup and
re-derive the full desired state from scratch — no in-memory work is lost.

All GitHub writes are idempotent by design. A re-sent action after a crash recovers rather
than duplicates: an already-merged PR is treated as success, a duplicate PR-open recovers
the existing PR on a 422, and issue-create guards on the stamped issue number so a replay
is a no-op.

The webhook inbox is durable. Deliveries recorded but not yet processed before a crash are
replayed on boot, so no incoming event is silently dropped.

Worker crashes self-heal. A silently-dead worker stops heartbeating; the lease watchdog
reaps its lease after a few missed beats (~4 minutes) — well before the absolute
`lease_ttl_s` cap — and the job re-queues for a live worker without operator action.

The four automatic recovery mechanisms, in summary:

- **Reconcile-from-truth**: restart re-derives all state from GitHub.
- **Idempotent writes**: merge / PR-open / issue-create are safe to replay.
- **Webhook replay**: the durable inbox is drained on boot.
- **Lease reap**: dead workers' leases are reclaimed in ~4 min.

---

## 7. Recovering from trouble

Flowbee is built so nothing wedges permanently — but here is the operator's toolkit:

| Symptom | What's happening | Action |
|---|---|---|
| `stranded: true` in fleet-health | jobs waiting, no live worker | start/restart `flowbee fleet` on a box |
| A job sits in `needs_human` | a transient failure escalated, or a no-eligible-worker dead-end | `flowbee card <job-id>` to see its full story, then fix the cause and `flowbee requeue <job-id>` to re-arm — or `flowbee cancel <job-id>` to dismiss it if it's a dead end |
| A job keeps failing CI | the rebuild bounced `max_bounces` times (total, across all reviewers) | it auto-escalates to `needs_human`; inspect the PR's CI logs, fix, requeue |
| A job parks with trigger `reviewer_rejections` | ONE review node requested changes on the same task 6 times — a genuine standoff, not normal iteration | read that reviewer's findings on the PR; the disagreement needs a human call, then `flowbee requeue <job-id>` |
| A job parks with trigger `ci_stalled` | its PR's CI never went green for the whole stall window — CI is wedged (runner down, no workflow triggered, or perpetually pending), not merely slow | fix CI (restart the runner / check the workflow triggers / re-run the run), then `flowbee requeue <job-id>` |
| A job parks with trigger `project_out` | a GitHub write for it (open-PR / merge / create-issue) failed permanently — the branch/PR was deleted, a 422/404 — so the action was dead-lettered (the rest of the outbox keeps flowing) | fix the GitHub state (the branch/PR), then `flowbee requeue <job-id>` |
| "which binary is running?" | a stale deploy | `flowbee version` prints the embedded git SHA (`flowbee version --json` for tooling: `{"version":"…"}`); compare it to `/healthz`'s `version` |
| Suspect a stuck `ready` job | a projection drifted from the ledger | the forward-progress watchdog resyncs it within 60s; it can't persist |

The **forward-progress watchdog** (runs every 60s) is the safety net: it re-folds each
`ready` job's ledger and repairs any projection that has drifted (so a capability mismatch
can never make a job unclaimable), and escalates a genuinely no-eligible-worker job to
`needs_human` so a human always sees it. You should rarely need the table above.

### Pausing the fleet (`flowbee pause` / `flowbee resume`)

When something looks wrong and you want to hold new work *without* dropping what's in
flight, pause gracefully instead of killing `serve`:

```bash
flowbee pause     # control plane stops issuing NEW leases; workers idle after their current job
# ...in-flight jobs finish + submit normally; investigate...
flowbee resume    # leasing resumes
```

`pause` only gates *new* claims — already-leased jobs keep heartbeating and submitting
results, so no work is lost. `flowbee status` shows a clear `PAUSED` banner while it's on.
(It's a marker file beside the DB; it takes effect live, no restart, and survives a
`serve` restart — `resume` to clear it.)

---

## 8. Self-merge, briefly

Once a reviewer's verdict binds to the reconciled head/base SHA and CI is green, the gate
merges autonomously (with `FLOWBEE_ALLOW_SELF_MERGE=1`). **Do not push to a repo's `main`
while one of its issues is in review** — moving the head supersedes the SHA-bound verdict
and the merge falls back to a human (`merge_handoff`). That is correct safety behavior,
not a bug.
