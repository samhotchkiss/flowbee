<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `work_intent_capture_unacked`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `interactor`
- Escalates to: `flowbee`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `interactor_restart_recovers_intent`

## Invariant

ordinary human requests are durable before acceptance is reported

## Authoritative predicate

conversation message has no work-intent persistence acknowledgement

## Repair

Run the domain action `persist_same_work_intent` under fence `project_id+source_message_id+intent_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
