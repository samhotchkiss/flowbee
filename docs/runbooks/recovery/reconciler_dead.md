<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `reconciler_dead`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `stale_reconciler_heartbeat_alerts`

## Invariant

every enabled reconciler advances a durable heartbeat

## Authoritative predicate

reconciler heartbeat is stale past its declared SLO

## Repair

Run the domain action `restart_control_plane_and_reconcile` under fence `process_incarnation+reconciler_generation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
