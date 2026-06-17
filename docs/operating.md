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
  Verify with `claude --version`; the fleet smoke-tests it before starting.
- **SSH push access** from each worker box to the managed repos if you run SSH remotes
  (`FLOWBEE_GIT_REMOTE=ssh`); otherwise an HTTPS token URL.

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

Flags: `--builders N`, `--mirror DIR`, `--agent-cmd` (review/author roles),
`--build-agent-cmd` (build role — writes files), `--no-smoke`, `--systemd` (print a
managed-service unit + env file and exit).

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

Operators can override the model per role via the `--agent-cmd` flag (reviewer, conflict
resolver, and spec-reviewer roles) and `--build-agent-cmd` flag (build and spec-author
roles), or the equivalent environment variables `FLOWBEE_AGENT_CMD` and
`FLOWBEE_BUILD_AGENT_CMD`. For example, to point all review roles at a custom wrapper:

```sh
flowbee fleet --url http://<host>:7070 --builders 3 --agent-cmd "claude --model opus-custom"
```

---

## 4. Feeding it work

There are two entry points, by design:

**A labeled GitHub issue → straight to build.** The issue body already *is* the spec, so
intake adopts it as a build cut from current `main`. Add the `flowbee:build` label to any
issue; the next reconcile sweep adopts it (no command needed). The issue body is parsed
into task / spec / acceptance-criteria sections.

**An idea → the full spec flow.** When you start from a vague idea rather than a written
issue, ingest it so a **spec author** drafts the spec first:

```sh
curl -s -X POST http://<host>:7070/v1/specs \
  -H 'Content-Type: application/json' \
  -d '{"title":"…","task":"…","acceptance":"…","repo":"flowbee","priority":5}'
```

This creates a `spec_authoring` job that flows: **spec_authoring → spec_review → ready →
building → review_pending → code_review → mergeable → merging → done** (see
[`pipeline.md`](pipeline.md)). Every stage is run by a real agent via the worker harness.

---

## 5. Watching it run

- **Liveness:** `GET /v1/fleet-health` → `{live_workers, stale_workers, waiting_jobs,
  stranded}`. `stranded: true` (work waiting, no live worker) is the loud "is the fleet
  up?" signal.
- **Metrics:** `GET /metrics` on the health listener (`:7001`, same unauthenticated port as
  `/healthz`) emits Prometheus text format — point a scrape at it. Series: `flowbee_jobs{repo,state}`
  (job counts; a missing state means zero — alert on `flowbee_jobs{state="needs_human"} > 0`),
  `flowbee_fleet_workers{status="live"|"stale"}`, `flowbee_fleet_waiting_jobs`,
  `flowbee_cost_micro_usd_total` (cumulative metered spend), and `flowbee_jobs_over_budget`.
  The pages that matter: a wedged `needs_human` job, `flowbee_fleet_workers{status="live"} == 0`
  with waiting jobs, or `over_budget` climbing.
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

No object store handy? A periodic `sqlite3 flowbee.db ".backup '/backups/flowbee-$(date +%F).db'"`
is a coarse floor, but litestream's continuous WAL replication is the production answer
(point-in-time recovery, seconds of data loss vs. a day). The ledger is append-only, so a
restore is always internally consistent — replay folds the jobs table back exactly.

---

## 7. Recovering from trouble

Flowbee is built so nothing wedges permanently — but here is the operator's toolkit:

| Symptom | What's happening | Action |
|---|---|---|
| `stranded: true` in fleet-health | jobs waiting, no live worker | start/restart `flowbee fleet` on a box |
| A job sits in `needs_human` | a transient failure escalated, or a no-eligible-worker dead-end | fix the cause, then `flowbee requeue <job-id>` to re-arm it with a fresh budget |
| A job keeps failing CI | the rebuild bounced `max_bounces` times | it auto-escalates to `needs_human`; inspect the PR's CI logs, fix, requeue |
| "which binary is running?" | a stale deploy | `flowbee version` prints the embedded git SHA |
| Suspect a stuck `ready` job | a projection drifted from the ledger | the forward-progress watchdog resyncs it within 60s; it can't persist |

The **forward-progress watchdog** (runs every 60s) is the safety net: it re-folds each
`ready` job's ledger and repairs any projection that has drifted (so a capability mismatch
can never make a job unclaimable), and escalates a genuinely no-eligible-worker job to
`needs_human` so a human always sees it. You should rarely need the table above.

---

## 8. Self-merge, briefly

Once a reviewer's verdict binds to the reconciled head/base SHA and CI is green, the gate
merges autonomously (with `FLOWBEE_ALLOW_SELF_MERGE=1`). **Do not push to a repo's `main`
while one of its issues is in review** — moving the head supersedes the SHA-bound verdict
and the merge falls back to a human (`merge_handoff`). That is correct safety behavior,
not a bug.
