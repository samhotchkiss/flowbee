<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `hold_overdue`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `false`
- Maximum automatic attempts: `0`
- Named proof: `recovery_hold_overdue`

## Invariant

visible holds have a typed owner and disposition

## Authoritative predicate

paused or needs_human is past state_due_at without a response

## Repair

Run the domain action `request_typed_disposition` under fence `delivery_state_version+decision_subject_hash`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
