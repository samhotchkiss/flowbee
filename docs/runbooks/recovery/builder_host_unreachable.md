<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `builder_host_unreachable`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `host_failure_isolated`

## Invariant

a builder target is observed from its exact registered host and store

## Authoritative predicate

expected host or Driver store is unreachable beyond threshold

## Repair

Run the domain action `verify_host_then_hold_or_rehome` under fence `host_id+store_id+target_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
