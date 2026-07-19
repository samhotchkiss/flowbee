<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `epic_admission_outcome_uncertain`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `orchestrator`
- Escalates to: `flowbee`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `admission_lost_ack_one_epic`

## Invariant

admission retry queries the original idempotency key first

## Authoritative predicate

an admission action outcome is unknown

## Repair

Run the domain action `query_then_retry_same_admission` under fence `project_id+work_intent_id+intent_version+contract_hash`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
