<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `conflict_resolution_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_conflict_resolution_stalled`

## Invariant

a conflict resolution returns through CI and independent review

## Authoritative predicate

conflict_resolution is past fact_progress_at without a newer resolved head

## Repair

Run the domain action `verify_or_relaunch_conflict_resolution` under fence `delivery_state_version+head_sha+action_epoch`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
