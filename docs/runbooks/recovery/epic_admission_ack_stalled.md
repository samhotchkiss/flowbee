<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `epic_admission_ack_stalled`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `interactor`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `admission_ack_restart`

## Invariant

admitted epic identity is acknowledged upstream exactly once

## Authoritative predicate

admitted epic has no acknowledged upstream admission response

## Repair

Run the domain action `ensure_admission_ack` under fence `project_id+epic_id+admission_key`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
