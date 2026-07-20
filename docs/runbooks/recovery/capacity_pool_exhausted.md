<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `capacity_pool_exhausted`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `zero_pool_pushes_alert`

## Invariant

eligible work with zero routable seats is durably visible and pushed

## Authoritative predicate

a required build review or operations pool has queued work and zero routes past threshold

## Repair

Run the domain action `reconcile_capacity_pool` under fence `project_id+pool+capacity_generation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
