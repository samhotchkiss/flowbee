# Pre-flight checklist

A short go-live checklist to run through before pointing Flowbee at a live
repository. Walk it top to bottom; every box should be checked before the first
real issue lands.

## 0. Run `flowbee doctor` — it automates most of this

```bash
flowbee doctor          # validates the config FLOWBEE_CONFIG points at (the one serve runs)
```

`doctor` checks, per repo, much of what follows and prints `green` only when nothing
**fails** (warnings are surfaced but don't block):

- **config / repo-coords / flows / identities** parse and resolve
- **token write** access (push branches, open + merge PRs, close issues)
- **CI triggers on `pull_request`** (the merge gate can go green)
- **branch protection** posture (autonomous merge needs the token to satisfy it)
- **token least-privilege** — warns on a broadly-scoped classic PAT (see §1)
- **durability** — warns if no backup is detected (see §6)
- **worker-auth** — your §7.6 trust posture (see §5)

Run it first; the rest of this list is the human judgement `doctor` can't make.

## 1. Token scopes (least privilege)

- [ ] The bot's GitHub token can read and write the target repos — limited to
      **Contents + Pull requests + Issues** (write). Prefer a **fine-grained PAT**
      (or a GitHub App installation) scoped to exactly those repos.
- [ ] **Not** a broadly-scoped classic PAT. `doctor` warns on one for good reason:
      a token with `repo`/`admin:org`/`delete_repo` that leaks (it lives in the CP
      env + the mirror's credential helper) can delete repos and admin your org.
- [ ] The token can create branches and open PRs; if self-merge is on (see §3),
      it also has merge permission and branch protection allows it.
- [ ] Token is supplied via the environment / secret store, not committed to the
      repo or baked into a config file.

## 2. Fleet is up

- [ ] The control plane (`flowbee serve`) is running and healthy — under systemd,
      `systemctl status flowbee` should be `active (running)`.
- [ ] At least one worker is registered and idle, ready to pick up work.
- [ ] The webhook endpoint is reachable from GitHub (or polling is enabled) so
      new issues and PR events are actually delivered.

## 3. Self-merge flag

- [ ] Decide intentionally whether workers may merge their own approved PRs.
- [ ] If self-merge is **off**, confirm a human (or a separate reviewer fleet) is
      watching the PR queue so work doesn't stall waiting for merge.
- [ ] If self-merge is **on**, confirm required checks and reviews are configured
      so nothing merges without passing the CI green path below.

## 4. CI green path

- [ ] The repo's CI runs on PRs from the bot's branches and reports status back
      to GitHub.
- [ ] Required status checks are listed in branch protection so an unverified
      change cannot merge.
- [ ] A trivial test PR goes green end-to-end (open → CI → approve → merge) before
      you trust the pipeline with real work.

## 5. Worker-API security posture

- [ ] Decide your §7.6 trust boundary. `flowbee serve` **refuses to start** on a
      non-loopback bind without one — pick exactly one:
  - [ ] **`worker_auth_secret`** (+ `enrolled_identities`) — mutual auth; the right
        choice for any untrusted network. (recommended)
  - [ ] **`FLOWBEE_INSECURE=1`** — an OPEN worker API, relying on a trusted private
        network (e.g. Tailscale) as the only boundary. Acceptable on a locked-down
        tailnet; never on the public internet, **especially with self-merge on**.
- [ ] `flowbee doctor`'s `worker-auth` line reflects your choice — confirm it.
- [ ] If distinct workers share one enrolled secret AND you rely on anti-affinity,
      **bind each identity's model family**: write enrolled entries as `identity:family`
      (e.g. `reviewer-bob:claude-opus`). The control plane then clamps the worker's
      self-asserted `model_family`, so it can't be spoofed to defeat the same-family
      review exclusion (§5.5). See [security-model.md](./security-model.md).

## 6. Durability & backup

- [ ] You have a backup strategy — a SQLite control plane on one disk with no
      backup loses **all** state (the ledger + every job) on disk failure.
  - [ ] **Litestream** to object storage — the production answer (continuous,
        off-disk, point-in-time recovery). See [operating.md §6](./operating.md).
  - [ ] and/or **`flowbee backup`** scheduled (cron/launchd) — the on-disk floor.
- [ ] `flowbee doctor`'s `durability` line is green (a backup is detected).
- [ ] You've confirmed a restore works — `litestream restore`, or **`flowbee restore
      --latest --force`** (verifies the snapshot + safety-backs-up the current DB
      first). A restore is internally consistent because the jobs table is a pure fold
      of the append-only ledger.

## 7. Rollback and observability

- [ ] You know how to pause the fleet quickly. Prefer graceful: **`flowbee pause`**
      (stops new leases, lets in-flight jobs finish), **`flowbee resume`** to unpause —
      not a blunt `serve` kill, which drops in-flight work.
- [ ] Logs from the control plane and workers are being captured somewhere you
      can read them (and `flowbee status` gives a one-glance live summary).
- [ ] You have a way to revert a bad merge (branch protection allows it, or a
      human has admin access).
