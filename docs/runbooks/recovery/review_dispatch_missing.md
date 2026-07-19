<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `review_dispatch_missing`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `critical`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `3`
- Named proof: `production_drop_4950_4951`

## Invariant

each eligible current artifact has one native review obligation

## Authoritative predicate

current CI-green artifact has no verdict live reviewer or native review job past dispatch_due_at

## Repair

Run the domain action `ensure_native_review` under fence `delivery_state_version+head_sha+base_sha`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
