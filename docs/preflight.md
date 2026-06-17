# Pre-flight checklist

A short go-live checklist to run through before pointing Flowbee at a live
repository. Walk it top to bottom; every box should be checked before the first
real issue lands.

## 1. Token scopes

- [ ] The bot's GitHub token (or App installation) can read and write to the
      target repository — `repo` contents, pull requests, and issues.
- [ ] The token can create branches and open PRs. If self-merge is enabled (see
      below), confirm it also has merge permission and the repo's branch
      protection allows it.
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

## 5. Rollback and observability

- [ ] You know how to pause the fleet quickly (stop `serve` / drain workers) if
      something goes wrong.
- [ ] Logs from the control plane and workers are being captured somewhere you
      can read them.
- [ ] You have a way to revert a bad merge (branch protection allows it, or a
      human has admin access).
