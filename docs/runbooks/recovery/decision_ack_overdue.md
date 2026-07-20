<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `decision_ack_overdue`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `decision_ack_restart`

## Invariant

an immutable typed response reaches every required actor

## Authoritative predicate

a current response is unacknowledged past its due clock

## Repair

Run the domain action `ensure_decision_ack_chain` under fence `request_id+subject_hash+response_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
