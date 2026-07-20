<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `review_verdict_overdue`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `hung_reviewer_fenced`

## Invariant

a live reviewer lease produces durable progress or is fenced

## Authoritative predicate

in_review is past last_reviewer_fact_at without a current verdict

## Repair

Run the domain action `fence_and_requeue_reviewer` under fence `delivery_state_version+review_lease_epoch+head_sha`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
