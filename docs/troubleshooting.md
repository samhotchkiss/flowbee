# Troubleshooting

An operator quick-reference for confirming and fixing Flowbee's common failure
modes. Each row maps a **symptom** to the single **diagnostic** that confirms it
and the **fix** to apply. Note that two of the modes below are *expected
behavior* — a safety gate doing its job, not a fault to force past.

| Symptom | Diagnostic | Fix |
|---|---|---|
| Jobs are waiting/queued but nothing is being processed — no live worker. | `GET /v1/fleet-health` reports the fleet as **stranded** (`stranded: true`). | Start the fleet: `flowbee fleet`. |
| A job is stuck in `needs_human` and will not progress on its own. | Job status shows state `needs_human` — the job is parked pending an operator. | Fix the underlying cause, then requeue it: `flowbee requeue <job-id>`. |
| A change to Flowbee's **own source** sits unmerged in `merge_handoff`. | The job/change is in `merge_handoff` and touches `flowbee_source` (the source-self-modification denylist). | **Expected behavior, not a bug.** The `flowbee_source` denylist requires a human to review and merge changes to Flowbee's own source. A human merges it manually. |
| Unsure which Flowbee binary/version is actually running. | `flowbee version` prints the build's git SHA. | Compare the printed SHA against the intended release; deploy/restart the correct binary if it differs. |
| A worker process crashed. | `systemctl status flowbee-fleet` shows the unit and its restarts. | **None required** — the systemd-managed fleet auto-respawns the crashed worker. Verify recovery via the same `systemctl status flowbee-fleet`. |
| `fleet-health` shows stale workers. | A worker lost its heartbeat and is no longer re-registering. | Restart the fleet on the affected box: `systemctl restart flowbee-fleet`. Workers re-register automatically on startup. |
| A PR sat in `merging` briefly, then merged on its own. | The merge log shows a transient "not mergeable" response followed by a successful retry. | **Expected behavior, not a bug.** GitHub was recomputing mergeability after a sibling merge; Flowbee retried and the merge completed — no action needed. |

## An epic's child issues never start.

**Cause:** The epic barrier review has not signed off yet, or the fan-out step has not run. Child issues are held until the epic's barrier review passes.

**Fix:** Check the epic job state. Once the barrier review approves and the fan-out runs, the children start automatically — no manual intervention is needed.
