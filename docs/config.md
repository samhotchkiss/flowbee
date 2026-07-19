# `flowbee.yaml` — configuration reference

Flowbee loads config in three layers, last-wins:

1. **built-in defaults** (`config.Default()`),
2. **`flowbee.yaml`** in the working directory (or `$FLOWBEE_CONFIG`),
3. **`FLOWBEE_*` environment variables**.

Secrets (the GitHub PAT, the worker-auth HMAC key) go in the environment, never
in the file. `flowbee init` scaffolds a `flowbee.yaml` with sane defaults and
your repo coords prefilled.

---

## GitHub coordinates

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `github_owner` | `FLOWBEE_GITHUB_OWNER` | — | GitHub owner/org, prefilled by `init` from the `origin` remote |
| `github_repo` | `FLOWBEE_GITHUB_REPO` | — | GitHub repo name |
| `github_default_branch` | `FLOWBEE_GITHUB_DEFAULT_BRANCH` | `main` | the integration branch (PR base + branch-protection target) |

The token itself is **not** a config key — it lives in `FLOWBEE_GITHUB_TOKEN`
(a fine-grained, repo-scoped PAT). Workers never receive it; Flowbee is the only
GitHub caller.

When the multi-repo `repos:` block (below) is present, it wins and these
single-repo keys are ignored.

## Store and listeners

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `database_url` | `FLOWBEE_DATABASE_URL` | `~/.flowbee/flowbee.db` | SQLite file (WAL). No database server. Gitignored + litestream'd. (The default is the absolute `~/.flowbee/flowbee.db`, not a cwd-relative path, so CLI queries find the live DB from any directory.) |
| `private_addr` | `FLOWBEE_PRIVATE_ADDR` | `:7070` | the worker API — keep it on loopback / Tailscale |
| `health_addr` | `FLOWBEE_HEALTH_ADDR` | `:7001` | `/healthz` |
| `webhook_addr` | `FLOWBEE_WEBHOOK_ADDR` | `:8443` | GitHub webhooks |
| `log_level` | `FLOWBEE_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Leasing and liveness

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `lease_ttl_s` | `FLOWBEE_LEASE_TTL_S` | `1200` | lease lifetime; **must be ≥ 3 × `heartbeat_interval_s`** (DESIGN §6.3.3). Set it ABOVE a real agent's wall time — too low revokes long builds mid-run and fences their results. |
| `heartbeat_interval_s` | `FLOWBEE_HEARTBEAT_INTERVAL_S` | `30` | worker heartbeat cadence |
| `long_poll_wait_s` | `FLOWBEE_LONG_POLL_WAIT_S` | `30` | worker long-poll hold |
| `river_max_workers` | `FLOWBEE_RIVER_MAX_WORKERS` | `10` | internal job-runner concurrency |
| `no_eligible_worker_s` | `FLOWBEE_NO_ELIGIBLE_WORKER_S` | `120` | how long a `ready` job may sit with no compliant worker before the alarm fires |
| `reconcile_interval_s` | `FLOWBEE_RECONCILE_INTERVAL_S` _(env only)_ | `45` | how often the reconciler probes GitHub for jobs that may have been missed by webhooks. **Env-only — there is no `flowbee.yaml` key**; setting `reconcile_interval_s:` in the file is a silent no-op. |
| `epic_review_handoff_v2` | `FLOWBEE_EPIC_REVIEW_HANDOFF_V2` _(serve selection)_ | `false` until first activation | flag-gated v2 incident slice: durable CI-green→review handoff reconciliation and alerting. `flowbee serve` persists an explicit `1` or `0` in the control-plane DB while holding the writer lock; when the variable is omitted, it reuses that durable selection. Offline CLIs cannot use `0` to bypass a persisted v2 session-control boundary. |
| `capacity_routing_v2` | `FLOWBEE_CAPACITY_ROUTING_V2` _(env only)_ | `false` | fail-closed routing from one complete, fresh, identity/credential-bound capacity generation. Requires the v2 review-handoff flag and exact worker `FLOWBEE_SEAT_ID` values. |
| `capacity_local_host_id` | `FLOWBEE_CAPACITY_LOCAL_HOST_ID` _(env only)_ | — | immutable host identity for the in-process live capacity collector; required while capacity v2 is active. It must match each local seat's operator binding. |
| `capacity_collector_id` | `FLOWBEE_CAPACITY_COLLECTOR_ID` _(env only)_ | — | collector identity for the local HostClient; required in `FLOWBEE_ENROLLED_IDENTITIES`. |
| `capacity_collect_interval` | `FLOWBEE_CAPACITY_COLLECT_INTERVAL` _(env only)_ | `2m` | startup/periodic live fleet-generation cadence; must be `>0` and `<=4m` so watchdog notification precedes the five-minute route-freshness expiry. |
| `phase1_dashboard` | `FLOWBEE_PHASE1_DASHBOARD` _(env only)_ | `false` | enables automatic work-intent promotion and durable Orchestrator delivery obligations. Requires the v2 review-handoff control plane. |
| `build_provider` | `FLOWBEE_BUILD_PROVIDER` _(env only)_ | `codex` | provider backing the v2 build capacity pool. |
| `review_provider` | `FLOWBEE_REVIEW_PROVIDER` _(env only)_ | `grok` | provider backing the v2 review capacity pool. |
| `operations_provider` | `FLOWBEE_OPERATIONS_PROVIDER` _(env only)_ | `grok` | provider backing the operational recovery pool. |
| `alert_webhook_url` | `FLOWBEE_ALERT_WEBHOOK_URL` _(env only)_ | — | authenticated immediate/dead-letter alert sink; required when v2 is active. |
| `alert_webhook_secret_file` | `FLOWBEE_ALERT_WEBHOOK_SECRET_FILE` _(env only)_ | — | owner-only file containing the HMAC-SHA256 key for outbound alert payloads; required when v2 is active. Inline/argv secrets are not accepted. |
| `external_watchdog_id` | `FLOWBEE_EXTERNAL_WATCHDOG_ID` _(env only)_ | — | configured independent dead-man identity; required when v2 is active. |
| `driver_socket` | `FLOWBEE_DRIVER_SOCKET` _(env only)_ | — | tmux-driver v2.4 Unix socket (for example `/tmp/tmux-driver-<uid>/default/api.sock`); required when v2 is active. |
| `driver_token_file` | `FLOWBEE_DRIVER_TOKEN_FILE` _(env only)_ | — | owner-only file containing the Driver control-plane bearer; required when v2 is active. |
| `driver_instance_ref` | `FLOWBEE_DRIVER_INSTANCE_REF` _(env only)_ | `local-driver` | Flowbee-owned inventory key for the configured Driver endpoint. A new Driver `store_id` under this key is handled as a fenced store reset, never cursor continuation. |
| `human_session_key_file` | `FLOWBEE_HUMAN_SESSION_KEY_FILE` _(env only)_ | — | owner-only file containing at least 32 random bytes used to sign expiring dashboard sessions. Required when Phase 1 is enabled or the dashboard listens off-loopback. The key is read from this file; it is never fetched from 1Password or accepted in `flowbee.yaml`. |
| `human_grants_file` | `FLOWBEE_HUMAN_GRANTS_FILE` _(env only)_ | — | owner-only file of explicit `identity@project=role` grants (newline or comma separated). `identity@*=role` is the distinct portfolio grant. Must be configured with the session-key file. |
| `human_loopback_dev` | `FLOWBEE_HUMAN_LOOPBACK_DEV` _(env only)_ | `false` | explicit unauthenticated browser posture for a loopback-only development listener. It is rejected on Tailnet/LAN listeners. |

Before enabling automatic v2 builder launch, explicitly bind every build seat to
its Driver inventory/profile/workspace target:

```bash
flowbee seat bind-driver \
  --box <stable-host-id> --family codex --codex-home <dir> \
  --instance-ref <driver-inventory-ref> \
  --tmux-server-instance-id <server-incarnation> \
  --profile-id codex-builder --workspace-root-id <root-id> \
  --workspace-relative-base <relative-base>
```

This command registers no session, pane, agent-run, or raw tmux identity. Driver
returns those exact UUID identities in the fenced lifecycle receipt. Flowbee then
routes the immutable epic contract through a directional Driver grant and does not
mark the epic `building` until separate provider-message evidence acknowledges it.

`flowbee doctor` fails the `config` check if `lease_ttl_s < 3 * heartbeat_interval_s`.

### Independent dead-man settings

These configure the standalone `flowbee watchdog`, not `flowbee serve`. Keep it under
an independent service manager, preferably on another tailnet host. See the
[external watchdog runbook](runbooks/external-watchdog.md).

| env | default | meaning |
|-----|---------|---------|
| `FLOWBEE_EXTERNAL_WATCHDOG_ID` | — | required stable observer identity; becomes part of every incident idempotency key |
| `FLOWBEE_WATCHDOG_HEALTH_URL` | `http://127.0.0.1:7001/healthz` | independently reachable Flowbee health endpoint |
| `FLOWBEE_WATCHDOG_STATE_FILE` | `$XDG_STATE_HOME/flowbee/watchdog.json` or `~/.local/state/flowbee/watchdog.json` | owner-only local incident and notification-outbox state |
| `FLOWBEE_WATCHDOG_INTERVAL` | `30s` | health polling and durable delivery retry cadence |
| `FLOWBEE_WATCHDOG_TIMEOUT` | `5s` | health and alert HTTP request timeout |
| `FLOWBEE_ALERT_WEBHOOK_URL` | — | required alert receiver shared with the in-process alert drainer |
| `FLOWBEE_ALERT_WEBHOOK_SECRET_FILE` | — | required owner-only file containing the HMAC key; the watchdog does not accept the key through argv or an inline secret environment variable |

## Durability (auto-backup)

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `backup_interval_s` | `FLOWBEE_BACKUP_INTERVAL_S` | `21600` (6h) | cadence of `serve`'s built-in self-backup loop (verified, pruned `VACUUM INTO` snapshot into the backup dir). **Negative = disable** (run your own cron/litestream). Positive values floor at 60s. |
| `backup_keep` | `FLOWBEE_BACKUP_KEEP` | `7` | snapshots retained in the backup dir (older pruned) |
| _(dir)_ | `FLOWBEE_BACKUP_DIR` | `~/.flowbee/backups` | where snapshots are written (use an external disk for real safety) |

The on-disk floor needs no extra services. Litestream to object storage is still the
off-disk production answer — see [operating.md §6](operating.md).

## Autonomous merge (§14)

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `allow_self_merge` | `FLOWBEE_ALLOW_SELF_MERGE` | `true` _(scaffold)_ / `false` _(engine)_ | **`true`** = Flowbee merges an approved + content-clean + CI-green-at-head job itself, no human gate (the production posture). `false` = every approved job hands off to a human. The `flowbee init` scaffold opts **in** (`true`); the built-in engine default when no `flowbee.yaml` is present is the safe `false` (handoff). |

The safety net for autonomous merge is entirely deterministic: a
content-integrity gate, CI green at the *integrated* head, and a reconciled,
SHA-bound verdict — never a worker's say-so.

## Content-integrity gate

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `content_max_diff_bytes` | `FLOWBEE_CONTENT_MAX_DIFF_BYTES` | shipped default | a diff over this is forced to handoff |
| `content_max_changed_files` | `FLOWBEE_CONTENT_MAX_CHANGED_FILES` | shipped default | a diff over this is forced to handoff |
| `content_deny_extra` | `FLOWBEE_CONTENT_DENY_EXTRA` (CSV) | — | extra path-prefix denylist; **augments**, never replaces, the always-on protected set (CI config, lockfiles, secrets, Flowbee's own source) |

### Per-repo: `allow_own_source_merge`

Set on a repo in the `repos:` registry (not a global key). The shipped denylist
includes a **`flowbee_source`** class — `internal/`, `cmd/flowbee/`, `tools/`,
`flows/`, `flowbee.yaml` — that stops an agent autonomously merging changes to
**Flowbee's own control-plane source**. That protection is correct **only for the
repo that actually contains Flowbee's source**. For any *other* managed repo, those
are the repo's own paths (most Go projects have `internal/` + `cmd/`), so leaving it
on wrongly forces every such change to the human gate, defeating autonomous
self-merge.

```yaml
repos:
  - id: web                    # a repo you manage that is NOT Flowbee itself
    owner: acme
    repo: web
    allow_own_source_merge: true   # its internal//cmd/ are ITS code → may self-merge
  - id: flowbee                # the control plane's OWN repo
    owner: acme
    repo: flowbee
    # (omit the flag → default false → fully protected; NEVER set it here)
```

Default **false** = the fully-protected posture (no behavior change). It relaxes
**only** the `flowbee_source` class — the universal classes (CI config, lockfiles,
Dockerfiles, secrets) are **never** relaxed in any repo. Never set it for the repo
that *is* Flowbee.

### Per-repo: `archive_history`

The §F **compounding memory**: when on, every merge lands a curated
`docs/history/<id>.md` card (status, attempts, verdicts, linked PR, lessons) plus a
regenerated `docs/history/README.md` TOC on the repo's integration branch — in-repo
provenance of how each issue was built, reconstructable from the event ledger. The
write is reconcile-first: each file is one atomic Contents-API commit onto the
branch's current tip (no force-push, no race with concurrent merges), Flowbee the sole
author, never entangled with the feature diff.

```yaml
repos:
  - id: flowbee
    owner: acme
    repo: flowbee
    archive_history: true   # populate docs/history/ on every merge
```

Default **false** because it commits to the repo's `main` on every merge — enable it
only for a repo whose owner wants that in-repo archive.

## Cost circuit-breaker (§6.7)

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `cost_ceiling_usd` | `FLOWBEE_COST_CEILING_USD` | `0` (off) | per-job spend cap in dollars. When `> 0`, the first worker cost report whose accumulated total reaches it revokes the lease and escalates the job to `needs_human` (`over_budget`). `0` = no cap — cost is still metered for the rollup, but a runaway job is bounded only by attempts/bounces. A per-job ceiling seeded at creation (e.g. a costly epic) takes precedence over this fleet default. |

> **The ceiling depends on the agent self-reporting its cost — by design there is no server-side price table** (Flowbee takes `total_cost_usd` directly from the agent, so a model/price change never desyncs the meter). The cost report is emitted only when the agent runs with `claude --output-format json` (the default fleet cmds and `flowbee up` all do this; `parseAgentUsage` reads `total_cost_usd` + `usage`). **A custom agent command that does NOT emit that JSON reports nothing, so the meter stays at $0, the rollup shows $0, and the ceiling can never fire** — the job is then bounded only by the hard caps (`max_attempts`, `max_bounces`, and the absolute `lease_ttl_s` wall-clock cap), not by dollars. If you set a ceiling, run an agent that emits `--output-format json`; a `$0.00` cost in `GET /v1/cost` for a job that clearly ran is the tell that your agent isn't reporting.

## Worker authentication

Empty `worker_auth_secret` = loopback-only dev (the listener must stay on
`127.0.0.1`). Set it for any non-loopback (Tailscale/LAN) listener.

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `worker_auth_secret` | `FLOWBEE_WORKER_AUTH_SECRET` | — | HMAC key signing per-worker bearer tokens (DESIGN §7.6) |
| `enrolled_identities` | `FLOWBEE_ENROLLED_IDENTITIES` (CSV) | — | allowlist of worker identities permitted to authenticate |
| `auth_loopback_bypass` | `FLOWBEE_AUTH_LOOPBACK_BYPASS` | `true` | same-box (127.0.0.1) workers may skip the token |

## Dashboard human authentication

Worker enrollment and dashboard authority are separate trust boundaries. An enrolled
worker cannot approve a design, answer a product question, or control a work intent
unless the same exact identity also has an explicit project-scoped human grant.

Create the signing key and grants as owner-only files:

```bash
umask 077
openssl rand -base64 48 > ~/.flowbee/human-session.key
printf '%s\n' 'sam@*=admin' > ~/.flowbee/human-grants
export FLOWBEE_HUMAN_SESSION_KEY_FILE=~/.flowbee/human-session.key
export FLOWBEE_HUMAN_GRANTS_FILE=~/.flowbee/human-grants
```

Roles are `viewer`, `approver`, `operator`, `planner`, and `admin`. Project `*`
is an explicit portfolio grant; access to one project never implies access to the
global Needs You inbox.

To open a fresh browser session, an already authenticated automation identity with
a matching human grant calls `POST /v1/human/login-links` with
`{"project_id":"<project>"}`. Flowbee returns a ten-minute
`/login#token=...` path. The fragment never reaches HTTP access logs; the exchange
stores only a domain-separated SHA-256 digest, is one-time even across restart, and
sets a signed 12-hour HttpOnly `SameSite=Strict` session cookie plus a separate CSRF
cookie. Dashboard mutations must echo the latter as `X-Flowbee-CSRF`.

The CLI performs that authenticated bootstrap and prints the complete Tailnet-safe
link. Mint/enroll the exact human automation identity once, keep its individual
bearer out of the browser, then run:

```bash
FLOWBEE_WORKER_TOKEN='<token for the granted identity>' \
  flowbee human login-link --url https://flowbee.example.ts.net --project default
```

For the first secure activation of an installation that previously ran with
`FLOWBEE_INSECURE=1` and therefore has no enrolled automation bearer, stop
`flowbee serve` and use the one-time offline bootstrap:

```bash
FLOWBEE_CONFIG=~/.flowbee/flowbee.yaml \
FLOWBEE_HUMAN_SESSION_KEY_FILE=~/.flowbee/human-session.key \
FLOWBEE_HUMAN_GRANTS_FILE=~/.flowbee/human-grants \
  flowbee human bootstrap-link \
    --identity sam --project default \
    --url https://flowbee.example.ts.net
```

This command fails closed when the configured control plane or database writer
is active, when either human file is a symlink/non-regular file or has permissions
wider than owner-only, or when the exact identity lacks a grant for the project.
It acquires the same OS writer lock as `serve`, stores only a domain-separated
digest of the ten-minute one-time bearer, and prints only the fragment URL and
expiry. Start the secure control plane, open the link once, and use authenticated
`human login-link` requests for later sessions. Never run offline bootstrap while
`serve` is active and never copy the fragment into logs or shell history.

## Multiple repos (F9)

One control plane can manage a **set** of repos over a shared, repo-agnostic
worker fleet and a global scheduler. Add a `repos:` list to `flowbee.yaml`:

```yaml
repos:
  - id: core               # short stable handle (scopes jobs/issues/PRs)
    owner: acme
    repo: core
    default_branch: main
  - id: web
    owner: acme
    repo: web
    default_branch: main
    token_env: WEB_PAT     # optional per-repo PAT env var; defaults to FLOWBEE_GITHUB_TOKEN
    # active: false        # register-but-park (loops + scheduling stop) without deleting history
```

When `repos:` is present it takes precedence over the single-repo
`github_owner`/`github_repo` keys.

Rather than hand-edit this block, use **`flowbee repo add`** — it validates and appends an
entry (preserving the file's comments and formatting), refuses a duplicate `id` or
`owner/repo`, and re-checks that the result still loads before writing:

```sh
flowbee repo add acme/web --id web --allow-own-source-merge --reviewers 1
```

`--allow-own-source-merge` sets the merge posture for a managed repo that is **not** the
Flowbee control plane (so its own `internal/`/`cmd/` changes can self-merge); omit it to keep
every merge at the human gate. The control plane reads config at startup, so **restart it** to
pick up the new repo. Then create the `flowbee:build` label + a `pull_request` CI workflow on
the repo, and queue work (`flowbee spec "…" --repo web`, or label an issue `flowbee:build`).

## Credential-less remote workers (bundle mode, F3)

By default a remote worker keeps its own local mirror and pushes the issue branch
itself (it needs repo **read**, and write only to push the branch). For a stricter
zero-trust posture, a worker can hold **no git access and no GitHub credential at
all**: it fetches a read-only git bundle of `base_sha`, returns only a diff, and
the control plane applies the patch and pushes the ref itself.

This requires **both ends to be set** — they are a pair:

| side | setting | meaning |
|------|---------|---------|
| worker | `--bundle` / `FLOWBEE_BUNDLE` | fetch a bundle, return a diff, push nothing |
| control plane | `FLOWBEE_BUNDLE_PROVISIONING` (+ a configured mirror) | apply the returned patch and push the epoch ref on the worker's behalf |

> **Pair them or the build stalls.** A `--bundle` worker pointed at a control
> plane *without* `FLOWBEE_BUNDLE_PROVISIONING` returns a diff the control plane
> never applies — there is no commit to open a PR from, so the job reaches the
> merge gate with no PR and stalls. If you enable `--bundle` on any worker, set
> `FLOWBEE_BUNDLE_PROVISIONING` on the control plane. Most fleets don't need bundle
> mode at all — `flowbee fleet`'s default `--remote` workers (own mirror, push the
> branch) are simpler and the recommended posture.
