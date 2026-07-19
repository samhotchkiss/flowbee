<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `review_capacity_unavailable`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `recovery_review_capacity_unavailable`

## Invariant

queued review has an eligible distinct-family route or visible hold

## Authoritative predicate

review_queued is past state_due_at with zero routable distinct-family seats

## Repair

Run the domain action `reconcile_review_capacity` under fence `delivery_state_version+capacity_generation`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
