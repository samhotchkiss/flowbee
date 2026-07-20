<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `action_ack_overdue`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `transport_is_not_stage_success`

## Invariant

transport and stage acknowledgement remain distinct

## Authoritative predicate

submitted action has no independent stage evidence before its due clock

## Repair

Run the domain action `reconcile_stage_evidence` under fence `action_id+action_epoch+artifact_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
