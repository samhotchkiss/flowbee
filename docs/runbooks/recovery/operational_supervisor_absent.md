<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `operational_supervisor_absent`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `human`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `operations_pool_absent_alert`

## Invariant

bounded operational work has a routable supervisor

## Authoritative predicate

operational actions are queued with no compatible operations seat

## Repair

Run the domain action `reconcile_operations_route` under fence `project_id+capacity_generation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
