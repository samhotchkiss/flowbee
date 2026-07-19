<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `work_intent_promotion_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `ready_intent_auto_promotes`

## Invariant

a ready intent automatically reaches the paired Orchestrator

## Authoritative predicate

ready work intent lacks a current route action or acknowledgement

## Repair

Run the domain action `ensure_orchestrator_route` under fence `project_id+work_intent_id+intent_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
