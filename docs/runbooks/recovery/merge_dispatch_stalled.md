<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `merge_dispatch_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_merge_dispatch_stalled`

## Invariant

approved current head has one exact-head merge effect

## Authoritative predicate

merge_queued or merging is overdue without a current merge fact

## Repair

Run the domain action `verify_or_dispatch_exact_head_merge` under fence `delivery_state_version+verdict_head_sha+action_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
