<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `lease_expired`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `stale_lease_result_fenced`

## Invariant

stale actors cannot report after their lease epoch

## Authoritative predicate

a lease deadline passed without renewal or completion

## Repair

Run the domain action `fence_and_requeue_lease` under fence `lease_id+lease_epoch+agent_run_id`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
