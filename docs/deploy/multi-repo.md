# Deploying Flowbee: one control plane, many repos

Flowbee runs as **one control plane** (the only GitHub-API caller + merger) plus a
**fungible worker fleet** (any box runs any role; the lease tells each worker which
repo a job belongs to). One board serves every repo, filterable by repo.

This guide is hardened from a real multi-box, multi-repo, SSH-authenticated run — the
[Troubleshooting](#troubleshooting) section covers the gotchas that only surface live.

## Roles of each machine

| Machine | Runs | Needs |
|---------|------|-------|
| Control plane | `flowbee serve` | a GitHub **token** (write: contents, PRs, issues) |
| Worker boxes  | `flowbee fleet` | **`git push` access** to every managed repo (SSH key or HTTPS helper) — no token, no `gh` |

The control plane is the only thing that calls the GitHub API and the only thing that
**merges**. Workers only ever `git commit`/`push` to per-issue branches; the merge gate
(green CI + minted verdict + content-integrity) is the security boundary.

## 1. Config (control plane)

`flowbee.yaml` (or `$FLOWBEE_CONFIG`):

```yaml
private_addr: ":7070"
database_url: /var/lib/flowbee/flowbee.db
repos:
  - id: app          # short stable handle; scopes jobs/issues/PRs/mirrors
    owner: acme
    repo: app
    default_branch: main
  - id: api
    owner: acme
    repo: api
    default_branch: main
    # token_env: API_PAT   # optional per-repo PAT; defaults to FLOWBEE_GITHUB_TOKEN
```

Leave `FLOWBEE_GITHUB_OWNER/REPO` **unset** when using a `repos:` block (those are the
single-repo fallback). The control plane provisions a **per-repo bare mirror** under the
directory of `FLOWBEE_MIRROR_PATH` (`<dir>/app.git`, `<dir>/api.git`).

## 2. Run the control plane

```bash
FLOWBEE_CONFIG=/etc/flowbee/flowbee.yaml \
FLOWBEE_GITHUB_TOKEN="$TOKEN" \
FLOWBEE_MIRROR_PATH=/var/lib/flowbee/cp-mirror.git \
FLOWBEE_ALLOW_SELF_MERGE=1 \
FLOWBEE_GIT_REMOTE=ssh \         # ship SSH repo URLs to workers (see below)
flowbee serve
```

Key environment variables:

| Var | Effect |
|-----|--------|
| `FLOWBEE_ALLOW_SELF_MERGE=1` | autonomous merge — approved + green-CI changes merge with no human gate |
| `FLOWBEE_GIT_REMOTE=ssh` | lease ships `git@github.com:owner/repo.git` to workers (use when boxes auth with SSH keys); default HTTPS |
| `FLOWBEE_INSECURE=1` | accept the open worker API on a non-loopback bind (trusted tailnet only); otherwise set `FLOWBEE_WORKER_AUTH_SECRET` + enrolled identities |
| `FLOWBEE_RECONCILE_INTERVAL_S` | how fast Flowbee notices CI/merge state (default 45s) |

Verify both repos wired: the log shows `multi-repo control plane wired repos=[app api]`.
Dashboard: `http://<host>:7070/dashboard`.

## 3. Run a fleet on each worker box

```bash
FLOWBEE_GITHUB_OWNER=acme FLOWBEE_GITHUB_REPO=app \
flowbee fleet --url http://<control-plane>:7070 --builders 3 \
  --agent-cmd 'claude -p "$(cat "$FLOWBEE_TASK_FILE")"'
```

- **Do not** point `--mirror` at a working-tree checkout. Omit it: the worker keeps its own
  **bare** mirrors at `~/.flowbee/mirrors/<repo>.git`. `--mirror` (if given) is the *directory*
  those bare mirrors live in.
- `FLOWBEE_GITHUB_REPO` here only satisfies the fleet's startup check; a worker builds **any**
  repo — the per-job repo URL comes from the lease.
- The box just needs working `git push` to every managed repo (`ssh -T git@github.com`).

## 4. Give it work

Label a GitHub issue `flowbee:build` in any managed repo, or `POST /v1/specs`. Each issue
becomes a branch `flowbee/issue-N`; the pipeline builds → reviews → self-merges on green CI.
The branch's `git log` is the node-by-node history (build commit, reviewer's empty
findings-commit, revisions); the merge is a merge commit, so `main` keeps the trail.

**`POST /v1/specs` and `POST /v1/epics` require an explicit `"repo"` field once more than
one repo is registered.** With two or more `repos:` entries configured, a repo-less ingest
is now a hard `400` — the API will not guess which managed repo a raw idea/context dump
belongs to. This isn't pedantry: an earlier version silently defaulted a repo-less ingest
to "the primary registered repo (first by id)", and three specs for one managed repo's
product feature (mail/inbox work meant for `russ`) got POSTed without `repo`, silently
landed in a *different* managed repo's (`flowbee`'s own) pipeline, and were built, reviewed,
and bounced there for days before anyone noticed — every worker correctly found nothing to
change, because the spec described a different repo's files entirely. Always pass `repo`
explicitly in a multi-repo setup:

```bash
curl -X POST http://<control-plane>:7070/v1/specs \
  -d '{"repo":"app","task":"add request timeouts"}'
```

If you're deciding *which* managed repo a spec belongs to and the answer isn't obvious from
the task text alone, that's a sign the idea needs to name the target file paths/feature area
before it's specced — don't guess and let the spec_author sort it out.

### Adopting a PR Flowbee didn't originate

A PR created outside Flowbee's pipeline — an external agent-pool branch, a hand-pushed
branch, anything on a `codex/*`-style branch — can be pulled INTO Flowbee's review pipeline:

```bash
flowbee adopt --repo russ 3966 3967 3968      # one or more PR numbers
```

Each adopted PR becomes an opted-in `code_reviewer` job in `review_pending`: Flowbee reads
the PR's real state from GitHub, its reviewer judges the diff, and on approval + green CI it
**self-merges** — or routes to `needs_human` on `changes_requested` (there is no `eng_worker`
bound to a foreign branch to bounce the change back to, so a requested change surfaces for a
human rather than looping). `--repo` is required with 2+ managed repos (PR numbers are
repo-scoped); omit it only in a single-repo setup. Adoption is idempotent — a PR Flowbee
already tracks is reported as already-tracked and left alone.

## Troubleshooting

| Symptom | Cause | Fix |
|---------|-------|-----|
| `not a git repository: ~/dev/<repo>` at worktree-add | `--mirror` pointed at a working tree | omit `--mirror` (bare mirrors live at `~/.flowbee/mirrors/<repo>.git`) |
| worker HTTPS clone/auth fails | boxes are SSH-only | set `FLOWBEE_GIT_REMOTE=ssh` on the control plane |
| job stuck in `needs_human` after a transient failure | attempt budget exhausted | `flowbee requeue <job-id>` (resets the budget, re-arms to ready) |
| approved PR routes to `merge_handoff` not self-merge | `main` moved during review (verdict can't bind) | don't push to `main` manually while an issue is in review; let Flowbee own merges |
| `POST /v1/specs`/`/v1/epics` returns 400 "multiple repos registered ... repo is required" | repo-less ingest with 2+ repos configured | pass `"repo": "<id>"` explicitly — see [Give it work](#4-give-it-work) |
| a built issue's diff/PR doesn't match its repo at all (agent "found nothing to change") | a spec was ingested without `repo` and landed on the wrong managed repo (pre-fix silent default, or a caller still guessing) | close/redirect the issue to the correct repo; re-ingest with an explicit `repo` |

## Rule of thumb

**Don't push to a managed repo's `main` by hand while one of its issues is in review** — it
supersedes the SHA-bound verdict. In steady state only Flowbee merges, so this never happens.
