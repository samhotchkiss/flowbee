<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `ci_never_green`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `warning`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `6`
- Named proof: `recovery_ci_never_green`

## Invariant

review requires real CI success at current head

## Authoritative predicate

awaiting_ci is past state_due_at without complete required checks

## Repair

Run the domain action `refresh_ci_facts` under fence `artifact_version+head_sha+base_sha`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
