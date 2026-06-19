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
| `database_url` | `FLOWBEE_DATABASE_URL` | `flowbee.db` | SQLite file (WAL). No database server. Gitignored + litestream'd. |
| `private_addr` | `FLOWBEE_PRIVATE_ADDR` | `:7070` | the worker API — keep it on loopback / Tailscale |
| `health_addr` | `FLOWBEE_HEALTH_ADDR` | `:7001` | `/healthz` |
| `webhook_addr` | `FLOWBEE_WEBHOOK_ADDR` | `:8443` | GitHub webhooks |
| `log_level` | `FLOWBEE_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |

## Leasing and liveness

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `lease_ttl_s` | `FLOWBEE_LEASE_TTL_S` | `300` | lease lifetime; **must be ≥ 3 × `heartbeat_interval_s`** (DESIGN §6.3.3) |
| `heartbeat_interval_s` | `FLOWBEE_HEARTBEAT_INTERVAL_S` | `30` | worker heartbeat cadence |
| `long_poll_wait_s` | `FLOWBEE_LONG_POLL_WAIT_S` | `30` | worker long-poll hold |
| `river_max_workers` | `FLOWBEE_RIVER_MAX_WORKERS` | `10` | internal job-runner concurrency |
| `no_eligible_worker_s` | `FLOWBEE_NO_ELIGIBLE_WORKER_S` | `120` | how long a `ready` job may sit with no compliant worker before the alarm fires |
| `reconcile_interval_s` | `FLOWBEE_RECONCILE_INTERVAL_S` | `45` | how often the reconciler probes GitHub for jobs that may have been missed by webhooks |

`flowbee doctor` fails the `config` check if `lease_ttl_s < 3 * heartbeat_interval_s`.

## Autonomous merge (§14)

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `allow_self_merge` | `FLOWBEE_ALLOW_SELF_MERGE` | `true` | **`true`** = Flowbee merges an approved + content-clean + CI-green-at-head job itself, no human gate (the production posture). `false` = every approved job hands off to a human. |

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

## Cost circuit-breaker (§6.7)

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `cost_ceiling_usd` | `FLOWBEE_COST_CEILING_USD` | `0` (off) | per-job spend cap in dollars. When `> 0`, the first worker cost report whose accumulated total reaches it revokes the lease and escalates the job to `needs_human` (`over_budget`). `0` = no cap — cost is still metered for the rollup, but a runaway job is bounded only by attempts/bounces. A per-job ceiling seeded at creation (e.g. a costly epic) takes precedence over this fleet default. |

## Worker authentication

Empty `worker_auth_secret` = loopback-only dev (the listener must stay on
`127.0.0.1`). Set it for any non-loopback (Tailscale/LAN) listener.

| key | env override | default | meaning |
|-----|--------------|---------|---------|
| `worker_auth_secret` | `FLOWBEE_WORKER_AUTH_SECRET` | — | HMAC key signing per-worker bearer tokens (DESIGN §7.6) |
| `enrolled_identities` | `FLOWBEE_ENROLLED_IDENTITIES` (CSV) | — | allowlist of worker identities permitted to authenticate |
| `auth_loopback_bypass` | `FLOWBEE_AUTH_LOOPBACK_BYPASS` | `true` | same-box (127.0.0.1) workers may skip the token |

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
