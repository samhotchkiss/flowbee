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
