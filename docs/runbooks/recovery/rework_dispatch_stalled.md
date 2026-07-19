<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `rework_dispatch_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_rework_dispatch_stalled`

## Invariant

rejected work receives one fenced rework action

## Authoritative predicate

changes_requested or rebuild_in_flight is overdue without current builder evidence

## Repair

Run the domain action `ensure_builder_rework` under fence `delivery_state_version+head_sha+action_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
