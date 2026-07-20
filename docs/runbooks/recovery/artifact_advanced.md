<!-- Code generated from protocol/flowbee/v2/actor-protocol.yaml; DO NOT EDIT. -->
# `artifact_advanced`

Protocol `2.0` · bundle `sha256:16276a800d760bf9b4a52d210a7d3fe35fae3f6edfeaa992f7313ae93aa539b2`

- Severity: `info`
- Owner: `flowbee`
- Escalates to: `operational_agent`
- Automatic: `true`
- Maximum automatic attempts: `1`
- Named proof: `head_advance_cancels_stale_effects`

## Invariant

old-head actions and verdicts never affect a new head

## Authoritative predicate

observed head or base differs from bound action and verdict SHAs

## Repair

Run the domain action `cancel_superseded_and_return_to_ci` under fence `artifact_version+head_sha+base_sha`. Reuse the existing identity and idempotency key. Never mutate state directly or invent a replacement action.
