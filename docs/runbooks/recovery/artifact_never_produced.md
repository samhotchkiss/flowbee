<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `artifact_never_produced`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `orchestrator`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_artifact_never_produced`

## Invariant

a completed build binds one owned artifact

## Authoritative predicate

awaiting_artifact is past state_due_at without an owned current artifact

## Repair

Run the domain action `reconcile_owned_artifact` under fence `delivery_state_version+artifact_version`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
