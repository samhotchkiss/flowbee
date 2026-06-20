# Prometheus Metrics

Flowbee exposes Prometheus text-format metrics from the health listener at
`:7001/metrics`, on the same unauthenticated listener as `/healthz`. Scrape this endpoint
from a private network or a host-local collector.

Metric names, labels, and types are defined by the `/metrics` handler in
`internal/api/server.go`. Treat that handler as the source of truth when changing this
reference.

## Alerting Conventions

The thresholds below are starting points. Tune them to the repository size, merge volume,
normal CI duration, and worker fleet size for each deployment.

For counters, alert on `rate()` or `increase()` over a window rather than the raw
cumulative value. For gauges that represent state, alert on sustained non-normal values to
avoid noise unless the condition should stop the line immediately.

Some series are emitted only when the backing aggregate exists. For example, a job state
with no jobs has no `flowbee_jobs` series, and a repo with no pending merge has no
`flowbee_oldest_pending_merge_age_seconds` series.

## Alerting Rules

The Prometheus alert definitions are maintained in
[`deploy/prometheus-rules.yml`](../deploy/prometheus-rules.yml). This section explains how
to interpret those deployed rules and how to tune their thresholds for different Flowbee
installations. The entries below document the alerts in the `flowbee.rules` group; keep
this section aligned with that file whenever alert expressions, thresholds, or `for`
durations change.

### FlowbeeGitHubReconcileFailing

- **Fires when:** `flowbee_github_last_success_age_seconds` stays above `900` seconds
  for `10m`. In plain terms, Flowbee has gone more than 15 minutes since a successful
  GitHub reconcile, and that stale condition persisted long enough to rule out a brief
  scheduling delay.
- **Why it matters:** GitHub reconcile is how Flowbee learns about issues, PRs, CI state,
  and merge outcomes. If it is stale, the board can stop adopting work, miss CI or review
  transitions, or delay merges even if workers are healthy.
- **Tuning:** Tune the `900` second threshold around the expected reconcile cadence and
  GitHub API reliability for the deployment. Keep the threshold several multiples above
  `FLOWBEE_RECONCILE_INTERVAL_S`, and adjust the `for: 10m` duration if transient GitHub
  or network blips are expected. Avoid making this too loose: stale reconcile means the
  control plane is no longer reliably seeing GitHub truth.

### FlowbeeApprovedPRAwaitingMerge

- **Fires when:** `flowbee_oldest_pending_merge_age_seconds` stays above `1800` seconds
  for `10m`. This means the oldest approved or currently merging PR has been waiting
  more than 30 minutes, and the delay is sustained.
- **Why it matters:** Approved work that cannot merge can indicate stuck CI, disabled
  autonomous merge, merge contention, branch protection trouble, or a wedged merge loop.
  The longer it waits, the more likely sibling work will rebase, conflict, or age out of
  its intended review context.
- **Tuning:** Set the `1800` second threshold from the repo's healthy merge latency SLO,
  normal CI runtime, and expected queueing buffer. Larger repositories or slower CI may
  need a higher threshold after checking healthy p95/p99 merge ages. Adjust `for: 10m`
  to suppress short bursts during expected merge waves while still catching a sustained
  stuck merge queue.

### FlowbeeJobsWaitingWithNoLiveWorkers

- **Fires when:** `flowbee_fleet_workers{status="live"} == 0` and
  `flowbee_fleet_waiting_jobs > 0` are both true for `5m`. The alert requires ready work
  to be waiting while the live worker count is zero, so an idle fleet with no queued jobs
  does not fire.
- **Why it matters:** Flowbee cannot make progress without live workers. Ready jobs will
  sit unleased, issues will not move through build or review, and automation can appear
  healthy at the control plane while the execution fleet is down.
- **Tuning:** Tune the `for: 5m` duration to normal worker restart, deploy, and agent
  startup time. Keep the zero-live-worker threshold strict for production fleets; if some
  roles run on separate pools, add role-specific alerting in the rule file rather than
  raising this aggregate threshold. For larger fleets, page on partial capacity loss with
  an additional rule, but keep this all-workers-down alert sensitive.

### FlowbeeDroppedGitHubWrites

- **Fires when:** `flowbee_outbox_abandoned > 0`. The metric is grouped by `action`, so
  the firing series identifies the kind of GitHub write that was abandoned or dead-lettered.
  This rule has no `for` duration, so it fires as soon as Prometheus observes any abandoned
  actionable write.
- **Why it matters:** Abandoned GitHub writes mean Flowbee gave up on an operation such
  as opening, updating, or merging through GitHub after retry policy was exhausted. The
  affected job usually needs operator repair before it can continue cleanly.
- **Tuning:** Any abandoned write is normally actionable, so keep the `> 0` threshold
  strict. If a deployment has a known harmless abandoned action, prefer fixing or
  suppressing that action label in Alertmanager rather than raising the rule globally.
  Add a short `for` duration only if scrape timing causes duplicate noise after operators
  have confirmed the gauge clears quickly during healthy recovery.

### FlowbeeJobsOverBudget

- **Fires when:** `flowbee_jobs_over_budget > 0`. The metric counts jobs whose recorded
  agent cost has breached the configured budget. This rule has no `for` duration, so it
  fires on the first scrape that sees one or more over-budget jobs.
- **Why it matters:** Over-budget jobs can indicate an undersized job budget, unexpectedly
  expensive work, repeated rebuild or review loops, model/tool failures, or a workload that
  should be split before automation continues spending.
- **Tuning:** Keep the `> 0` threshold for unattended autonomous deployments where cost
  overruns require immediate inspection. If budgets are intentionally tight during tuning,
  either adjust job budgets or add Alertmanager routing for known test repositories. Add a
  `for` duration only when operators accept a short delay before investigating spend
  overruns.

### Tuning Guidance

- **Repository size:** Larger repositories may naturally increase scan, indexing, merge,
  rebase, and processing durations. Raise latency or duration thresholds only after checking
  healthy-period p95 and p99 behavior for that repository, then leave margin for normal CI
  variance. Keep error-rate alerts strict unless a larger repository is known to produce
  harmless retries that are already understood and bounded.
- **Merge volume:** Higher merge frequency can increase queue depth, branch contention,
  conflict rates, and background reconcile or merge load. Tune backlog, queue, and
  throughput thresholds against normal peak merge windows rather than quiet periods. Adjust
  `for` durations to avoid paging on short expected bursts, but keep them short enough to
  catch sustained overload before approved work ages past its SLO.
- **Fleet size:** Larger fleets can increase aggregate counts while reducing per-instance
  pressure, depending on the metric. Distinguish per-instance alerts from fleet-wide
  aggregate alerts before changing thresholds, and scale thresholds in proportion to
  worker or instance count only when the expression is an aggregate count. Keep
  availability, scrape, and missing-target alerts sensitive enough to catch partial fleet
  loss instead of only total outages.
- **Concrete thresholds:** Change alert thresholds, rate windows, and `for` durations in
  `deploy/prometheus-rules.yml` when operators need different sensitivity. Prefer changing
  one dimension at a time: the threshold for what is abnormal, the PromQL lookback window
  for how the signal is smoothed, or the `for` duration for how long the abnormal condition
  must persist before firing.

## Metric Reference

| Metric | Type | Labels | Meaning | Alert guidance |
|---|---|---|---|---|
| `flowbee_build_info` | gauge | `version` | Build metadata. The value is always `1`; the `version` label identifies the running Flowbee build. | Do not page on the value. Use it for inventory, deployment verification, and detecting mixed versions across control planes. |
| `flowbee_github_last_success_age_seconds` | gauge | none | Seconds since the last successful GitHub reconcile sweep. It grows while Flowbee cannot complete GitHub reconciliation. | Alert when the age exceeds the expected reconcile cadence, commonly 5-15 minutes. This can indicate an expired or revoked token, GitHub API/rate-limit trouble, network failure, or a stalled reconcile loop. |
| `flowbee_db_size_bytes` | gauge | none | Current on-disk SQLite database size in bytes, including main DB, WAL, and SHM files. | Alert when it approaches the storage budget or grows faster than expected. Pair this with host disk free-space monitoring when the DB is local. |
| `flowbee_jobs` | gauge | `repo`, `state` | Current job count grouped by repository ID and Flowbee job state. Missing `repo`/`state` series mean zero jobs in that bucket. | Alert on actionable states such as `needs_human` staying above zero, or on unexpected queue buildup for active repos. Use this with fleet and GitHub health metrics to tell stuck work from normal backlog. |
| `flowbee_oldest_pending_merge_age_seconds` | gauge | `repo` | Age in seconds of the oldest job in `merge_handoff` or `merging` for each repository. This is the age of the oldest approved or in-flight merge waiting to finish. | Alert when the value exceeds the repo's merge latency SLO. A common starting point is sustained age above 30-60 minutes for active repos, or above normal CI duration plus review/merge buffer. |
| `flowbee_unstick_total` | counter | none | Cumulative number of `merge_handoff` PRs fast-forwarded by the unstick sweep since process start. | Alert on repeated or accelerating `increase(flowbee_unstick_total[...])`. Occasional movement can be normal on repos requiring up-to-date branches; frequent increases mean approved PRs are repeatedly falling behind and relying on recovery. |
| `flowbee_dispatch_paused` | gauge | none | Global dispatch state. `0` means dispatch is active; `1` means global dispatch is paused and no leases are issued to workers. | Alert when it remains `1` outside a planned maintenance or incident window. New work may stop dispatching while this is set. |
| `flowbee_repo_parked` | gauge | `repo` | Per-repository park state. `0` means the repo is active; `1` means the repo is parked and its jobs are withheld from leasing. | Alert when a production repo is parked unexpectedly or remains parked longer than intended. Use a sustained alert unless parking should be immediately visible for that repo. |
| `flowbee_main_ci_red` | gauge | `repo` | Main/integration branch CI state by repository. `0` means main CI is not red; `1` means the repo's integration branch CI is red. | Alert immediately or after a short debounce for protected or high-traffic repos. Red main can block merges and make feature PR CI results unreliable. |
| `flowbee_fleet_workers` | gauge | `status` | Registered worker count by liveness bucket. The handler emits `status="live"` and `status="stale"`. | Alert when `status="live"` is zero while work is expected or `flowbee_fleet_waiting_jobs` is non-zero. Alert when stale workers remain non-zero for a sustained interval, or when live worker count drops below the configured pool size. |
| `flowbee_fleet_waiting_jobs` | gauge | none | Number of ready jobs with no worker yet. | Alert when this stays above zero while live workers are available, or when it is above zero and `flowbee_fleet_workers{status="live"} == 0`. Tune duration to normal dispatch and build startup latency. |
| `flowbee_cost_micro_usd_total` | counter | none | Cumulative metered agent spend in micro-USD. One USD is `1,000,000` micro-USD. | Alert on spend burn rate using `rate()` or `increase()`, such as hourly or daily spend above budget or historical baseline. Do not alert on the raw cumulative total. |
| `flowbee_jobs_over_budget` | gauge | none | Current count of jobs whose recorded cost breached their configured budget. | Alert on sustained non-zero values or sudden increases. This can indicate mis-sized budgets, runaway work, model/tool failures, or unexpected workload changes. This is a gauge in the handler, not a counter. |
| `flowbee_outbox_abandoned` | gauge | `action` | Current count of actionable abandoned, dead-lettered GitHub writes grouped by outbox action. The handler uses the `action` label; action values come from stored outbox actions and are not enumerated by the handler. | Alert on any sustained non-zero value or increase for the same action. Abandoned outbox work usually means automation failed permanently or exceeded retry policy. This is a gauge in the handler, not a counter. |
