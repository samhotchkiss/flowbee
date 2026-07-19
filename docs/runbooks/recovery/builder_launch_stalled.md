<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `builder_launch_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_builder_launch_stalled`

## Invariant

admitted work has one fenced launch action

## Authoritative predicate

admitted delivery is past state_due_at without a live launch action or visible hold

## Repair

Run the domain action `ensure_builder_launch` under fence `delivery_state_version+action_epoch+target_incarnation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
