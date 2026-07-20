<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `builder_progress_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_builder_progress_stalled`

## Invariant

a building delivery produces durable progress

## Authoritative predicate

building delivery is past fact_progress_at without a newer builder fact

## Repair

Run the domain action `verify_or_relaunch_builder` under fence `delivery_state_version+lease_epoch+agent_run_id`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
