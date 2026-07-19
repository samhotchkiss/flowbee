<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `cleanup_overdue`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_cleanup_overdue`

## Invariant

merged delivery resources are proven absent

## Authoritative predicate

merged or cleanup_pending is overdue without remote absence evidence

## Repair

Run the domain action `verify_or_dispatch_cleanup` under fence `delivery_state_version+target_incarnation+action_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
